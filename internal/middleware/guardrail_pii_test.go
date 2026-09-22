package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// piiGuardrail builds the middleware with a real inline PII scanner, the
// gateway policies listed globally, and one tenant behind it.
// The tenant is taken as a store.Store rather than the concrete double, so a
// test can pass one that records what the guardrail found as well.
func piiGuardrail(t *testing.T, policies []string, tenant store.Store) *Guardrail {
	t.Helper()
	engine := guardrail.NewEngine(
		[]guardrail.Scanner{guardrail.NewPIIScanner(), guardrail.NewSecretsScanner()},
		tenant, nil, nil, true, guardLogger(),
	)
	g := NewGuardrail(config.GuardrailConfig{
		Enabled:    true,
		Guardrails: policies,
	}, engine, tenant, guardLogger())
	if g == nil {
		t.Fatal("expected a guardrail middleware")
	}
	return g
}

// piiPipelineRequest mirrors the production ordering: the early sensitive-data
// pass runs before an arbitrary downstream component, while the regular
// guardrail middleware remains later for all other policies.
func piiPipelineRequest(t *testing.T, g *Guardrail, path, body string) (*httptest.ResponseRecorder, string, string) {
	t.Helper()
	var observedBeforeLateGuard, forwarded string

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded = string(b)
		if r.ContentLength != int64(len(b)) {
			t.Errorf("ContentLength %d does not match forwarded body of %d bytes", r.ContentLength, len(b))
		}
		w.WriteHeader(http.StatusOK)
	})
	lateGuard := g.Middleware(upstream)
	downstreamComponent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		observedBeforeLateGuard = string(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
		lateGuard.ServeHTTP(w, r)
	})
	handler := g.SensitiveDataMiddleware(downstreamComponent)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{
		KeyHash: "hash", TeamID: "team_1",
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr, observedBeforeLateGuard, forwarded
}

// piiRequest sends one prompt through the middleware and reports the recorder
// plus the body the upstream handler would have received.
func piiRequest(t *testing.T, g *Guardrail, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var forwarded string
	handler := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded = string(b)
		if r.ContentLength != int64(len(b)) {
			t.Errorf("ContentLength %d does not match forwarded body of %d bytes", r.ContentLength, len(b))
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{
		KeyHash: "hash", TeamID: "team_1",
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr, forwarded
}

const piiPrompt = `{"model":"gpt-4o","temperature":1.0,"messages":[{"role":"user","content":"Scrivimi a mario.rossi@example.com"}]}`

func TestGuardrailPII_RedactionForwardsPlaceholder(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
	})

	rr, forwarded := piiRequest(t, g, piiPrompt)

	if rr.Code != http.StatusOK {
		t.Fatalf("redaction must forward the request, got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(forwarded, "mario.rossi@example.com") {
		t.Errorf("PII reached the upstream: %s", forwarded)
	}
	if !strings.Contains(forwarded, "[EMAIL_ADDRESS]") {
		t.Errorf("expected an [EMAIL_ADDRESS] placeholder, got %s", forwarded)
	}
	// The rest of the request must survive the rewrite untouched.
	var parsed map[string]any
	dec := json.NewDecoder(strings.NewReader(forwarded))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		t.Fatalf("forwarded body is not valid JSON: %v", err)
	}
	if parsed["model"] != "gpt-4o" {
		t.Errorf("model was altered: %v", parsed["model"])
	}
	if temp, _ := parsed["temperature"].(json.Number); temp.String() != "1.0" {
		t.Errorf("temperature was reformatted: %v", parsed["temperature"])
	}
}

func TestGuardrailPII_BlockingRefusesWithoutForwarding(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data-block"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data-block": true},
	})

	rr, forwarded := piiRequest(t, g, piiPrompt)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
	if forwarded != "" {
		t.Errorf("blocked request was forwarded: %s", forwarded)
	}
	if !strings.Contains(rr.Body.String(), "EMAIL_ADDRESS") {
		t.Errorf("expected the entity in the refusal, got %s", rr.Body.String())
	}
}

// With both policies on, blocking decides: no redacted request slips through.
func TestGuardrailPII_BlockingWinsOverRedaction(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data", "sensitive-data-block"}, &guardrailEntityStore{
		guardrails: map[string]bool{
			"sensitive-data":       true,
			"sensitive-data-block": true,
		},
	})

	rr, forwarded := piiRequest(t, g, piiPrompt)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected blocking to win, got %d", rr.Code)
	}
	if forwarded != "" {
		t.Errorf("request was forwarded despite blocking: %s", forwarded)
	}
}

// Blocking is opt-in: a tenant that never enabled it keeps being redacted even
// when the gateway lists the policy globally.
func TestGuardrailPII_BlockingIsOptIn(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data", "sensitive-data-block"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
	})

	rr, forwarded := piiRequest(t, g, piiPrompt)

	if rr.Code != http.StatusOK {
		t.Fatalf("opt-in policy must not block by default, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(forwarded, "[EMAIL_ADDRESS]") {
		t.Errorf("expected redaction to still apply, got %s", forwarded)
	}
}

func TestGuardrailPII_BlockingStaysOffWithoutTenantSettings(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data", "sensitive-data-block"}, &guardrailEntityStore{
		guardrails: nil,
	})

	rr, observed, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", piiPrompt)
	if rr.Code != http.StatusOK {
		t.Fatalf("a tenant without stored settings must keep blocking off, got %d: %s", rr.Code, rr.Body.String())
	}
	for stage, body := range map[string]string{"downstream component": observed, "upstream": forwarded} {
		if strings.Contains(body, "mario.rossi@example.com") || !strings.Contains(body, "[EMAIL_ADDRESS]") {
			t.Errorf("redaction default did not apply at %s: %s", stage, body)
		}
	}
}

// A tenant that switched redaction off gets neither policy: the prompt goes
// through as written.
func TestGuardrailPII_RedactionCanBeDisabled(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": false},
	})

	rr, forwarded := piiRequest(t, g, piiPrompt)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected the request to pass, got %d", rr.Code)
	}
	if !strings.Contains(forwarded, "mario.rossi@example.com") {
		t.Errorf("prompt was rewritten with the policy off: %s", forwarded)
	}
}

// The entity selection is shared: what redaction rewrites is what blocking
// would have refused, and nothing outside the selection is touched.
func TestGuardrailPII_EntitySelectionIsShared(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"mario.rossi@example.com sta su 192.168.1.100"}]}`

	rr, forwarded := piiRequest(t, piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
		entities:   entitySelection("IP_ADDRESS"),
	}), body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(forwarded, "[IP_ADDRESS]") {
		t.Errorf("selected entity was not redacted: %s", forwarded)
	}
	if !strings.Contains(forwarded, "mario.rossi@example.com") {
		t.Errorf("unselected entity was redacted: %s", forwarded)
	}

	rr, _ = piiRequest(t, piiGuardrail(t, []string{"sensitive-data-block"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data-block": true},
		entities:   entitySelection("IP_ADDRESS"),
	}), body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected the same selection to block, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "IP_ADDRESS") {
		t.Errorf("expected IP_ADDRESS in the refusal, got %s", rr.Body.String())
	}

	rr, forwarded = piiRequest(t, piiGuardrail(t, []string{"sensitive-data-block"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data-block": true},
		entities:   entitySelection("US_SSN"),
	}), body)
	if rr.Code != http.StatusOK {
		t.Fatalf("entities outside the selection must not block, got %d", rr.Code)
	}
	if !strings.Contains(forwarded, "192.168.1.100") {
		t.Errorf("blocking policy must not rewrite the prompt: %s", forwarded)
	}
}

// Unticking every entity means the prompt is forwarded as written.
func TestGuardrailPII_EmptySelectionRedactsNothing(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
		entities:   entitySelection(),
	})

	rr, forwarded := piiRequest(t, g, piiPrompt)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if forwarded != piiPrompt {
		t.Errorf("body was rewritten despite an empty selection: %s", forwarded)
	}
}

// A tenant that never opened the entity dialog keeps full coverage.
func TestGuardrailPII_NoSelectionRedactsEverything(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
	})

	body := `{"messages":[{"role":"user","content":"mario.rossi@example.com sta su 192.168.1.100"}]}`
	_, forwarded := piiRequest(t, g, body)

	if strings.Contains(forwarded, "mario.rossi@example.com") || strings.Contains(forwarded, "192.168.1.100") {
		t.Errorf("PII survived without a selection: %s", forwarded)
	}
}

func TestGuardrailPII_CleanPromptIsForwardedUnchanged(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
	})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Come funziona il caching?"}]}`
	rr, forwarded := piiRequest(t, g, body)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if forwarded != body {
		t.Errorf("clean body was rewritten:\n got: %s\nwant: %s", forwarded, body)
	}
}

func TestGuardrailPII_RedactsBeforeDownstreamComponents(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data", "prompt-injection"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true, "prompt-injection": true},
	})

	rr, observed, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", piiPrompt)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	for stage, body := range map[string]string{"downstream component": observed, "upstream": forwarded} {
		if strings.Contains(body, "mario.rossi@example.com") {
			t.Errorf("raw PII reached %s: %s", stage, body)
		}
		if !strings.Contains(body, "[EMAIL_ADDRESS]") {
			t.Errorf("placeholder missing at %s: %s", stage, body)
		}
	}
}

func TestGuardrailPII_BlocksBeforeDownstreamComponents(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data-block"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data-block": true},
	})

	rr, observed, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", piiPrompt)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if observed != "" || forwarded != "" {
		t.Fatalf("blocked PII crossed the early policy boundary: observed=%q forwarded=%q", observed, forwarded)
	}
}

func TestGuardrailSensitiveData_BlocksSecretsBeforeDownstreamComponents(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
	})
	body := `{"messages":[{"role":"user","content":"token=sk-123456789012345678901234567890"}]}`

	rr, observed, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected secret to be blocked with 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if observed != "" || forwarded != "" {
		t.Fatalf("secret crossed the early policy boundary: observed=%q forwarded=%q", observed, forwarded)
	}
}

func TestGuardrailPII_ResponsesAndCompletionsUseRealMiddlewarePath(t *testing.T) {
	g := piiGuardrail(t, []string{"sensitive-data"}, &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
	})

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "responses structured input",
			path: "/v1/responses",
			body: `{"instructions":"scrivi a admin@example.com","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"server 2001:db8::1"}]}]}`,
		},
		{
			name: "responses string input",
			path: "/v1/responses",
			body: `{"input":"scrivi a admin@example.com"}`,
		},
		{
			name: "legacy completion prompt",
			path: "/v1/completions",
			body: `{"prompt":"scrivi a admin@example.com"}`,
		},
		{
			name: "legacy completion prompt array",
			path: "/v1/completions",
			body: `{"prompt":["scrivi a admin@example.com","server 10.0.0.1"]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr, observed, forwarded := piiPipelineRequest(t, g, tc.path, tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			for stage, body := range map[string]string{"downstream component": observed, "upstream": forwarded} {
				if strings.Contains(body, "admin@example.com") || strings.Contains(body, "2001:db8::1") || strings.Contains(body, "10.0.0.1") {
					t.Errorf("PII survived at %s: %s", stage, body)
				}
			}
		})
	}
}

func TestGuardrailPII_AllPortalEntitiesRedactAndBlock(t *testing.T) {
	var entities, fragments []string
	for _, sample := range guardrailPIITestMatrix {
		entities = append(entities, sample.entity)
		fragments = append(fragments, sample.text)
	}
	bodyBytes, err := json.Marshal(map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]string{{
			"role":    "user",
			"content": strings.Join(fragments, "; "),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)

	redactionTenant := &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
		entities:   entitySelection(entities...),
	}
	rr, observed, forwarded := piiPipelineRequest(t, piiGuardrail(t, []string{"sensitive-data"}, redactionTenant), "/v1/chat/completions", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("all-entity redaction returned %d: %s", rr.Code, rr.Body.String())
	}
	for _, sample := range guardrailPIITestMatrix {
		for stage, value := range map[string]string{"downstream component": observed, "upstream": forwarded} {
			if strings.Contains(value, sample.span) {
				t.Errorf("%s survived all-entity redaction at %s: %s", sample.entity, stage, value)
			}
			if !strings.Contains(value, "["+sample.entity+"]") {
				t.Errorf("[%s] missing at %s: %s", sample.entity, stage, value)
			}
		}
	}

	blockingTenant := &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data-block": true},
		entities:   entitySelection(entities...),
	}
	rr, observed, forwarded = piiPipelineRequest(t, piiGuardrail(t, []string{"sensitive-data-block"}, blockingTenant), "/v1/chat/completions", body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("all-entity blocking returned %d: %s", rr.Code, rr.Body.String())
	}
	if observed != "" || forwarded != "" {
		t.Fatalf("all-entity blocking reached downstream: observed=%q forwarded=%q", observed, forwarded)
	}
}

var guardrailPIITestMatrix = []struct {
	entity string
	text   string
	span   string
}{
	{"EMAIL_ADDRESS", "Scrivimi a mario.rossi@example.com", "mario.rossi@example.com"},
	{"PHONE_NUMBER", "Chiamami al +39 340 1234567", "+39 340 1234567"},
	{"PERSON", "Mi chiamo Mario Rossi", "Mario Rossi"},
	{"CREDIT_CARD", "Carta 4111 1111 1111 1111", "4111 1111 1111 1111"},
	{"CRYPTO", "Wallet 0x52908400098527886E0F7030069857D2E4169EE7", "0x52908400098527886E0F7030069857D2E4169EE7"},
	{"IBAN_CODE", "IBAN DE89370400440532013000", "DE89370400440532013000"},
	{"IP_ADDRESS", "Server 192.168.1.100", "192.168.1.100"},
	{"US_SSN", "SSN 123-45-6789", "123-45-6789"},
	{"US_BANK_NUMBER", "Account number 12345678901", "12345678901"},
	{"LOCATION", "Sono residente a Milano", "Milano"},
}

func TestRedactBody_ContentBlocksAndResponsesInput(t *testing.T) {
	scanner := guardrail.NewPIIScanner()
	redact := func(text string) (string, []string) { return scanner.Redact(text, nil) }

	t.Run("anthropic content blocks", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"scrivi a a@b.com"},{"type":"image","source":"…"}]}]}`)
		out, entities := redactBody(body, redact)
		if !strings.Contains(string(out), "[EMAIL_ADDRESS]") {
			t.Errorf("block text not redacted: %s", out)
		}
		if !strings.Contains(string(out), `"type":"image"`) {
			t.Errorf("non-text block was dropped: %s", out)
		}
		if len(entities) != 1 || entities[0] != "EMAIL_ADDRESS" {
			t.Errorf("expected [EMAIL_ADDRESS], got %v", entities)
		}
	})

	t.Run("responses instructions and input", func(t *testing.T) {
		body := []byte(`{"instructions":"rispondi a a@b.com","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"il server è 10.0.0.1"}]}]}`)
		out, entities := redactBody(body, redact)
		if strings.Contains(string(out), "a@b.com") || strings.Contains(string(out), "10.0.0.1") {
			t.Errorf("PII survived in a Responses body: %s", out)
		}
		if len(entities) != 2 {
			t.Errorf("expected two entities, got %v", entities)
		}
	})

	t.Run("responses string input", func(t *testing.T) {
		body := []byte(`{"input":"scrivi a a@b.com"}`)
		out, _ := redactBody(body, redact)
		if !strings.Contains(string(out), "[EMAIL_ADDRESS]") {
			t.Errorf("string input not redacted: %s", out)
		}
	})

	t.Run("non json body is returned untouched", func(t *testing.T) {
		body := []byte("not json")
		out, entities := redactBody(body, redact)
		if string(out) != "not json" || entities != nil {
			t.Errorf("unexpected rewrite: %s %v", out, entities)
		}
	})
}

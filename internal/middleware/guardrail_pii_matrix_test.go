package middleware

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// piiChatBody wraps prompt text in a chat completions request.
func piiChatBody(t *testing.T, prompt string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestGuardrailPII_PolicyMatrix walks every on/off combination of the two
// policies. The point of the split is that each cell is distinct: only one of
// them rewrites, only one of them refuses, and together the refusal wins.
func TestGuardrailPII_PolicyMatrix(t *testing.T) {
	prompt := "Scrivimi a mario.rossi@example.com"
	body := piiChatBody(t, prompt)

	cases := []struct {
		name       string
		redact     bool
		block      bool
		wantStatus int
		wantPII    bool // the address still readable upstream
		wantMarker bool // an [EMAIL_ADDRESS] placeholder upstream
	}{
		{name: "entrambe spente", redact: false, block: false, wantStatus: http.StatusOK, wantPII: true},
		{name: "solo redazione", redact: true, block: false, wantStatus: http.StatusOK, wantMarker: true},
		{name: "solo blocco", redact: false, block: true, wantStatus: http.StatusForbidden},
		{name: "entrambe accese", redact: true, block: true, wantStatus: http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant := &guardrailEntityStore{
				guardrails: map[string]bool{
					"sensitive-data":       tc.redact,
					"sensitive-data-block": tc.block,
				},
			}
			g := piiGuardrail(t, []string{"sensitive-data", "sensitive-data-block"}, tenant)
			rr, observed, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", body)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus == http.StatusForbidden {
				if observed != "" || forwarded != "" {
					t.Fatalf("refused request still travelled: observed=%q forwarded=%q", observed, forwarded)
				}
				return
			}
			if got := strings.Contains(forwarded, prompt); got != tc.wantPII {
				t.Errorf("PII readable upstream = %v, want %v: %s", got, tc.wantPII, forwarded)
			}
			if got := strings.Contains(forwarded, "[EMAIL_ADDRESS]"); got != tc.wantMarker {
				t.Errorf("placeholder upstream = %v, want %v: %s", got, tc.wantMarker, forwarded)
			}
			if forwarded != observed {
				t.Errorf("downstream and upstream disagree:\n  downstream: %s\n  upstream:   %s", observed, forwarded)
			}
		})
	}
}

// TestGuardrailPII_OneEntityAtATime is the cross check: a prompt carrying all
// ten entities, with exactly one of them selected. Only that entity may change,
// the other nine must reach the provider verbatim — and the same single
// selection must be what blocking refuses on.
func TestGuardrailPII_OneEntityAtATime(t *testing.T) {
	var fragments []string
	for _, sample := range guardrailPIITestMatrix {
		fragments = append(fragments, sample.text)
	}
	fullPrompt := strings.Join(fragments, "; ")
	fullBody := piiChatBody(t, fullPrompt)

	for _, sample := range guardrailPIITestMatrix {
		t.Run(sample.entity, func(t *testing.T) {
			// --- redaction: only this entity is rewritten ---
			redactTenant := &guardrailEntityStore{
				guardrails: map[string]bool{"sensitive-data": true},
				entities:   entitySelection(sample.entity),
			}
			rr, _, forwarded := piiPipelineRequest(t,
				piiGuardrail(t, []string{"sensitive-data"}, redactTenant), "/v1/chat/completions", fullBody)
			if rr.Code != http.StatusOK {
				t.Fatalf("redaction returned %d: %s", rr.Code, rr.Body.String())
			}
			if strings.Contains(forwarded, sample.span) {
				t.Errorf("selected %s survived: %s", sample.entity, forwarded)
			}
			if !strings.Contains(forwarded, "["+sample.entity+"]") {
				t.Errorf("no [%s] placeholder: %s", sample.entity, forwarded)
			}
			for _, other := range guardrailPIITestMatrix {
				if other.entity == sample.entity {
					continue
				}
				if !strings.Contains(forwarded, other.span) {
					t.Errorf("unselected %s was redacted while only %s was on: %s", other.entity, sample.entity, forwarded)
				}
				if strings.Contains(forwarded, "["+other.entity+"]") {
					t.Errorf("unexpected [%s] placeholder while only %s was on: %s", other.entity, sample.entity, forwarded)
				}
			}

			// --- blocking: the same selection refuses, naming this entity ---
			blockTenant := &guardrailEntityStore{
				guardrails: map[string]bool{"sensitive-data-block": true},
				entities:   entitySelection(sample.entity),
			}
			rr, observed, forwarded := piiPipelineRequest(t,
				piiGuardrail(t, []string{"sensitive-data-block"}, blockTenant), "/v1/chat/completions", fullBody)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("blocking on %s returned %d: %s", sample.entity, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), sample.entity) {
				t.Errorf("refusal does not name %s: %s", sample.entity, rr.Body.String())
			}
			if observed != "" || forwarded != "" {
				t.Errorf("refused request travelled: observed=%q forwarded=%q", observed, forwarded)
			}

			// --- the same entity selected, but absent from the prompt ---
			var withoutEntity []string
			for _, other := range guardrailPIITestMatrix {
				if other.entity != sample.entity {
					withoutEntity = append(withoutEntity, other.text)
				}
			}
			cleanPrompt := strings.Join(withoutEntity, "; ")
			cleanBody := piiChatBody(t, cleanPrompt)

			rr, _, forwarded = piiPipelineRequest(t,
				piiGuardrail(t, []string{"sensitive-data-block"}, blockTenant), "/v1/chat/completions", cleanBody)
			if rr.Code != http.StatusOK {
				t.Fatalf("blocking on absent %s returned %d: %s", sample.entity, rr.Code, rr.Body.String())
			}
			if forwarded != cleanBody {
				t.Errorf("blocking policy altered the prompt: %s", forwarded)
			}

			rr, _, forwarded = piiPipelineRequest(t,
				piiGuardrail(t, []string{"sensitive-data"}, redactTenant), "/v1/chat/completions", cleanBody)
			if rr.Code != http.StatusOK {
				t.Fatalf("redaction on absent %s returned %d: %s", sample.entity, rr.Code, rr.Body.String())
			}
			if forwarded != cleanBody {
				t.Errorf("redaction touched a prompt without %s: %s", sample.entity, forwarded)
			}
		})
	}
}

// TestGuardrailPII_ConfigChangesTakeEffectPerRequest flips the tenant's
// settings under a live middleware. Both the policy switch and the entity
// selection are read per request, so a change in the portal must show up on the
// next call without restarting the gateway — and nothing may be cached in
// between.
func TestGuardrailPII_ConfigChangesTakeEffectPerRequest(t *testing.T) {
	tenant := &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
		entities:   entitySelection("EMAIL_ADDRESS"),
	}
	g := piiGuardrail(t, []string{"sensitive-data", "sensitive-data-block"}, tenant)
	body := piiChatBody(t, "Scrivimi a mario.rossi@example.com")

	redacted := func(t *testing.T, step string) {
		t.Helper()
		rr, _, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", step, rr.Code)
		}
		if !strings.Contains(forwarded, "[EMAIL_ADDRESS]") {
			t.Errorf("%s: expected redaction, got %s", step, forwarded)
		}
	}
	untouched := func(t *testing.T, step string) {
		t.Helper()
		rr, _, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", step, rr.Code)
		}
		if forwarded != body {
			t.Errorf("%s: expected the prompt untouched, got %s", step, forwarded)
		}
	}

	redacted(t, "selezione iniziale")

	tenant.entities = entitySelection() // tolte tutte le spunte
	untouched(t, "entità deselezionate")

	tenant.entities = entitySelection("EMAIL_ADDRESS")
	redacted(t, "entità riselezionata")

	tenant.guardrails = map[string]bool{"sensitive-data": false}
	untouched(t, "policy spenta")

	tenant.guardrails = map[string]bool{"sensitive-data": true}
	redacted(t, "policy riaccesa")

	tenant.guardrails = map[string]bool{"sensitive-data": true, "sensitive-data-block": true}
	rr, _, forwarded := piiPipelineRequest(t, g, "/v1/chat/completions", body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("blocco acceso a caldo: status %d", rr.Code)
	}
	if forwarded != "" {
		t.Errorf("blocco acceso a caldo: la richiesta è passata: %s", forwarded)
	}

	tenant.guardrails = map[string]bool{"sensitive-data": true, "sensitive-data-block": false}
	redacted(t, "blocco rispento")
}

// The Italian identifiers have no checkbox in the portal, so they follow the
// policy that is on regardless of the entity selection — including an empty one.
func TestGuardrailPII_ItalianIdentifiersFollowThePolicy(t *testing.T) {
	body := piiChatBody(t, "Il mio codice fiscale è RSSMRA85M01H501Q")

	redactTenant := &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": true},
		entities:   entitySelection(),
	}
	rr, _, forwarded := piiPipelineRequest(t,
		piiGuardrail(t, []string{"sensitive-data"}, redactTenant), "/v1/chat/completions", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if strings.Contains(forwarded, "RSSMRA85M01H501Q") || !strings.Contains(forwarded, "[CODICE_FISCALE]") {
		t.Errorf("codice fiscale non redatto con selezione vuota: %s", forwarded)
	}

	blockTenant := &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data-block": true},
		entities:   entitySelection(),
	}
	rr, _, forwarded = piiPipelineRequest(t,
		piiGuardrail(t, []string{"sensitive-data-block"}, blockTenant), "/v1/chat/completions", body)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("codice fiscale non bloccato con selezione vuota: %d", rr.Code)
	}
	if forwarded != "" {
		t.Errorf("richiesta inoltrata: %s", forwarded)
	}

	offTenant := &guardrailEntityStore{
		guardrails: map[string]bool{"sensitive-data": false, "sensitive-data-block": false},
	}
	rr, _, forwarded = piiPipelineRequest(t,
		piiGuardrail(t, []string{"sensitive-data", "sensitive-data-block"}, offTenant), "/v1/chat/completions", body)
	if rr.Code != http.StatusOK || forwarded != body {
		t.Errorf("con entrambe spente il prompt deve passare intatto: %d %s", rr.Code, forwarded)
	}
}

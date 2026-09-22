package middleware

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
)

var piiTestScanner = guardrail.NewPIIScanner()

// A file an agent reads does not arrive as a typed user message: it comes back
// as a tool result. These bodies used to be invisible to the guardrail — the
// scanner extracted nothing from them, so neither policy acted — which meant a
// whole document could reach the provider untouched.
func TestGuardrailPII_ToolResultsAreScannedAndRedacted(t *testing.T) {
	cases := []struct {
		name string
		body string
		span string // must not survive redaction
	}{
		{
			name: "anthropic system prompt",
			body: `{"system":"L'operatore è mario.rossi@example.com","messages":[{"role":"user","content":"ciao"}]}`,
			span: "mario.rossi@example.com",
		},
		{
			name: "anthropic system blocks",
			body: `{"system":[{"type":"text","text":"Scrivi a mario.rossi@example.com"}],"messages":[{"role":"user","content":"ciao"}]}`,
			span: "mario.rossi@example.com",
		},
		{
			name: "tool result string",
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"router 192.168.1.100 admin mario.rossi@example.com"}]}]}`,
			span: "mario.rossi@example.com",
		},
		{
			name: "tool result blocks",
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"IBAN DE89370400440532013000"}]}]}]}`,
			span: "DE89370400440532013000",
		},
		{
			name: "responses function call output",
			body: `{"input":[{"type":"function_call_output","call_id":"c1","output":"admin mario.rossi@example.com"}]}`,
			span: "mario.rossi@example.com",
		},
		{
			name: "responses function call output blocks",
			body: `{"input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"output_text","text":"admin mario.rossi@example.com"}]}]}`,
			span: "mario.rossi@example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The scanner has to see the text at all: with nothing extracted,
			// both policies silently do nothing.
			if len(extractMessages([]byte(tc.body))) == 0 {
				t.Fatalf("nothing extracted from %s", tc.body)
			}

			redactTenant := &guardrailEntityStore{
				guardrails: map[string]bool{"sensitive-data": true},
			}
			rr, observed, forwarded := piiPipelineRequest(t,
				piiGuardrail(t, []string{"sensitive-data"}, redactTenant), "/v1/chat/completions", tc.body)
			if rr.Code != http.StatusOK {
				t.Fatalf("redaction returned %d: %s", rr.Code, rr.Body.String())
			}
			for stage, value := range map[string]string{"downstream": observed, "upstream": forwarded} {
				if strings.Contains(value, tc.span) {
					t.Errorf("%s survived at %s: %s", tc.span, stage, value)
				}
			}

			blockTenant := &guardrailEntityStore{
				guardrails: map[string]bool{"sensitive-data-block": true},
			}
			rr, observed, forwarded = piiPipelineRequest(t,
				piiGuardrail(t, []string{"sensitive-data-block"}, blockTenant), "/v1/chat/completions", tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("blocking returned %d: %s", rr.Code, rr.Body.String())
			}
			if observed != "" || forwarded != "" {
				t.Errorf("refused request travelled: observed=%q forwarded=%q", observed, forwarded)
			}
		})
	}
}

// Redaction must rewrite the payload without disturbing the envelope the
// provider needs to match a result back to its call.
func TestRedactBody_ToolResultKeepsItsEnvelope(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_123","is_error":false,"content":"admin a@b.com"},` +
		`{"type":"text","text":"riassumi"}]}]}`)

	out, entities := redactBody(body, func(s string) (string, []string) {
		return piiTestScanner.Redact(s, nil)
	})

	rewritten := string(out)
	if strings.Contains(rewritten, "a@b.com") {
		t.Errorf("tool result not redacted: %s", rewritten)
	}
	if !strings.Contains(rewritten, "[EMAIL_ADDRESS]") {
		t.Errorf("no placeholder: %s", rewritten)
	}
	if !strings.Contains(rewritten, `"tool_use_id":"toolu_123"`) {
		t.Errorf("tool_use_id was dropped: %s", rewritten)
	}
	if !strings.Contains(rewritten, `"is_error":false`) {
		t.Errorf("block metadata was dropped: %s", rewritten)
	}
	if !strings.Contains(rewritten, `"riassumi"`) {
		t.Errorf("sibling text block was lost: %s", rewritten)
	}
	if len(entities) != 1 || entities[0] != "EMAIL_ADDRESS" {
		t.Errorf("expected [EMAIL_ADDRESS], got %v", entities)
	}
}

// Nesting past a sane depth is a body shaped to exhaust the scanner, not a
// prompt: it must terminate rather than recurse.
func TestRedactBody_StopsAtNestingLimit(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":[` +
		`{"type":"tool_result","content":[{"type":"tool_result","content":[` +
		`{"type":"tool_result","content":[{"type":"tool_result","content":[` +
		`{"type":"text","text":"a@b.com"}]}]}]}]}]}]}]}`)

	done := make(chan struct{})
	go func() {
		defer close(done)
		redactBody(body, func(s string) (string, []string) {
			return piiTestScanner.Redact(s, nil)
		})
		extractMessages(body)
	}()
	<-done
}

package middleware

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// recordingStore is a tenant store that also keeps the security events written
// through it, the way the portal's table does.
type recordingStore struct {
	guardrailEntityStore
	events []store.SecurityEvent
	fail   error
}

func (s *recordingStore) LogSecurityEvent(_ context.Context, event store.SecurityEvent) error {
	if s.fail != nil {
		return s.fail
	}
	s.events = append(s.events, event)
	return nil
}

func recordingTenant(guardrails map[string]bool, entities *[]string) *recordingStore {
	return &recordingStore{guardrailEntityStore: guardrailEntityStore{
		guardrails: guardrails,
		entities:   entities,
	}}
}

func findingsOf(events []store.SecurityEvent, eventType string) []string {
	var found []string
	for _, e := range events {
		if e.EventType == eventType {
			found = append(found, e.Finding)
		}
	}
	return found
}

// The card this feeds counts rows in security_events. Nothing had written one
// since May 2026 — the LLM Guard service that used to do it was replaced by
// this gateway — so "PII protected: 0" was displayed to every tenant while the
// guardrails were redacting and refusing all along.
func TestSecurityEvents_RedactionIsRecorded(t *testing.T) {
	tenant := recordingTenant(map[string]bool{guardPIIRedact: true}, nil)
	g := piiGuardrail(t, []string{guardPIIRedact}, tenant)

	rr, forwarded := piiRequest(t, g, `{"messages":[{"role":"user","content":"write to mario@example.com about card 4111 1111 1111 1111"}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("redaction forwards the request: got %d", rr.Code)
	}
	if !strings.Contains(forwarded, "[EMAIL_ADDRESS]") {
		t.Fatalf("expected the address to be redacted, forwarded: %s", forwarded)
	}

	found := findingsOf(tenant.events, eventPIIDetected)
	if len(found) < 2 {
		t.Fatalf("expected one event per kind of data redacted, got %v", found)
	}
	var sawEmail, sawCard bool
	for _, f := range found {
		sawEmail = sawEmail || f == "EMAIL_ADDRESS"
		sawCard = sawCard || f == "CREDIT_CARD"
	}
	if !sawEmail || !sawCard {
		t.Fatalf("expected EMAIL_ADDRESS and CREDIT_CARD, got %v", found)
	}
}

func TestSecurityEvents_CarryTheKeyAndTeamButNotTheData(t *testing.T) {
	tenant := recordingTenant(map[string]bool{guardPIIRedact: true}, nil)
	g := piiGuardrail(t, []string{guardPIIRedact}, tenant)

	piiRequest(t, g, `{"messages":[{"role":"user","content":"mario@example.com"}]}`)

	if len(tenant.events) == 0 {
		t.Fatal("nothing recorded")
	}
	for _, e := range tenant.events {
		// The portal finds a tenant's events by team, falling back to the key.
		if e.TenantID != "team_1" || e.KeyHash != "hash" {
			t.Fatalf("event is not attributable: team=%q key=%q", e.TenantID, e.KeyHash)
		}
		if e.Timestamp.IsZero() {
			t.Fatal("event has no timestamp")
		}
		// Recording the address in order to report that we stopped it from
		// leaving would be the same leak with a longer retention.
		if strings.Contains(e.Masked+e.Details+e.Finding, "mario@example.com") {
			t.Fatalf("the event stores the data it was protecting: %+v", e)
		}
	}
}

func TestSecurityEvents_ABlockedSecretIsRecordedAsASecret(t *testing.T) {
	// Secrets keep blocking under the redaction policy: an API key is not a
	// PII entity and must never reach the provider.
	tenant := recordingTenant(map[string]bool{guardPIIRedact: true}, nil)
	g := piiGuardrail(t, []string{guardPIIRedact}, tenant)

	rr, _ := piiRequest(t, g, `{"messages":[{"role":"user","content":"use sk-ant-api03-`+strings.Repeat("a", 95)+` please"}]}`)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected the request to be refused, got %d", rr.Code)
	}
	if got := findingsOf(tenant.events, eventSecretDetected); len(got) == 0 {
		t.Fatalf("a refused secret left no trace: %+v", tenant.events)
	}
}

func TestSecurityEvents_BlockedPIIIsRecordedAsPII(t *testing.T) {
	tenant := recordingTenant(map[string]bool{guardPIIBlock: true}, nil)
	g := piiGuardrail(t, []string{guardPIIBlock}, tenant)

	rr, _ := piiRequest(t, g, `{"messages":[{"role":"user","content":"my email is mario@example.com"}]}`)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected the request to be refused, got %d", rr.Code)
	}
	found := findingsOf(tenant.events, eventPIIDetected)
	if len(found) == 0 {
		t.Fatalf("a refused request left no trace: %+v", tenant.events)
	}
	if found[0] != "EMAIL_ADDRESS" {
		t.Fatalf("the event should say what was found, got %q", found[0])
	}
}

func TestSecurityEvents_NothingFoundRecordsNothing(t *testing.T) {
	tenant := recordingTenant(map[string]bool{guardPIIRedact: true}, nil)
	g := piiGuardrail(t, []string{guardPIIRedact}, tenant)

	rr, _ := piiRequest(t, g, `{"messages":[{"role":"user","content":"what is the capital of France"}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if len(tenant.events) != 0 {
		t.Fatalf("a clean prompt should record nothing, got %+v", tenant.events)
	}
}

func TestSecurityEvents_AFailedWriteDoesNotFailTheRequest(t *testing.T) {
	// These rows report on work the guardrail has already done. Refusing the
	// request because the audit trail is unavailable would turn a reporting
	// outage into an outage.
	tenant := recordingTenant(map[string]bool{guardPIIRedact: true}, nil)
	tenant.fail = context.DeadlineExceeded
	g := piiGuardrail(t, []string{guardPIIRedact}, tenant)

	rr, forwarded := piiRequest(t, g, `{"messages":[{"role":"user","content":"mario@example.com"}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected the request to go through, got %d", rr.Code)
	}
	if !strings.Contains(forwarded, "[EMAIL_ADDRESS]") {
		t.Fatalf("the redaction itself must still have happened: %s", forwarded)
	}
}

// A store that cannot record — a test double, an alternative implementation —
// is asked and skipped, not required to grow a method.
func TestSecurityEvents_AStoreThatCannotRecordIsSkipped(t *testing.T) {
	tenant := &guardrailEntityStore{guardrails: map[string]bool{guardPIIRedact: true}}
	g := piiGuardrail(t, []string{guardPIIRedact}, tenant)

	rr, forwarded := piiRequest(t, g, `{"messages":[{"role":"user","content":"mario@example.com"}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(forwarded, "[EMAIL_ADDRESS]") {
		t.Fatalf("expected redaction to still happen: %s", forwarded)
	}
}

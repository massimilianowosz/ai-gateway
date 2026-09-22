package middleware

import (
	"context"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// securityEventStore is the one method this middleware needs to record what a
// guardrail found. Like anonymizationConfigStore it is asked for rather than
// required, so a test double or an alternative Store implementation does not
// have to grow a method to keep compiling — it simply records nothing.
type securityEventStore interface {
	LogSecurityEvent(ctx context.Context, event store.SecurityEvent) error
}

// Event types the portal counts. The Security Defense card reads the first two
// as "PII protected" and "secrets blocked"; anything else lands in the
// violations total and the breakdown below it.
const (
	eventPIIDetected      = "pii_detected"
	eventSecretDetected   = "secret_detected"
	eventSecurityViolated = "security_violation"
)

// eventTypeFor maps a scanner to the bucket the dashboard counts it in.
func eventTypeFor(scanner string) string {
	switch strings.ToLower(scanner) {
	case "pii", "anonymization", "sensitive-data-block":
		return eventPIIDetected
	case "secrets":
		return eventSecretDetected
	default:
		return eventSecurityViolated
	}
}

// recordBlocked writes one event per reason the request was refused.
func (g *Guardrail) recordBlocked(ctx context.Context, keyInfo *store.APIKey, result guardrail.Result) {
	events := make([]store.SecurityEvent, 0, len(result.Findings))
	for _, f := range result.Findings {
		events = append(events, store.SecurityEvent{
			EventType: eventTypeFor(f.Scanner),
			Scanner:   f.Scanner,
			Finding:   f.Category,
			Details:   "blocked: " + f.Reason,
		})
	}
	// The legacy HTTP guardrail answers with scanner names and no findings.
	// It still refused a request, and a refusal that leaves no trace is the
	// gap this whole thing exists to close.
	if len(events) == 0 {
		for _, scanner := range result.TriggeredScanners {
			events = append(events, store.SecurityEvent{
				EventType: eventTypeFor(scanner),
				Scanner:   scanner,
				Details:   "blocked: " + result.BlockedReason,
			})
		}
	}
	g.logSecurityEvents(ctx, keyInfo, events)
}

// recordRedactions writes one event per kind of personal data replaced with a
// placeholder. The request went through: what was prevented is the leak, not
// the call, which is exactly what the card claims to count.
func (g *Guardrail) recordRedactions(ctx context.Context, keyInfo *store.APIKey, entities []string) {
	events := make([]store.SecurityEvent, 0, len(entities))
	for _, entity := range entities {
		events = append(events, store.SecurityEvent{
			EventType: eventPIIDetected,
			Scanner:   "pii",
			Finding:   entity,
			Details:   "redacted before the request left the gateway",
		})
	}
	g.logSecurityEvents(ctx, keyInfo, events)
}

// logSecurityEvents stamps the events with who the request belonged to and
// writes them.
//
// A failed write is logged and dropped. These rows are a report on work the
// guardrail has already done: refusing the request a second time because the
// audit trail is unavailable would turn a reporting outage into an outage.
func (g *Guardrail) logSecurityEvents(ctx context.Context, keyInfo *store.APIKey, events []store.SecurityEvent) {
	if len(events) == 0 {
		return
	}
	writer, ok := g.store.(securityEventStore)
	if !ok || writer == nil {
		return
	}

	now := time.Now().UTC()
	for _, event := range events {
		if keyInfo != nil {
			// The portal filters these by team, and falls back to the key
			// hash. Both are on the key, and neither is the customer's data.
			event.TenantID = keyInfo.TeamID
			event.KeyHash = keyInfo.KeyHash
		}
		event.Timestamp = now
		if err := writer.LogSecurityEvent(ctx, event); err != nil {
			g.logger.Warn("guardrail: failed to record security event",
				"event", event.EventType,
				"finding", event.Finding,
				"error", err,
			)
		}
	}
}

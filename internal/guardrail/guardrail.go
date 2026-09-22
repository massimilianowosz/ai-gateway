package guardrail

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ScanResult is the outcome of a single scanner check.
type ScanResult struct {
	Blocked  bool
	Reason   string
	Scanner  string // e.g. "pii", "injection", "toxicity", "secrets"
	Category string // e.g. "CREDIT_CARD", "harassment", "jailbreak"
	Score    float64

	// Spend tracking: tokens consumed by ML-based scanners (e.g. GPT-nano call).
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// Scanner detects specific types of harmful content.
type Scanner interface {
	// Name returns the guardrail identifier (e.g. "sensitive-data", "prompt-injection").
	Name() string
	// Scan checks messages and returns a result.
	// userText contains only user-role messages; fullText contains all messages.
	Scan(ctx context.Context, userText, fullText string) (ScanResult, error)
}

// piiEntityScanner is implemented by scanners that can restrict themselves to
// the entity names configured in the portal. It is intentionally optional so
// the generic Scanner contract and third-party scanners remain unchanged.
type piiEntityScanner interface {
	ScanWithEntities(ctx context.Context, userText, fullText string, enabledEntities []string) (ScanResult, error)
}

// Message is a role+content pair extracted from a chat request.
type Message struct {
	Role    string
	Content string
}

// Result is the aggregate outcome from all scanners.
type Result struct {
	Action              string   `json:"action"` // "ALLOWED" or "BLOCKED"
	BlockedReason       string   `json:"blocked_reason,omitempty"`
	TriggeredScanners   []string `json:"triggered_scanners,omitempty"`
	TriggeredGuardrails []string `json:"triggered_guardrails,omitempty"`

	// Findings are the individual verdicts behind a block. The scanner names
	// above say that something was found; these say what, which is what the
	// security log has to record — "blocked by pii" and "blocked because a
	// credit card number was in the prompt" are not the same answer.
	Findings []Finding `json:"findings,omitempty"`
}

// Finding is one scanner's reason for blocking a request.
type Finding struct {
	Scanner  string `json:"scanner"`            // "pii", "secrets", "injection", "moderation"
	Category string `json:"category,omitempty"` // "CREDIT_CARD", "AWS_KEY", "jailbreak", ...
	Reason   string `json:"reason,omitempty"`
}

// scannerTimeout bounds a single scanner's Scan/ScanWithEntities call.
// Generous for a classifier meant to be fast (moderation's own doc comment:
// "a fast LLM (e.g. GPT-nano)") without being so tight that a legitimately
// slow-but-working call gets treated as a scanner failure. A var, not a
// const, so a test can shrink it rather than waiting out the real bound.
var scannerTimeout = 15 * time.Second

// Engine orchestrates multiple scanners in parallel.
type Engine struct {
	scanners []Scanner
	store    store.Store
	pc       *pricing.Calculator
	spender  *spend.BatchWriter
	failOpen bool
	logger   *slog.Logger
}

// NewEngine creates a guardrail engine with the given scanners.
func NewEngine(scanners []Scanner, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, failOpen bool, logger *slog.Logger) *Engine {
	return &Engine{
		scanners: scanners,
		store:    db,
		pc:       pc,
		spender:  spender,
		failOpen: failOpen,
		logger:   logger,
	}
}

// guardrailToScanners maps guardrail names to the scanners that refuse a
// request. The two PII policies are deliberately split here:
//
//	sensitive-data       — PII Redaction: PII is rewritten by the proxy
//	                       middleware, not blocked, so the pii scanner is not
//	                       listed. Secrets keep blocking: an API key is not a
//	                       PII entity and must never reach the provider.
//	sensitive-data-block — PII Blocking: refuses the request with 403.
//
// Enabling both is not a contradiction: blocking runs first, so it wins and
// nothing is forwarded.
var guardrailToScanners = map[string][]string{
	"prompt-injection":     {"injection"},
	"toxicity":             {"moderation"},
	"sensitive-data":       {"secrets"},
	"sensitive-data-block": {"pii"},
	// Legacy names: callers that predate the split (workflow nodes, the
	// analyze API) still expect one guardrail that reports both.
	"sensitive-data-protection": {"pii", "secrets"},
	"anonymization":             {"pii"},
	// Direct scanner name aliases (for workflow nodes and API callers)
	"pii":        {"pii"},
	"secrets":    {"secrets"},
	"injection":  {"injection"},
	"moderation": {"moderation"},
}

// Scan runs all enabled scanners concurrently and returns the aggregate result.
// enabledGuardrails filters which scanners to run (nil = run all).
func (e *Engine) Scan(ctx context.Context, messages []Message, enabledGuardrails []string, keyInfo *store.APIKey) Result {
	return e.scan(ctx, messages, enabledGuardrails, nil, keyInfo)
}

// ScanWithPIIEntities applies the portal's per-entity PII selection. A nil
// pointer means no tenant configuration exists and retains the legacy behavior;
// a non-nil empty slice explicitly disables all configurable PII detectors.
func (e *Engine) ScanWithPIIEntities(ctx context.Context, messages []Message, enabledGuardrails []string, piiEntities *[]string, keyInfo *store.APIKey) Result {
	return e.scan(ctx, messages, enabledGuardrails, piiEntities, keyInfo)
}

func (e *Engine) scan(ctx context.Context, messages []Message, enabledGuardrails []string, piiEntities *[]string, keyInfo *store.APIKey) Result {
	userText, fullText := splitByRole(messages)
	if fullText == "" {
		return Result{Action: "ALLOWED"}
	}

	// Determine which scanners to run
	scanners := e.scanners
	if len(enabledGuardrails) > 0 {
		scanners = e.filterScanners(enabledGuardrails)
	}
	if len(scanners) == 0 {
		return Result{Action: "ALLOWED"}
	}

	type scanOutput struct {
		result ScanResult
		err    error
	}

	results := make([]scanOutput, len(scanners))
	var wg sync.WaitGroup
	wg.Add(len(scanners))

	for i, s := range scanners {
		go func(idx int, scanner Scanner) {
			defer wg.Done()
			// GW-03 ("applicare timeout... agli servizi scanner"): a
			// network-backed scanner (moderation calls an upstream LLM)
			// otherwise inherits whatever deadline the caller's own request
			// context carries — often none, since the streaming HTTP client
			// providers share (perf.NewStreamingClient) sets Timeout: 0 and
			// only bounds response headers, not a stalled body. Without this,
			// one hung scanner call blocks wg.Wait() below forever, and the
			// whole request with it — worse than the fail-open/BLOCKED
			// outcome an explicit error already produces just below.
			// http.NewRequestWithContext (every provider implementation)
			// aborts its in-flight call the moment this deadline fires, so a
			// timeout here reliably surfaces as a scanner error, not a panic.
			scanCtx, cancel := context.WithTimeout(ctx, scannerTimeout)
			defer cancel()
			var r ScanResult
			var err error
			if entityScanner, ok := scanner.(piiEntityScanner); ok && piiEntities != nil {
				r, err = entityScanner.ScanWithEntities(scanCtx, userText, fullText, *piiEntities)
			} else {
				r, err = scanner.Scan(scanCtx, userText, fullText)
			}
			results[idx] = scanOutput{result: r, err: err}
		}(i, s)
	}
	wg.Wait()

	// Collect results and track spend
	var blockedReasons []string
	var triggeredScanners []string
	var triggeredGuardrails []string
	var findings []Finding

	for i, out := range results {
		if out.err != nil {
			e.logger.Warn("guardrail: scanner error",
				"scanner", scanners[i].Name(),
				"error", out.err,
			)
			if !e.failOpen {
				return Result{
					Action:        "BLOCKED",
					BlockedReason: "guardrail scanner error: " + scanners[i].Name(),
				}
			}
			continue
		}

		// Track spend for ML-based scanners
		if out.result.PromptTokens > 0 && e.spender != nil {
			go e.trackSpend(keyInfo, out.result)
		}

		if out.result.Blocked {
			triggeredScanners = append(triggeredScanners, out.result.Scanner)
			findings = append(findings, Finding{
				Scanner:  out.result.Scanner,
				Category: out.result.Category,
				Reason:   out.result.Reason,
			})
			if out.result.Reason != "" {
				blockedReasons = append(blockedReasons, out.result.Reason)
			}
			// Map scanner name to guardrail names
			for gr, scannerNames := range guardrailToScanners {
				for _, sn := range scannerNames {
					if sn == out.result.Scanner {
						triggeredGuardrails = appendUnique(triggeredGuardrails, gr)
					}
				}
			}
		}
	}

	if len(triggeredScanners) > 0 {
		reason := strings.Join(blockedReasons, "; ")
		if reason == "" {
			reason = "request blocked by content guardrail"
		}
		return Result{
			Action:              "BLOCKED",
			BlockedReason:       reason,
			TriggeredScanners:   triggeredScanners,
			TriggeredGuardrails: triggeredGuardrails,
			Findings:            findings,
		}
	}

	return Result{Action: "ALLOWED"}
}

// trackSpend records the cost of ML-based scanner calls attributed to the customer key.
func (e *Engine) trackSpend(keyInfo *store.APIKey, sr ScanResult) {
	record := store.SpendRecord{
		Model:            sr.Model,
		Provider:         "@hiveguard",
		PromptTokens:     sr.PromptTokens,
		CompletionTokens: sr.CompletionTokens,
		TotalTokens:      sr.PromptTokens + sr.CompletionTokens,
		Status:           200,
		CreatedAt:        time.Now(),
	}
	if e.pc != nil {
		record.Cost = e.pc.Cost(sr.Model, sr.PromptTokens, sr.CompletionTokens)
	}
	if keyInfo != nil {
		record.ApplyIdentity(keyInfo)
	}
	e.spender.Record(record)
}

// PIIScanner returns the configured PII scanner, or nil when PII detection is
// off. The proxy middleware needs it directly to redact instead of block.
func (e *Engine) PIIScanner() *PIIScanner {
	if e == nil {
		return nil
	}
	for _, s := range e.scanners {
		if p, ok := s.(*PIIScanner); ok {
			return p
		}
	}
	return nil
}

// filterScanners returns only scanners whose name matches one of the enabled guardrails.
func (e *Engine) filterScanners(guardrails []string) []Scanner {
	allowed := make(map[string]bool)
	for _, gr := range guardrails {
		for _, sn := range guardrailToScanners[gr] {
			allowed[sn] = true
		}
	}
	var out []Scanner
	for _, s := range e.scanners {
		if allowed[s.Name()] {
			out = append(out, s)
		}
	}
	return out
}

// splitByRole separates messages into user-only text and full text.
func splitByRole(messages []Message) (userText, fullText string) {
	var userParts, allParts []string
	for _, m := range messages {
		if m.Content == "" {
			continue
		}
		allParts = append(allParts, m.Content)
		if m.Role == "user" {
			userParts = append(userParts, m.Content)
		}
	}
	return strings.Join(userParts, "\n"), strings.Join(allParts, "\n")
}

func appendUnique(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}

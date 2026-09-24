package hivetrace

import (
	"context"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// firewallRuleStore is the appliance's declared MCP/tool/file rules. Optional,
// the same way watchlistStore is: a gateway running without a database still
// records traffic, it just has nothing to check it against.
type firewallRuleStore interface {
	ListFirewallRules(ctx context.Context) ([]store.FirewallRule, error)
}

// firewallEvaluator turns a turn's extracted tools and files into findings.
//
// It runs in shadow mode only: it reports what a rule matched, it does not
// remove or refuse anything. The gateway already streams a turn's response to
// the caller as it arrives, and by the time a call is fully extracted here the
// client may already have received and acted on it — reporting what happened
// is the honest claim this can make today. Enforcement (stripping a denied
// call before it reaches the client) needs a synchronous chokepoint in the
// proxy response path, not this asynchronous ingestion worker.
type firewallEvaluator struct {
	mcpServers *guardrail.WatchlistScanner
	tools      *guardrail.WatchlistScanner
	filesRead  *guardrail.WatchlistScanner
	filesWrite *guardrail.WatchlistScanner
}

// newFirewallEvaluator groups rules by kind into one scanner each: a call is
// checked against its own kind's rules only, never all of them at once.
func newFirewallEvaluator(rules []store.FirewallRule) *firewallEvaluator {
	byKind := map[string][]guardrail.WatchTerm{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		byKind[r.Kind] = append(byKind[r.Kind], guardrail.WatchTerm{Label: r.Label, Pattern: r.Pattern})
	}
	return &firewallEvaluator{
		mcpServers: guardrail.NewWatchlistScanner(byKind[store.FirewallKindMCPServer]),
		tools:      guardrail.NewWatchlistScanner(byKind[store.FirewallKindTool]),
		filesRead:  guardrail.NewWatchlistScanner(byKind[store.FirewallKindFileRead]),
		filesWrite: guardrail.NewWatchlistScanner(byKind[store.FirewallKindFileWrite]),
	}
}

// findings evaluates one turn's tools and files against the loaded rules.
// Occurrences are aggregated per matched rule, the same shape detector.findings
// already produces for secrets, PII and watchlist terms.
func (fw *firewallEvaluator) findings(tools []ToolInvocation, files []FileAccess) []Finding {
	if fw == nil {
		return nil
	}
	counts := map[string]int{}
	for _, t := range tools {
		if t.Source == ToolSourceMCP && t.Server != "" {
			if label, ok := fw.mcpServers.Match(t.Server); ok {
				counts[label]++
				continue
			}
		}
		if label, ok := fw.tools.Match(t.Tool); ok {
			counts[label]++
		}
	}
	for _, f := range files {
		switch f.Operation {
		case FileOpWrite:
			if label, ok := fw.filesWrite.Match(f.Path); ok {
				counts[label]++
			}
		default: // read and search: both disclose the file's contents
			if label, ok := fw.filesRead.Match(f.Path); ok {
				counts[label]++
			}
		}
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(counts))
	for label, n := range counts {
		out = append(out, Finding{Kind: KindFirewall, Type: label, Origin: OriginResponse, Occurrences: n})
	}
	return out
}

// firewallReload matches watchlistReload: a rule edited in the console takes
// up to this long to reach the evaluator.
const firewallReload = 30 * time.Second

// reloadFirewallRules rebuilds the evaluator from the stored rules. A failure
// keeps the evaluator already loaded, following reloadWatchlist's reasoning:
// dropping every rule because one query timed out would silently wave every
// call through.
func (r *Recorder) reloadFirewallRules() {
	if r.rules == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.rules.ListFirewallRules(ctx)
	if err != nil {
		r.logger.Warn("hivetrace: failed to load firewall rules", "error", err)
		return
	}
	r.fw = newFirewallEvaluator(rows)
}

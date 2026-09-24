package hivetrace

import (
	"sync/atomic"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
)

// detector runs the sensitive-data scanners over captured traffic.
//
// It reuses the guardrail scanners rather than keeping a second set of
// patterns: a detector that fires when blocking must also fire when only
// observing, or the trace would tell an operator the traffic was clean while
// the guardrail was blocking the same request.
//
// Unlike the guardrail it scans both directions. The request side catches what
// the organisation sent out — a key pasted into a prompt, a customer record in
// a tool result the agent handed back to the model. The response side catches
// what the model returned, which nothing in the gateway looked at until now.
type detector struct {
	pii     *guardrail.PIIScanner
	secrets *guardrail.SecretsScanner
	// watchlist is swapped whole when the operator edits the terms, so a
	// flush in progress keeps scanning with a consistent set rather than
	// seeing half an edit.
	watchlist atomic.Pointer[guardrail.WatchlistScanner]
	// hidden holds the PII types the operator does not want reported. Only
	// findings are filtered: redaction still covers them, since storing a
	// value nobody asked to see is worse than masking one.
	hidden atomic.Pointer[map[string]bool]
}

func newDetector() *detector {
	d := &detector{
		pii:     guardrail.NewPIIScanner(),
		secrets: guardrail.NewSecretsScanner(),
	}
	d.setReported(nil)
	return d
}

// setReported applies operator overrides on top of the built-in defaults.
func (d *detector) setReported(overrides map[string]bool) {
	hidden := map[string]bool{}
	for _, t := range guardrail.PIITypes() {
		on, set := overrides[t]
		if !set {
			on = guardrail.PIIReportedByDefault(t)
		}
		if !on {
			hidden[t] = true
		}
	}
	d.hidden.Store(&hidden)
}

func (d *detector) setWatchlist(s *guardrail.WatchlistScanner) {
	if d != nil {
		d.watchlist.Store(s)
	}
}

// findings scans one side of a turn. entities narrows PII detection to a
// tenant's selection; nil means every detector applies.
func (d *detector) findings(text, origin string, entities *[]string, samples int) []Finding {
	if d == nil || text == "" {
		return nil
	}
	var out []Finding
	for _, m := range d.secrets.FindingsWithSamples(text, samples) {
		out = append(out, Finding{Kind: KindSecret, Type: m.Name, Origin: origin, Occurrences: m.Count, Samples: m.Samples})
	}
	hidden := *d.hidden.Load()
	for _, m := range d.pii.FindingsWithSamples(text, entities, samples) {
		if hidden[m.Name] {
			continue
		}
		out = append(out, Finding{Kind: KindPII, Type: m.Name, Origin: origin, Occurrences: m.Count, Samples: m.Samples})
	}
	// Watchlist hits always keep their value, whatever finding_samples says:
	// the operator supplied the term, so the matched text cannot tell them
	// anything they did not already write down.
	for _, m := range d.watchlist.Load().FindingsWithSamples(text, maxFindingSamples) {
		out = append(out, Finding{Kind: KindWatchlist, Type: m.Name, Origin: origin, Occurrences: m.Count, Samples: m.Samples})
	}
	return out
}

// redact rewrites text for storage, replacing both secrets and PII with
// placeholders, and reports the detector names it replaced.
func (d *detector) redact(text string, entities *[]string) (string, []string) {
	if d == nil || text == "" {
		return text, nil
	}
	out, replaced := d.secrets.Redact(text)
	out, piiReplaced := d.pii.Redact(out, entities)
	out, watchReplaced := d.watchlist.Load().Redact(out)
	replaced = append(replaced, piiReplaced...)
	return out, append(replaced, watchReplaced...)
}

// maxFindingSamples bounds how many distinct values are kept per detector per
// turn, and per detector per session once merged.
//
// High enough that the list is the whole answer in practice: a capped list is
// worse than none for the one job this has, which is deciding whether a
// detector fires on real data. Seeing three of six matches hides exactly the
// case worth finding — the fourth one that is a false positive. The bound
// exists only so a pathological session cannot grow the record without end.
//
// Values are deduplicated, so a count far above the number shown means the
// same value matched repeatedly, not that anything was truncated.
const maxFindingSamples = 50

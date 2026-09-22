package livezone

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Exact repeats are the easy half of the problem. The expensive half is the
// *near* repeat: an agent reads a file, edits two lines, reads it again. The
// two copies differ by a few bytes and hash differently, so exact dedupe sees
// nothing, yet the second copy costs as much as the first.
//
// This is what dominates a long coding session. A single 40 KB file read four
// times across a task is 160 KB of prompt carrying 40 KB of information.
//
// The rule here is the same one exact dedupe follows, and safe for the same
// reason: the first copy is never touched, so every line number and offset the
// agent might quote back still resolves against it. Only a later copy is
// replaced, and it is replaced by a statement of what changed — which is
// strictly more useful to the agent than a re-read it has to diff by eye.

// deltaMaxCandidates bounds how many earlier results one repeat is compared
// against. The work is linear per comparison, and the match, when there is
// one, is almost always the most recent copy of the same file.
const deltaMaxCandidates = 8

// deltaMaxLengthRatio rejects a candidate whose size is too far from the
// repeat's for a common prefix and suffix to plausibly cover them.
const deltaMaxLengthRatio = 0.35

// dedupeIndex records the tool results seen so far in one request and decides
// what a later copy of the same content should be replaced with.
type dedupeIndex struct {
	exact   map[[32]byte]string
	entries []dedupeEntry
	pol     Policy
}

type dedupeEntry struct {
	text  string
	label string
	lines []string
}

func newDedupeIndex(pol Policy) *dedupeIndex {
	return &dedupeIndex{exact: make(map[[32]byte]string), pol: pol}
}

// observe records a result as the canonical copy of its content.
func (d *dedupeIndex) observe(text, label string) {
	d.exact[sha256.Sum256([]byte(text))] = label
	if d.pol.DeltaRepeats {
		d.entries = append(d.entries, dedupeEntry{text: text, label: label})
	}
}

// reference returns the replacement for a result whose content already appears
// earlier in the request, and false when it does not or when saying so would
// not be smaller than repeating it.
//
// It reports whether the match was exact so the caller can record the two
// cases under different transformer names: they have very different sizes and
// an operator reading the metrics back should not see them merged.
func (d *dedupeIndex) reference(text string) (marker string, exact bool, ok bool) {
	if label, found := d.exact[sha256.Sum256([]byte(text))]; found {
		return fmt.Sprintf(
			"[identical to the earlier %s in this conversation — %d bytes, not repeated]",
			label, len(text)), true, true
	}
	if !d.pol.DeltaRepeats {
		return "", false, false
	}
	if m, found := d.delta(text); found {
		return m, false, true
	}
	return "", false, false
}

// delta finds an earlier result this one is a small edit of, and describes the
// edit. It only ever considers a common leading and trailing run of lines: a
// file with an edit in it has both, and anything that does not is a different
// payload rather than a new version of the same one.
func (d *dedupeIndex) delta(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	best := ""
	bestLen := len(text)

	seen := 0
	for i := len(d.entries) - 1; i >= 0 && seen < deltaMaxCandidates; i-- {
		e := &d.entries[i]
		if !plausibleSibling(len(e.text), len(text)) {
			continue
		}
		seen++
		if e.lines == nil {
			e.lines = strings.Split(e.text, "\n")
		}
		m, ok := describeEdit(e.lines, lines, e.label)
		if !ok {
			continue
		}
		if len(m) < bestLen {
			best, bestLen = m, len(m)
		}
	}
	if best == "" {
		return "", false
	}
	// The same gain test every transformer answers to: a marginal win is not
	// worth changing the bytes the model sees.
	if float64(len(text)-len(best))/float64(len(text)) < d.pol.Options.MinGainRatio {
		return "", false
	}
	return best, true
}

// plausibleSibling is a cheap pre-filter: two versions of one file differ by
// an edit, not by an order of magnitude.
func plausibleSibling(a, b int) bool {
	if a == 0 || b == 0 {
		return false
	}
	hi, lo := a, b
	if lo > hi {
		hi, lo = lo, hi
	}
	return float64(hi-lo)/float64(hi) <= deltaMaxLengthRatio
}

// describeEdit renders how cur differs from prev, or false when the difference
// is too diffuse to state briefly. The changed region is reproduced in full —
// nothing about the new content is summarised or dropped, only the unchanged
// lines around it are left to the earlier copy.
func describeEdit(prev, cur []string, label string) (string, bool) {
	if len(prev) == 0 || len(cur) == 0 {
		return "", false
	}

	prefix := 0
	for prefix < len(prev) && prefix < len(cur) && prev[prefix] == cur[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(prev)-prefix && suffix < len(cur)-prefix &&
		prev[len(prev)-1-suffix] == cur[len(cur)-1-suffix] {
		suffix++
	}

	// No shared context at all means these are simply different payloads.
	if prefix+suffix == 0 {
		return "", false
	}
	// Identical content is the exact case, handled before this is reached.
	if prefix == len(prev) && prefix == len(cur) {
		return "", false
	}

	replaced := prev[prefix : len(prev)-suffix]
	inserted := cur[prefix : len(cur)-suffix]

	var sb strings.Builder
	fmt.Fprintf(&sb, "[same content as the earlier %s in this conversation (%d lines), with one difference. ",
		label, len(prev))
	switch {
	case len(inserted) == 0:
		fmt.Fprintf(&sb, "Its lines %d-%d are no longer present; everything else is unchanged.]",
			prefix+1, prefix+len(replaced))
		return sb.String(), true
	case len(replaced) == 0:
		fmt.Fprintf(&sb, "%d lines are now inserted after line %d; everything else is unchanged:]\n",
			len(inserted), prefix)
	default:
		fmt.Fprintf(&sb, "Its lines %d-%d now read as follows; everything else is unchanged:]\n",
			prefix+1, prefix+len(replaced))
	}
	sb.WriteString(strings.Join(inserted, "\n"))
	return sb.String(), true
}

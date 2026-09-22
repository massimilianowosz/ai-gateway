package livezone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// omittedKey labels the marker that replaces the middle of a truncated array.
const omittedKey = "_ubiquum_omitted"

// notableRe marks a value the agent probably still needs after truncation:
// anything reporting a failure or an unusual state.
var notableRe = regexp.MustCompile(`(?i)\b(error|failed|failure|fatal|exception|invalid|denied|missing|timeout|warn|critical|unhealthy|degraded)\b`)

// truncateJSONArrays shortens long arrays inside a JSON document.
//
// This is the lossy half of JSON handling and the one that pays: an API
// response with five hundred near-identical records costs the same tokens as
// five hundred distinct ones, and the agent almost never needs them all. What
// survives is the shape and the exceptions:
//
//   - the first KeepHead and last KeepTail elements, verbatim;
//   - every element that mentions an error, failure or other notable state,
//     wherever it sits in the array;
//   - a marker recording exactly how many elements were dropped.
//
// Numbers are decoded as json.Number, so literals keep their written form
// rather than round-tripping through float64. Object key order is not
// preserved — Go marshals map keys sorted — which is why this transform is
// lossy and opt-in, while plain compaction is neither.
func truncateJSONArrays(s string, cfg JSONTruncateConfig) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", false
	}

	changed := false
	out := walkTruncate(doc, cfg, &changed)
	if !changed {
		return "", false
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

// JSONTruncateConfig bounds array truncation.
type JSONTruncateConfig struct {
	// MinItems is the array length below which nothing is truncated. Short
	// arrays are usually the answer itself, not a haystack.
	MinItems int
	// KeepHead and KeepTail are the elements preserved at each end.
	KeepHead, KeepTail int
}

// DefaultJSONTruncate returns conservative limits.
func DefaultJSONTruncate() JSONTruncateConfig {
	return JSONTruncateConfig{MinItems: 20, KeepHead: 5, KeepTail: 3}
}

func walkTruncate(v any, cfg JSONTruncateConfig, changed *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = walkTruncate(val, cfg, changed)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = walkTruncate(val, cfg, changed)
		}
		if len(t) < cfg.MinItems || len(t) <= cfg.KeepHead+cfg.KeepTail+1 {
			return t
		}
		// A pure numeric array is better described than sampled: statistics
		// over every element beat an arbitrary window of them.
		if sum, ok := numericSummary(t, cfg); ok {
			*changed = true
			return sum
		}
		// Collapsing exact repeats first means truncation only ever drops
		// content that is genuinely distinct.
		if deduped, removed, ok := dedupeItems(t, cfg); ok && removed > 0 {
			*changed = true
			t = deduped
			if len(t) < cfg.MinItems || len(t) <= cfg.KeepHead+cfg.KeepTail+1 {
				return t
			}
		}
		return truncateSlice(t, cfg, changed)
	default:
		return v
	}
}

func truncateSlice(items []any, cfg JSONTruncateConfig, changed *bool) []any {
	keep := make([]bool, len(items))
	for i := 0; i < cfg.KeepHead && i < len(items); i++ {
		keep[i] = true
	}
	for i := 0; i < cfg.KeepTail && i < len(items); i++ {
		keep[len(items)-1-i] = true
	}
	// An element reporting a problem is kept wherever it is: dropping the one
	// failure in a list of successes is the failure mode that matters.
	for i, it := range items {
		if !keep[i] && isNotable(it) {
			keep[i] = true
		}
	}

	kept := 0
	for _, k := range keep {
		if k {
			kept++
		}
	}
	omitted := len(items) - kept
	if omitted <= 0 {
		return items
	}

	out := make([]any, 0, kept+1)
	marked := false
	for i, it := range items {
		if keep[i] {
			out = append(out, it)
			continue
		}
		if !marked {
			out = append(out, map[string]any{
				omittedKey: json.Number(fmt.Sprint(omitted)),
				"_note": fmt.Sprintf(
					"%d of %d elements omitted by ubiquum live compression; "+
						"first %d, last %d and all elements reporting errors were kept",
					omitted, len(items), cfg.KeepHead, cfg.KeepTail),
			})
			marked = true
		}
	}
	*changed = true
	return out
}

// isNotable reports whether an element mentions a failure or unusual state.
func isNotable(v any) bool {
	switch t := v.(type) {
	case string:
		return notableRe.MatchString(t)
	case map[string]any:
		for k, val := range t {
			if notableRe.MatchString(k) {
				// A key like "error" only matters when it carries something.
				if val != nil && val != "" && val != false {
					return true
				}
			}
			if isNotable(val) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if isNotable(val) {
				return true
			}
		}
	case bool:
		return false
	}
	return false
}

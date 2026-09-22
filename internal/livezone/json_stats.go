package livezone

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// numericSummary replaces a long array of plain numbers with its shape.
//
// Metric series, latency samples and column dumps arrive as thousands of bare
// numbers. The agent almost never needs each value — it needs the range, the
// central tendency and the ends. Five thousand numbers become an object of
// eight fields, and unlike element truncation nothing about the distribution
// is guessed at: min, max, sum and mean are computed over every element.
func numericSummary(items []any, cfg JSONTruncateConfig) (any, bool) {
	if len(items) < cfg.MinItems {
		return nil, false
	}
	vals := make([]float64, 0, len(items))
	for _, it := range items {
		n, ok := it.(json.Number)
		if !ok {
			return nil, false // not a pure numeric array
		}
		f, err := n.Float64()
		if err != nil {
			return nil, false
		}
		vals = append(vals, f)
	}

	head := cfg.KeepHead
	if head > len(items) {
		head = len(items)
	}
	tail := cfg.KeepTail
	if tail > len(items)-head {
		tail = len(items) - head
	}

	min, max, sum := vals[0], vals[0], 0.0
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
		sum += v
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)

	return map[string]any{
		omittedKey: json.Number(strconv.Itoa(len(items) - head - tail)),
		"_note": fmt.Sprintf(
			"numeric array of %d elements summarised by ubiquum live compression", len(items)),
		"count":  json.Number(strconv.Itoa(len(items))),
		"min":    numToken(min),
		"max":    numToken(max),
		"mean":   numToken(sum / float64(len(vals))),
		"median": numToken(sorted[len(sorted)/2]),
		"first":  items[:head],
		"last":   items[len(items)-tail:],
	}, true
}

// numToken renders a statistic without exponent noise, dropping a trailing
// ".0" so integers read as integers.
func numToken(f float64) json.Number {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return json.Number(strconv.FormatInt(int64(f), 10))
	}
	return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
}

// dedupeItems collapses elements that are byte-identical once canonicalised.
//
// API responses repeat whole records more often than they look like they do —
// the same health entry per shard, the same permission per resource. A repeat
// carries no information the first occurrence did not, so one copy plus a
// count is not an approximation, it is the same content stated once.
//
// Only exact duplicates are collapsed. Grouping "similar" records would mean
// deciding which differing field is unimportant, and that decision belongs to
// the agent, not the gateway.
func dedupeItems(items []any, cfg JSONTruncateConfig) ([]any, int, bool) {
	if len(items) < cfg.MinItems {
		return items, 0, false
	}

	type entry struct {
		value any
		count int
		order int
	}
	seen := make(map[string]*entry, len(items))
	order := 0
	for _, it := range items {
		key, ok := canonicalKey(it)
		if !ok {
			return items, 0, false // unhashable element: leave the array alone
		}
		if e, exists := seen[key]; exists {
			e.count++
			continue
		}
		seen[key] = &entry{value: it, count: 1, order: order}
		order++
	}
	if len(seen) == len(items) {
		return items, 0, false // nothing repeated
	}

	entries := make([]*entry, 0, len(seen))
	for _, e := range seen {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].order < entries[j].order })

	out := make([]any, 0, len(entries))
	removed := 0
	for _, e := range entries {
		if e.count == 1 {
			out = append(out, e.value)
			continue
		}
		removed += e.count - 1
		// The count rides on the element itself, so the record stays readable
		// and the repetition is stated rather than implied.
		if m, ok := e.value.(map[string]any); ok {
			clone := make(map[string]any, len(m)+1)
			for k, v := range m {
				clone[k] = v
			}
			clone["_ubiquum_repeated"] = json.Number(strconv.Itoa(e.count))
			out = append(out, clone)
			continue
		}
		out = append(out, map[string]any{
			"value":             e.value,
			"_ubiquum_repeated": json.Number(strconv.Itoa(e.count)),
		})
	}
	return out, removed, true
}

// canonicalKey renders an element to a stable string for equality. Go's
// map marshalling sorts keys, so two objects with the same content hash
// identically regardless of the order they arrived in.
func canonicalKey(v any) (string, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), true
}

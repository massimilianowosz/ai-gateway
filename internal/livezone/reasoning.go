package livezone

import "encoding/json"

// pruneReasoning drops the reasoning items belonging to turns the model has
// already finished.
//
// The Responses API asks a client to send back the reasoning items it emitted,
// so the model can pick up its own chain of thought within a turn. Codex does,
// and never stops: by the middle of a task the input array carries the
// reasoning of every step the agent has taken, none of which it will resume.
// Only the chain since the last thing the user said is still live.
//
// The boundary is that last user message. Everything after it is the current
// turn — reasoning the model may still be continuing — and is left alone.
// Everything before it belonged to a turn that has already produced its
// answer.
//
// start is the frozen floor: nothing below it may be removed, even if it
// qualifies. A message frozen by the prompt-cache tracker (internal/promptcache)
// is being forwarded byte-identical to what a previous turn already sent and
// the provider already cached; removing it now would shift every later
// position out from under that guarantee, which is exactly the misalignment
// the floor exists to prevent.
func pruneReasoning(items []json.RawMessage, start int, stats *Stats) ([]json.RawMessage, bool) {
	boundary := -1
	for i, raw := range items {
		if isResponsesUserTurn(raw) {
			boundary = i
		}
	}
	if boundary <= 0 {
		return items, false // no completed turn to prune
	}

	out := make([]json.RawMessage, 0, len(items))
	removed := 0
	for i, raw := range items {
		if i >= start && i < boundary && isReasoningItem(raw) {
			stats.record(Result{
				Applied: true, Kind: KindUnknown,
				BytesBefore: len(raw), BytesAfter: 0,
			}, "reasoning_prune")
			removed++
			continue
		}
		out = append(out, raw)
	}
	return out, removed > 0
}

func isReasoningItem(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return false
	}
	return item.Type == "reasoning"
}

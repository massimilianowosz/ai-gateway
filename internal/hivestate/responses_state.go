package hivestate

import "encoding/json"

// responsesStateItems renders the state as one input item per frozen block.
//
// Emitted as a single item whose text grows, the state capped the provider's
// cache at the last byte of the preamble: measured on live Codex traffic the
// hit sat at exactly 16896 tokens turn after turn, the size of the preamble,
// while the state region behind it was never recognised. An item that changes
// invalidates everything from itself onward, so a growing item is the worst
// possible container for content that is supposed to be stable.
//
// One item per block keeps every earlier item byte-identical between turns,
// which is the whole point of building the state as an append-only log.
func responsesStateItems(result *Result) ([]json.RawMessage, error) {
	parts := result.StateParts
	if len(parts) == 0 {
		parts = []string{result.StateJSON}
	}

	items := make([]json.RawMessage, 0, len(parts)+1)
	texts := append([]string{stateFraming}, parts...)
	for _, text := range texts {
		if text == "" {
			continue
		}
		raw, err := json.Marshal(map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": text},
			},
		})
		if err != nil {
			return nil, err
		}
		items = append(items, raw)
	}
	return items, nil
}

package hivestate

import "encoding/json"

const stateFraming = "Conversation state (summary of older context):\n"

// anthropicStateMessage builds the message that replaces compressed history.
//
// Anthropic's cache is explicit and matched per content block. A rewrite that
// emits one growing text block therefore gets no cache at all: the block
// differs from last turn's, so the whole prompt is billed at full price on
// every turn — which is why the guard, correctly, refused to rewrite.
//
// Emitting one block per frozen state block makes every earlier block
// byte-identical to the previous turn, so the provider matches them and writes
// only the new one. The cache_control breakpoint goes on the last block, which
// is the end of the region that only ever grows by appending.
func anthropicStateMessage(result *Result) (json.RawMessage, error) {
	if len(result.StateParts) == 0 {
		return json.Marshal(map[string]any{
			"role":    "user",
			"content": stateFraming + result.StateJSON,
		})
	}

	blocks := make([]map[string]any, 0, len(result.StateParts)+1)
	blocks = append(blocks, map[string]any{"type": "text", "text": stateFraming})
	for _, part := range result.StateParts {
		blocks = append(blocks, map[string]any{"type": "text", "text": part})
	}
	blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}

	return json.Marshal(map[string]any{"role": "user", "content": blocks})
}

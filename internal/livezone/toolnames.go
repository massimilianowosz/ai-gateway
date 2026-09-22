package livezone

import "encoding/json"

// Both wire formats put the tool's name on the *call* and only an id on the
// result. Without resolving that link every result looks unnamed, and since an
// unnamed result is treated as protected, nothing would ever be compressed —
// the feature would be a silent no-op. These builders walk the conversation
// once and map result id → tool name.

// anthropicToolNames maps tool_use_id → tool name from the assistant messages.
func anthropicToolNames(msgs []json.RawMessage) map[string]string {
	names := make(map[string]string)
	for _, raw := range msgs {
		var msg struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Role != "assistant" {
			continue
		}
		for _, b := range msg.Content {
			var block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(b, &block); err != nil {
				continue
			}
			if block.Type == "tool_use" && block.ID != "" && block.Name != "" {
				names[block.ID] = block.Name
			}
		}
	}
	return names
}

// openAIToolNames maps tool_call_id → function name from the assistant messages.
func openAIToolNames(msgs []json.RawMessage) map[string]string {
	names := make(map[string]string)
	for _, raw := range msgs {
		var msg struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
				Name string `json:"name"` // some clients flatten it
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			name := tc.Function.Name
			if name == "" {
				name = tc.Name
			}
			if tc.ID != "" && name != "" {
				names[tc.ID] = name
			}
		}
	}
	return names
}

// responsesToolNames maps call_id → tool name from function_call and
// custom_tool_call items — the Responses API's equivalent of OpenAI's
// assistant tool_calls. Codex CLI sends custom_tool_call for freeform
// (non-JSON-schema) tools, function_call for the rest.
func responsesToolNames(items []json.RawMessage) map[string]string {
	names := make(map[string]string)
	for _, raw := range items {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		switch item.Type {
		case "function_call", "custom_tool_call":
		default:
			continue
		}
		if item.CallID != "" && item.Name != "" {
			names[item.CallID] = item.Name
		}
	}
	return names
}

// resultToolName resolves the tool behind a result, preferring a name echoed
// on the result itself and falling back to the id → name map.
func resultToolName(echoed, id string, names map[string]string) string {
	if echoed != "" {
		return echoed
	}
	return names[id]
}

// rawString reads a string field, returning "" when absent or not a string.
func rawString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

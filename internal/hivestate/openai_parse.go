package hivestate

import (
	"encoding/json"
	"strings"
)

// parseOpenAIMessages reads a /v1/chat/completions body into the analysis view.
//
// The body used to be unmarshalled straight into []Message, whose Content is a
// plain string. That silently lost two things: a message carrying content
// blocks instead of a string failed to unmarshal and emptied the whole request,
// so HiveState skipped it without a word; and an assistant's tool_calls live in
// their own field with a null content, so every tool call in the conversation
// counted as nothing. Both made the measured size disagree with what is sent,
// which is the number every threshold and cache comparison rests on.
func parseOpenAIMessages(body []byte) []Message {
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var messages []Message
	for _, m := range req.Messages {
		var parts []string
		if text := openAIContentText(m.Content); text != "" {
			parts = append(parts, text)
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			parts = append(parts, "[TOOL_CALL "+tc.Function.Name+"] "+tc.Function.Arguments)
		}
		if len(parts) == 0 {
			continue
		}
		messages = append(messages, Message{
			Role:       m.Role,
			Content:    strings.Join(parts, "\n"),
			ToolCallID: m.ToolCallID,
		})
	}
	return messages
}

// openAIContentText accepts both the string form and the content-block array.
func openAIContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out []string
	for _, b := range blocks {
		if b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

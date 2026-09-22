package hivestate

import (
	"encoding/json"
	"testing"
)

// Unmarshalling straight into []Message required every content to be a string,
// so one message carrying content blocks emptied the whole request and
// HiveState skipped it without a word.
func TestParseOpenAIMessages_ReadsContentBlocks(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]any{
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "look at this failure"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := parseOpenAIMessages(body)
	if len(msgs) != 2 {
		t.Fatalf("parsed %d messages, want 2: %+v", len(msgs), msgs)
	}
	if !contains(msgs[1].Content, "look at this failure") {
		t.Errorf("block content lost: %q", msgs[1].Content)
	}
}

// An assistant's tool calls live in their own field with a null content, so
// every tool call in the conversation used to count as nothing.
func TestParseOpenAIMessages_CountsToolCalls(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "fix the build"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
				{"id": "c1", "type": "function", "function": map[string]any{
					"name": "shell", "arguments": `{"command":"go build ./..."}`,
				}},
			}},
			{"role": "tool", "tool_call_id": "c1", "content": "undefined: truncate"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := parseOpenAIMessages(body)
	if len(msgs) != 3 {
		t.Fatalf("parsed %d messages, want 3: %+v", len(msgs), msgs)
	}
	if !contains(msgs[1].Content, "go build ./...") {
		t.Errorf("tool call arguments lost: %q", msgs[1].Content)
	}
	if msgs[2].ToolCallID != "c1" {
		t.Errorf("tool result lost its call id: %+v", msgs[2])
	}
}

// The count drives the threshold and every cache comparison, so it has to grow
// with the real request.
func TestParseOpenAIMessages_CountGrowsWithRealSize(t *testing.T) {
	bodyWith := func(chars int) []byte {
		out := make([]byte, 0, chars)
		for len(out) < chars {
			out = append(out, "tool output line "...)
		}
		b, err := json.Marshal(map[string]any{
			"messages": []map[string]any{
				{"role": "user", "content": "go"},
				{"role": "tool", "tool_call_id": "c1", "content": string(out)},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	counter := NewTokenCounter()
	small := counter.CountMessages(parseOpenAIMessages(bodyWith(4000)))
	large := counter.CountMessages(parseOpenAIMessages(bodyWith(40000)))
	if large < small*5 {
		t.Fatalf("a 10x larger body counted %d against %d; the count is clipped", large, small)
	}
}

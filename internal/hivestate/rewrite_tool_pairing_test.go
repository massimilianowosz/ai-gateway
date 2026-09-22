package hivestate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A rewrite that drops an assistant tool_use while keeping its tool_result
// (or the reverse) produces a body the provider rejects, or worse, one it
// accepts and answers incoherently. These tests assert the invariant directly
// on the rewritten body rather than trusting the split logic by inspection.
//
// Both splits turn out to be safe by construction: Anthropic cuts at assistant
// step boundaries and OpenAI at user-turn boundaries, and in both formats a
// tool call and its result always sit inside the same such segment. These
// tests exist to keep that true if the boundary logic ever changes.
func TestRewriteAnthropicKeepsToolPairsAtomic(t *testing.T) {
	filler := strings.Repeat("x ", 100)
	var msgs []map[string]any
	msgs = append(msgs, map[string]any{"role": "user",
		"content": []map[string]any{{"type": "text", "text": "start " + filler}}})
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("toolu_%02d", i)
		msgs = append(msgs, map[string]any{"role": "assistant",
			"content": []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]string{"cmd": "ls"}}}})
		msgs = append(msgs, map[string]any{"role": "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": "out " + filler}}})
	}
	body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4", "messages": msgs})

	out, err := rewriteAnthropicBodyWithWindow(body, &Result{StateJSON: `{"intent":"x"}`}, 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(out, &parsed)

	uses, results := map[string]bool{}, map[string]bool{}
	var roles []string
	for _, m := range parsed.Messages {
		roles = append(roles, m.Role)
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b["type"] == "tool_use" {
				uses[b["id"].(string)] = true
			}
			if b["type"] == "tool_result" {
				results[b["tool_use_id"].(string)] = true
			}
		}
	}
	t.Logf("roles after rewrite: %v", roles)
	for id := range uses {
		if !results[id] {
			t.Errorf("ORPHANED tool_use %s: no matching tool_result", id)
		}
	}
	for id := range results {
		if !uses[id] {
			t.Errorf("ORPHANED tool_result %s: no matching tool_use", id)
		}
	}
}

func TestRewriteOpenAIKeepsToolPairsAtomic(t *testing.T) {
	filler := strings.Repeat("x ", 100)
	msgs := []map[string]any{
		{"role": "system", "content": "sys"},
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call_%02d", i)
		msgs = append(msgs, map[string]any{"role": "user", "content": "ask " + filler})
		msgs = append(msgs, map[string]any{"role": "assistant", "tool_calls": []map[string]any{
			{"id": id, "type": "function", "function": map[string]string{"name": "bash", "arguments": "{}"}}}})
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": "out " + filler})
	}
	body, _ := json.Marshal(map[string]any{"model": "gpt-4o", "messages": msgs})

	out, err := rewriteOpenAIBodyWithWindow(body, &Result{StateJSON: `{"intent":"x"}`}, 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(out, &parsed)

	calls, results := map[string]bool{}, map[string]bool{}
	var roles []string
	for _, m := range parsed.Messages {
		roles = append(roles, m.Role)
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
		if m.ToolCallID != "" {
			results[m.ToolCallID] = true
		}
	}
	t.Logf("roles after rewrite: %v", roles)
	for id := range calls {
		if !results[id] {
			t.Errorf("ORPHANED tool_call %s: no matching tool message", id)
		}
	}
	for id := range results {
		if !calls[id] {
			t.Errorf("ORPHANED tool message %s: no matching tool_calls entry", id)
		}
	}
}

// The tests above never set CCRMessages, which is exactly where the invariant
// broke: recovered context was spliced immediately before the final message,
// and in an agentic loop that message is a tool response. Anthropic and OpenAI
// both reject a tool call that is not immediately followed by its response, so
// every compressed agentic turn that retrieved a file failed outright.
//
// The assertion is adjacency, not just presence: a pair can survive the split
// and still be separated by an injected message.
func agenticAnthropicBody(t *testing.T, pairs int) []byte {
	t.Helper()
	filler := strings.Repeat("x ", 100)
	msgs := []map[string]any{{"role": "user",
		"content": []map[string]any{{"type": "text", "text": "start " + filler}}}}
	for i := 0; i < pairs; i++ {
		id := fmt.Sprintf("toolu_%02d", i)
		msgs = append(msgs, map[string]any{"role": "assistant",
			"content": []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]string{"cmd": "ls"}}}})
		msgs = append(msgs, map[string]any{"role": "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": "out " + filler}}})
	}
	body, err := json.Marshal(map[string]any{"model": "claude-sonnet-4", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func ccrResult() *Result {
	return &Result{
		StateJSON: `{"intent":"x"}`,
		CCRMessages: []Message{
			{Role: "user", Content: "Recovered context follows."},
			{Role: "user", Content: "// contents of main.go"},
		},
	}
}

func TestRewriteAnthropicKeepsToolPairsAdjacentWithCCR(t *testing.T) {
	out, err := rewriteAnthropicBodyWithWindow(agenticAnthropicBody(t, 12), ccrResult(), 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}

	blocksOf := func(raw json.RawMessage) []map[string]any {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil
		}
		return blocks
	}

	for i, raw := range parsed.Messages {
		for _, b := range blocksOf(raw) {
			if b["type"] != "tool_use" {
				continue
			}
			id, _ := b["id"].(string)
			if i+1 >= len(parsed.Messages) {
				t.Fatalf("tool_use %s is the final message: nothing answers it", id)
			}
			var answered bool
			for _, nb := range blocksOf(parsed.Messages[i+1]) {
				if nb["type"] == "tool_result" && nb["tool_use_id"] == id {
					answered = true
				}
			}
			if !answered {
				t.Fatalf("tool_use %s at index %d is not immediately followed by its tool_result; "+
					"next message is %s", id, i, string(parsed.Messages[i+1]))
			}
		}
	}
	if !strings.Contains(string(out), "contents of main.go") {
		t.Error("recovered context was dropped entirely")
	}
}

func TestRewriteOpenAIKeepsToolPairsAdjacentWithCCR(t *testing.T) {
	filler := strings.Repeat("x ", 100)
	msgs := []map[string]any{{"role": "user", "content": "start " + filler}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call_%02d", i)
		msgs = append(msgs, map[string]any{"role": "assistant", "tool_calls": []map[string]any{
			{"id": id, "type": "function", "function": map[string]string{"name": "bash", "arguments": "{}"}},
		}})
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": "out " + filler})
		msgs = append(msgs, map[string]any{"role": "user", "content": "next " + filler})
	}
	// End on a tool response, as an agentic loop does.
	msgs = msgs[:len(msgs)-1]
	body, err := json.Marshal(map[string]any{"model": "gpt-4o", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}

	out, err := rewriteOpenAIBodyWithWindow(body, ccrResult(), 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	var parsed struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	for i, m := range parsed.Messages {
		for _, tc := range m.ToolCalls {
			if i+1 >= len(parsed.Messages) {
				t.Fatalf("tool_call %s is the final message: nothing answers it", tc.ID)
			}
			next := parsed.Messages[i+1]
			if next.Role != "tool" || next.ToolCallID != tc.ID {
				t.Fatalf("tool_call %s at index %d is followed by role=%q tool_call_id=%q, not its response",
					tc.ID, i, next.Role, next.ToolCallID)
			}
		}
	}
	if !strings.Contains(string(out), "contents of main.go") {
		t.Error("recovered context was dropped entirely")
	}
}

func TestRewriteResponsesKeepsCallPairsAdjacentWithCCR(t *testing.T) {
	filler := strings.Repeat("x ", 100)
	items := []map[string]any{{"type": "message", "role": "user",
		"content": []map[string]string{{"type": "input_text", "text": "start " + filler}}}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call_%02d", i)
		items = append(items, map[string]any{"type": "function_call", "call_id": id, "name": "bash", "arguments": "{}"})
		items = append(items, map[string]any{"type": "function_call_output", "call_id": id, "output": "out " + filler})
		items = append(items, map[string]any{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "next " + filler}}})
	}
	items = items[:len(items)-1] // end on a call output
	body, err := json.Marshal(map[string]any{"model": "gpt-4o", "input": items})
	if err != nil {
		t.Fatal(err)
	}

	out, err := rewriteResponsesBodyWithWindow(body, ccrResult(), 4)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	var parsed struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	for i, it := range parsed.Input {
		if it.Type != "function_call" {
			continue
		}
		if i+1 >= len(parsed.Input) {
			t.Fatalf("function_call %s is the final item: nothing answers it", it.CallID)
		}
		next := parsed.Input[i+1]
		if next.Type != "function_call_output" || next.CallID != it.CallID {
			t.Fatalf("function_call %s at index %d is followed by type=%q call_id=%q, not its output",
				it.CallID, i, next.Type, next.CallID)
		}
	}
	if !strings.Contains(string(out), "contents of main.go") {
		t.Error("recovered context was dropped entirely")
	}
}

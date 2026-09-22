package hivestate

import (
	"encoding/json"
	"testing"
)

func stateBlocks(t *testing.T, raw json.RawMessage) []map[string]any {
	t.Helper()
	var msg struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("state message is not block form: %v\n%s", err, raw)
	}
	if msg.Role != "user" {
		t.Fatalf("state message role = %q, want user", msg.Role)
	}
	return msg.Content
}

// Without a breakpoint Anthropic caches nothing, so a rewritten prompt is
// billed in full on every turn — the reason rewriting lost to passthrough.
func TestAnthropicStateMessage_MarksTheEndOfTheStateRegion(t *testing.T) {
	result := &Result{StateParts: []string{"preamble", "block one", "block two"}}

	raw, err := anthropicStateMessage(result)
	if err != nil {
		t.Fatal(err)
	}
	blocks := stateBlocks(t, raw)

	if len(blocks) != 4 {
		t.Fatalf("got %d content blocks, want framing + 3 parts", len(blocks))
	}
	if _, ok := blocks[len(blocks)-1]["cache_control"]; !ok {
		t.Fatal("the last block carries no cache_control; nothing will be cached")
	}
	for i, b := range blocks[:len(blocks)-1] {
		if _, ok := b["cache_control"]; ok {
			t.Errorf("block %d carries a breakpoint; only the end of the region should", i)
		}
	}
}

// The reason for splitting at all: appending a block must leave every earlier
// block untouched, or the provider matches none of them.
func TestAnthropicStateMessage_EarlierBlocksSurviveAnAppend(t *testing.T) {
	before := stateBlocks(t, mustState(t, &Result{StateParts: []string{"preamble", "one"}}))
	after := stateBlocks(t, mustState(t, &Result{StateParts: []string{"preamble", "one", "two"}}))

	if len(after) != len(before)+1 {
		t.Fatalf("appending one part produced %d blocks, want %d", len(after), len(before)+1)
	}
	for i := range before[:len(before)-1] {
		if before[i]["text"] != after[i]["text"] {
			t.Fatalf("block %d text changed on append: %q vs %q", i, before[i]["text"], after[i]["text"])
		}
	}
	// The old final block loses its breakpoint but keeps its text, so the
	// provider can still match it as part of the longer prefix.
	last := len(before) - 1
	if before[last]["text"] != after[last]["text"] {
		t.Fatalf("the previously-last block changed text: %q vs %q", before[last]["text"], after[last]["text"])
	}
}

// A state that is not a log has nothing to split; it must still be valid.
func TestAnthropicStateMessage_FallsBackToPlainContent(t *testing.T) {
	raw, err := anthropicStateMessage(&Result{StateJSON: `{"intent":"x"}`})
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("expected plain string content without parts: %v", err)
	}
	if !contains(msg.Content, `{"intent":"x"}`) {
		t.Errorf("state content lost: %q", msg.Content)
	}
}

func mustState(t *testing.T, r *Result) json.RawMessage {
	t.Helper()
	raw, err := anthropicStateMessage(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

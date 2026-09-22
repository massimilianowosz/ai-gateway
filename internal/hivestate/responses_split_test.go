package hivestate

import (
	"encoding/json"
	"fmt"
	"testing"
)

func respItems(t *testing.T, raws ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(raws))
	for i, r := range raws {
		out[i] = json.RawMessage(r)
	}
	return out
}

func call(id string) string { return fmt.Sprintf(`{"type":"custom_tool_call","call_id":%q}`, id) }
func output(id string) string {
	return fmt.Sprintf(`{"type":"custom_tool_call_output","call_id":%q}`, id)
}
func userMsg() string      { return `{"type":"message","role":"user"}` }
func assistantMsg() string { return `{"type":"message","role":"assistant"}` }

// The window exists for this: whatever is kept verbatim must never contain an
// output whose call was compressed away. An orphan output is a malformed
// request, which is a far worse outcome than compressing nothing.
func TestResponsesSplitPoint_NeverOrphansAnOutput(t *testing.T) {
	var raws []string
	raws = append(raws, userMsg())
	for i := 0; i < 12; i++ {
		raws = append(raws, call(fmt.Sprintf("c%d", i)), output(fmt.Sprintf("c%d", i)))
	}
	items := respItems(t, raws...)

	for steps := 1; steps <= 12; steps++ {
		split := responsesSplitPoint(items, steps)
		kept := map[string]bool{}
		for _, raw := range items[split:] {
			if id, ok := responsesCallID(raw); ok {
				kept[id] = true
			}
		}
		for i, raw := range items[split:] {
			if id, ok := responsesOutputCallID(raw); ok && !kept[id] {
				t.Fatalf("steps=%d split=%d: output %q at tail index %d lost its call", steps, split, id, i)
			}
		}
	}
}

// A cut that lands between a call and its output must be pulled back to include
// the call, even when the step count would have placed it in between.
func TestResponsesSplitPoint_PullsBackToIncludeTheCall(t *testing.T) {
	items := respItems(t,
		userMsg(),
		call("a"), output("a"),
		call("b"), output("b"),
		call("c"), output("c"),
	)

	// Two steps back from the end lands on call "b" (index 3); the tail then
	// holds both calls and both outputs.
	if got := responsesSplitPoint(items, 2); got != 3 {
		t.Fatalf("split = %d, want 3", got)
	}
}

// The whole point of the change: a session made of tool calls with a single
// human message must still be compressible. Counting user turns returned 1 here
// and nothing was ever compressed.
func TestResponsesSplitPoint_CompressesAgenticSessions(t *testing.T) {
	var raws []string
	raws = append(raws, userMsg())
	for i := 0; i < 30; i++ {
		raws = append(raws, call(fmt.Sprintf("c%d", i)), output(fmt.Sprintf("c%d", i)))
	}
	items := respItems(t, raws...)

	split := responsesSplitPoint(items, 10)
	if split <= 0 {
		t.Fatal("an agentic session with 30 tool calls was left uncompressed")
	}
	if split >= len(items)-1 {
		t.Fatalf("split = %d leaves nothing verbatim out of %d items", split, len(items))
	}
}

// Below the window there is nothing older to summarise, so the body is left
// alone rather than rewritten for no gain.
func TestResponsesSplitPoint_LeavesShortSessionsAlone(t *testing.T) {
	items := respItems(t, userMsg(), call("a"), output("a"), assistantMsg())
	if got := responsesSplitPoint(items, 10); got != 0 {
		t.Fatalf("split = %d, want 0 for a session shorter than the window", got)
	}
}

// Codex puts its tool schema and system prompt in the first input items rather
// than in the instructions field. Summarising them away takes the agent's tools
// off it, and rewriting the first item makes the provider re-read the whole
// prompt at full price. They must come out byte-identical.
func TestRewriteResponsesBody_PreservesToolSchemaAndSystemPrompt(t *testing.T) {
	input := []map[string]any{
		{"type": "additional_tools", "role": "developer", "tools": []string{"shell", "apply_patch"}},
		{"type": "message", "role": "developer", "content": "you are codex"},
		{"type": "message", "role": "user", "content": "start"},
	}
	for i := 0; i < 14; i++ {
		input = append(input,
			map[string]any{"type": "custom_tool_call", "call_id": fmt.Sprintf("c%d", i), "name": "shell", "input": "ls"},
			map[string]any{"type": "custom_tool_call_output", "call_id": fmt.Sprintf("c%d", i), "output": "files"},
		)
	}
	input = append(input, map[string]any{"type": "message", "role": "user", "content": "continue"})

	original, err := json.Marshal(map[string]any{"model": "gpt-5", "input": input})
	if err != nil {
		t.Fatal(err)
	}

	out, err := rewriteResponsesBodyWithWindow(original, &Result{StateJSON: `{"intent":"x"}`}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) == string(original) {
		t.Fatal("nothing was rewritten; the test cannot say anything about the preamble")
	}

	var before, after struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if string(after.Input[i]) != string(before.Input[i]) {
			t.Fatalf("preamble item %d was altered\nbefore: %s\nafter:  %s",
				i, before.Input[i], after.Input[i])
		}
	}
	if it := parseResponsesItem(after.Input[0]); it.Type != "additional_tools" {
		t.Fatalf("first outgoing item is %q; the tool schema no longer leads the prompt", it.Type)
	}
}

// One item per frozen block. As a single growing item the state capped the
// provider's cache at the end of the preamble — measured at exactly 16896
// tokens, turn after turn — because an item that changes invalidates
// everything from itself onward.
func TestResponsesStateItems_OneItemPerBlockAndFrozen(t *testing.T) {
	before, err := responsesStateItems(&Result{StateParts: []string{"preamble", "one"}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := responsesStateItems(&Result{StateParts: []string{"preamble", "one", "two"}})
	if err != nil {
		t.Fatal(err)
	}

	if len(after) != len(before)+1 {
		t.Fatalf("appending one block produced %d items, want %d", len(after), len(before)+1)
	}
	for i := range before {
		if string(before[i]) != string(after[i]) {
			t.Fatalf("item %d changed when a block was appended\nbefore: %s\nafter:  %s",
				i, before[i], after[i])
		}
	}
	for i, raw := range after {
		if it := parseResponsesItem(raw); it.Type != "message" || it.Role != "user" {
			t.Errorf("state item %d is %s/%s, want message/user", i, it.Type, it.Role)
		}
	}
}

// A state that is not a log still has to reach the model.
func TestResponsesStateItems_FallsBackToASingleItem(t *testing.T) {
	items, err := responsesStateItems(&Result{StateJSON: `{"intent":"x"}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("no state item emitted without parts")
	}
	joined := ""
	for _, raw := range items {
		joined += string(raw)
	}
	if !contains(joined, `{\"intent\":\"x\"}`) {
		t.Errorf("state content lost: %s", joined)
	}
}

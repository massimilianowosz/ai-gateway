package livezone

import (
	"encoding/json"
	"strings"
	"testing"
)

func prunePolicy() Policy {
	pol := DefaultPolicy()
	pol.LiveTurns = 9999
	pol.PruneReasoning = true
	return pol
}

// reasoningBody builds two completed turns and one in progress. Only the
// reasoning of the turn still running is still live.
func reasoningBody(t *testing.T) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "first question"}}},
			map[string]any{"type": "reasoning", "id": "rs_1",
				"summary": []any{map[string]string{"type": "summary_text", "text": "thinking about the first question"}}},
			map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]string{"type": "output_text", "text": "first answer"}}},
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "second question"}}},
			map[string]any{"type": "reasoning", "id": "rs_2",
				"summary": []any{map[string]string{"type": "summary_text", "text": "thinking about the second question"}}},
			map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "shell", "input": "ls"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "a.go"},
		},
	})
}

func TestPruneReasoning_DropsAnsweredTurnsAndKeepsTheLiveOne(t *testing.T) {
	body := reasoningBody(t)

	out, stats, err := RewriteResponses(body, prunePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["reasoning_prune"] != 1 {
		t.Fatalf("pruned %d reasoning items, want 1", stats.Transformers["reasoning_prune"])
	}
	if len(out) >= len(body) {
		t.Errorf("bytes %d -> %d, expected a reduction", len(body), len(out))
	}

	got := string(out)
	if strings.Contains(got, "rs_1") {
		t.Error("the reasoning of an already-answered turn was kept")
	}
	if !strings.Contains(got, "rs_2") {
		t.Error("the reasoning of the turn still in progress was dropped")
	}
	// Nothing but reasoning may be removed.
	for _, want := range []string{"first question", "first answer", "second question", "a.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("pruning removed %q, which is not a reasoning item", want)
		}
	}
}

// With only one turn there is nothing answered yet, so nothing to prune.
func TestPruneReasoning_KeepsEverythingInASingleTurn(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": "codex-gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]string{"type": "input_text", "text": "only question"}}},
			map[string]any{"type": "reasoning", "id": "rs_1",
				"summary": []any{map[string]string{"type": "summary_text", "text": "thinking"}}},
		},
	})

	out, stats, err := RewriteResponses(body, prunePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["reasoning_prune"] != 0 {
		t.Error("pruned the reasoning of the only turn in the request")
	}
	if len(out) != len(body) {
		t.Errorf("body changed with nothing to prune: %d -> %d", len(body), len(out))
	}
}

func TestPruneReasoning_IsOptIn(t *testing.T) {
	body := reasoningBody(t)
	pol := DefaultPolicy()
	pol.LiveTurns = 9999

	out, stats, err := RewriteResponses(body, pol)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Transformers["reasoning_prune"] != 0 {
		t.Error("pruned without being asked to")
	}
	if len(out) != len(body) {
		t.Errorf("body changed with pruning off: %d -> %d", len(body), len(out))
	}
}

// The tool call and the result answering it must stay adjacent: both APIs
// reject a request where they are not, and pruning removes items between them.
func TestPruneReasoning_LeavesToolPairsAdjacent(t *testing.T) {
	body := reasoningBody(t)
	out, _, err := RewriteResponses(body, prunePolicy())
	if err != nil {
		t.Fatal(err)
	}

	var env struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	for i, it := range env.Input {
		if it.Type != "custom_tool_call" {
			continue
		}
		if i+1 >= len(env.Input) {
			t.Fatalf("tool call %s is the final item: nothing answers it", it.CallID)
		}
		next := env.Input[i+1]
		if next.Type != "custom_tool_call_output" || next.CallID != it.CallID {
			t.Errorf("tool call %s is answered by %s/%s", it.CallID, next.Type, next.CallID)
		}
	}
}

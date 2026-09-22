package hivestate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/livezone"
)

// Today's actual bug, reproduced across the real boundary between the two
// packages: with live_turns wide enough to reach the whole conversation (a
// real, deployed setting), live-zone compression recompressed messages a
// previous turn had already forwarded — and since HiveState's own log freezes
// blocks by hashing exactly what live-zone handed it, that recompression drift
// showed up as the log discarding itself and restarting from one block,
// turn after turn.
//
// This runs the two middlewares' pure rewrite functions back to back, the way
// the real request path chains them, and checks the property that matters:
// once HiveState's log covers a span of history, further turns must not make
// it forget that span — regardless of what live-zone is doing to messages
// still ahead of it.
func TestComposedWithLivezone_StateLogDoesNotResetAcrossTurns(t *testing.T) {
	mock := &scriptedExtractor{response: func(call int) string {
		return fmt.Sprintf(`{"intent":"step_%d","difficulty":"standard","reasoning_effort":"low","active_constraints":{"values":{"step":%d}},"conversation_status":"in_progress"}`, call, call)
	}}
	engine := appendOnlyEngine(t, mock)
	scope := Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"}

	lzPol := livezone.DefaultPolicy()
	lzPol.Options.AllowLossy = true
	lzPol.Options.MinBytes = 0
	lzPol.LiveTurns = 9999 // the exact misconfiguration behind the original bug
	lzPol.Scope = "shared-conv-scope"

	var maxBlocks int
	toolReply := strings.Repeat("2026-01-02 build log line filler content here\n", 60)

	for turn := 0; turn < 12; turn++ {
		body := anthropicTurn(turn, toolReply)

		out, _, err := livezone.RewriteAnthropic(body, lzPol)
		if err != nil {
			t.Fatalf("turn %d: livezone rewrite failed: %v", turn, err)
		}

		messages := parseAnthropicMessages(out)
		if len(messages) == 0 {
			t.Fatalf("turn %d: livezone's output did not parse back into messages", turn)
		}

		res := engine.Process(context.Background(), messages, scope)
		_ = res // the log advances whether or not this turn's result was used

		history := SplitMessages(messages, engine.cfg.StepWindow).History
		key := conversationKey(scope, history)
		log, found := engine.logs.Get(key, history)
		if !found {
			continue // nothing extracted yet on an early, still-small turn
		}
		if len(log.Blocks) < maxBlocks {
			t.Fatalf("turn %d: state log shrank from %d blocks to %d — it reset", turn, maxBlocks, len(log.Blocks))
		}
		maxBlocks = len(log.Blocks)
	}

	if maxBlocks < 2 {
		t.Fatalf("the log never grew past %d block(s); the test set up too little history to exercise the composition", maxBlocks)
	}
}

// anthropicTurn builds a growing conversation: an initial exchange plus one
// new tool call/tool result pair per turn, exactly what a real client resends
// — the earlier pairs verbatim, one more appended each time.
func anthropicTurn(turn int, toolReply string) []byte {
	msgs := []map[string]any{
		{"role": "user", "content": "start the task"},
	}
	for i := 0; i <= turn; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "Bash",
					"input": map[string]string{"command": fmt.Sprintf("step %d", i)}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": toolReply},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "continue"})

	body := map[string]any{
		"model":    "claude-sonnet-4",
		"system":   strings.Repeat("You are a build agent. ", 20),
		"messages": msgs,
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

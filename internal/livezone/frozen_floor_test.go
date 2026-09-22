package livezone

import (
	"encoding/json"
	"testing"
)

// The bug this exists to fix, reproduced directly: with LiveTurns set wide
// enough to reach the whole conversation — a real, deployed setting used to
// measure the compression ceiling — a second request re-mutated tool output
// the first request had already forwarded (and the provider had presumably
// already cached), busting the cache every single turn for content that never
// changed. The tracker floor must stop that regardless of LiveTurns.
func TestFrozenFloor_SecondTurnDoesNotReopenAlreadyForwardedOutput(t *testing.T) {
	pol := DefaultPolicy()
	pol.Options.AllowLossy = true
	pol.Options.MinBytes = 0
	pol.LiveTurns = 9999 // the exact misconfiguration that caused today's bug
	pol.Scope = "team-a:key-1"

	turn1 := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash",
					"input": map[string]string{"command": "go test ./..."}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": bigLog()},
			}},
		},
	})

	out1, stats1, err := RewriteAnthropic(turn1, pol)
	if err != nil {
		t.Fatal(err)
	}
	if stats1.BlocksChanged == 0 {
		t.Fatal("first turn compressed nothing; the test fixture is not exercising the transform")
	}

	// Second turn: the client resends turn 1 verbatim (as every real client
	// does) plus a new exchange. Nothing about the OLD tool result changed.
	turn2 := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{
			map[string]any{"role": "user", "content": "run the tests"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash",
					"input": map[string]string{"command": "go test ./..."}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": bigLog()},
			}},
			map[string]any{"role": "assistant", "content": "tests passed"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": bigLog()},
			}},
		},
	})

	out2, stats2, err := RewriteAnthropic(turn2, pol)
	if err != nil {
		t.Fatal(err)
	}

	// The old tool result (message index 2) must be forwarded byte-identical
	// to what turn 1 actually sent — not re-run through the transform, which
	// would be a second, different compression of the same original bytes and
	// would look, on the wire, exactly like a busted cache.
	var forwarded1, forwarded2 struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out1, &forwarded1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out2, &forwarded2); err != nil {
		t.Fatal(err)
	}

	if len(forwarded2.Messages) < 3 {
		t.Fatalf("turn 2 has %d messages, expected at least 3", len(forwarded2.Messages))
	}
	if string(forwarded1.Messages[2].Content) != string(forwarded2.Messages[2].Content) {
		t.Fatalf("the already-forwarded tool result changed between turns\nturn1: %s\nturn2: %s",
			forwarded1.Messages[2].Content, forwarded2.Messages[2].Content)
	}

	if stats2.BlocksChanged == 0 {
		t.Fatal("turn 2 compressed nothing at all; the new tool result should still be live")
	}
}

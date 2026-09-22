package hivestate

import (
	"fmt"
	"strings"
	"testing"
)

// The append-only log assumes the History zone grows as a prefix: turn N's
// history must be the opening of turn N+1's. If the zone splitter ever
// reshuffles what falls into History, every frozen block describes turns that
// are no longer where the log thinks they are.
func TestSplitMessages_HistoryGrowsAsAPrefix(t *testing.T) {
	turn := strings.Repeat("body ", 20)
	messages := []Message{
		{Role: "system", Content: "policy"},
		{Role: "user", Content: turn + " one"},
		{Role: "assistant", Content: turn + " two"},
		{Role: "user", Content: turn + " three"},
		{Role: "assistant", Content: turn + " four"},
		{Role: "user", Content: turn + " five"},
		{Role: "assistant", Content: "latest step"},
		{Role: "user", Content: "Continue."},
	}

	prev := SplitMessages(messages, 1).History
	for i := 0; i < 3; i++ {
		messages = append(messages[:len(messages)-1],
			Message{Role: "assistant", Content: turn + fmt.Sprintf(" reply %d", i)},
			Message{Role: "user", Content: turn + fmt.Sprintf(" ask %d", i)},
			Message{Role: "user", Content: "Continue."},
		)
		next := SplitMessages(messages, 1).History

		if len(next) < len(prev) {
			t.Fatalf("turn %d: history shrank from %d to %d", i, len(prev), len(next))
		}
		for j := range prev {
			if prev[j].Role != next[j].Role || prev[j].Content != next[j].Content {
				t.Fatalf("turn %d: history[%d] changed\nwas:  [%s] %.60q\nnow:  [%s] %.60q",
					i, j, prev[j].Role, prev[j].Content, next[j].Role, next[j].Content)
			}
		}
		prev = next
	}
}

// The log freezes a hash of the history it summarised. If Process edits the
// caller's messages in place, that hash describes something the next turn will
// never reproduce, and every log is discarded on the turn after it is built.
func TestProcess_DoesNotMutateCallerMessages(t *testing.T) {
	mock := &scriptedExtractor{response: func(int) string {
		return `{"intent":"hold","difficulty":"standard","reasoning_effort":"low","active_constraints":{},"conversation_status":"in_progress"}`
	}}
	engine := appendOnlyEngine(t, mock)

	turn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	messages := baseConversation(turn, "first")
	before := historyHash(messages)

	engine.Process(t.Context(), messages, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})

	if after := historyHash(messages); after != before {
		t.Fatalf("Process modified the caller's messages in place\nbefore: %s\nafter:  %s", before, after)
	}
}

package hivestate

import (
	"strings"
	"testing"
	"time"
)

func msgs(n int) []Message {
	out := make([]Message, n)
	for i := range out {
		out[i] = Message{Role: "user", Content: string(rune('a'+i%26)) + "-turn"}
	}
	return out
}

func appendBlock(t *testing.T, log *StateLog, history []Message, upTo int, json string) {
	t.Helper()
	res := &ExtractionResult{JSON: json, State: &State{Intent: json}}
	if !log.Append(upTo, res, historyHash(history[:upTo])) {
		t.Fatalf("append of block covering %d was refused", upTo)
	}
}

// The property the whole design rests on: adding a block may only add bytes at
// the end. If a shorter render ever stops being a prefix of a longer one, the
// provider re-reads the entire prompt from that point and the design has
// negative value.
func TestStateLog_RenderGrowsOnlyByAppending(t *testing.T) {
	history := msgs(30)
	log := &StateLog{}

	var renders []string
	for _, upTo := range []int{5, 12, 19, 27} {
		appendBlock(t, log, history, upTo, `{"intent":"step to `+string(rune('0'+upTo%10))+`"}`)
		renders = append(renders, log.Render())
	}

	for i := 1; i < len(renders); i++ {
		if !strings.HasPrefix(renders[i], renders[i-1]) {
			t.Fatalf("render %d is not a byte-exact prefix of render %d\nbefore:\n%q\nafter:\n%q",
				i-1, i, renders[i-1], renders[i])
		}
		if len(renders[i]) <= len(renders[i-1]) {
			t.Fatalf("render %d did not grow", i)
		}
	}
}

// A block that would cover no new ground must be refused: accepting it would
// either duplicate text or, worse, invite a caller to rewrite history.
func TestStateLog_RefusesNonAdvancingBlocks(t *testing.T) {
	history := msgs(10)
	log := &StateLog{}
	appendBlock(t, log, history, 6, `{"intent":"first"}`)

	for _, upTo := range []int{6, 3, 0, -1} {
		res := &ExtractionResult{JSON: `{"intent":"again"}`}
		if log.Append(upTo, res, "h") {
			t.Errorf("append covering %d was accepted; it does not advance past %d", upTo, log.Covered())
		}
	}
	if log.Covered() != 6 {
		t.Fatalf("Covered() = %d, want 6", log.Covered())
	}
	if n := strings.Count(log.Render(), "--- state ---"); n != 1 {
		t.Fatalf("render carries %d blocks, want 1", n)
	}
}

// Empty extractions are refused too: a block with nothing in it costs bytes in
// every future prompt and says nothing.
func TestStateLog_RefusesEmptyBlocks(t *testing.T) {
	log := &StateLog{}
	for _, empty := range []string{"", "   ", "\n"} {
		if log.Append(3, &ExtractionResult{JSON: empty}, "h") {
			t.Errorf("accepted an empty block %q", empty)
		}
	}
	if log.Append(3, nil, "h") {
		t.Error("accepted a nil extraction result")
	}
}

// The parts are what reaches the wire, so they must say exactly what Render
// says, and every part but the last must be untouched by a new block. That is
// the property Anthropic's block-level cache matching depends on.
func TestStateLog_RenderPartsMatchRenderAndStayFrozen(t *testing.T) {
	history := msgs(30)
	log := &StateLog{}
	appendBlock(t, log, history, 6, `{"intent":"a"}`)
	appendBlock(t, log, history, 13, `{"intent":"b"}`)
	before := log.RenderParts()

	appendBlock(t, log, history, 21, `{"intent":"c"}`)
	after := log.RenderParts()

	if joined := strings.Join(after, ""); joined != log.Render() {
		t.Fatalf("parts do not reassemble into Render()\nparts: %q\nrender: %q", joined, log.Render())
	}
	if len(after) != len(before)+1 {
		t.Fatalf("appending one block produced %d parts, want %d", len(after), len(before)+1)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("part %d changed when a block was appended\nbefore: %q\nafter:  %q", i, before[i], after[i])
		}
	}
}

// Routing reads the state from the newest block, because a later block
// supersedes an earlier one.
func TestStateLog_CurrentIsTheNewestState(t *testing.T) {
	history := msgs(12)
	log := &StateLog{}
	if log.Current() != nil {
		t.Fatal("an empty log reported a current state")
	}

	log.Append(4, &ExtractionResult{JSON: `{"intent":"a"}`, State: &State{Intent: "explore_repo"}}, historyHash(history[:4]))
	log.Append(9, &ExtractionResult{JSON: `{"intent":"b"}`, State: &State{Intent: "fix_build"}}, historyHash(history[:9]))

	if got := log.Current(); got == nil || got.Intent != "fix_build" {
		t.Fatalf("Current() = %+v, want the newest intent fix_build", got)
	}
}

// A log describes a specific conversation. If the history it was built from is
// no longer what the caller is sending, the state describes turns that no
// longer exist and must not be reused.
func TestStateLog_MatchesOnlyItsOwnHistory(t *testing.T) {
	history := msgs(20)
	log := &StateLog{}
	appendBlock(t, log, history, 8, `{"intent":"a"}`)
	appendBlock(t, log, history, 15, `{"intent":"b"}`)

	if !log.Matches(history) {
		t.Fatal("log does not match the history it was built from")
	}
	if !log.Matches(append(append([]Message{}, history...), msgs(3)...)) {
		t.Error("a conversation that only grew should still match")
	}

	edited := append([]Message{}, history...)
	edited[4] = Message{Role: "user", Content: "something else entirely"}
	if log.Matches(edited) {
		t.Error("a log matched a history that was edited underneath it")
	}
	if log.Matches(history[:10]) {
		t.Error("a log matched a history too short to contain its blocks")
	}
}

// The step window is not fixed, so History can shrink between turns. Dropping
// the whole log on that wobble restarts the state from nothing: the prompt
// collapses and every state item changes at once, losing the provider cache.
// Blocks that still describe the history must survive.
func TestStateLog_KeepsTheBlocksThatStillMatch(t *testing.T) {
	history := msgs(30)
	log := &StateLog{}
	appendBlock(t, log, history, 5, `{"intent":"a"}`)
	appendBlock(t, log, history, 11, `{"intent":"b"}`)
	appendBlock(t, log, history, 18, `{"intent":"c"}`)

	// A wider window leaves less history behind, so the newest block no longer
	// has the turns it summarised.
	shorter := history[:14]
	kept := log.MatchingBlocks(shorter)
	if kept != 2 {
		t.Fatalf("MatchingBlocks = %d, want the two blocks that still fit", kept)
	}

	before := log.RenderParts()
	log.TruncateTo(kept)
	after := log.RenderParts()

	if len(after) != 3 {
		t.Fatalf("after truncation the log renders %d parts, want framing + 2 blocks", len(after))
	}
	for i := range after {
		if before[i] != after[i] {
			t.Fatalf("part %d changed on truncation: %q vs %q", i, before[i], after[i])
		}
	}
	if log.Covered() != 11 {
		t.Fatalf("Covered() = %d, want 11", log.Covered())
	}
}

// Two conversations that happen to open the same way, but belong to different
// callers or sessions, must not share a log.
func TestConversationKey_SeparatesCallersAndSessions(t *testing.T) {
	history := msgs(4)
	base := Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"}

	same := conversationKey(base, history)
	if same != conversationKey(base, append(append([]Message{}, history...), msgs(2)...)) {
		t.Error("the key changed as the conversation grew; it must not")
	}

	for _, other := range []Scope{
		{TeamID: "t2", KeyHash: "k1", SessionID: "s1"},
		{TeamID: "t1", KeyHash: "k2", SessionID: "s1"},
		{TeamID: "t1", KeyHash: "k1", SessionID: "s2"},
	} {
		if conversationKey(other, history) == same {
			t.Errorf("scope %+v produced the same key as %+v", other, base)
		}
	}

	different := append([]Message{}, history...)
	different[0] = Message{Role: "user", Content: "a different opening"}
	if conversationKey(base, different) == same {
		t.Error("a different opening message produced the same key")
	}
}

func TestMemoryLogStore_RoundTripsAndEvicts(t *testing.T) {
	store := newMemoryLogStore(2, time.Hour)
	history := msgs(10)

	first := &StateLog{}
	appendBlock(t, first, history, 5, `{"intent":"one"}`)
	store.Put("a", first)

	got, ok := store.Get("a", history)
	if !ok || got.Render() != first.Render() {
		t.Fatal("a stored log did not come back identical")
	}
	if _, ok := store.Get("missing", history); ok {
		t.Error("an unknown key returned a log")
	}

	second := &StateLog{}
	appendBlock(t, second, history, 5, `{"intent":"two"}`)
	third := &StateLog{}
	appendBlock(t, third, history, 5, `{"intent":"three"}`)
	store.Put("b", second)
	store.Put("c", third)
	if len(store.logs) > 2 {
		t.Fatalf("store holds %d keys, above its cap of 2", len(store.logs))
	}
}

// A key is an opening fingerprint, not an identity: two conversations that
// begin the same way land on it. With one log per key they overwrite each
// other every turn and both restart from nothing forever.
func TestMemoryLogStore_KeepsCollidingConversationsApart(t *testing.T) {
	store := newMemoryLogStore(8, time.Hour)

	shared := msgs(6)
	longRun := append(append([]Message{}, shared...), msgs(20)...)
	shortRun := append(append([]Message{}, shared...), Message{Role: "user", Content: "a different path"})

	longLog := &StateLog{}
	appendBlock(t, longLog, longRun, 18, `{"intent":"long"}`)
	shortLog := &StateLog{}
	appendBlock(t, shortLog, shortRun, 7, `{"intent":"short"}`)

	store.Put("same-key", longLog)
	store.Put("same-key", shortLog)

	gotLong, ok := store.Get("same-key", longRun)
	if !ok || gotLong.Current().Intent != `{"intent":"long"}` {
		t.Fatalf("the long conversation got the wrong log: %+v", gotLong)
	}
	gotShort, ok := store.Get("same-key", shortRun)
	if !ok || gotShort.Current().Intent != `{"intent":"short"}` {
		t.Fatalf("the short conversation got the wrong log: %+v", gotShort)
	}
}

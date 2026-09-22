package hivestate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func ccrTestScope() Scope {
	return Scope{TeamID: "team-1", KeyHash: "key-1", SessionID: "sess-1"}
}

// fileReadHistory returns the message pair extractFileReads recognises as a
// read of path.
func fileReadHistory(path, content string) []Message {
	return []Message{
		{Role: "assistant", Content: "Read(" + path + ")"},
		{Role: "tool", Content: content},
	}
}

// A file read on turn 3 is still sitting in History on turn 20, so Store
// re-extracts it every turn. Re-stamping the entry each time reset createdAt
// and lastCallSeen, which made both the decay check and the TTL unreachable:
// stale files stayed in the working set forever and were re-injected on every
// turn, up to the full retrieval budget.
func TestCCRStore_UnmentionedFileDecaysOutOfWorkingSet(t *testing.T) {
	store := newCCRStore(8, 8, time.Hour)
	scope := ccrTestScope()
	counter := NewTokenCounter()

	history := fileReadHistory("/src/legacy.go", strings.Repeat("legacy code line\n", 20))

	// Turn 1: the read happens and the file enters the working set.
	store.Store(scope, history, counter)
	if got := store.GetWorkingSet(scope, []string{"/src/legacy.go"}, 8000, counter); len(got) == 0 {
		t.Fatal("the file should be retrievable on the turn it was read")
	}

	// Later turns never mention it, but History still carries the original
	// read, so Store keeps re-extracting it.
	for turn := 0; turn < store.decayCalls+2; turn++ {
		store.Store(scope, history, counter)
		store.GetWorkingSet(scope, []string{"something unrelated"}, 8000, counter)
	}

	if got := store.GetWorkingSet(scope, []string{"something unrelated"}, 8000, counter); len(got) != 0 {
		t.Fatalf("after %d turns without a mention the file should have decayed out, got %d messages",
			store.decayCalls+2, len(got))
	}
}

// The TTL was unreachable for the same reason: createdAt was refreshed on every
// re-extraction, so an entry was never older than one turn.
func TestCCRStore_EntryExpiresOnTTL(t *testing.T) {
	store := newCCRStore(8, 8, 30*time.Millisecond)
	scope := ccrTestScope()
	counter := NewTokenCounter()
	history := fileReadHistory("/src/app.go", strings.Repeat("app code line\n", 20))

	store.Store(scope, history, counter)
	time.Sleep(60 * time.Millisecond)
	// Re-extracting the same read must not reset the clock.
	store.Store(scope, history, counter)

	if got := store.GetWorkingSet(scope, []string{"/src/app.go"}, 8000, counter); len(got) != 0 {
		t.Fatalf("entry outlived its TTL, got %d messages", len(got))
	}
}

// A genuine re-read with different content is a new read and must reset,
// otherwise an edited file would be recovered at its stale contents.
func TestCCRStore_ChangedContentReplacesEntry(t *testing.T) {
	store := newCCRStore(8, 8, time.Hour)
	scope := ccrTestScope()
	counter := NewTokenCounter()
	path := "/src/edited.go"

	store.Store(scope, fileReadHistory(path, strings.Repeat("old body line\n", 20)), counter)
	store.Store(scope, fileReadHistory(path, strings.Repeat("new body line\n", 20)), counter)

	got := store.GetWorkingSet(scope, []string{path}, 8000, counter)
	if len(got) == 0 {
		t.Fatal("the re-read file should be retrievable")
	}
	joined := strings.Join(func() []string {
		out := make([]string, 0, len(got))
		for _, m := range got {
			out = append(out, m.Content)
		}
		return out
	}(), "\n")
	if !strings.Contains(joined, "new body line") {
		t.Error("the working set still holds the stale contents")
	}
	if strings.Contains(joined, "old body line") {
		t.Error("the stale contents were not replaced")
	}
}

// Entry counts alone do not bound memory: maxScopes*maxPerScope entries of the
// largest content extractFileReads accepts is hundreds of megabytes, and
// nothing else applies eviction pressure.
func TestCCRStore_ByteBudgetsAreEnforced(t *testing.T) {
	store := newCCRStore(64, 32, time.Hour)
	counter := NewTokenCounter()
	big := strings.Repeat("x", 40000)

	for s := 0; s < 64; s++ {
		scope := Scope{TeamID: "team-1", KeyHash: "key-1", SessionID: fmt.Sprintf("sess-%d", s)}
		for f := 0; f < 32; f++ {
			path := fmt.Sprintf("/src/f%02d_%02d.go", s, f)
			store.Store(scope, fileReadHistory(path, big), counter)
		}
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	if store.bytes > ccrMaxTotalBytes {
		t.Errorf("store retains %d bytes, over the %d budget", store.bytes, ccrMaxTotalBytes)
	}
	sum := 0
	for key, sc := range store.scopes {
		if sc.bytes > ccrMaxScopeBytes {
			t.Errorf("scope %s retains %d bytes, over the %d per-scope budget",
				key, sc.bytes, ccrMaxScopeBytes)
		}
		content := 0
		for _, e := range sc.files {
			content += len(e.content)
		}
		if content != sc.bytes {
			t.Errorf("scope %s byte counter drifted: tracked %d, actual %d", key, sc.bytes, content)
		}
		sum += content
	}
	if sum != store.bytes {
		t.Errorf("store byte counter drifted: tracked %d, actual %d", store.bytes, sum)
	}
}

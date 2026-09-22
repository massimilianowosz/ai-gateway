package promptcache

import (
	"fmt"
	"testing"
	"time"
)

func rawMsgs(n int, seed string) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf(`{"role":"user","content":"%s-%d"}`, seed, i))
	}
	return out
}

func compressedMsgs(n int, seed string) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf(`{"role":"user","content":"%s-%d[compressed]"}`, seed, i))
	}
	return out
}

// The whole mechanism exists for this: a client resending the same prefix
// across turns must produce a growing floor, so a transformer stops being
// able to re-open content the previous turn already forwarded.
func TestTracker_MatchingPrefixGrowsTheFloor(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	turn1 := HashAll(rawMsgs(5, "conv"))
	tr.Confirm("k", turn1, rawMsgs(5, "conv"), 5)

	turn2 := HashAll(append(rawMsgs(5, "conv"), rawMsgs(3, "conv")[:3]...))
	n, _ := tr.Frozen("k", turn2)
	if n != 5 {
		t.Fatalf("Frozen = %d, want 5 (all of turn1 matched)", n)
	}
	turn2Forwarded := append(rawMsgs(5, "conv"), rawMsgs(3, "conv")[:3]...)
	tr.Confirm("k", turn2, turn2Forwarded, 8)

	turn3 := HashAll(append(append(rawMsgs(5, "conv"), rawMsgs(3, "conv")[:3]...), rawMsgs(1, "conv")...))
	n, _ = tr.Frozen("k", turn3)
	if n != 8 {
		t.Fatalf("Frozen = %d, want 8 (all of turn2 matched)", n)
	}
}

// The reason Frozen returns bytes and not just a count: turn 1 may have
// compressed a message before forwarding it. Turn 2 must reuse exactly those
// forwarded bytes, not the client's raw resend — forwarding the raw form
// instead would itself be a change on the wire, exactly as cache-busting as a
// fresh, different recompression.
func TestTracker_FrozenReturnsWhatWasForwardedNotWhatTheClientSent(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	client := rawMsgs(3, "conv")
	forwarded := compressedMsgs(3, "conv") // turn 1 compressed before sending
	tr.Confirm("k", HashAll(client), forwarded, 3)

	n, got := tr.Frozen("k", HashAll(client))
	if n != 3 {
		t.Fatalf("Frozen = %d, want 3", n)
	}
	for i := range got {
		if string(got[i]) != string(forwarded[i]) {
			t.Fatalf("forwarded[%d] = %q, want the previously-forwarded %q", i, got[i], forwarded[i])
		}
		if string(got[i]) == string(client[i]) {
			t.Fatalf("forwarded[%d] equals the client's raw content; turn 1's compression was lost", i)
		}
	}
}

// A conversation that diverges at the very first message — a different
// conversation sharing the same key, or an edited history — must get a floor
// of 0, not a false positive from a partial resemblance.
func TestTracker_DivergingHistoryGetsNoFloor(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	tr.Confirm("k", HashAll(rawMsgs(5, "conv-a")), rawMsgs(5, "conv-a"), 5)

	n, got := tr.Frozen("k", HashAll(rawMsgs(5, "conv-b")))
	if n != 0 || got != nil {
		t.Fatalf("Frozen = (%d, %v), want (0, nil) for an unrelated history", n, got)
	}
}

// A floor stops exactly where the histories stop agreeing — mid-conversation
// edits or a shorter resend must not be treated as a full match.
func TestTracker_FloorStopsAtFirstDivergence(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	base := rawMsgs(6, "conv")
	tr.Confirm("k", HashAll(base), base, 6)

	edited := append([][]byte(nil), base...)
	edited[3] = []byte(`{"role":"user","content":"edited"}`)
	n, _ := tr.Frozen("k", HashAll(edited))
	if n != 3 {
		t.Fatalf("Frozen = %d, want 3 (stop at the edited message)", n)
	}
}

func TestTracker_UnknownKeyOrExpiredEntryReturnsZero(t *testing.T) {
	tr := NewTracker(8, time.Millisecond)
	n, _ := tr.Frozen("missing", HashAll(rawMsgs(3, "x")))
	if n != 0 {
		t.Fatalf("Frozen = %d, want 0 for an unknown key", n)
	}

	tr.Confirm("k", HashAll(rawMsgs(3, "x")), rawMsgs(3, "x"), 3)
	time.Sleep(5 * time.Millisecond)
	n, _ = tr.Frozen("k", HashAll(rawMsgs(3, "x")))
	if n != 0 {
		t.Fatalf("Frozen = %d, want 0 once the TTL has lapsed", n)
	}
}

// Confirm must not retroactively change the slices it was given.
func TestTracker_ConfirmCopiesItsInput(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	hashes := HashAll(rawMsgs(4, "conv"))
	forwarded := rawMsgs(4, "conv")
	tr.Confirm("k", hashes, forwarded, 4)
	hashes[0] = "tampered"
	forwarded[0] = []byte("tampered")

	n, got := tr.Frozen("k", HashAll(rawMsgs(4, "conv")))
	if n != 4 {
		t.Fatalf("Frozen = %d, want 4; Confirm must have copied its hashes", n)
	}
	if string(got[0]) == "tampered" {
		t.Fatal("Confirm must have copied its forwarded bytes")
	}
}

// A caller must only claim alignment it can prove. Confirm must not silently
// widen that claim from what the caller (correctly, conservatively) passed —
// a wider claim is exactly what would let a later removal shift positions
// without anyone noticing, splicing one message's bytes into another's slot.
func TestTracker_ConfirmNeverStoresBeyondTheProvenAlignment(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	hashes := HashAll(rawMsgs(5, "conv"))
	// A message was removed from the middle: forwarded[2] is no longer the
	// message hashes[2] describes, so alignment can only be proven up to 2.
	forwarded := rawMsgs(5, "conv")[:2]
	tr.Confirm("k", hashes, forwarded, 2)

	n, got := tr.Frozen("k", hashes)
	if n != 2 || len(got) != 2 {
		t.Fatalf("Frozen = (%d, len=%d), want (2, 2); a wider match risks a misaligned splice", n, len(got))
	}
}

// A caller that removed messages beyond its own proven boundary (e.g.
// pruning outside the frozen prefix) must pass the SMALLER, honest
// alignedUpTo — Confirm clamps to whatever is actually available, so passing
// too large a claim does not overrun either slice.
func TestTracker_ConfirmClampsAnOverstatedAlignment(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	hashes := HashAll(rawMsgs(5, "conv"))
	forwarded := rawMsgs(3, "conv")
	tr.Confirm("k", hashes, forwarded, 10) // overstated on purpose

	n, got := tr.Frozen("k", hashes)
	if n != 3 || len(got) != 3 {
		t.Fatalf("Frozen = (%d, len=%d), want (3, 3)", n, len(got))
	}
}

func TestTracker_EvictsOldestWhenFull(t *testing.T) {
	tr := NewTracker(2, time.Hour)
	tr.Confirm("a", HashAll(rawMsgs(2, "a")), rawMsgs(2, "a"), 2)
	time.Sleep(time.Millisecond)
	tr.Confirm("b", HashAll(rawMsgs(2, "b")), rawMsgs(2, "b"), 2)
	time.Sleep(time.Millisecond)
	tr.Confirm("c", HashAll(rawMsgs(2, "c")), rawMsgs(2, "c"), 2) // should evict "a"

	if n, _ := tr.Frozen("a", HashAll(rawMsgs(2, "a"))); n != 0 {
		t.Fatalf("Frozen(a) = %d, want 0; oldest entry should have been evicted", n)
	}
	if n, _ := tr.Frozen("c", HashAll(rawMsgs(2, "c"))); n != 2 {
		t.Fatalf("Frozen(c) = %d, want 2; newest entry should survive", n)
	}
}

// Package promptcache tracks, per conversation, how much of the client's
// resent message history matched the previous turn — the part almost
// certainly already sitting in the provider's prompt cache.
//
// Today's bug, found by measurement rather than by design: live-zone
// compression decides what it may touch from a turn-count window
// (live_turns), not from cache awareness. With live_turns configured wide
// enough to reach whole conversations (a real, deployed setting — canary
// measurement wanted the ceiling), it mutated 38-39 blocks per request,
// including messages a previous turn had already forwarded and the provider
// had already cached. Every such mutation invalidated the cached prefix from
// that point on, at a moment nothing else in the request path could see or
// prevent.
//
// A message the client resent byte-identical to what it sent last turn needs
// no repair to stay cache-stable — almost. It needs one thing more: last
// turn may itself have compressed that message before forwarding it, so
// "unchanged" and "forward the client's raw bytes" are different claims. A
// transformer that forwards the raw bytes on turn 2 after forwarding a
// compressed form on turn 1 busts the cache exactly as surely as one that
// recompresses differently each time — the bytes on the wire still changed.
// Tracker's answer is therefore not just a floor but the exact bytes to
// reuse: for however many leading messages match last turn's, verbatim what
// was actually forwarded for them last time, so the wire content is provably
// unchanged rather than merely left alone.
package promptcache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Tracker records the client-sent message hashes seen on each conversation's
// most recent turn.
type Tracker struct {
	mu      sync.RWMutex
	entries map[string]*entry
	maxSize int
	ttl     time.Duration
}

type entry struct {
	hashes    []string
	forwarded [][]byte
	at        time.Time
}

// NewTracker creates a Tracker holding up to maxSize conversations, each
// forgotten after ttl of inactivity.
func NewTracker(maxSize int, ttl time.Duration) *Tracker {
	return &Tracker{entries: make(map[string]*entry, maxSize), maxSize: maxSize, ttl: ttl}
}

// Frozen returns how many of hashes' leading entries match the previous
// turn's recorded hashes for this conversation, in order, and the exact bytes
// that were forwarded for them last time. 0 and nil if the conversation is
// unknown, expired, or diverges at the first message.
//
// The returned bytes are what a caller must forward for that many leading
// messages this turn — not the client's original content for them, and not a
// fresh recompression of it. Either of those would change the wire bytes,
// which is indistinguishable from a cache bust regardless of why they changed.
func (t *Tracker) Frozen(key string, hashes []string) (n int, forwarded [][]byte) {
	if key == "" {
		return 0, nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[key]
	if !ok || time.Since(e.at) > t.ttl {
		return 0, nil
	}
	for n < len(e.hashes) && n < len(hashes) && e.hashes[n] == hashes[n] {
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, append([][]byte(nil), e.forwarded[:n]...)
}

// Confirm records this turn's client-sent hashes and the bytes actually
// forwarded for each, as the comparison and reuse point for the next turn.
// hashes must come from the RAW incoming messages — the client resends its
// own original content next turn, not whatever was forwarded, so that is what
// next turn's Frozen call compares against.
//
// alignedUpTo is how many LEADING pairs the caller can prove are positionally
// aligned — typically its own frozen-floor boundary, below which nothing it
// ran was allowed to insert or remove a message. Beyond that point a
// transform that removes messages (pruning stale reasoning, deduplication)
// shifts every later position, so hashes[i] and forwarded[i] stop describing
// the same message; storing them paired anyway would let a later Frozen call
// splice one message's bytes into another message's position. Passing
// anything greater than what is actually proven is a correctness bug in the
// caller, not a tolerance this function can compensate for.
func (t *Tracker) Confirm(key string, hashes []string, forwarded [][]byte, alignedUpTo int) {
	if key == "" {
		return
	}
	n := alignedUpTo
	if n > len(hashes) {
		n = len(hashes)
	}
	if n > len(forwarded) {
		n = len(forwarded)
	}
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.entries[key]; !exists && len(t.entries) >= t.maxSize {
		t.evictOldestLocked()
	}
	t.entries[key] = &entry{
		hashes:    append([]string(nil), hashes[:n]...),
		forwarded: append([][]byte(nil), forwarded[:n]...),
		at:        time.Now(),
	}
}

func (t *Tracker) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range t.entries {
		if oldestKey == "" || e.at.Before(oldest) {
			oldestKey, oldest = k, e.at
		}
	}
	if oldestKey != "" {
		delete(t.entries, oldestKey)
	}
}

// HashAll hashes each raw message so Frozen/Confirm can compare conversations
// without holding their content in memory.
func HashAll(raw [][]byte) []string {
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = Hash(r)
	}
	return out
}

// Hash hashes a single raw message.
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

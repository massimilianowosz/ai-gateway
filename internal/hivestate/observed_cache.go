package hivestate

import (
	"sync"
	"time"
)

// The prefix-cache guard's implicit-cache detectors (detectOpenAIPrefix,
// detectResponsesPrefix) can only guess how much of a prompt the provider
// still has cached: OpenAI exposes no markers, so the guess is purely
// positional — "everything before the newest user turn is warm". That guess
// has no way to notice a cache that already collapsed (TTL eviction, an
// upstream limit, whatever): it just keeps assuming the best case forever,
// which lets the guard skip a rewrite to "protect" a cache that in reality
// stopped growing turns ago.
//
// The gateway does see ground truth for this, one response at a time:
// store.UsageCapture.CachedPromptTokens is the real cached-read count the
// provider just reported. observedCacheStore remembers the latest one per
// conversation scope so the NEXT request's guess can be capped by what
// actually happened last turn, instead of trusting the positional guess
// unconditionally.
type observedCacheEntry struct {
	tokens int
	at     time.Time
}

type observedCacheStore struct {
	mu      sync.Mutex
	entries map[string]observedCacheEntry
	ttl     time.Duration
}

func newObservedCacheStore(ttl time.Duration) *observedCacheStore {
	return &observedCacheStore{entries: make(map[string]observedCacheEntry), ttl: ttl}
}

// record saves the real cached-prompt-token count observed for a scope.
func (s *observedCacheStore) record(key string, tokens int) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = observedCacheEntry{tokens: tokens, at: time.Now()}
}

// get returns the last observed count for a scope, if any and not stale.
func (s *observedCacheStore) get(key string) (int, bool) {
	if key == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || time.Since(e.at) > s.ttl {
		return 0, false
	}
	return e.tokens, true
}

// globalObservedCache is process-wide: the guard needs the previous request's
// real usage regardless of which goroutine served it. The TTL matches a
// provider prompt cache's own idle lifetime, so a stale entry from a long-
// dead conversation cannot cap a fresh one that happens to reuse the scope.
var globalObservedCache = newObservedCacheStore(30 * time.Minute)

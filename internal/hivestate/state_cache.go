package hivestate

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// stateCache is an in-memory LRU cache for state extraction results.
// Key: SHA256 hash of History zone messages. Value: extracted state + timestamp.
type stateCache struct {
	mu      sync.RWMutex
	entries map[string]*stateCacheEntry
	maxSize int
	ttl     time.Duration
}

type stateCacheEntry struct {
	stateJSON   string
	state       *State
	createdAt   time.Time
	historyLen  int    // number of messages in the History zone when this was extracted
	historyHash string // full hash of the history at extraction time
}

func newStateCache(maxSize int, ttl time.Duration) *stateCache {
	return &stateCache{
		entries: make(map[string]*stateCacheEntry, maxSize),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// historyHash computes a deterministic hash of a History zone.
func historyHash(messages []Message) string {
	h := sha256.New()
	for _, m := range messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
		h.Write([]byte(m.ToolCallID))
		h.Write([]byte{1}) // message separator
	}
	return hex.EncodeToString(h.Sum(nil))
}

// get returns a cached state if it exists and hasn't expired.
func (c *stateCache) get(key string) (string, *State, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok {
		return "", nil, false
	}
	if time.Since(entry.createdAt) > c.ttl {
		return "", nil, false
	}
	return entry.stateJSON, entry.state, true
}

// put stores a state extraction result. Evicts oldest if at capacity.
func (c *stateCache) put(key string, stateJSON string, state *State, historyLen int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Simple eviction: if full, delete oldest entry
	if len(c.entries) >= c.maxSize {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range c.entries {
			if oldestKey == "" || v.createdAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.createdAt
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}

	c.entries[key] = &stateCacheEntry{
		stateJSON:   stateJSON,
		state:       state,
		createdAt:   time.Now(),
		historyLen:  historyLen,
		historyHash: key,
	}
}

// findPrefix looks for a cached entry whose history is a prefix of the given messages.
// Returns the cached state and the number of messages already extracted, or nil if no prefix match.
func (c *stateCache) findPrefix(messages []Message) (string, *State, int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Look for an entry with a shorter history that is a prefix of current messages
	var bestEntry *stateCacheEntry
	var bestLen int

	for _, entry := range c.entries {
		if time.Since(entry.createdAt) > c.ttl {
			continue
		}
		// Entry's history must be shorter than current
		if entry.historyLen >= len(messages) || entry.historyLen <= 0 {
			continue
		}
		// Check if entry's history is a prefix: hash of messages[:entry.historyLen] == entry.historyHash
		prefixHash := historyHash(messages[:entry.historyLen])
		if prefixHash == entry.historyHash && entry.historyLen > bestLen {
			bestEntry = entry
			bestLen = entry.historyLen
		}
	}

	if bestEntry == nil {
		return "", nil, 0, false
	}

	return bestEntry.stateJSON, bestEntry.state, bestEntry.historyLen, true
}

// stats returns cache size for observability.
func (c *stateCache) stats() (size int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

package livezone

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Vault holds the originals of content a lossy transform removed, so a caller
// — an agent through its shell, a human reading a transcript — can ask for
// what was taken out.
//
// Entries are filed under a scope and only ever returned under that identical
// scope. What it holds is verbatim tool output: source, logs, whatever the
// conversation carried. A lookup that crossed scopes would hand one tenant
// another tenant's data, so an under-specified scope stores nothing rather
// than falling back to a shared bucket.
type Vault struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // least-recently stored at the front
	bytes   int
	maxEnt  int
	maxByte int
	ttl     time.Duration
}

type vaultEntry struct {
	key      string
	content  string
	storedAt time.Time
}

// NewVault returns a store bounded by both entry count and total bytes. Either
// bound at zero disables the store: a vault that cannot forget is a leak, not
// a cache.
func NewVault(maxEntries, maxBytes int, ttl time.Duration) *Vault {
	if maxEntries <= 0 || maxBytes <= 0 || ttl <= 0 {
		return nil
	}
	return &Vault{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		maxEnt:  maxEntries,
		maxByte: maxBytes,
		ttl:     ttl,
	}
}

// ID derives the identifier for a piece of content.
//
// It is a hash of the content itself, not a random token, and that is load
// bearing: the identifier ends up inside the prompt, so it has to come out the
// same on every turn. A fresh random id per request would change the bytes the
// provider sees and cost the prefix cache the request was trying to protect.
func ID(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:6])
}

// Put files content under a scope and returns its identifier. A zero vault or
// an empty scope stores nothing and reports false, which leaves the caller to
// forward the content without a retrieval offer it could not honour.
func (v *Vault) Put(scope, content string) (string, bool) {
	if v == nil || scope == "" || content == "" {
		return "", false
	}
	id := ID(content)
	key := scope + ":" + id

	v.mu.Lock()
	defer v.mu.Unlock()
	v.expireLocked()

	if el, ok := v.entries[key]; ok {
		el.Value.(*vaultEntry).storedAt = time.Now()
		v.order.MoveToBack(el)
		return id, true
	}
	if len(content) > v.maxByte {
		// One payload larger than the whole budget would evict everything else
		// to store something nobody can afford to keep.
		return "", false
	}

	el := v.order.PushBack(&vaultEntry{key: key, content: content, storedAt: time.Now()})
	v.entries[key] = el
	v.bytes += len(content)
	v.evictLocked()
	return id, true
}

// Get returns content stored under the same scope.
func (v *Vault) Get(scope, id string) (string, bool) {
	if v == nil || scope == "" || id == "" {
		return "", false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.expireLocked()

	el, ok := v.entries[scope+":"+id]
	if !ok {
		return "", false
	}
	return el.Value.(*vaultEntry).content, true
}

func (v *Vault) expireLocked() {
	cutoff := time.Now().Add(-v.ttl)
	for {
		el := v.order.Front()
		if el == nil || el.Value.(*vaultEntry).storedAt.After(cutoff) {
			return
		}
		v.removeLocked(el)
	}
}

func (v *Vault) evictLocked() {
	for (len(v.entries) > v.maxEnt || v.bytes > v.maxByte) && v.order.Len() > 0 {
		v.removeLocked(v.order.Front())
	}
}

func (v *Vault) removeLocked(el *list.Element) {
	entry := el.Value.(*vaultEntry)
	v.order.Remove(el)
	delete(v.entries, entry.key)
	v.bytes -= len(entry.content)
}

// Stats reports what the vault currently holds.
func (v *Vault) Stats() (entries, bytes int) {
	if v == nil {
		return 0, 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.entries), v.bytes
}

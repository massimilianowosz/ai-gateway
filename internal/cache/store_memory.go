package cache

import (
	"context"
	"encoding/gob"
	"os"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory vector store with optional file persistence.
// Uses brute-force cosine similarity search — suitable for <50k entries.
type MemoryStore struct {
	mu       sync.RWMutex
	entries  []Entry
	filePath string
}

// NewMemoryStore creates a new in-memory store. If filePath is non-empty,
// entries are loaded from and persisted to that file.
func NewMemoryStore(filePath string) (*MemoryStore, error) {
	s := &MemoryStore{filePath: filePath}
	if filePath != "" {
		if err := s.load(); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return s, nil
}

func (s *MemoryStore) Search(_ context.Context, teamID, model string, queryEmbedding []float32, topK int) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type scored struct {
		entry Entry
		score float64
	}

	var results []scored
	for _, e := range s.entries {
		if e.TeamID != teamID || e.Model != model {
			continue
		}
		sim := CosineSimilarity(queryEmbedding, e.QueryEmbedding)
		results = append(results, scored{entry: e, score: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > topK {
		results = results[:topK]
	}

	out := make([]Entry, len(results))
	for i, r := range results {
		out[i] = r.entry
	}
	return out, nil
}

func (s *MemoryStore) Insert(_ context.Context, entry Entry) error {
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	s.mu.Unlock()
	return s.persist()
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	for i, e := range s.entries {
		if e.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	return s.persist()
}

func (s *MemoryStore) Evict(_ context.Context, maxAge time.Duration, maxEntries int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var kept []Entry
	for _, e := range s.entries {
		if e.CreatedAt.After(cutoff) {
			kept = append(kept, e)
		}
	}

	// If still over max, remove oldest
	if len(kept) > maxEntries {
		sort.Slice(kept, func(i, j int) bool {
			return kept[i].CreatedAt.After(kept[j].CreatedAt)
		})
		kept = kept[:maxEntries]
	}

	s.entries = kept
	return s.persistLocked()
}

func (s *MemoryStore) Flush(_ context.Context) error {
	s.mu.Lock()
	s.entries = nil
	s.mu.Unlock()
	return s.persist()
}

func (s *MemoryStore) Count(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries), nil
}

func (s *MemoryStore) Close() error {
	return s.persist()
}

func (s *MemoryStore) persist() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.persistLocked()
}

func (s *MemoryStore) persistLocked() error {
	if s.filePath == "" {
		return nil
	}
	f, err := os.Create(s.filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewEncoder(f).Encode(s.entries)
}

func (s *MemoryStore) load() error {
	f, err := os.Open(s.filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewDecoder(f).Decode(&s.entries)
}

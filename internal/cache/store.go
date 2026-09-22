package cache

import (
	"context"
	"time"
)

// VectorStore is the interface for cache storage backends.
// Implementations: memory (file-backed), Redis, Qdrant, pgvector.
type VectorStore interface {
	// Search finds the top-K most similar entries for the given team+model.
	Search(ctx context.Context, teamID, model string, queryEmbedding []float32, topK int) ([]Entry, error)

	// Insert adds a new entry to the store.
	Insert(ctx context.Context, entry Entry) error

	// Delete removes a specific entry by ID.
	Delete(ctx context.Context, id string) error

	// Evict removes entries older than maxAge and trims to maxEntries.
	Evict(ctx context.Context, maxAge time.Duration, maxEntries int) error

	// Flush removes all entries from the store.
	Flush(ctx context.Context) error

	// Count returns the total number of entries.
	Count(ctx context.Context) (int, error)

	// Close releases resources.
	Close() error
}

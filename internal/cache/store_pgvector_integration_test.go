package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPgvectorStoreIntegration runs against a real PostgreSQL instance with
// the vector extension. It is opt-in so the default unit suite remains
// hermetic:
//
//	UBIQUUM_TEST_PGVECTOR_DSN=postgres://... go test ./internal/cache \
//	  -run TestPgvectorStoreIntegration
func TestPgvectorStoreIntegration(t *testing.T) {
	dsn := os.Getenv("UBIQUUM_TEST_PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("set UBIQUUM_TEST_PGVECTOR_DSN to run the pgvector integration test")
	}

	ctx := context.Background()
	store, err := NewPgvectorStore(dsn, pgvectorDimension)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = store.Close()
	})
	require.NoError(t, store.Flush(ctx))

	vector := func(x, y float32) []float32 {
		result := make([]float32, pgvectorDimension)
		result[0] = x
		result[1] = y
		return result
	}
	now := time.Now().UTC().Truncate(time.Second)
	entries := []Entry{
		{
			ID:             "best",
			TeamID:         "team-a",
			Model:          "model-a",
			QueryEmbedding: vector(1, 0),
			Messages:       "messages",
			Response:       "best response",
			TotalTokens:    100,
			Meta:           "intent:best",
			CreatedAt:      now.Add(-time.Minute),
		},
		{
			ID:             "second",
			TeamID:         "team-a",
			Model:          "model-a",
			QueryEmbedding: vector(0.8, 0.2),
			CtxEmbedding:   vector(0, 1),
			Messages:       "messages",
			Response:       "second response",
			TotalTokens:    80,
			Meta:           "intent:second",
			CreatedAt:      now.Add(-2 * time.Minute),
		},
		{
			ID:             "other-team",
			TeamID:         "team-b",
			Model:          "model-a",
			QueryEmbedding: vector(1, 0),
			CreatedAt:      now,
		},
		{
			ID:             "other-model",
			TeamID:         "team-a",
			Model:          "model-b",
			QueryEmbedding: vector(1, 0),
			CreatedAt:      now,
		},
	}
	for _, entry := range entries {
		require.NoError(t, store.Insert(ctx, entry))
	}

	count, err := store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, count)

	results, err := store.Search(ctx, "team-a", "model-a", vector(1, 0), 5)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "best", results[0].ID)
	assert.Equal(t, "intent:best", results[0].Meta)
	assert.Nil(t, results[0].CtxEmbedding)
	assert.Equal(t, "second", results[1].ID)
	assert.Equal(t, vector(0, 1), results[1].CtxEmbedding)

	require.NoError(t, store.Delete(ctx, "other-team"))
	require.NoError(t, store.Evict(ctx, 24*time.Hour, 2))
	count, err = store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	require.NoError(t, store.Flush(ctx))
	count, err = store.Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

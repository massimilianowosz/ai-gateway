package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRedisStore(t *testing.T, prefix string) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	store, err := NewRedisStore("redis://"+server.Addr(), prefix)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store, server
}

func redisTestEntry(id, teamID, model string, embedding []float32, createdAt time.Time) Entry {
	return Entry{
		ID:             id,
		TeamID:         teamID,
		Model:          model,
		QueryEmbedding: embedding,
		CtxEmbedding:   []float32{0, 1},
		Messages:       `[{"role":"user","content":"hello"}]`,
		Response:       `{"answer":"cached"}`,
		TotalTokens:    42,
		Meta:           "intent:test",
		CreatedAt:      createdAt,
	}
}

func TestNewRedisStoreValidationAndDefaults(t *testing.T) {
	_, err := NewRedisStore("://invalid", "")
	require.ErrorContains(t, err, "parse URL")

	_, err = NewRedisStore("redis://127.0.0.1:1", "")
	require.ErrorContains(t, err, "ping")

	store, _ := newTestRedisStore(t, "")
	assert.Equal(t, "hivecache:", store.prefix)
	assert.Equal(t, "hivecache:idx:team:model", store.indexKey("team", "model"))
	assert.Equal(t, "hivecache:entry:id", store.entryKey("id"))
}

func TestRedisStoreLifecycleSearchAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store, server := newTestRedisStore(t, "test-cache:")
	now := time.Now().UTC().Truncate(time.Second)

	entries := []Entry{
		redisTestEntry("best", "team-a", "model-a", []float32{1, 0}, now),
		redisTestEntry("second", "team-a", "model-a", []float32{0.8, 0.2}, now.Add(-time.Minute)),
		redisTestEntry("other-team", "team-b", "model-a", []float32{1, 0}, now),
		redisTestEntry("other-model", "team-a", "model-b", []float32{1, 0}, now),
	}
	for _, entry := range entries {
		require.NoError(t, store.Insert(ctx, entry))
	}

	count, err := store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, count)

	results, err := store.Search(ctx, "team-a", "model-a", []float32{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, entries[0], results[0])
	assert.Equal(t, "intent:test", results[0].Meta)

	otherTeam, err := store.Search(ctx, "team-b", "model-a", []float32{1, 0}, 5)
	require.NoError(t, err)
	require.Len(t, otherTeam, 1)
	assert.Equal(t, "other-team", otherTeam[0].ID)

	// Corrupt or missing entry documents must not break otherwise valid search
	// results referenced by the same sorted-set index.
	require.NoError(t, store.client.ZAdd(ctx, store.indexKey("team-a", "model-a"),
		redis.Z{Score: float64(now.Unix()), Member: "missing"},
		redis.Z{Score: float64(now.Unix()), Member: "corrupt"},
	).Err())
	require.NoError(t, store.client.Set(ctx, store.entryKey("corrupt"), "{", 0).Err())
	results, err = store.Search(ctx, "team-a", "model-a", []float32{1, 0}, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"best", "second"}, []string{results[0].ID, results[1].ID})

	require.NoError(t, store.Delete(ctx, "best"))
	assert.False(t, server.Exists(store.entryKey("best")))
	score, err := store.client.ZScore(ctx, store.indexKey("team-a", "model-a"), "best").Result()
	assert.ErrorIs(t, err, redis.Nil)
	assert.Zero(t, score)

	require.ErrorIs(t, store.Delete(ctx, "does-not-exist"), redis.Nil)
	require.NoError(t, store.client.Set(ctx, store.entryKey("invalid-json"), "{", 0).Err())
	require.Error(t, store.Delete(ctx, "invalid-json"))
}

func TestRedisStoreEvictHonorsTTLAndGlobalEntryCap(t *testing.T) {
	ctx := context.Background()
	store, server := newTestRedisStore(t, "evict:")
	now := time.Now().UTC().Truncate(time.Second)

	entries := []Entry{
		redisTestEntry("expired", "team-a", "model-a", []float32{1, 0}, now.Add(-3*time.Hour)),
		redisTestEntry("oldest-kept", "team-a", "model-a", []float32{1, 0}, now.Add(-90*time.Minute)),
		redisTestEntry("middle", "team-b", "model-a", []float32{1, 0}, now.Add(-30*time.Minute)),
		redisTestEntry("newest", "team-a", "model-b", []float32{1, 0}, now.Add(-time.Minute)),
	}
	for _, entry := range entries {
		require.NoError(t, store.Insert(ctx, entry))
	}

	require.NoError(t, store.Evict(ctx, 2*time.Hour, 2))

	count, err := store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.False(t, server.Exists(store.entryKey("expired")))
	assert.False(t, server.Exists(store.entryKey("oldest-kept")))
	assert.True(t, server.Exists(store.entryKey("middle")))
	assert.True(t, server.Exists(store.entryKey("newest")))
}

func TestRedisStoreFlushIsPrefixScopedAndPropagatesClientErrors(t *testing.T) {
	ctx := context.Background()
	store, server := newTestRedisStore(t, "scoped:")
	require.NoError(t, store.Insert(ctx,
		redisTestEntry("entry", "team", "model", []float32{1, 0}, time.Now())))
	server.Set("unrelated:key", "keep")

	require.NoError(t, store.Flush(ctx))
	count, err := store.Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.True(t, server.Exists("unrelated:key"))

	require.NoError(t, store.Close())
	_, err = store.Search(ctx, "team", "model", []float32{1, 0}, 5)
	require.Error(t, err)
	require.Error(t, store.Insert(ctx,
		redisTestEntry("closed", "team", "model", []float32{1, 0}, time.Now())))
	require.Error(t, store.Evict(ctx, time.Hour, 1))
	_, err = store.Count(ctx)
	require.Error(t, err)
	require.Error(t, store.Flush(ctx))
}

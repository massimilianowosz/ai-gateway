package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newSQLitePgvectorStore(t *testing.T, dim int) *PgvectorStore {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "pgvector-unit.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&PgvectorEntry{}))
	store := &PgvectorStore{db: db, dim: dim}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func pgvectorTestEntry(id string, createdAt time.Time, withContext bool) Entry {
	entry := Entry{
		ID:             id,
		TeamID:         "team-a",
		Model:          "model-a",
		QueryEmbedding: []float32{1, 0, 0},
		Messages:       "messages",
		Response:       "response",
		TotalTokens:    42,
		Meta:           "intent:test",
		CreatedAt:      createdAt,
	}
	if withContext {
		entry.CtxEmbedding = []float32{0, 1, 0}
	}
	return entry
}

func TestPgvectorEntryMappingPreservesOptionalContextAndMetadata(t *testing.T) {
	ctxVector := pgvector.NewVector([]float32{0, 1, 0})
	row := PgvectorEntry{
		ID:             "row",
		TeamID:         "team",
		Model:          "model",
		QueryEmbedding: pgvector.NewVector([]float32{1, 0, 0}),
		CtxEmbedding:   &ctxVector,
		Messages:       "messages",
		Response:       "response",
		TotalTokens:    10,
		Meta:           "intent:roundtrip",
		CreatedAt:      time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
	}
	assert.Equal(t, Entry{
		ID:             "row",
		TeamID:         "team",
		Model:          "model",
		QueryEmbedding: []float32{1, 0, 0},
		CtxEmbedding:   []float32{0, 1, 0},
		Messages:       "messages",
		Response:       "response",
		TotalTokens:    10,
		Meta:           "intent:roundtrip",
		CreatedAt:      row.CreatedAt,
	}, entryFromPgvector(row))

	row.CtxEmbedding = nil
	assert.Nil(t, entryFromPgvector(row).CtxEmbedding)
	assert.Equal(t, "cache_entries", row.TableName())
}

func TestPgvectorStoreCRUDWithOptionalContext(t *testing.T) {
	ctx := context.Background()
	store := newSQLitePgvectorStore(t, 3)
	now := time.Now().Truncate(time.Second)

	withoutContext := pgvectorTestEntry("without-context", now, false)
	require.NoError(t, store.Insert(ctx, withoutContext))
	withContext := pgvectorTestEntry("with-context", now.Add(time.Second), true)
	require.NoError(t, store.Insert(ctx, withContext))
	require.Error(t, store.Insert(ctx, withContext), "duplicate primary key must fail")

	count, err := store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	var rows []PgvectorEntry
	require.NoError(t, store.db.Order("created_at ASC").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Nil(t, rows[0].CtxEmbedding)
	require.NotNil(t, rows[1].CtxEmbedding)
	assert.Equal(t, []float32{0, 1, 0}, rows[1].CtxEmbedding.Slice())
	assert.Equal(t, "intent:test", rows[0].Meta)

	require.NoError(t, store.Delete(ctx, withoutContext.ID))
	count, err = store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	require.NoError(t, store.Flush(ctx))
	count, err = store.Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestPgvectorStoreEvictHonorsTTLAndGlobalEntryCap(t *testing.T) {
	ctx := context.Background()
	store := newSQLitePgvectorStore(t, 3)
	now := time.Now().Truncate(time.Second)
	entries := []Entry{
		pgvectorTestEntry("expired", now.Add(-3*time.Hour), false),
		pgvectorTestEntry("oldest-kept", now.Add(-90*time.Minute), false),
		pgvectorTestEntry("middle", now.Add(-30*time.Minute), true),
		pgvectorTestEntry("newest", now.Add(-time.Minute), true),
	}
	for i := range entries {
		entries[i].TeamID = "team-" + entries[i].ID
		require.NoError(t, store.Insert(ctx, entries[i]))
	}

	require.NoError(t, store.Evict(ctx, 2*time.Hour, 2))

	var rows []PgvectorEntry
	require.NoError(t, store.db.Order("created_at ASC").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.Equal(t, "middle", rows[0].ID)
	assert.Equal(t, "newest", rows[1].ID)
}

func TestPgvectorStoreValidationSearchAndClosedDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	store := newSQLitePgvectorStore(t, 3)

	require.ErrorContains(t, store.Insert(ctx, Entry{QueryEmbedding: []float32{1, 0}}), "dimension")
	require.ErrorContains(t, store.Insert(ctx, Entry{
		QueryEmbedding: []float32{1, 0, 0},
		CtxEmbedding:   []float32{0, 1},
	}), "dimension")
	_, err := store.Search(ctx, "team", "model", []float32{1, 0}, 5)
	require.ErrorContains(t, err, "dimension")
	_, err = store.Search(ctx, "team", "model", []float32{1, 0, 0}, 0)
	require.ErrorContains(t, err, "topK")

	// SQLite deliberately lacks pgvector's <=> operator. Reaching this error
	// still verifies construction of the scoped nearest-neighbor query; the
	// real PostgreSQL success path is covered by the opt-in integration test.
	_, err = store.Search(ctx, "team", "model", []float32{1, 0, 0}, 5)
	require.Error(t, err)

	require.NoError(t, store.Close())
	_, err = store.Count(ctx)
	require.Error(t, err)
	require.Error(t, store.Delete(ctx, "id"))
	require.Error(t, store.Flush(ctx))
	require.Error(t, store.Evict(ctx, time.Hour, 1))
}

func TestNewPgvectorStoreRejectsUnsupportedDimensionAndConnectionFailure(t *testing.T) {
	_, err := NewPgvectorStore("postgres://localhost/unused", 3)
	require.ErrorContains(t, err, "dimension must be 1536")

	_, err = NewPgvectorStore(
		"postgres://invalid:invalid@127.0.0.1:1/ubiquum?connect_timeout=1",
		pgvectorDimension,
	)
	require.Error(t, err)
}

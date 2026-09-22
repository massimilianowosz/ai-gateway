package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const pgvectorDimension = 1536

// PgvectorEntry is the GORM model for cache entries stored with pgvector.
type PgvectorEntry struct {
	ID             string           `gorm:"primaryKey"`
	TeamID         string           `gorm:"index"`
	Model          string           `gorm:"index"`
	QueryEmbedding pgvector.Vector  `gorm:"type:vector(1536)"`
	CtxEmbedding   *pgvector.Vector `gorm:"type:vector(1536)"`
	Messages       string           `gorm:"type:text"`
	Response       string           `gorm:"type:text"`
	TotalTokens    int
	Meta           string
	CreatedAt      time.Time `gorm:"index"`
}

func (PgvectorEntry) TableName() string { return "cache_entries" }

// PgvectorStore uses PostgreSQL with the pgvector extension for similarity search.
type PgvectorStore struct {
	db  *gorm.DB
	dim int
}

// NewPgvectorStore creates a pgvector-backed cache store. Opens its own DB connection
// and auto-creates the cache_entries table + HNSW index on first run.
func NewPgvectorStore(dsn string, dim int) (*PgvectorStore, error) {
	if dim != pgvectorDimension {
		return nil, fmt.Errorf("pgvector: vector dimension must be %d, got %d", pgvectorDimension, dim)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}

	// Enable pgvector extension
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		closeGormDB(db)
		return nil, fmt.Errorf("pgvector: enable extension: %w", err)
	}

	s := &PgvectorStore{db: db, dim: dim}

	// Auto-migrate the table
	if err := db.AutoMigrate(&PgvectorEntry{}); err != nil {
		closeGormDB(db)
		return nil, err
	}

	// Create HNSW index for cosine similarity
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_cache_entries_embedding ON cache_entries USING hnsw (query_embedding vector_cosine_ops)").Error; err != nil {
		closeGormDB(db)
		return nil, fmt.Errorf("pgvector: create HNSW index: %w", err)
	}

	return s, nil
}

func (s *PgvectorStore) Search(ctx context.Context, teamID, model string, queryEmbedding []float32, topK int) ([]Entry, error) {
	if err := s.validateVector(queryEmbedding, false); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return nil, fmt.Errorf("pgvector: topK must be positive")
	}

	var rows []PgvectorEntry

	err := s.db.WithContext(ctx).
		Where("team_id = ? AND model = ?", teamID, model).
		Clauses(clause.OrderBy{
			Expression: clause.Expr{
				SQL:  "query_embedding <=> ?",
				Vars: []interface{}{pgvector.NewVector(queryEmbedding)},
			},
		}).
		Limit(topK).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(rows))
	for i, r := range rows {
		entries[i] = entryFromPgvector(r)
	}
	return entries, nil
}

func (s *PgvectorStore) Insert(ctx context.Context, entry Entry) error {
	if err := s.validateVector(entry.QueryEmbedding, false); err != nil {
		return err
	}
	if err := s.validateVector(entry.CtxEmbedding, true); err != nil {
		return err
	}

	var ctxEmbedding *pgvector.Vector
	if len(entry.CtxEmbedding) > 0 {
		vector := pgvector.NewVector(entry.CtxEmbedding)
		ctxEmbedding = &vector
	}
	row := PgvectorEntry{
		ID:             entry.ID,
		TeamID:         entry.TeamID,
		Model:          entry.Model,
		QueryEmbedding: pgvector.NewVector(entry.QueryEmbedding),
		CtxEmbedding:   ctxEmbedding,
		Messages:       entry.Messages,
		Response:       entry.Response,
		TotalTokens:    entry.TotalTokens,
		Meta:           entry.Meta,
		CreatedAt:      entry.CreatedAt,
	}
	return s.db.WithContext(ctx).Create(&row).Error
}

func (s *PgvectorStore) Delete(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Delete(&PgvectorEntry{}, "id = ?", id).Error
}

func (s *PgvectorStore) Evict(ctx context.Context, maxAge time.Duration, maxEntries int) error {
	cutoff := time.Now().Add(-maxAge)
	if err := s.db.WithContext(ctx).Delete(&PgvectorEntry{}, "created_at < ?", cutoff).Error; err != nil {
		return err
	}

	// Trim to max entries if needed
	var count int64
	if err := s.db.WithContext(ctx).Model(&PgvectorEntry{}).Count(&count).Error; err != nil {
		return err
	}
	if maxEntries >= 0 && int(count) > maxEntries {
		excess := int(count) - maxEntries
		if err := s.db.WithContext(ctx).
			Delete(&PgvectorEntry{}, "id IN (?)",
				s.db.Model(&PgvectorEntry{}).Select("id").Order("created_at ASC").Limit(excess)).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *PgvectorStore) Count(ctx context.Context) (int, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&PgvectorEntry{}).Count(&count).Error
	return int(count), err
}

func (s *PgvectorStore) Flush(ctx context.Context) error {
	return s.db.WithContext(ctx).Where("1=1").Delete(&PgvectorEntry{}).Error
}

func (s *PgvectorStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *PgvectorStore) validateVector(vector []float32, optional bool) error {
	if optional && len(vector) == 0 {
		return nil
	}
	if len(vector) != s.dim {
		return fmt.Errorf("pgvector: vector dimension %d does not match configured dimension %d", len(vector), s.dim)
	}
	return nil
}

func entryFromPgvector(row PgvectorEntry) Entry {
	var ctxEmbedding []float32
	if row.CtxEmbedding != nil {
		ctxEmbedding = row.CtxEmbedding.Slice()
	}
	return Entry{
		ID:             row.ID,
		TeamID:         row.TeamID,
		Model:          row.Model,
		QueryEmbedding: row.QueryEmbedding.Slice(),
		CtxEmbedding:   ctxEmbedding,
		Messages:       row.Messages,
		Response:       row.Response,
		TotalTokens:    row.TotalTokens,
		Meta:           row.Meta,
		CreatedAt:      row.CreatedAt,
	}
}

func closeGormDB(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}

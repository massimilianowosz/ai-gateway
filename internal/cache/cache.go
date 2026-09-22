// Package cache implements a semantic cache for LLM chat completions.
// It intercepts requests, computes embeddings, and returns cached responses
// when similarity exceeds configured thresholds.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

// Band represents the cache routing decision.
type Band string

const (
	BandDirect Band = "DIRECT" // score >= direct_threshold: return as-is
	BandReuse  Band = "REUSE"  // score >= reuse_threshold: return with light validation
	BandTweak  Band = "TWEAK"  // score >= tweak_threshold: adapt via cheap LLM
	BandMiss   Band = "MISS"   // below all thresholds: forward to upstream
)

// Entry represents a cached response.
type Entry struct {
	ID             string    `json:"id"`
	TeamID         string    `json:"team_id"`
	Model          string    `json:"model"`
	QueryEmbedding []float32 `json:"query_embedding"`
	CtxEmbedding   []float32 `json:"ctx_embedding"`
	Messages       string    `json:"messages"`     // JSON of original messages
	Response       string    `json:"response"`     // JSON of full completion response
	TotalTokens    int       `json:"total_tokens"` // tokens in the cached response
	Meta           string    `json:"meta"`         // opaque client metadata (e.g. intent label for benchmarking)
	CreatedAt      time.Time `json:"created_at"`
}

// ThresholdOverrides lets a per-key response_cache policy replace one or
// more of the engine's classification thresholds for that key alone. A nil
// field means "use the engine's own config value" for that threshold.
type ThresholdOverrides struct {
	DirectThreshold *float64
	ReuseThreshold  *float64
	TweakThreshold  *float64
}

// LookupResult is returned from a cache lookup.
type LookupResult struct {
	Hit         bool
	Band        Band
	Score       float64
	Entry       *Entry
	TokensSaved int
	EmbedTokens int // total embedding tokens used for this lookup
}

// Cache is the main semantic cache engine.
type Cache struct {
	cfg     config.CacheConfig
	store   VectorStore
	embedFn EmbedFunc
	tweakFn TweakFunc
	logger  *slog.Logger
}

// EmbedResult holds an embedding vector and its token usage.
type EmbedResult struct {
	Vector []float32
	Tokens int // prompt tokens consumed by the embedding call
}

// EmbedFunc generates an embedding vector for the given text.
type EmbedFunc func(ctx context.Context, text string) (EmbedResult, error)

// New creates a new Cache instance.
func New(cfg config.CacheConfig, store VectorStore, embedFn EmbedFunc, logger *slog.Logger, tweakFn ...TweakFunc) *Cache {
	cfg.ApplyDefaults()
	c := &Cache{
		cfg:     cfg,
		store:   store,
		embedFn: embedFn,
		logger:  logger,
	}
	if len(tweakFn) > 0 && tweakFn[0] != nil {
		c.tweakFn = tweakFn[0]
	}
	return c
}

// Lookup searches the cache for a similar request. overrides is optional
// (variadic so existing callers are unaffected): pass a ThresholdOverrides
// to replace one or more of the engine's configured thresholds for this
// lookup only.
func (c *Cache) Lookup(ctx context.Context, teamID, model string, messages []Message, overrides ...ThresholdOverrides) (*LookupResult, error) {
	var ov ThresholdOverrides
	if len(overrides) > 0 {
		ov = overrides[0]
	}
	queryText := ExtractQuery(messages)
	ctxText := ExtractContext(messages)

	queryRes, err := c.embedFn(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	embedTokens := queryRes.Tokens

	var ctxEmb []float32
	if ctxText != "" {
		ctxRes, err := c.embedFn(ctx, ctxText)
		if err != nil {
			return nil, fmt.Errorf("embed context: %w", err)
		}
		ctxEmb = ctxRes.Vector
		embedTokens += ctxRes.Tokens
	}

	candidates, err := c.store.Search(ctx, teamID, model, queryRes.Vector, 5)
	if err != nil {
		return nil, fmt.Errorf("store search: %w", err)
	}

	var best *Entry
	var bestScore float64

	for _, cand := range candidates {
		score := DualScore(queryRes.Vector, ctxEmb, cand.QueryEmbedding, cand.CtxEmbedding, c.cfg.AlphaWeight, c.cfg.BetaWeight)
		if score > bestScore {
			bestScore = score
			entry := cand // copy
			best = &entry
		}
	}

	if best == nil {
		return &LookupResult{Hit: false, Band: BandMiss, Score: 0, EmbedTokens: embedTokens}, nil
	}

	band := c.classifyBand(bestScore, ov)
	if band == BandMiss {
		return &LookupResult{Hit: false, Band: BandMiss, Score: bestScore, EmbedTokens: embedTokens}, nil
	}

	return &LookupResult{
		Hit:         true,
		Band:        band,
		Score:       bestScore,
		Entry:       best,
		TokensSaved: best.TotalTokens,
		EmbedTokens: embedTokens,
	}, nil
}

// Store saves a response in the cache.
func (c *Cache) Store(ctx context.Context, teamID, model string, messages []Message, response string, totalTokens int, meta string) error {
	queryText := ExtractQuery(messages)
	ctxText := ExtractContext(messages)

	queryRes, err := c.embedFn(ctx, queryText)
	if err != nil {
		return fmt.Errorf("embed query: %w", err)
	}

	var ctxEmb []float32
	if ctxText != "" {
		ctxRes, err := c.embedFn(ctx, ctxText)
		if err != nil {
			return fmt.Errorf("embed context: %w", err)
		}
		ctxEmb = ctxRes.Vector
	}

	entry := Entry{
		ID:             generateID(),
		TeamID:         teamID,
		Model:          model,
		QueryEmbedding: queryRes.Vector,
		CtxEmbedding:   ctxEmb,
		Messages:       marshalMessages(messages),
		Response:       response,
		TotalTokens:    totalTokens,
		Meta:           meta,
		CreatedAt:      time.Now(),
	}

	return c.store.Insert(ctx, entry)
}

// Evict removes expired entries.
func (c *Cache) Evict(ctx context.Context) error {
	return c.store.Evict(ctx, c.cfg.TTL, c.cfg.MaxEntries)
}

// Flush removes all entries from the cache.
func (c *Cache) Flush(ctx context.Context) error {
	return c.store.Flush(ctx)
}

// Count returns the number of entries in the cache.
func (c *Cache) Count(ctx context.Context) (int, error) {
	return c.store.Count(ctx)
}

// Tweak adapts a cached response to a new query using the configured tweak model.
func (c *Cache) Tweak(ctx context.Context, newQuery string, entry *Entry) (string, error) {
	if c.tweakFn == nil {
		return "", fmt.Errorf("tweak function not configured")
	}
	return c.tweakFn(ctx, newQuery, entry.Messages, entry.Response)
}

// HasTweak returns true if the cache has a configured tweak function.
func (c *Cache) HasTweak() bool {
	return c.tweakFn != nil
}

// TweakModel returns the configured tweak model name.
func (c *Cache) TweakModel() string {
	return c.cfg.TweakModel
}

// EmbeddingModel returns the configured embedding model name.
func (c *Cache) EmbeddingModel() string {
	return c.cfg.EmbeddingModel
}

func (c *Cache) classifyBand(score float64, overrides ThresholdOverrides) Band {
	direct, reuse, tweak := c.cfg.DirectThreshold, c.cfg.ReuseThreshold, c.cfg.TweakThreshold
	if overrides.DirectThreshold != nil {
		direct = *overrides.DirectThreshold
	}
	if overrides.ReuseThreshold != nil {
		reuse = *overrides.ReuseThreshold
	}
	if overrides.TweakThreshold != nil {
		tweak = *overrides.TweakThreshold
	}
	switch {
	case score >= direct:
		return BandDirect
	case score >= reuse:
		return BandReuse
	case score >= tweak && c.cfg.TweakModel != "":
		return BandTweak
	default:
		return BandMiss
	}
}

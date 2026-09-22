package cache

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1.0},
		{"similar", []float32{1, 1, 0}, []float32{1, 0, 0}, 1.0 / math.Sqrt(2)},
		{"empty", []float32{}, []float32{}, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("CosineSimilarity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDualScore(t *testing.T) {
	q := []float32{1, 0, 0}
	c := []float32{0, 1, 0}

	// Same query and context → perfect score
	score := DualScore(q, c, q, c, 0.7, 0.3)
	if math.Abs(score-1.0) > 1e-6 {
		t.Errorf("DualScore same vectors = %v, want 1.0", score)
	}

	// Orthogonal query, same context
	score = DualScore(q, c, []float32{0, 1, 0}, c, 0.7, 0.3)
	want := 0.7*0.0 + 0.3*1.0
	if math.Abs(score-want) > 1e-6 {
		t.Errorf("DualScore orthogonal query = %v, want %v", score, want)
	}
}

func TestExtractQuery(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a helper."},
		{Role: "user", Content: "Hello world"},
		{Role: "assistant", Content: "Hi!"},
		{Role: "user", Content: "What is 2+2?"},
	}
	got := ExtractQuery(msgs)
	if got != "What is 2+2?" {
		t.Errorf("ExtractQuery = %q, want %q", got, "What is 2+2?")
	}
}

func TestExtractContext(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a helper."},
		{Role: "user", Content: "Hello world"},
		{Role: "assistant", Content: "Hi!"},
		{Role: "user", Content: "What is 2+2?"},
	}
	got := ExtractContext(msgs)
	if got == "" {
		t.Error("ExtractContext should return non-empty context")
	}
	// Should not contain the last user message
	if got == "What is 2+2?" {
		t.Error("ExtractContext should not be last user message")
	}
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	store, err := NewMemoryStore("")
	if err != nil {
		t.Fatal(err)
	}

	// Insert entries
	e1 := Entry{
		ID:             "entry-1",
		TeamID:         "team-a",
		Model:          "gpt-4o",
		QueryEmbedding: []float32{1, 0, 0},
		CtxEmbedding:   []float32{0, 1, 0},
		Messages:       `[{"role":"user","content":"hello"}]`,
		Response:       `{"choices":[]}`,
		TotalTokens:    100,
		CreatedAt:      time.Now(),
	}
	e2 := Entry{
		ID:             "entry-2",
		TeamID:         "team-a",
		Model:          "gpt-4o",
		QueryEmbedding: []float32{0.9, 0.1, 0},
		CtxEmbedding:   []float32{0, 0.9, 0.1},
		Messages:       `[{"role":"user","content":"hi"}]`,
		Response:       `{"choices":[]}`,
		TotalTokens:    80,
		CreatedAt:      time.Now(),
	}

	if err := store.Insert(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, e2); err != nil {
		t.Fatal(err)
	}

	// Count
	count, err := store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("Count = %d, want 2", count)
	}

	// Search — query close to e1
	results, err := store.Search(ctx, "team-a", "gpt-4o", []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results")
	}
	if results[0].ID != "entry-1" {
		t.Errorf("best result = %s, want entry-1", results[0].ID)
	}

	// Search — different team should return no results
	results, err = store.Search(ctx, "team-b", "gpt-4o", []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results for team-b, got %d", len(results))
	}

	// Delete
	if err := store.Delete(ctx, "entry-1"); err != nil {
		t.Fatal(err)
	}
	count, _ = store.Count(ctx)
	if count != 1 {
		t.Errorf("Count after delete = %d, want 1", count)
	}
}

func TestCache_Lookup_Miss(t *testing.T) {
	ctx := context.Background()
	store, _ := NewMemoryStore("")

	mockEmbed := func(_ context.Context, text string) (EmbedResult, error) {
		return EmbedResult{Vector: []float32{0.5, 0.5, 0}}, nil
	}

	cfg := defaultCfg()
	c := New(cfg, store, mockEmbed, nil)

	// Empty store → MISS
	result, err := c.Lookup(ctx, "team", "gpt-4o", []Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Hit {
		t.Error("expected miss")
	}
	if result.Band != BandMiss {
		t.Errorf("band = %s, want MISS", result.Band)
	}
}

func TestCache_StoreAndLookup_Hit(t *testing.T) {
	ctx := context.Background()
	store, _ := NewMemoryStore("")

	// Simple embed: returns the text length normalized
	mockEmbed := func(_ context.Context, text string) (EmbedResult, error) {
		// Always return the same embedding for same input
		return EmbedResult{Vector: []float32{1, 0, 0}, Tokens: 5}, nil
	}

	cfg := defaultCfg()
	cfg.DirectThreshold = 0.9
	c := New(cfg, store, mockEmbed, nil)

	msgs := []Message{{Role: "user", Content: "What is 2+2?"}}

	// Store
	err := c.Store(ctx, "team", "gpt-4o", msgs, `{"choices":[{"message":{"content":"4"}}]}`, 50, "")
	if err != nil {
		t.Fatal(err)
	}

	// Lookup — same embedding → score = 1.0 → DIRECT
	result, err := c.Lookup(ctx, "team", "gpt-4o", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Hit {
		t.Fatal("expected hit")
	}
	if result.Band != BandDirect {
		t.Errorf("band = %s, want DIRECT", result.Band)
	}
	if result.TokensSaved != 50 {
		t.Errorf("tokens_saved = %d, want 50", result.TokensSaved)
	}
}

func defaultCfg() config.CacheConfig {
	cfg := config.CacheConfig{
		Enabled: true,
		Backend: "memory",
	}
	cfg.ApplyDefaults()
	return cfg
}

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	dbstore "github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func TestCacheBandClassificationAndTweakAccessors(t *testing.T) {
	cfg := config.CacheConfig{
		EmbeddingModel:  "embed-model",
		TweakModel:      "tweak-model",
		DirectThreshold: 0.95,
		ReuseThreshold:  0.90,
		TweakThreshold:  0.85,
	}
	tweak := func(_ context.Context, query, _, _ string) (string, error) {
		return "adapted:" + query, nil
	}
	memory, err := NewMemoryStore("")
	require.NoError(t, err)
	cache := New(cfg, memory, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), tweak)

	assert.Equal(t, BandDirect, cache.classifyBand(0.95, ThresholdOverrides{}))
	assert.Equal(t, BandReuse, cache.classifyBand(0.91, ThresholdOverrides{}))
	assert.Equal(t, BandTweak, cache.classifyBand(0.86, ThresholdOverrides{}))
	assert.Equal(t, BandMiss, cache.classifyBand(0.84, ThresholdOverrides{}))
	assert.True(t, cache.HasTweak())
	assert.Equal(t, "tweak-model", cache.TweakModel())
	assert.Equal(t, "embed-model", cache.EmbeddingModel())

	got, err := cache.Tweak(context.Background(), "new question", &Entry{})
	require.NoError(t, err)
	assert.Equal(t, "adapted:new question", got)

	cache.tweakFn = nil
	assert.False(t, cache.HasTweak())
	_, err = cache.Tweak(context.Background(), "new question", &Entry{})
	require.ErrorContains(t, err, "not configured")

	cache.cfg.TweakModel = ""
	assert.Equal(t, BandMiss, cache.classifyBand(0.86, ThresholdOverrides{}))
}

func TestClassifyBand_PerKeyOverridesReplaceConfigThresholds(t *testing.T) {
	cfg := config.CacheConfig{DirectThreshold: 0.95, ReuseThreshold: 0.90, TweakThreshold: 0.85, TweakModel: "tweak-model"}
	memory, err := NewMemoryStore("")
	require.NoError(t, err)
	c := New(cfg, memory, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A score that misses every config threshold, but a policy has narrowed
	// direct_threshold below it: the override must decide, not the config.
	direct := 0.5
	assert.Equal(t, BandDirect, c.classifyBand(0.6, ThresholdOverrides{DirectThreshold: &direct}))

	// An unset field in the override falls back to the config value.
	reuse := 0.3
	assert.Equal(t, BandReuse, c.classifyBand(0.6, ThresholdOverrides{ReuseThreshold: &reuse}))
	assert.Equal(t, BandMiss, c.classifyBand(0.6, ThresholdOverrides{}), "unoverridden config thresholds still apply")
}

func TestCacheLifecycleUsesMemoryStorePersistenceAndEviction(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.gob")
	memory, err := NewMemoryStore(path)
	require.NoError(t, err)

	now := time.Now()
	for _, entry := range []Entry{
		{ID: "expired", TeamID: "team", Model: "model", QueryEmbedding: []float32{1}, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "older", TeamID: "team", Model: "model", QueryEmbedding: []float32{1}, CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "newer", TeamID: "team", Model: "model", QueryEmbedding: []float32{1}, CreatedAt: now.Add(-time.Minute)},
		{ID: "newest", TeamID: "team", Model: "model", QueryEmbedding: []float32{1}, CreatedAt: now},
	} {
		require.NoError(t, memory.Insert(ctx, entry))
	}

	cfg := config.CacheConfig{TTL: time.Hour, MaxEntries: 2}
	cache := New(cfg, memory, func(context.Context, string) (EmbedResult, error) {
		return EmbedResult{Vector: []float32{1}}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, cache.Evict(ctx))
	count, err := cache.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	require.NoError(t, memory.Close())

	reloaded, err := NewMemoryStore(path)
	require.NoError(t, err)
	defer reloaded.Close()
	results, err := reloaded.Search(ctx, "team", "model", []float32{1}, 10)
	require.NoError(t, err)
	require.Len(t, results, 2)
	ids := []string{results[0].ID, results[1].ID}
	assert.ElementsMatch(t, []string{"newer", "newest"}, ids)

	cache.store = reloaded
	require.NoError(t, cache.Flush(ctx))
	count, err = cache.Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

type cacheVectorStore struct {
	searchResults []Entry
	searchErr     error
	inserted      []Entry
	insertErr     error
}

func (s *cacheVectorStore) Search(context.Context, string, string, []float32, int) ([]Entry, error) {
	return s.searchResults, s.searchErr
}
func (s *cacheVectorStore) Insert(_ context.Context, entry Entry) error {
	s.inserted = append(s.inserted, entry)
	return s.insertErr
}
func (s *cacheVectorStore) Delete(context.Context, string) error            { return nil }
func (s *cacheVectorStore) Evict(context.Context, time.Duration, int) error { return nil }
func (s *cacheVectorStore) Flush(context.Context) error                     { return nil }
func (s *cacheVectorStore) Count(context.Context) (int, error)              { return len(s.inserted), nil }
func (s *cacheVectorStore) Close() error                                    { return nil }

func TestCacheLookupTracksEmbeddingTokensAndPropagatesFailures(t *testing.T) {
	store := &cacheVectorStore{searchResults: []Entry{{
		ID:             "best",
		QueryEmbedding: []float32{1, 0},
		CtxEmbedding:   []float32{0, 1},
		TotalTokens:    77,
	}}}
	embedCalls := 0
	embed := func(_ context.Context, text string) (EmbedResult, error) {
		embedCalls++
		if text == "explode" {
			return EmbedResult{}, errors.New("embedding unavailable")
		}
		if strings.Contains(text, "assistant:") {
			return EmbedResult{Vector: []float32{0, 1}, Tokens: 3}, nil
		}
		return EmbedResult{Vector: []float32{1, 0}, Tokens: 2}, nil
	}
	cfg := config.CacheConfig{DirectThreshold: 0.95, ReuseThreshold: 0.90, AlphaWeight: 0.7, BetaWeight: 0.3}
	cache := New(cfg, store, embed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	messages := []Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new question"},
	}

	result, err := cache.Lookup(context.Background(), "team", "model", messages)
	require.NoError(t, err)
	require.True(t, result.Hit)
	assert.Equal(t, BandDirect, result.Band)
	assert.Equal(t, 5, result.EmbedTokens)
	assert.Equal(t, 77, result.TokensSaved)
	assert.Equal(t, 2, embedCalls)

	cache.embedFn = func(context.Context, string) (EmbedResult, error) {
		return EmbedResult{}, errors.New("offline")
	}
	_, err = cache.Lookup(context.Background(), "team", "model", []Message{{Role: "user", Content: "question"}})
	require.ErrorContains(t, err, "embed query")

	cache.embedFn = embed
	store.searchErr = errors.New("vector backend down")
	_, err = cache.Lookup(context.Background(), "team", "model", []Message{{Role: "user", Content: "question"}})
	require.ErrorContains(t, err, "store search")
}

func TestCacheLookup_ThresholdOverrideChangesTheBand(t *testing.T) {
	// Same fixture as TestCacheLookupTracksEmbeddingTokensAndPropagatesFailures,
	// but the best score (~0.92 per DualScore with these vectors and default
	// weights) sits between the config's reuse and direct thresholds.
	store := &cacheVectorStore{searchResults: []Entry{{
		ID: "best", QueryEmbedding: []float32{1, 0}, CtxEmbedding: []float32{0, 1}, TotalTokens: 77,
	}}}
	embed := func(_ context.Context, text string) (EmbedResult, error) {
		if strings.Contains(text, "assistant:") {
			return EmbedResult{Vector: []float32{0, 1}, Tokens: 3}, nil
		}
		return EmbedResult{Vector: []float32{1, 0}, Tokens: 2}, nil
	}
	cfg := config.CacheConfig{DirectThreshold: 0.95, ReuseThreshold: 0.90, AlphaWeight: 0.7, BetaWeight: 0.3}
	cache := New(cfg, store, embed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	messages := []Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new question"},
	}

	withoutOverride, err := cache.Lookup(context.Background(), "team", "model", messages)
	require.NoError(t, err)
	assert.Equal(t, BandDirect, withoutOverride.Band, "score of 1.0 exceeds the config's own direct threshold")

	narrowedDirect := 1.5 // above any possible score: DIRECT becomes unreachable for this key
	withOverride, err := cache.Lookup(context.Background(), "team", "model", messages, ThresholdOverrides{
		DirectThreshold: &narrowedDirect,
	})
	require.NoError(t, err)
	assert.Equal(t, BandReuse, withOverride.Band, "a per-key override must actually change the classification")
}

func TestCacheStoreBuildsScopedEntryAndPropagatesInsertError(t *testing.T) {
	store := &cacheVectorStore{}
	cache := New(defaultCfg(), store, func(_ context.Context, text string) (EmbedResult, error) {
		if strings.Contains(text, "assistant:") {
			return EmbedResult{Vector: []float32{0, 1}}, nil
		}
		return EmbedResult{Vector: []float32{1, 0}}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	messages := []Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "new"},
	}

	require.NoError(t, cache.Store(context.Background(), "team-a", "model-a", messages, `{"ok":true}`, 42, "case-7"))
	require.Len(t, store.inserted, 1)
	entry := store.inserted[0]
	assert.Len(t, entry.ID, 32)
	assert.Equal(t, "team-a", entry.TeamID)
	assert.Equal(t, "model-a", entry.Model)
	assert.Equal(t, []float32{1, 0}, entry.QueryEmbedding)
	assert.Equal(t, []float32{0, 1}, entry.CtxEmbedding)
	assert.Equal(t, 42, entry.TotalTokens)
	assert.Equal(t, "case-7", entry.Meta)
	assert.JSONEq(t, `[{"role":"user","content":"old"},{"role":"assistant","content":"answer"},{"role":"user","content":"new"}]`, entry.Messages)

	store.insertErr = errors.New("write failed")
	require.ErrorContains(t, cache.Store(context.Background(), "team", "model", []Message{{Role: "user", Content: "q"}}, `{}`, 0, ""), "write failed")
}

type cacheFeatureProvider struct {
	mu                sync.Mutex
	embeddingResponse *provider.EmbeddingResponse
	completionContent string
}

func (p *cacheFeatureProvider) Complete(context.Context, *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &provider.CompletionResponse{
		Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: p.completionContent}}},
		Usage:   &provider.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
	}, nil
}
func (p *cacheFeatureProvider) Stream(context.Context, *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, errors.New("stream not expected")
}
func (p *cacheFeatureProvider) Name() string { return "cache-feature-test" }
func (p *cacheFeatureProvider) Embed(context.Context, *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return p.embeddingResponse, nil
}

type cacheProviderFactory struct {
	provider provider.Provider
}

func (f cacheProviderFactory) Create(config.ModelConfig) (provider.Provider, error) {
	return f.provider, nil
}

func TestEmbeddingAndTweakFunctionsUseRegisteredProviders(t *testing.T) {
	mock := &cacheFeatureProvider{
		embeddingResponse: &provider.EmbeddingResponse{
			Data:  []provider.EmbeddingObject{{Embedding: []interface{}{0.25, float32(0.75)}}},
			Usage: provider.EmbeddingUsage{PromptTokens: 6},
		},
		completionContent: "adapted answer",
	}
	registry, err := provider.NewRegistry([]config.ModelConfig{
		{Name: "embed-model", Provider: "test", ProviderModel: "embed-upstream"},
		{Name: "tweak-model", Provider: "test", ProviderModel: "tweak-upstream"},
	}, cacheProviderFactory{provider: mock})
	require.NoError(t, err)

	embed, err := NewEmbedFunc(registry, "embed-model")
	require.NoError(t, err)
	embedResult, err := embed(context.Background(), "hello")
	require.NoError(t, err)
	assert.Equal(t, []float32{0.25, 0.75}, embedResult.Vector)
	assert.Equal(t, 6, embedResult.Tokens)

	tweak, err := NewTweakFunc(registry, "tweak-model")
	require.NoError(t, err)
	tweaked, err := tweak(
		context.Background(),
		"new question",
		`[{"role":"user","content":"old question"}]`,
		`{"id":"cached-1","choices":[{"message":{"role":"assistant","content":"old answer"}}],"usage":{"total_tokens":20}}`,
	)
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(tweaked), &decoded))
	assert.Equal(t, "cached-1", decoded["id"])
	choices := decoded["choices"].([]interface{})
	message := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	assert.Equal(t, "adapted answer", message["content"])

	_, err = NewEmbedFunc(registry, "missing")
	require.ErrorContains(t, err, "not found")
	_, err = NewTweakFunc(registry, "missing")
	require.ErrorContains(t, err, "not found")
}

// TestTweakFuncSkipsWhenEUOnlyKeyMeetsANonEUModel pins GW-02/CTX-03: an
// EU-only key's cache-tweak adaptation must never reach a tweak model with
// no all-EU deployment. Returning an error here is the existing "tweak
// failed" path middleware.go already falls through to upstream on, so this
// must fail closed by skipping the tweak, not by sending the prompt to a
// non-EU deployment.
func TestTweakFuncSkipsWhenEUOnlyKeyMeetsANonEUModel(t *testing.T) {
	mock := &cacheFeatureProvider{completionContent: "adapted answer"}
	registry, err := provider.NewRegistry([]config.ModelConfig{
		{Name: "tweak-model", Provider: "test", ProviderModel: "tweak-upstream", IsEU: false},
	}, cacheProviderFactory{provider: mock})
	require.NoError(t, err)

	tweak, err := NewTweakFunc(registry, "tweak-model")
	require.NoError(t, err)

	ctx := auth.ContextWithKeyInfo(context.Background(), &dbstore.APIKey{KeyHash: "eu-only", RequireEUResidency: true})
	_, err = tweak(ctx, "new question", `[]`, `{"id":"cached-1","choices":[{"message":{"role":"assistant","content":"old"}}]}`)
	require.Error(t, err, "an EU-only key must not have its cache-tweak adaptation sent to a non-EU-only model")

	unrestricted := auth.ContextWithKeyInfo(context.Background(), &dbstore.APIKey{KeyHash: "no-residency"})
	tweaked, err := tweak(unrestricted, "new question", `[]`, `{"id":"cached-1","choices":[{"message":{"role":"assistant","content":"old"}}]}`)
	require.NoError(t, err, "a key with no residency requirement must still get a real tweak")
	require.Contains(t, tweaked, "adapted answer")
}

func TestEmbeddingConversionAndTweakedResponseFallbacks(t *testing.T) {
	got, err := toFloat32([]float64{1.5, 2.5})
	require.NoError(t, err)
	assert.Equal(t, []float32{1.5, 2.5}, got)
	got, err = toFloat32([]float32{3, 4})
	require.NoError(t, err)
	assert.Equal(t, []float32{3, 4}, got)
	_, err = toFloat32([]interface{}{1.0, "bad"})
	require.ErrorContains(t, err, "element type")
	_, err = toFloat32("bad")
	require.ErrorContains(t, err, "embedding type")

	assert.Equal(t, "plain response", extractAssistantContent("plain response"))
	assert.Equal(t, "answer", extractAssistantContent(`{"choices":[{"message":{"content":"answer"}}]}`))

	response := &provider.CompletionResponse{
		ID:      "tweak-1",
		Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "replacement"}}},
	}
	minimal := buildTweakedResponse("not-json", response)
	assert.JSONEq(t, `{"id":"tweak-1","object":"","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":"replacement"},"finish_reason":null}]}`, minimal)
}

func TestAnthropicHelpersRoundTripJSONAndSSE(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet",
		"system":[{"type":"text","text":"system one"},{"type":"image","source":"ignored"},{"type":"text","text":"system two"}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"text","text":"world"},{"type":"tool_use","name":"ignored"}]}
		],
		"stream":true
	}`)

	model, messages, stream := parseAnthropicBody(body)
	assert.Equal(t, "claude-sonnet", model)
	assert.True(t, stream)
	assert.Equal(t, []Message{
		{Role: "system", Content: "system one\nsystem two"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}, messages)
	assert.Equal(t, "", extractAnthropicText(nil))
	assert.Equal(t, "", extractAnthropicText(42))
	assert.Equal(t, 15, extractAnthropicTokenCount(`{"usage":{"input_tokens":10,"output_tokens":5}}`))
	assert.Zero(t, extractAnthropicTokenCount(`not-json`))

	response := `{"id":"msg-1","content":[{"type":"text","text":"cached answer"},{"type":"tool_use"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	recorder := httptest.NewRecorder()
	serveAnthropicSSEFromJSON(recorder, response, "claude-sonnet")
	sse := recorder.Body.String()
	assert.Contains(t, sse, "event: message_start")
	assert.Contains(t, sse, "cached answer")
	assert.Contains(t, sse, "event: message_stop")

	reconstructed, tokens := reconstructAnthropicJSON(sse, "claude-sonnet")
	assert.Equal(t, 15, tokens)
	assert.JSONEq(t, `{
		"id":"msg-1",
		"type":"message",
		"role":"assistant",
		"model":"claude-sonnet",
		"content":[{"type":"text","text":"cached answer"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":5}
	}`, reconstructed)

	rawRecorder := httptest.NewRecorder()
	serveAnthropicSSEFromJSON(rawRecorder, "not-json", "claude-sonnet")
	assert.Contains(t, rawRecorder.Body.String(), "data: not-json")
	empty, tokens := reconstructAnthropicJSON("data: not-json\n", "claude-sonnet")
	assert.Empty(t, empty)
	assert.Zero(t, tokens)
}

func TestOpenAISSEAndCaptureWriterHelpers(t *testing.T) {
	recorder := httptest.NewRecorder()
	buffer := &bytes.Buffer{}
	writer := &captureWriter{ResponseWriter: recorder, buf: buffer, statusCode: http.StatusOK}
	writer.WriteHeader(http.StatusCreated)
	_, err := writer.Write([]byte(`{"ok":true}`))
	require.NoError(t, err)
	writer.Flush()
	assert.Equal(t, http.StatusCreated, writer.statusCode)
	assert.JSONEq(t, `{"ok":true}`, buffer.String())

	sseRecorder := httptest.NewRecorder()
	serveSSE(sseRecorder, `{"choices":[]}`)
	assert.Equal(t, "text/event-stream", sseRecorder.Header().Get("Content-Type"))
	assert.Contains(t, sseRecorder.Body.String(), `data: {"choices":[]}`)
	assert.Contains(t, sseRecorder.Body.String(), "data: [DONE]")
	assert.Equal(t, "0.9877", formatFloat(0.98765))
	assert.Equal(t, "42", formatInt(42))
	assert.Equal(t, 12, extractTokenCount(`{"usage":{"total_tokens":12}}`))
	assert.Zero(t, extractTokenCount(`bad`))
}

func TestCacheMiddlewareMissStoresThenServesDirectHit(t *testing.T) {
	ctx := context.Background()
	memory, err := NewMemoryStore("")
	require.NoError(t, err)
	cache := New(defaultCfg(), memory, func(context.Context, string) (EmbedResult, error) {
		return EmbedResult{Vector: []float32{1, 0}}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	database, err := dbstore.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "cache-middleware.db"),
	})
	require.NoError(t, err)
	require.NoError(t, database.Migrate(ctx))
	defer database.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstreamCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream-1","choices":[{"message":{"role":"assistant","content":"four"}}],"usage":{"total_tokens":21}}`))
	})
	handler := Middleware(cache, database, nil, nil, nil, logger)(next)
	requestBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"What is two plus two?"}]}`

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody)))
	assert.Equal(t, http.StatusOK, first.Code)
	assert.Equal(t, "MISS", first.Header().Get("x-hivecache-status"))
	assert.Equal(t, 1, upstreamCalls)
	require.Eventually(t, func() bool {
		count, _ := memory.Count(ctx)
		return count == 1
	}, time.Second, 10*time.Millisecond)

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody)))
	assert.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, "DIRECT", second.Header().Get("x-hivecache-status"))
	assert.Equal(t, "21", second.Header().Get("x-hivecache-tokens-saved"))
	assert.JSONEq(t, first.Body.String(), second.Body.String())
	assert.Equal(t, 1, upstreamCalls, "a direct cache hit must not call the upstream handler")

	streamBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"What is two plus two?"}],"stream":true}`
	streamed := httptest.NewRecorder()
	handler.ServeHTTP(streamed, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	assert.Equal(t, "DIRECT", streamed.Header().Get("x-hivecache-status"))
	assert.Contains(t, streamed.Body.String(), "data: [DONE]")
	assert.Equal(t, 1, upstreamCalls)

	require.Eventually(t, func() bool {
		metrics, _ := database.ListCacheMetrics(ctx, 10)
		return len(metrics) >= 3
	}, time.Second, 10*time.Millisecond)
}

// TestCacheMiddlewareDeniedModelNeverReadsATeammatesCachedAnswer pins GW-02:
// the semantic cache is looked up by team, not by key, so without its own
// authorization check a key denied a model could read another key's cached
// answer for it straight out of the cache — never reaching the proxy
// handler's own auth.IsModelAllowed check at all. Two keys on the same team
// here: the first (unrestricted) primes the cache; the second (denied_models
// includes the model) must miss it, fall through to next, and never see the
// cached content.
func TestCacheMiddlewareDeniedModelNeverReadsATeammatesCachedAnswer(t *testing.T) {
	ctx := context.Background()
	memory, err := NewMemoryStore("")
	require.NoError(t, err)
	c := New(defaultCfg(), memory, func(context.Context, string) (EmbedResult, error) {
		return EmbedResult{Vector: []float32{1, 0}}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	database, err := dbstore.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "cache-middleware-deny.db"),
	})
	require.NoError(t, err)
	require.NoError(t, database.Migrate(ctx))
	defer database.Close()

	mock := &cacheFeatureProvider{completionContent: "four"}
	registry, err := provider.NewRegistry([]config.ModelConfig{
		{Name: "gpt-4o", Provider: "test", ProviderModel: "gpt-4o-upstream"},
	}, cacheProviderFactory{provider: mock})
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstreamCalls := 0
	// Stands in for the proxy handler's own gate (auth.IsModelAllowed),
	// which every cache-miss path already reaches downstream — this test
	// is about whether the cache middleware lets a denied key skip that
	// gate entirely via a HIT, not about re-testing the gate itself.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if !auth.IsModelAllowed(r.Context(), "gpt-4o", registry.IsRestricted("gpt-4o")) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream-1","choices":[{"message":{"role":"assistant","content":"four"}}],"usage":{"total_tokens":21}}`))
	})
	handler := Middleware(c, database, nil, nil, registry, logger)(next)
	requestBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"What is two plus two?"}]}`

	primingKey := &dbstore.APIKey{KeyHash: "priming-key", TeamID: "team-shared"}
	priming := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	priming = priming.WithContext(auth.ContextWithKeyInfo(priming.Context(), primingKey))
	primingRec := httptest.NewRecorder()
	handler.ServeHTTP(primingRec, priming)
	require.Equal(t, http.StatusOK, primingRec.Code)
	require.Equal(t, "MISS", primingRec.Header().Get("x-hivecache-status"))
	require.Eventually(t, func() bool {
		count, _ := memory.Count(ctx)
		return count == 1
	}, time.Second, 10*time.Millisecond)

	deniedKey := &dbstore.APIKey{KeyHash: "denied-key", TeamID: "team-shared", DeniedModels: dbstore.StringList{"gpt-4o"}}
	denied := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	denied = denied.WithContext(auth.ContextWithKeyInfo(denied.Context(), deniedKey))
	deniedRec := httptest.NewRecorder()
	handler.ServeHTTP(deniedRec, denied)

	assert.Equal(t, http.StatusForbidden, deniedRec.Code, "a denied key must never receive a teammate's cached answer for that model")
	assert.NotEqual(t, "DIRECT", deniedRec.Header().Get("x-hivecache-status"))
	assert.Equal(t, 2, upstreamCalls, "the denied request must reach the handler (which itself refuses it) rather than being answered from cache")
}

// TestCacheMiddlewareEUOnlyKeyNeverReadsANonEUTeammatesCachedAnswer pins
// CTX-03: the cache is not scoped by residency, so without its own
// residency check an EU-only key could read a non-EU-only teammate's cached
// answer for a model with a non-EU deployment straight out of the cache —
// never reaching the proxy handler's own auth.IsResidencyAllowed check.
func TestCacheMiddlewareEUOnlyKeyNeverReadsANonEUTeammatesCachedAnswer(t *testing.T) {
	ctx := context.Background()
	memory, err := NewMemoryStore("")
	require.NoError(t, err)
	c := New(defaultCfg(), memory, func(context.Context, string) (EmbedResult, error) {
		return EmbedResult{Vector: []float32{1, 0}}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	database, err := dbstore.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "cache-middleware-residency.db"),
	})
	require.NoError(t, err)
	require.NoError(t, database.Migrate(ctx))
	defer database.Close()

	mock := &cacheFeatureProvider{completionContent: "four"}
	registry, err := provider.NewRegistry([]config.ModelConfig{
		// Not all-EU: one EU deployment, one not — AllDeploymentsEU must be false.
		{Name: "gpt-4o", Provider: "test", ProviderModel: "gpt-4o-upstream", IsEU: true},
		{Name: "gpt-4o", Provider: "test2", ProviderModel: "gpt-4o-upstream-2", IsEU: false},
	}, cacheProviderFactory{provider: mock})
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstreamCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		allEU, hasDeployments := registry.AllDeploymentsEU("gpt-4o")
		if !auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream-1","choices":[{"message":{"role":"assistant","content":"four"}}],"usage":{"total_tokens":21}}`))
	})
	handler := Middleware(c, database, nil, nil, registry, logger)(next)
	requestBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"What is two plus two?"}]}`

	primingKey := &dbstore.APIKey{KeyHash: "priming-key", TeamID: "team-shared"}
	priming := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	priming = priming.WithContext(auth.ContextWithKeyInfo(priming.Context(), primingKey))
	primingRec := httptest.NewRecorder()
	handler.ServeHTTP(primingRec, priming)
	require.Equal(t, http.StatusOK, primingRec.Code)
	require.Equal(t, "MISS", primingRec.Header().Get("x-hivecache-status"))
	require.Eventually(t, func() bool {
		count, _ := memory.Count(ctx)
		return count == 1
	}, time.Second, 10*time.Millisecond)

	euOnlyKey := &dbstore.APIKey{KeyHash: "eu-only-key", TeamID: "team-shared", RequireEUResidency: true}
	euOnlyReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	euOnlyReq = euOnlyReq.WithContext(auth.ContextWithKeyInfo(euOnlyReq.Context(), euOnlyKey))
	euOnlyRec := httptest.NewRecorder()
	handler.ServeHTTP(euOnlyRec, euOnlyReq)

	assert.Equal(t, http.StatusForbidden, euOnlyRec.Code, "an EU-only key must never receive a non-EU-only teammate's cached answer")
	assert.NotEqual(t, "DIRECT", euOnlyRec.Header().Get("x-hivecache-status"))
	assert.Equal(t, 2, upstreamCalls, "the EU-only request must reach the handler (which itself refuses it) rather than being answered from cache")
}

func TestCacheMiddlewareBypassesUnsupportedAndAgenticRequests(t *testing.T) {
	nextCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	handler := Middleware(nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(next)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	agentic := `{"model":"gpt-4o","messages":[{"role":"user","content":"do it"}],"tools":[{"type":"function"}]}`
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(agentic)))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`not-json`)))

	assert.Equal(t, 3, nextCalls)
}

func TestCacheEnabledFor_KeyOverrideWinsOverTeamDefault(t *testing.T) {
	ctx := context.Background()
	database, err := dbstore.Open(config.DatabaseConfig{Driver: "sqlite", URL: ":memory:"})
	require.NoError(t, err)
	require.NoError(t, database.Migrate(ctx))
	defer database.Close()

	require.NoError(t, database.UpsertTenantSettings(ctx, &dbstore.TenantSettings{
		TeamID: "team-1", CacheEnabled: true,
	}))
	require.NoError(t, database.UpsertTenantSettings(ctx, &dbstore.TenantSettings{
		TeamID: "team-1", CacheEnabled: false,
	}))
	enabled := true
	require.NoError(t, database.UpsertKeyRouteSettings(ctx, &dbstore.KeyRouteSettings{
		KeyID: "key-hash-1", TeamID: "team-1", CacheEnabled: &enabled,
	}))

	got, _ := cacheSettingsFor(ctx, database, &dbstore.APIKey{TeamID: "team-1", KeyHash: "key-hash-1"})
	assert.True(t, got, "a per-key override enabling cache must win over a disabled team default")

	require.NoError(t, database.UpsertTenantSettings(ctx, &dbstore.TenantSettings{
		TeamID: "team-2", CacheEnabled: true,
	}))
	disabled := false
	require.NoError(t, database.UpsertKeyRouteSettings(ctx, &dbstore.KeyRouteSettings{
		KeyID: "key-hash-2", TeamID: "team-2", CacheEnabled: &disabled,
	}))

	got, _ = cacheSettingsFor(ctx, database, &dbstore.APIKey{TeamID: "team-2", KeyHash: "key-hash-2"})
	assert.False(t, got, "a per-key override disabling cache must win over an enabled team default")
}

func TestCacheEnabledFor_FallsBackToTeamThenDefaultsOn(t *testing.T) {
	ctx := context.Background()
	database, err := dbstore.Open(config.DatabaseConfig{Driver: "sqlite", URL: ":memory:"})
	require.NoError(t, err)
	require.NoError(t, database.Migrate(ctx))
	defer database.Close()

	// No key override, no team row at all: defaults on.
	got, overrides := cacheSettingsFor(ctx, database, &dbstore.APIKey{TeamID: "team-3", KeyHash: "key-hash-3"})
	assert.True(t, got)
	assert.Equal(t, ThresholdOverrides{}, overrides)

	// No key override, team disabled: team default applies.
	require.NoError(t, database.UpsertTenantSettings(ctx, &dbstore.TenantSettings{
		TeamID: "team-3", CacheEnabled: true,
	}))
	require.NoError(t, database.UpsertTenantSettings(ctx, &dbstore.TenantSettings{
		TeamID: "team-3", CacheEnabled: false,
	}))
	got, _ = cacheSettingsFor(ctx, database, &dbstore.APIKey{TeamID: "team-3", KeyHash: "key-hash-3"})
	assert.False(t, got)
}

func TestCacheSettingsFor_ThresholdOverridesSurviveAlongsideDisabledCache(t *testing.T) {
	ctx := context.Background()
	database, err := dbstore.Open(config.DatabaseConfig{Driver: "sqlite", URL: ":memory:"})
	require.NoError(t, err)
	require.NoError(t, database.Migrate(ctx))
	defer database.Close()

	direct, reuse, tweak := 0.5, 0.4, 0.3
	require.NoError(t, database.UpsertKeyRouteSettings(ctx, &dbstore.KeyRouteSettings{
		KeyID: "key-hash-4", TeamID: "team-4",
		CacheDirectThreshold: &direct, CacheReuseThreshold: &reuse, CacheTweakThreshold: &tweak,
	}))

	got, overrides := cacheSettingsFor(ctx, database, &dbstore.APIKey{TeamID: "team-4", KeyHash: "key-hash-4"})
	assert.True(t, got, "no cache_enabled override was set: defaults on")
	require.NotNil(t, overrides.DirectThreshold)
	assert.Equal(t, direct, *overrides.DirectThreshold)
	require.NotNil(t, overrides.ReuseThreshold)
	assert.Equal(t, reuse, *overrides.ReuseThreshold)
	require.NotNil(t, overrides.TweakThreshold)
	assert.Equal(t, tweak, *overrides.TweakThreshold)
}

func TestExtractContextBoundsWindowAndQueryFallback(t *testing.T) {
	longContext := strings.Repeat("context-", 400)
	messages := []Message{
		{Role: "system", Content: longContext},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "latest"},
	}
	contextText := ExtractContext(messages)
	assert.Len(t, contextText, 2000)
	assert.Equal(t, "latest", ExtractQuery(messages))
	assert.Equal(t, "system assistant", ExtractQuery([]Message{{Role: "system", Content: "system"}, {Role: "assistant", Content: "assistant"}}))
	assert.Empty(t, ExtractContext([]Message{{Role: "user", Content: "only"}}))
}

var _ VectorStore = (*cacheVectorStore)(nil)
var _ provider.Provider = (*cacheFeatureProvider)(nil)
var _ provider.Embedder = (*cacheFeatureProvider)(nil)
var _ provider.ProviderFactory = cacheProviderFactory{}

func ExampleCache_classifyBand() {
	cfg := config.CacheConfig{DirectThreshold: 0.95, ReuseThreshold: 0.90, TweakThreshold: 0.85, TweakModel: "small-model"}
	cache := New(cfg, &cacheVectorStore{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fmt.Println(cache.classifyBand(0.96, ThresholdOverrides{}))
	fmt.Println(cache.classifyBand(0.92, ThresholdOverrides{}))
	fmt.Println(cache.classifyBand(0.86, ThresholdOverrides{}))
	fmt.Println(cache.classifyBand(0.50, ThresholdOverrides{}))
	// Output:
	// DIRECT
	// REUSE
	// TWEAK
	// MISS
}

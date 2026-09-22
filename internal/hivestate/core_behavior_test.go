package hivestate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestStateCacheExactHitExpiryEvictionAndPrefixLookup(t *testing.T) {
	cache := newStateCache(2, time.Minute)
	historyA := []Message{{Role: "user", Content: "a"}}
	historyAB := append(append([]Message{}, historyA...), Message{Role: "assistant", Content: "b"})
	historyABC := append(append([]Message{}, historyAB...), Message{Role: "user", Content: "c"})
	stateA := &State{Intent: "first"}
	stateAB := &State{Intent: "second"}

	keyA := historyHash(historyA)
	keyAB := historyHash(historyAB)
	assert.Equal(t, keyA, historyHash(historyA))
	assert.NotEqual(t, keyA, historyHash([]Message{{Role: "assistant", Content: "a"}}))

	cache.put(keyA, `{"intent":"first"}`, stateA, len(historyA))
	time.Sleep(time.Millisecond)
	cache.put(keyAB, `{"intent":"second"}`, stateAB, len(historyAB))
	assert.Equal(t, 2, cache.stats())

	jsonValue, stateValue, ok := cache.get(keyAB)
	require.True(t, ok)
	assert.Equal(t, `{"intent":"second"}`, jsonValue)
	assert.Same(t, stateAB, stateValue)

	prefixJSON, prefixState, prefixLen, ok := cache.findPrefix(historyABC)
	require.True(t, ok)
	assert.Equal(t, `{"intent":"second"}`, prefixJSON)
	assert.Same(t, stateAB, prefixState)
	assert.Equal(t, 2, prefixLen)

	historyD := []Message{{Role: "user", Content: "d"}}
	keyD := historyHash(historyD)
	cache.put(keyD, `{"intent":"third"}`, &State{Intent: "third"}, 1)
	assert.Equal(t, 2, cache.stats())
	_, _, ok = cache.get(keyA)
	assert.False(t, ok, "the oldest entry must be evicted at capacity")

	cache.entries[keyAB].createdAt = time.Now().Add(-2 * time.Minute)
	_, _, ok = cache.get(keyAB)
	assert.False(t, ok)
	_, _, _, ok = cache.findPrefix(historyABC)
	assert.False(t, ok)
}

type hiveStateProvider struct {
	mu       sync.Mutex
	calls    int
	response string
	err      error
}

func (p *hiveStateProvider) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()

	if p.err != nil {
		return nil, p.err
	}
	if req.MaxTokens == nil || *req.MaxTokens < 512 {
		return nil, fmt.Errorf("expected a bounded extraction request")
	}
	return &provider.CompletionResponse{
		Choices: []provider.Choice{{
			Message: &provider.Message{Role: "assistant", Content: p.response},
		}},
		Usage: &provider.Usage{PromptTokens: 120, CompletionTokens: 24, TotalTokens: 144},
	}, nil
}

func (p *hiveStateProvider) Stream(context.Context, *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, fmt.Errorf("stream not expected")
}

func (p *hiveStateProvider) Name() string { return "hivestate-test" }

func (p *hiveStateProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type hiveStateFactory struct {
	provider provider.Provider
}

func (f hiveStateFactory) Create(config.ModelConfig) (provider.Provider, error) {
	return f.provider, nil
}

func TestHiveStateProcessExtractsRewritesAndReusesCachedState(t *testing.T) {
	mock := &hiveStateProvider{
		response: `{"intent":"continue_implementation","difficulty":"complex","reasoning_effort":"high","active_constraints":{"values":{"port":4000}},"conversation_status":"in_progress"}`,
	}
	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          "state-model",
		Provider:      "test",
		ProviderModel: "state-model-upstream",
	}}, hiveStateFactory{provider: mock})
	require.NoError(t, err)

	cfg := config.HiveStateConfig{
		Enabled:      true,
		Model:        "state-model",
		Threshold:    1,
		MaxLatencyMs: 1000,
		StepWindow:   1,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine, err := New(cfg, registry, logger)
	require.NoError(t, err)

	longTurn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	messages := []Message{
		{Role: "system", Content: strings.Repeat("Stable system policy. ", 10) + "Current date: 2026-07-24"},
		{Role: "user", Content: longTurn + " first"},
		{Role: "assistant", Content: longTurn + " second"},
		{Role: "user", Content: longTurn + " third"},
		{Role: "assistant", Content: longTurn + " fourth"},
		{Role: "user", Content: longTurn + " fifth"},
		{Role: "assistant", Content: "The latest step remains verbatim."},
		{Role: "user", Content: "Continue with the next implementation step."},
	}

	first := engine.Process(context.Background(), messages, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})

	require.Equal(t, ModeState, first.Mode)
	require.NotNil(t, first.State)
	assert.Equal(t, "continue_implementation", first.State.Intent)
	assert.False(t, first.CacheHit)
	assert.Equal(t, 120, first.PromptTokens)
	assert.Equal(t, 24, first.CompletionTokens)
	assert.Less(t, first.ResultTokens, first.OriginalTokens)
	assert.Equal(t, "Continue with the next implementation step.", first.Messages[len(first.Messages)-1].Content)
	// The system prompt is forwarded byte-for-byte. It sits in the region every
	// provider prompt cache covers, so rewriting it — as the retired
	// CacheAligner did, hoisting volatile content to a tail marker —
	// invalidated the cached prefix on every request that touched it.
	assert.Equal(t, messages[0].Content, first.Messages[0].Content,
		"system prompt must reach the provider unmodified")
	assert.Contains(t, first.Messages[1].Content, "Conversation state (summary of older context)")
	assert.Equal(t, 1, mock.callCount())

	second := engine.Process(context.Background(), messages, Scope{TeamID: "t1", KeyHash: "k1", SessionID: "s1"})

	require.Equal(t, ModeState, second.Mode)
	assert.True(t, second.CacheHit)
	assert.Equal(t, first.StateJSON, second.StateJSON)
	assert.Equal(t, 1, mock.callCount(), "an exact history cache hit must not call the extraction provider again")
}

func TestStateExtractorValidatesResponseAndUsage(t *testing.T) {
	mock := &hiveStateProvider{response: "prefix ```json\n{\"intent\":\"ship_release\",\"active_constraints\":{}}\n``` suffix"}
	extractor := &StateExtractor{
		provider:      mock,
		providerModel: "state-upstream",
		timeout:       time.Second,
	}

	result, err := extractor.ExtractWithBudget(
		context.Background(),
		[]Message{{Role: "user", Content: "implemented the release"}},
		Message{Role: "user", Content: "what next?"},
		300,
		nil,
	)

	require.NoError(t, err)
	assert.Equal(t, "ship_release", result.State.Intent)
	assert.Equal(t, 120, result.PromptTokens)
	assert.Equal(t, 24, result.CompletionTokens)
	assert.JSONEq(t, `{"intent":"ship_release","active_constraints":{}}`, result.JSON)

	mock.response = `{"active_constraints":{}}`
	_, err = extractor.Extract(context.Background(), nil, Message{Content: "next"})
	require.ErrorContains(t, err, "missing intent")

	mock.response = `not valid json`
	_, err = extractor.Extract(context.Background(), nil, Message{Content: "next"})
	require.ErrorContains(t, err, "invalid JSON")
}

func TestStateJSONTruncationKeepsEssentialFields(t *testing.T) {
	raw := `{
		"intent":"continue_work",
		"domain":"software",
		"errors_encountered":[{"error":"old"}],
		"files_modified":[{"path":"a"},{"path":"b"}],
		"resolved_items":["one","two","three"],
		"pending_items":["one","two","three","four"],
		"important_context":"verbose",
		"active_constraints":{
			"identifiers":{"a":1,"b":2,"c":3,"d":4},
			"values":{"a":1,"b":2,"c":3,"d":4},
			"actions_taken":[1,2,3,4,5]
		}
	}`

	got := truncateStateJSON(raw, 45)

	assert.LessOrEqual(t, len(got), len(raw))
	assert.Contains(t, got, `"intent":"continue_work"`)
	assert.NotContains(t, got, "errors_encountered")
	assert.NotContains(t, got, "files_modified")
	assert.Equal(t, "unchanged", truncateStateJSON("unchanged", 100))
	assert.Equal(t, "{invalid", truncateStateJSON("{invalid", 1))
}

func TestPromptBuildersIncludeLevelsAndBoundLongHistory(t *testing.T) {
	levels := []config.HiveRouteLevel{{Name: "complex", Description: "deep multi-module work"}}
	suffix := hiveRouteDifficultyPromptSuffix(levels)
	assert.Contains(t, suffix, `"complex": deep multi-module work`)
	assert.Empty(t, hiveRouteDifficultyPromptSuffix(nil))

	longContent := strings.Repeat("x", 9000)
	prompt := buildExtractionPrompt(
		[]Message{{Role: "assistant", Content: longContent}},
		Message{Role: "user", Content: "continue"},
		levels,
	)
	assert.Contains(t, prompt, "[truncated]")
	assert.Contains(t, prompt, "Latest user message")
	assert.Contains(t, prompt, "continue")
	assert.Less(t, len(prompt), len(longContent))
}

func TestHiveStateNewGracefullyDisablesMissingModel(t *testing.T) {
	registry, err := provider.NewRegistry(nil, hiveStateFactory{provider: &hiveStateProvider{}})
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	engine, err := New(config.HiveStateConfig{
		Model:        "missing",
		MaxLatencyMs: 100,
		StepWindow:   1,
	}, registry, logger)

	require.NoError(t, err)
	assert.Nil(t, engine.extractor)
}

var _ provider.Provider = (*hiveStateProvider)(nil)
var _ provider.ProviderFactory = hiveStateFactory{}

package hivestate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ============================================================
// Test helpers
// ============================================================

func mwTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openMiddlewareTestStore(t *testing.T) store.Store {
	t.Helper()
	// In-memory, not a temp file: the middleware writes its metric from a
	// detached goroutine, so a file-backed store had sqlite recreating its WAL
	// sidecars after the test returned and TempDir cleanup then failed on a
	// non-empty directory. Safe since the store pins SQLite to one connection.
	s, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    ":memory:",
	})
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestSpender(t *testing.T, db store.Store) *spend.BatchWriter {
	t.Helper()
	bw := spend.NewBatchWriter(db, mwTestLogger(), 50*time.Millisecond)
	t.Cleanup(func() { _ = bw.Close() })
	return bw
}

// newNoOpHiveState returns a HiveState whose threshold is high enough that
// Process() always falls back to ModeNoOp, without needing a real extractor.
func newNoOpHiveState() *HiveState {
	return &HiveState{
		cfg:     config.HiveStateConfig{Enabled: true, Threshold: 999999, MaxLatencyMs: 3000, StepWindow: 4, Model: "state-model"},
		counter: NewTokenCounter(),
		logger:  mwTestLogger(),
		cache:   newStateCache(128, 10*time.Minute),
		ccr:     newCCRStore(512, 32, 30*time.Minute),
	}
}

// newStateHiveState returns a HiveState configured with a low threshold and a
// non-nil (but never-invoked) extractor, ready to have its cache pre-seeded
// so Process() takes the cache-hit path deterministically without a real LLM
// call. routeCfg is optional HiveRoute config to exercise routing.
func newStateHiveState(routeCfg config.HiveRouteConfig) *HiveState {
	// The prefix-cache guard is off for these fixtures. They exercise the
	// HiveRoute and metrics plumbing on a deliberately tiny conversation
	// (~145 tokens), where replacing the history with a state summary makes
	// the prompt *larger* — so the guard correctly refuses to rewrite and the
	// plumbing under test never runs. The guard has its own tests in
	// prefix_cache_test.go and prefix_cache_middleware_test.go.
	guardOff := false
	return &HiveState{
		cfg: config.HiveStateConfig{
			Enabled: true, Threshold: 1, MaxLatencyMs: 3000, StepWindow: 1,
			Model: "state-model", HiveRoute: routeCfg,
			PrefixCacheGuard: config.PrefixCacheGuardConfig{Enabled: &guardOff},
		},
		extractor: &StateExtractor{}, // never invoked: cache-hit path returns before extraction
		counter:   NewTokenCounter(),
		logger:    mwTestLogger(),
		cache:     newStateCache(128, 10*time.Minute),
		ccr:       newCCRStore(512, 32, 30*time.Minute),
	}
}

// seedStateCache primes hs's cache so that Process(messages) takes the
// cache-hit branch and deterministically returns ModeState, by replicating
// the exact cache-key derivation Process uses (SplitMessages with the
// configured step window). Keeps History <= 4 messages so Process's
// importance-based History reordering (only triggered above 4 messages)
// never runs and can't shift the derived cache key.
func seedStateCache(t *testing.T, hs *HiveState, messages []Message, stateJSON string, state *State) {
	t.Helper()
	zones := SplitMessages(messages, hs.cfg.StepWindow)
	require.NotEmpty(t, zones.History)
	require.LessOrEqual(t, len(zones.History), 4)
	hKey := historyHash(zones.History)
	hs.cache.put(hKey, stateJSON, state, len(zones.History))
}

// longConversation returns a message set with enough History content (and
// small enough Recent/Last) to clear Process's insufficient_headroom and
// no_savings guards once compressed to a short cached state summary.
func longConversation() []Message {
	return []Message{
		{Role: "system", Content: "You are a helpful assistant. Answer concisely and stay on topic at all times."},
		{Role: "user", Content: "I want a deep, thorough explanation of the history of the Roman Empire, from its founding through its eventual fall, including major emperors and turning points."},
		{Role: "assistant", Content: "The Roman Empire began around 27 BC under Augustus and lasted for centuries, eventually splitting into Eastern and Western halves before the Western half fell in 476 AD."},
		{Role: "user", Content: "What were the main causes of its eventual decline and collapse over time?"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "thanks"},
	}
}

func requestBodyFor(model string, messages []Message) []byte {
	raw := map[string]any{
		"model":    model,
		"messages": messages,
	}
	b, _ := json.Marshal(raw)
	return b
}

type capturingHandler struct {
	called      bool
	receivedReq *http.Request
	body        []byte
	statusCode  int
}

func (c *capturingHandler) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.called = true
		c.receivedReq = r
		c.body, _ = io.ReadAll(r.Body)
		status := c.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}
}

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("boom: read failure") }
func (errReadCloser) Close() error             { return nil }

func waitForMetric(t *testing.T, db store.Store, want int) []store.HiveStateMetric {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		metrics, err := db.ListHiveStateMetrics(context.Background(), 50)
		require.NoError(t, err)
		if len(metrics) >= want {
			return metrics
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d hivestate metric(s)", want)
	return nil
}

func waitForSpend(t *testing.T, db store.Store, keyHash string, want int) []store.SpendRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records, err := db.GetSpend(context.Background(), store.SpendFilter{KeyHash: keyHash, Limit: 50})
		require.NoError(t, err)
		if len(records) >= want {
			return records
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d spend record(s) for key %q", want, keyHash)
	return nil
}

// ============================================================
// Middleware: routing / opt-out / passthrough behavior
// ============================================================

func TestMiddleware_NonMatchingPath_Passthrough(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())

	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"))
}

func TestMiddleware_OptOutHeader_Passthrough(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())

	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", longConversation())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-ubiquum-state", "false")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"))
	assert.Equal(t, body, next.body, "body should be forwarded unmodified")
}

func TestMiddleware_TeamCompressionDisabled_Passthrough(t *testing.T) {
	db := openMiddlewareTestStore(t)
	// CompressionEnabled carries a GORM `default:true` tag, so a fresh INSERT
	// silently replaces false (Go zero value) with the column default. Insert
	// first, then flip via the UPDATE path like production does.
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-1", CompressionEnabled: true,
	}))
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-1", CompressionEnabled: false,
	}))

	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", longConversation())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"))
}

func TestMiddleware_KeyCompressionOverride_EnablesDespiteTeamDisabled(t *testing.T) {
	db := openMiddlewareTestStore(t)
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-3", CompressionEnabled: true,
	}))
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-3", CompressionEnabled: false,
	}))
	enabled := true
	require.NoError(t, db.UpsertKeyRouteSettings(context.Background(), &store.KeyRouteSettings{
		KeyID: "key-hash-3", TeamID: "team-3", CompressionEnabled: &enabled,
	}))

	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", longConversation())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-3", KeyHash: "key-hash-3"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, "none", rec.Header().Get("x-hivestate-mode"),
		"a per-key override enabling compression must win over a disabled team default")
}

func TestMiddleware_KeyCompressionOverride_DisablesDespiteTeamEnabled(t *testing.T) {
	db := openMiddlewareTestStore(t)
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-4", CompressionEnabled: true,
	}))
	disabled := false
	require.NoError(t, db.UpsertKeyRouteSettings(context.Background(), &store.KeyRouteSettings{
		KeyID: "key-hash-4", TeamID: "team-4", CompressionEnabled: &disabled,
	}))

	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", longConversation())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-4", KeyHash: "key-hash-4"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"),
		"a per-key override disabling compression must win over an enabled team default")
}

func TestMiddleware_KeyCompressionThresholdOverride_ForcesBelowThreshold(t *testing.T) {
	db := openMiddlewareTestStore(t)
	// A conversation this engine would normally compress (low config
	// threshold, seeded cache) but a policy has raised this key's own
	// threshold above what the conversation actually contains.
	hs := newStateHiveState(config.HiveRouteConfig{})
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug","difficulty":"complex"}`,
		&State{Intent: "debug", Difficulty: "complex"})

	narrowed := 999999
	require.NoError(t, db.UpsertKeyRouteSettings(context.Background(), &store.KeyRouteSettings{
		KeyID: "key-hash-5", TeamID: "team-5", CompressionThreshold: &narrowed,
	}))

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-5", KeyHash: "key-hash-5"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	assert.Equal(t, "none", rec.Header().Get("x-hivestate-mode"),
		"a per-key compression_threshold override must reach Process() and force a below-threshold no-op")
}

func TestMiddleware_BodyReadError_Passthrough(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Body = errReadCloser{}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called, "on body read error, request should still be forwarded")
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"))
}

func TestMiddleware_MalformedJSON_Passthrough(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"))
}

func TestMiddleware_EmptyMessages_Passthrough(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hivestate-mode"))
}

// ============================================================
// Middleware: ModeNoOp — headers set, async metric logged
// ============================================================

func TestMiddleware_ModeNoOp_OpenAI_SetsHeadersAndLogsMetric(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", longConversation())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-request-id", "req-noop-openai")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, "none", rec.Header().Get("x-hivestate-mode"))
	assert.NotEmpty(t, rec.Header().Get("x-hivestate-original-tokens"))

	metrics := waitForMetric(t, db, 1)
	assert.Equal(t, "none", metrics[0].Mode)
}

func TestMiddleware_ModeNoOp_Anthropic_ParsesMessages(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	raw := map[string]any{
		"model": "claude-sonnet-5",
		"system": []map[string]any{
			{"type": "text", "text": "You are helpful."},
		},
		"messages": []map[string]any{
			{"role": "user", "content": "hello there"},
			{"role": "assistant", "content": "hi, how can I help?"},
		},
	}
	body, _ := json.Marshal(raw)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, "none", rec.Header().Get("x-hivestate-mode"))
}

func TestMiddleware_ModeNoOp_Responses_ParsesMessages(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newNoOpHiveState()
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	raw := map[string]any{
		"model":        "gpt-5",
		"instructions": "You are helpful.",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hello"},
		},
	}
	body, _ := json.Marshal(raw)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, "none", rec.Header().Get("x-hivestate-mode"))
}

// ============================================================
// Middleware: ModeState (cache hit) — body rewrite + HiveRoute
// ============================================================

func TestMiddleware_ModeState_RewritesBodyAndLogsMetric(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newStateHiveState(config.HiveRouteConfig{})
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"learn_history"}`, &State{Intent: "learn_history"})

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-request-id", "req-state-openai")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	assert.Equal(t, "state", rec.Header().Get("x-hivestate-mode"))
	assert.Equal(t, "learn_history", rec.Header().Get("x-hivestate-intent"))

	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(next.body, &forwarded))
	forwardedMsgs, ok := forwarded["messages"].([]any)
	require.True(t, ok)
	assert.Less(t, len(forwardedMsgs), len(msgs), "history should have been compressed into a shorter message list")

	metrics := waitForMetric(t, db, 1)
	assert.Equal(t, "state", metrics[0].Mode)
	assert.Equal(t, "learn_history", metrics[0].Intent)
}

func TestMiddleware_ModeState_HiveRoute_OverridesModel(t *testing.T) {
	db := openMiddlewareTestStore(t)
	routeCfg := config.HiveRouteConfig{
		Enabled: true,
		Levels: []config.HiveRouteLevel{
			{Name: "complex", Model: "gpt-5-complex", Description: "hard tasks"},
		},
	}
	hs := newStateHiveState(routeCfg)
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug","difficulty":"complex","reasoning_effort":"high"}`,
		&State{Intent: "debug", Difficulty: "complex", ReasoningEffort: "high"})

	// registry=nil: the restricted-model guard (registry.IsRestricted) is
	// skipped entirely, so routing proceeds unconditionally once a level matches.
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	assert.Equal(t, "complex", rec.Header().Get("x-hiveroute-level"))
	assert.Equal(t, "gpt-5-complex", rec.Header().Get("x-hiveroute-model"))
	assert.Equal(t, "high", rec.Header().Get("x-hiveroute-effort"))
	assert.NotEmpty(t, rec.Header().Get("x-hiveroute-savings"))

	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(next.body, &forwarded))
	assert.Equal(t, "gpt-5-complex", forwarded["model"])
	assert.Equal(t, "high", forwarded["reasoning_effort"])
}

func TestMiddleware_ModeState_HiveRoute_DownstreamFailure_ZeroesSavings(t *testing.T) {
	db := openMiddlewareTestStore(t)
	routeCfg := config.HiveRouteConfig{
		Enabled: true,
		Levels: []config.HiveRouteLevel{
			{Name: "complex", Model: "gpt-5-complex"},
		},
	}
	hs := newStateHiveState(routeCfg)
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug","difficulty":"complex"}`,
		&State{Intent: "debug", Difficulty: "complex"})

	pc := pricing.NewCalculator(mwTestLogger(), "", nil)
	mw := Middleware(hs, db, pc, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{statusCode: http.StatusInternalServerError}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-request-id", "req-state-fail")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	assert.Equal(t, "gpt-5-complex", rec.Header().Get("x-hiveroute-model"))

	metrics := waitForMetric(t, db, 1)
	assert.Equal(t, 0.0, metrics[0].RouteSavedCost, "savings should be zeroed when the routed call failed downstream")
}

func TestMiddleware_ModeState_KeyRouteSettingsOverrideTeamLevels(t *testing.T) {
	db := openMiddlewareTestStore(t)
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-2", CompressionEnabled: true,

		HiveRouteLevels: []store.RouteLevel{{Name: "complex", Model: "team-model"}},
	}))
	require.NoError(t, db.UpsertKeyRouteSettings(context.Background(), &store.KeyRouteSettings{
		KeyID: "key-hash-2", TeamID: "team-2",
		Levels: []store.RouteLevel{{Name: "complex", Model: "key-model"}},
	}))

	hs := newStateHiveState(config.HiveRouteConfig{Enabled: true})
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug","difficulty":"complex"}`,
		&State{Intent: "debug", Difficulty: "complex"})

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-2", KeyHash: "key-hash-2"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	assert.Equal(t, "key-model", rec.Header().Get("x-hiveroute-model"), "per-key route settings should win over per-team settings")
}

// TestMiddleware_SpendGoroutine_DoesNotRaceRequestReassignment pins VAL-02's
// flagged data race: the spend-logging goroutine used to close over the `r`
// variable directly and read r.Context() from it, while the same request's
// own handler goroutine reassigns `r` (`r = r.WithContext(...)`) moments
// later on both the NO_OP and STATE paths — a write racing a read of the
// same variable from two goroutines with nothing ordering them.
//
// Needs a real extraction call, not the cache-hit fixtures the rest of this
// file uses: PromptTokens/CompletionTokens — the only thing that gates the
// spend goroutine at all — are set from the extraction call's own Usage
// (hivestate.go), never from the cached State, so every cache-hit path in
// this file legitimately reports zero and would never reach it. Built the
// same way core_behavior_test.go's TestHiveStateProcessExtractsRewritesAnd
// ReusesCachedState constructs a real, non-stubbed HiveState.
//
// This alone reproduced the race reliably under `go test -race` before the
// fix (keyInfo now resolved on the handler goroutine, before the closure
// spawns). Run with `-race -count=20` for the strongest signal, matching
// VAL-02's own acceptance bar — each run builds a fresh HiveState, so
// repeating the whole test widens the race window without a stale cache
// hiding the extraction path on later iterations the way a loop inside one
// run would.
func TestMiddleware_SpendGoroutine_DoesNotRaceRequestReassignment(t *testing.T) {
	mock := &hiveStateProvider{
		response: `{"intent":"continue_implementation"}`,
	}
	registry, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          "state-model",
		Provider:      "test",
		ProviderModel: "state-model-upstream",
	}}, hiveStateFactory{provider: mock})
	require.NoError(t, err)

	hs, err := New(config.HiveStateConfig{
		Enabled:      true,
		Model:        "state-model",
		Threshold:    1,
		MaxLatencyMs: 1000,
		StepWindow:   1,
	}, registry, mwTestLogger())
	require.NoError(t, err)

	db := openMiddlewareTestStore(t)
	spender := newTestSpender(t, db)
	mw := Middleware(hs, db, nil, spender, nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())
	keyHash := "race-key-hash"

	// longConversation() (used by every other test in this file) is
	// deliberately short — tuned so the cache-hit fixtures never trip the
	// headroom/no_savings guards. This path has no cache to hit, so it
	// needs a conversation actually long enough for a real extraction call
	// to shrink — the same shape core_behavior_test.go's own real-
	// extraction test uses.
	longTurn := strings.Repeat("Detailed implementation context with identifiers and constraints. ", 45)
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("Stable system policy. ", 10) + "Current date: 2026-07-24"},
		{Role: "user", Content: longTurn + " first"},
		{Role: "assistant", Content: longTurn + " second"},
		{Role: "user", Content: longTurn + " third"},
		{Role: "assistant", Content: longTurn + " fourth"},
		{Role: "user", Content: longTurn + " fifth"},
		{Role: "assistant", Content: "The latest step remains verbatim."},
		{Role: "user", Content: "Continue with the next implementation step."},
	}
	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{KeyHash: keyHash, TeamID: "team-race"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	records := waitForSpend(t, db, keyHash, 1)
	assert.Equal(t, keyHash, records[0].KeyHash, "the goroutine must attribute spend to the request's own key, not a stale or zero value")
	assert.Equal(t, "team-race", records[0].TeamID)
	assert.Equal(t, 120, records[0].PromptTokens)
	assert.Equal(t, 24, records[0].CompletionTokens)
}

// ============================================================
// hiveStateStatusWriter
// ============================================================

func TestHiveStateStatusWriter_WriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &hiveStateStatusWriter{ResponseWriter: rec, status: http.StatusOK}
	sw.WriteHeader(http.StatusTeapot)
	assert.Equal(t, http.StatusTeapot, sw.status)
	assert.True(t, sw.wroteHeader)
	assert.Equal(t, http.StatusTeapot, rec.Code)
}

func TestHiveStateStatusWriter_WriteWithoutHeaderDefaultsTo200(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &hiveStateStatusWriter{ResponseWriter: rec}
	n, err := sw.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, http.StatusOK, sw.status)
	assert.True(t, sw.wroteHeader)
}

func TestHiveStateStatusWriter_WriteAfterHeaderKeepsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &hiveStateStatusWriter{ResponseWriter: rec}
	sw.WriteHeader(http.StatusCreated)
	_, err := sw.Write([]byte("body"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, sw.status)
}

func TestHiveStateStatusWriter_Flush(t *testing.T) {
	rec := httptest.NewRecorder() // implements http.Flusher
	sw := &hiveStateStatusWriter{ResponseWriter: rec}
	assert.NotPanics(t, func() { sw.Flush() })
}

type nonFlushingWriter struct{ http.ResponseWriter }

func TestHiveStateStatusWriter_Flush_NonFlusherNoPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &hiveStateStatusWriter{ResponseWriter: nonFlushingWriter{rec}}
	assert.NotPanics(t, func() { sw.Flush() })
}

// ============================================================
// truncate / stripANSI
// ============================================================

func TestTruncate(t *testing.T) {
	assert.Equal(t, "hello", truncate("hello", 10))
	assert.Equal(t, "hel...", truncate("hello", 3))
	assert.Equal(t, "", truncate("", 5))
}

func TestStripANSI(t *testing.T) {
	assert.Equal(t, "hello world", stripANSI("hello world"))
	assert.Equal(t, "red text", stripANSI("\x1b[31mred text\x1b[0m"))
	assert.Equal(t, "plain", stripANSI("\x1b[1;32mplain\x1b[0m"))
}

// ============================================================
// isOpenAIUserTurn / isAnthropicAssistant / isResponsesUserTurn
// ============================================================

func TestIsOpenAIUserTurn(t *testing.T) {
	assert.True(t, isOpenAIUserTurn(json.RawMessage(`{"role":"user","content":"hi"}`)))
	assert.False(t, isOpenAIUserTurn(json.RawMessage(`{"role":"assistant","content":"hi"}`)))
	assert.False(t, isOpenAIUserTurn(json.RawMessage(`{"role":"tool","content":"hi"}`)))
	assert.False(t, isOpenAIUserTurn(json.RawMessage(`not json`)))
}

func TestIsAnthropicAssistant(t *testing.T) {
	assert.True(t, isAnthropicAssistant(json.RawMessage(`{"role":"assistant"}`)))
	assert.False(t, isAnthropicAssistant(json.RawMessage(`{"role":"user"}`)))
	assert.False(t, isAnthropicAssistant(json.RawMessage(`not json`)))
}

func TestIsResponsesStepStart(t *testing.T) {
	assert.True(t, isResponsesStepStart(json.RawMessage(`{"type":"message","role":"assistant"}`)))
	assert.True(t, isResponsesStepStart(json.RawMessage(`{"type":"custom_tool_call","call_id":"a"}`)))
	assert.True(t, isResponsesStepStart(json.RawMessage(`{"type":"function_call","call_id":"a"}`)))
	assert.False(t, isResponsesStepStart(json.RawMessage(`{"type":"message","role":"user"}`)))
	assert.False(t, isResponsesStepStart(json.RawMessage(`{"type":"custom_tool_call_output","call_id":"a"}`)))
	assert.False(t, isResponsesStepStart(json.RawMessage(`not json`)))
}

// ============================================================
// extractToolCallID / extractTextFromContent
// ============================================================

func TestExtractToolCallID(t *testing.T) {
	toolResult := []interface{}{
		map[string]interface{}{"type": "tool_result", "tool_use_id": "tool-123", "content": "output"},
	}
	assert.Equal(t, "tool-123", extractToolCallID(toolResult))

	withText := []interface{}{
		map[string]interface{}{"type": "text", "text": "a human wrote this"},
		map[string]interface{}{"type": "tool_result", "tool_use_id": "tool-456", "content": "output"},
	}
	assert.Empty(t, extractToolCallID(withText), "presence of a text block means this is a human message, not a pure tool response")

	assert.Empty(t, extractToolCallID("plain string content"))
	assert.Empty(t, extractToolCallID(nil))
}

func TestExtractTextFromContent(t *testing.T) {
	assert.Equal(t, "plain text", extractTextFromContent("plain text"))
	assert.Equal(t, "", extractTextFromContent(nil))

	blocks := []interface{}{
		map[string]interface{}{"type": "text", "text": "hello"},
	}
	assert.Equal(t, "hello", extractTextFromContent(blocks))

	toolUse := []interface{}{
		map[string]interface{}{
			"type": "tool_use", "name": "run_command",
			"input": map[string]interface{}{"command": "ls -la"},
		},
	}
	got := extractTextFromContent(toolUse)
	assert.Contains(t, got, "[TOOL_CALL run_command")
	assert.Contains(t, got, "ls -la")

	toolUseFile := []interface{}{
		map[string]interface{}{
			"type": "tool_use", "name": "read_file",
			"input": map[string]interface{}{"file_path": "/tmp/x.go"},
		},
	}
	assert.Contains(t, extractTextFromContent(toolUseFile), "file=/tmp/x.go")

	toolUseWrite := []interface{}{
		map[string]interface{}{
			"type": "tool_use", "name": "write_file",
			"input": map[string]interface{}{"file_path": "/tmp/y.go", "content": "package main"},
		},
	}
	assert.Contains(t, extractTextFromContent(toolUseWrite), "wrote package main")

	toolResultStr := []interface{}{
		map[string]interface{}{"type": "tool_result", "content": "some \x1b[31moutput\x1b[0m", "is_error": false},
	}
	got = extractTextFromContent(toolResultStr)
	assert.Contains(t, got, "[TOOL_OUTPUT]")
	assert.Contains(t, got, "some output")

	toolResultErr := []interface{}{
		map[string]interface{}{"type": "tool_result", "content": "boom", "is_error": true},
	}
	assert.Contains(t, extractTextFromContent(toolResultErr), "[TOOL_ERROR]")

	toolResultBlocks := []interface{}{
		map[string]interface{}{
			"type": "tool_result",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "nested output"},
			},
		},
	}
	assert.Contains(t, extractTextFromContent(toolResultBlocks), "nested output")
}

// ============================================================
// parseAnthropicMessages / parseResponsesMessages
// ============================================================

func TestParseAnthropicMessages(t *testing.T) {
	body := []byte(`{
		"system": "You are helpful.",
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello!"}
		]
	}`)
	msgs := parseAnthropicMessages(body)
	require.Len(t, msgs, 3)
	assert.Equal(t, "system", msgs[0].Role)
	assert.Equal(t, "hi", msgs[1].Content)
}

func TestParseAnthropicMessages_ToolResultMarksToolCallID(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"abc","content":"done"}]}
		]
	}`)
	msgs := parseAnthropicMessages(body)
	require.Len(t, msgs, 1)
	assert.Equal(t, "abc", msgs[0].ToolCallID)
}

func TestParseAnthropicMessages_InvalidJSON(t *testing.T) {
	assert.Nil(t, parseAnthropicMessages([]byte("not json")))
}

func TestParseResponsesMessages(t *testing.T) {
	body := []byte(`{
		"instructions": "You are helpful.",
		"input": [
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","call_id":"c1","name":"search","arguments":"{\"q\":\"x\"}"},
			{"type":"function_call_output","call_id":"c1","output":"result text"}
		]
	}`)
	msgs := parseResponsesMessages(body)
	require.Len(t, msgs, 4)
	assert.Equal(t, "system", msgs[0].Role)
	assert.Contains(t, msgs[2].Content, "[TOOL_CALL search]")
	assert.Equal(t, "tool", msgs[3].Role)
	assert.Contains(t, msgs[3].Content, "result text")
	assert.Equal(t, "c1", msgs[3].ToolCallID)
}

func TestParseResponsesMessages_InvalidJSON(t *testing.T) {
	assert.Nil(t, parseResponsesMessages([]byte("not json")))
}

func TestParseResponsesMessages_NoInstructions(t *testing.T) {
	body := []byte(`{"input": [{"type":"message","role":"user","content":"hi"}]}`)
	msgs := parseResponsesMessages(body)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].Role)
}

func TestExtractTextFromResponsesContent(t *testing.T) {
	assert.Equal(t, "", extractTextFromResponsesContent(nil))
	assert.Equal(t, "plain", extractTextFromResponsesContent(json.RawMessage(`"plain"`)))

	parts := json.RawMessage(`[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]`)
	assert.Equal(t, "a\nb", extractTextFromResponsesContent(parts))

	assert.Equal(t, "", extractTextFromResponsesContent(json.RawMessage(`123`)))
}

// ============================================================
// rewriteOpenAIBodyWithWindow / rewriteAnthropicBodyWithWindow / rewriteResponsesBodyWithWindow
// ============================================================

func TestRewriteOpenAIBodyWithWindow_CompressesWhenEnoughTurns(t *testing.T) {
	original := requestBodyFor("gpt-4o", []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "turn1"},
		{Role: "assistant", Content: "reply1"},
		{Role: "user", Content: "turn2"},
		{Role: "assistant", Content: "reply2"},
		{Role: "user", Content: "turn3"},
	})
	result := &Result{StateJSON: `{"intent":"x"}`}
	out, err := rewriteOpenAIBodyWithWindow(original, result, 1)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out, &body))
	msgs := body["messages"].([]any)
	assert.Less(t, len(msgs), 6)

	first := msgs[0].(map[string]any)
	assert.Equal(t, "system", first["role"])
}

func TestRewriteOpenAIBodyWithWindow_NotEnoughTurns_ReturnsOriginal(t *testing.T) {
	original := requestBodyFor("gpt-4o", []Message{
		{Role: "user", Content: "only turn"},
	})
	result := &Result{StateJSON: `{}`}
	out, err := rewriteOpenAIBodyWithWindow(original, result, 4)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(out))
}

func TestRewriteOpenAIBodyWithWindow_DefaultWindow(t *testing.T) {
	msgs := []Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, Message{Role: "user", Content: "u"}, Message{Role: "assistant", Content: "a"})
	}
	original := requestBodyFor("gpt-4o", msgs)
	result := &Result{StateJSON: `{}`}
	out, err := rewriteOpenAIBodyWithWindow(original, result, 0)
	require.NoError(t, err)
	assert.NotEqual(t, string(original), string(out))
}

func TestRewriteOpenAIBodyWithWindow_InvalidJSON(t *testing.T) {
	_, err := rewriteOpenAIBodyWithWindow([]byte("not json"), &Result{}, 4)
	require.Error(t, err)
}

func TestRewriteOpenAIBodyWithWindow_MissingMessages(t *testing.T) {
	_, err := rewriteOpenAIBodyWithWindow([]byte(`{"model":"x"}`), &Result{}, 4)
	require.Error(t, err)
}

func TestRewriteAnthropicBodyWithWindow_CompressesWhenEnoughSteps(t *testing.T) {
	msgs := []Message{}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, Message{Role: "user", Content: "u"}, Message{Role: "assistant", Content: "a"})
	}
	original := requestBodyFor("claude-sonnet-5", msgs)
	result := &Result{StateJSON: `{"intent":"y"}`}
	out, err := rewriteAnthropicBodyWithWindow(original, result, 1)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out, &body))
	rewrittenMsgs := body["messages"].([]any)
	assert.Less(t, len(rewrittenMsgs), len(msgs))
}

func TestRewriteAnthropicBodyWithWindow_EmptyMessages_ReturnsOriginal(t *testing.T) {
	original := requestBodyFor("claude-sonnet-5", []Message{})
	out, err := rewriteAnthropicBodyWithWindow(original, &Result{}, 4)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(out))
}

func TestRewriteAnthropicBodyWithWindow_SingleMessage_ReturnsOriginal(t *testing.T) {
	original := requestBodyFor("claude-sonnet-5", []Message{{Role: "user", Content: "only"}})
	out, err := rewriteAnthropicBodyWithWindow(original, &Result{}, 4)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(out))
}

func TestRewriteAnthropicBodyWithWindow_NotEnoughSteps_ReturnsOriginal(t *testing.T) {
	original := requestBodyFor("claude-sonnet-5", []Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
	})
	out, err := rewriteAnthropicBodyWithWindow(original, &Result{}, 4)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(out))
}

func TestRewriteAnthropicBodyWithWindow_InvalidJSON(t *testing.T) {
	_, err := rewriteAnthropicBodyWithWindow([]byte("not json"), &Result{}, 4)
	require.Error(t, err)
}

func TestRewriteAnthropicBodyWithWindow_MissingMessages(t *testing.T) {
	_, err := rewriteAnthropicBodyWithWindow([]byte(`{"model":"x"}`), &Result{}, 4)
	require.Error(t, err)
}

func TestRewriteResponsesBodyWithWindow_CompressesWhenEnoughTurns(t *testing.T) {
	items := []map[string]any{}
	for i := 0; i < 6; i++ {
		items = append(items, map[string]any{"type": "message", "role": "user", "content": "u"})
		items = append(items, map[string]any{"type": "message", "role": "assistant", "content": "a"})
	}
	raw := map[string]any{"model": "gpt-5", "input": items}
	original, _ := json.Marshal(raw)

	result := &Result{StateJSON: `{"intent":"z"}`}
	out, err := rewriteResponsesBodyWithWindow(original, result, 1)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(out, &body))
	rewrittenItems := body["input"].([]any)
	assert.Less(t, len(rewrittenItems), len(items))
}

func TestRewriteResponsesBodyWithWindow_EmptyItems_ReturnsOriginal(t *testing.T) {
	raw := map[string]any{"model": "gpt-5", "input": []map[string]any{}}
	original, _ := json.Marshal(raw)
	out, err := rewriteResponsesBodyWithWindow(original, &Result{}, 4)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(out))
}

func TestRewriteResponsesBodyWithWindow_NotEnoughTurns_ReturnsOriginal(t *testing.T) {
	raw := map[string]any{
		"model": "gpt-5",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "only"},
		},
	}
	original, _ := json.Marshal(raw)
	out, err := rewriteResponsesBodyWithWindow(original, &Result{}, 4)
	require.NoError(t, err)
	assert.JSONEq(t, string(original), string(out))
}

func TestRewriteResponsesBodyWithWindow_DefaultWindow(t *testing.T) {
	// Steps, not user messages: the window counts what the assistant did.
	items := []map[string]any{}
	for i := 0; i < 6; i++ {
		items = append(items,
			map[string]any{"type": "message", "role": "user", "content": "u"},
			map[string]any{"type": "message", "role": "assistant", "content": "a"},
		)
	}
	raw := map[string]any{"model": "gpt-5", "input": items}
	original, _ := json.Marshal(raw)
	out, err := rewriteResponsesBodyWithWindow(original, &Result{StateJSON: `{}`}, 0)
	require.NoError(t, err)
	assert.NotEqual(t, string(original), string(out))
}

func TestRewriteResponsesBodyWithWindow_InvalidJSON(t *testing.T) {
	_, err := rewriteResponsesBodyWithWindow([]byte("not json"), &Result{}, 4)
	require.Error(t, err)
}

func TestRewriteResponsesBodyWithWindow_MissingInput(t *testing.T) {
	_, err := rewriteResponsesBodyWithWindow([]byte(`{"model":"x"}`), &Result{}, 4)
	require.Error(t, err)
}

// newGuardedStateHiveState is newStateHiveState with the prefix-cache guard
// left on. On this deliberately tiny conversation the guard refuses the
// rewrite, which is exactly the path under test.
func newGuardedStateHiveState(routeCfg config.HiveRouteConfig) *HiveState {
	hs := newStateHiveState(routeCfg)
	hs.cfg.PrefixCacheGuard = config.PrefixCacheGuardConfig{}
	hs.cfg.PrefixCacheGuard.ApplyDefaults()
	return hs
}

// The guard declines to *rewrite the body*; which model should serve the
// request is a separate decision. Synthesizing a NO_OP result without calling
// Process left State nil and returned before the routing block, so a guard skip
// silently disabled HiveRoute — for most traffic, with the guard on by default.
func TestMiddleware_GuardSkip_StillRoutes(t *testing.T) {
	db := openMiddlewareTestStore(t)
	routeCfg := config.HiveRouteConfig{
		Enabled: true,
		Levels: []config.HiveRouteLevel{
			{Name: "complex", Model: "gpt-5-complex", Description: "hard tasks"},
		},
	}
	hs := newGuardedStateHiveState(routeCfg)
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug","difficulty":"complex","reasoning_effort":"high"}`,
		&State{Intent: "debug", Difficulty: "complex", ReasoningEffort: "high"})

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	require.Equal(t, string(ModeNoOp), rec.Header().Get("x-hivestate-mode"),
		"fixture must take the guard-skip path for this test to mean anything")

	assert.Equal(t, "complex", rec.Header().Get("x-hiveroute-level"))
	assert.Equal(t, "gpt-5-complex", rec.Header().Get("x-hiveroute-model"))

	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(next.body, &forwarded))
	assert.Equal(t, "gpt-5-complex", forwarded["model"], "the route must reach the provider")

	// The guard's whole point: the conversation itself is untouched.
	fwdMsgs, _ := json.Marshal(forwarded["messages"])
	var orig map[string]any
	require.NoError(t, json.Unmarshal(body, &orig))
	origMsgs, _ := json.Marshal(orig["messages"])
	assert.JSONEq(t, string(origMsgs), string(fwdMsgs), "the guard skipped, so the history must be unrewritten")

	// The metric is written asynchronously; wait for it so the store is idle
	// before the temp dir is torn down.
	waitForMetric(t, db, 1)
}

// Routing must stay off when it is not configured, even though the guard-skip
// path now reaches the routing block.
func TestMiddleware_GuardSkip_NoRouteConfigLeavesBodyAlone(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newGuardedStateHiveState(config.HiveRouteConfig{})
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug","difficulty":"complex"}`,
		&State{Intent: "debug", Difficulty: "complex"})

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	body := requestBodyFor("gpt-4o", msgs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	assert.Empty(t, rec.Header().Get("x-hiveroute-model"))
	assert.JSONEq(t, string(body), string(next.body))
	waitForMetric(t, db, 1)
}

// fakeOptRecorder captures the optimization counters the middleware emits.
type fakeOptRecorder struct {
	guard       []string
	injections  int
	budgetSkips int
}

func (f *fakeOptRecorder) RecordGuardDecision(reason, api string) {
	f.guard = append(f.guard, reason+":"+api)
}
func (f *fakeOptRecorder) RecordCCRInjection(files, tokens int) { f.injections++ }
func (f *fakeOptRecorder) RecordCCRBudgetSkip()                 { f.budgetSkips++ }

// The budget decision is taken inside Process, which has no recorder, so
// ubiquum_ccr_budget_skips_total sat at zero permanently while being
// documented as a real series. Process now reports it on the Result and the
// middleware counts it.
func TestMiddleware_CCRBudgetSkipIsRecorded(t *testing.T) {
	db := openMiddlewareTestStore(t)
	hs := newStateHiveState(config.HiveRouteConfig{})
	msgs := longConversation()
	seedStateCache(t, hs, msgs, `{"intent":"debug"}`, &State{Intent: "debug"})

	// A recovered file far larger than this tiny conversation's CCR ceiling,
	// filed under exactly the scope the middleware will derive.
	scope := Scope{TeamID: "team-1", KeyHash: "key-1", SessionID: "sess-budget"}
	hs.ccr.Store(scope, []Message{
		{Role: "assistant", Content: "Read(/src/huge.go)"},
		{Role: "tool", Content: strings.Repeat("recovered source line\n", 200)},
	}, hs.counter)

	rec := &fakeOptRecorder{}
	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, rec, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(requestBodyFor("gpt-4o", msgs)))
	req.Header.Set(SessionHeader, "sess-budget")
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyHash: "key-1"}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.True(t, next.called)
	assert.Equal(t, 0, rec.injections, "the working set is far over budget; nothing should be injected")
	assert.Equal(t, 1, rec.budgetSkips, "the declined retrieval must be counted")
	waitForMetric(t, db, 1)
}

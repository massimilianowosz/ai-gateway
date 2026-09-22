package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// --- Mock store ---

type mockStore struct {
	mu               sync.Mutex
	feedbackRecords  []*store.FeedbackRecord
	firedToday       []string
	firedTodayCount  int
	sessionSignals   *store.FeedbackSessionSignals
	keyRouteSettings *store.KeyRouteSettings
	tenantSettings   *store.TenantSettings
	keyFeedbackSet   bool
	keyFeedbackValue bool
}

// StoreFeedback models the real store's upsert: one row per record id.
func (m *mockStore) StoreFeedback(_ context.Context, r *store.FeedbackRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == "" {
		r.ID = fmt.Sprintf("fb-%d", len(m.feedbackRecords)+1)
	}
	for i, existing := range m.feedbackRecords {
		if existing.ID == r.ID {
			m.feedbackRecords[i] = r
			return nil
		}
	}
	m.feedbackRecords = append(m.feedbackRecords, r)
	return nil
}

// answeredCount reports how many stored records carry an answer.
func (m *mockStore) answeredCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.feedbackRecords {
		if r.AnswerNormalized != "" {
			n++
		}
	}
	return n
}

func (m *mockStore) ListFeedback(_ context.Context, _ string, _ int) ([]store.FeedbackRecord, error) {
	return nil, nil
}
func (m *mockStore) GetFeedbackFiredToday(_ context.Context, _ string) ([]string, int, error) {
	return m.firedToday, m.firedTodayCount, nil
}
func (m *mockStore) SetKeyFeedbackEnabled(_ context.Context, _ string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyFeedbackSet = true
	m.keyFeedbackValue = enabled
	return nil
}
func (m *mockStore) GetFeedbackSessionSignals(_ context.Context, _ string) (*store.FeedbackSessionSignals, error) {
	return m.sessionSignals, nil
}
func (m *mockStore) GetKeyRouteSettings(_ context.Context, _ string) (*store.KeyRouteSettings, error) {
	return m.keyRouteSettings, nil
}
func (m *mockStore) LogKeyEvent(_ context.Context, _ store.KeyEvent) error { return nil }

func (m *mockStore) GetTenantSettings(_ context.Context, _ string) (*store.TenantSettings, error) {
	return m.tenantSettings, nil
}

// Unused store methods — stub implementations
func (m *mockStore) CreateKey(_ context.Context, _ *store.APIKey) error              { return nil }
func (m *mockStore) GetKeyByHash(_ context.Context, _ string) (*store.APIKey, error) { return nil, nil }
func (m *mockStore) ListKeys(_ context.Context, _ store.KeyFilter) ([]store.APIKey, error) {
	return nil, nil
}
func (m *mockStore) UpdateKey(_ context.Context, _ string, _ store.UpdateKeyParams) error {
	return nil
}
func (m *mockStore) DeleteKey(_ context.Context, _ string) error              { return nil }
func (m *mockStore) CreateTeam(_ context.Context, _ *store.Team) error        { return nil }
func (m *mockStore) GetTeam(_ context.Context, _ string) (*store.Team, error) { return nil, nil }
func (m *mockStore) ListTeams(_ context.Context, _ store.TeamFilter) ([]store.Team, error) {
	return nil, nil
}
func (m *mockStore) UpdateTeam(_ context.Context, _ string, _ store.UpdateTeamParams) error {
	return nil
}
func (m *mockStore) DeleteTeam(_ context.Context, _ string) error                    { return nil }
func (m *mockStore) CreateUser(_ context.Context, _ *store.User) error               { return nil }
func (m *mockStore) GetUser(_ context.Context, _ string) (*store.User, error)        { return nil, nil }
func (m *mockStore) GetUserByEmail(_ context.Context, _ string) (*store.User, error) { return nil, nil }
func (m *mockStore) ListUsers(_ context.Context, _ store.UserFilter) ([]store.User, error) {
	return nil, nil
}
func (m *mockStore) UpdateUser(_ context.Context, _ string, _ store.UpdateUserParams) error {
	return nil
}
func (m *mockStore) DeleteUser(_ context.Context, _ string) error          { return nil }
func (m *mockStore) LogSpend(_ context.Context, _ store.SpendRecord) error { return nil }
func (m *mockStore) GetSpend(_ context.Context, _ store.SpendFilter) ([]store.SpendRecord, error) {
	return nil, nil
}
func (m *mockStore) GetSpendSummary(_ context.Context, _ store.SpendFilter) ([]store.SpendSummary, error) {
	return nil, nil
}
func (m *mockStore) GetTotalSpend(_ context.Context, _ string) (float64, error)  { return 0, nil }
func (m *mockStore) PurgeExpiredSpendRecords(_ context.Context) (int64, error)   { return 0, nil }
func (m *mockStore) LogCacheMetric(_ context.Context, _ store.CacheMetric) error { return nil }
func (m *mockStore) ListCacheMetrics(_ context.Context, _ int) ([]store.CacheMetric, error) {
	return nil, nil
}
func (m *mockStore) DeleteCacheMetrics(_ context.Context) error { return nil }
func (m *mockStore) LogHiveStateMetric(_ context.Context, _ store.HiveStateMetric) error {
	return nil
}
func (m *mockStore) ListHiveStateMetrics(_ context.Context, _ int) ([]store.HiveStateMetric, error) {
	return nil, nil
}
func (m *mockStore) UpsertTenantSettings(_ context.Context, _ *store.TenantSettings) error {
	return nil
}
func (m *mockStore) UpsertKeyRouteSettings(_ context.Context, _ *store.KeyRouteSettings) error {
	return nil
}
func (m *mockStore) DeleteKeyRouteSettings(_ context.Context, _ string) error { return nil }
func (m *mockStore) Migrate(_ context.Context) error                          { return nil }
func (m *mockStore) Close() error                                             { return nil }

// --- Helpers ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testKeyInfo() *store.APIKey {
	return &store.APIKey{
		ID:        "key-123",
		KeyHash:   "hash123",
		KeyPrefix: "sk-ubq-abcd",
		TeamID:    "team-1",
	}
}

func ctxWithKeyInfo(keyInfo *store.APIKey) context.Context {
	return auth.ContextWithKeyInfo(context.Background(), keyInfo)
}

func newTestFeedback(t *testing.T, ms *mockStore) *Feedback {
	t.Helper()
	cfg := config.FeedbackConfig{Enabled: true}
	f := NewFeedback(cfg, ms, testLogger())
	require.NotNil(t, f)
	return f
}

func nopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// --- Tests ---

func TestNewFeedback_Disabled_ReturnsNil(t *testing.T) {
	f := NewFeedback(config.FeedbackConfig{Enabled: false}, nil, testLogger())
	assert.Nil(t, f)
}

func TestFeedback_Middleware_Nil_PassesThrough(t *testing.T) {
	var f *Feedback
	handler := f.Middleware(nopHandler())
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFeedback_NoKeyInfo_PassesThrough(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	handler := f.Middleware(nopHandler())

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, ms.feedbackRecords)
}

func TestFeedback_KeyDisabled_PassesThrough(t *testing.T) {
	disabled := false
	ms := &mockStore{
		keyRouteSettings: &store.KeyRouteSettings{FeedbackEnabled: &disabled},
	}
	f := newTestFeedback(t, ms)
	handler := f.Middleware(nopHandler())

	keyInfo := testKeyInfo()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(ctxWithKeyInfo(keyInfo))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFeedback_TeamNotInScope_PassesThrough(t *testing.T) {
	ms := &mockStore{}
	cfg := config.FeedbackConfig{Enabled: true, Teams: []string{"team-other"}}
	f := NewFeedback(cfg, ms, testLogger())
	require.NotNil(t, f)

	handler := f.Middleware(nopHandler())
	keyInfo := testKeyInfo()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(ctxWithKeyInfo(keyInfo))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Tool call response format tests ---

// The feedback prompt rides along with the model's own answer; it never
// replaces it. These assert the appended shape on each surface.

// assistantHandler stands in for a provider returning an ordinary answer.
func assistantHandler(body string, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

// fireTrigger drives enough requests to trip long_session and returns the
// recorder for the request that carries the prompt.
func fireTrigger(t *testing.T, f *Feedback, next http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := f.Middleware(next)
	keyInfo := testKeyInfo()
	for i := 0; i < 49; i++ {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(ctxWithKeyInfo(keyInfo))
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctxWithKeyInfo(keyInfo))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestFeedback_OpenAI_JSON_QuestionIsAppendedToTheAnswer(t *testing.T) {
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"La risposta vera."},"finish_reason":"stop"}]}`
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "application/json"),
		"/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	choices, ok := resp["choices"].([]any)
	require.True(t, ok, "the model's own response shape must survive")
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	content, _ := msg["content"].(string)

	assert.Contains(t, content, "La risposta vera.", "the model's answer must not be lost")
	assert.Contains(t, content, "📊", "the question must be appended")
	assert.Equal(t, "stop", choices[0].(map[string]any)["finish_reason"],
		"appending must not change how the turn ended")
}

func TestFeedback_Anthropic_JSON_QuestionIsAppendedToTheAnswer(t *testing.T) {
	upstream := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"La risposta vera."}],"stop_reason":"end_turn"}`
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "application/json"),
		"/v1/messages", `{"model":"claude-3","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Content, 2, "the question is a block of its own")
	assert.Equal(t, "La risposta vera.", resp.Content[0].Text)
	assert.Contains(t, resp.Content[1].Text, "📊")
	assert.Equal(t, "end_turn", resp.StopReason)
}

// A turn that ends in a tool call belongs to the agent's loop: the user never
// sees text there, so the question must not be spent on it.
func TestFeedback_ToolCallTurnIsLeftAlone(t *testing.T) {
	upstream := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}],"stop_reason":"tool_use"}`
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "application/json"),
		"/v1/messages", `{"model":"claude-3","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`)

	assert.JSONEq(t, upstream, rec.Body.String(),
		"a tool-call turn must reach the client byte-for-byte")
}

func TestFeedback_OpenAI_Streaming_QuestionPrecedesDone(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"ciao\"}}]}\n\ndata: [DONE]\n\n"
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "text/event-stream"),
		"/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	out := rec.Body.String()
	assert.Contains(t, out, "ciao", "the model's own deltas must survive")
	require.Contains(t, out, "📊", "the question must be appended")
	assert.Less(t, strings.Index(out, "📊"), strings.Index(out, "[DONE]"),
		"the question must go out before the stream closes")
}

func TestFeedback_Anthropic_Streaming_QuestionPrecedesMessageStop(t *testing.T) {
	upstream := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ciao\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "text/event-stream"),
		"/v1/messages", `{"model":"claude-3","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"stream":true}`)

	out := rec.Body.String()
	assert.Contains(t, out, "ciao")
	require.Contains(t, out, "📊")
	assert.Less(t, strings.Index(out, "📊"), strings.Index(out, "message_stop"),
		"the question must go out before the stream closes")
}

// The answer arrives as an ordinary next message, which is what captureAnswer
// reads.
func TestFeedback_AnswerOnTheNextTurnIsCaptured(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	next := assistantHandler(upstream, "application/json")
	fireTrigger(t, f, next, "/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)

	handler := f.Middleware(next)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"A"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctxWithKeyInfo(testKeyInfo()))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Eventually(t, func() bool {
		return ms.answeredCount() > 0
	}, time.Second, 10*time.Millisecond, "the answer was never recorded")
}

func TestFeedback_RecordSignals_Compression(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	// Simulate response headers from HiveState
	h := http.Header{}
	h.Set("X-Hivestate-Ratio", "0.45")
	h.Set("X-Hivestate-Original-Tokens", "10000")
	h.Set("X-Hivestate-Tokens", "5500")
	h.Set("X-Hivestate-Status", "completed")
	h.Set("X-Hiveroute-Savings", "0.15")
	h.Set("X-Hiveroute-Level", "standard")

	rw := &feedbackAppender{header: h}
	f.recordSignals(sess, rw)

	sess.mu.Lock()
	defer sess.mu.Unlock()

	assert.Len(t, sess.recentRatios, 1)
	assert.InDelta(t, 0.45, sess.recentRatios[0], 0.01)
	assert.Equal(t, 4500, sess.savedTokens)
	assert.True(t, sess.taskCompleted)
	assert.InDelta(t, 0.15, sess.totalSavings, 0.01)
	assert.Contains(t, sess.recentDifficulty, "standard")
}

// --- Trigger evaluation tests ---

func TestFeedback_Trigger_Compression(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	// Simulate 5 compression ratios > 30%
	sess.mu.Lock()
	sess.recentRatios = []float64{0.35, 0.40, 0.38, 0.42, 0.36}
	sess.mu.Unlock()

	prompt := f.checkTriggers(context.Background(), sess, keyInfo, 10)
	require.NotNil(t, prompt)
	assert.Equal(t, TriggerHiveStateCompression, prompt.Trigger)
	assert.Contains(t, prompt.Question, "HiveState compressed")
	assert.Contains(t, prompt.Question, "38%") // avg of last 5
	assert.Len(t, prompt.Options, 4)
}

func TestFeedback_Trigger_TaskCompleted_WithOptOut(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	// Simulate task completion
	sess.mu.Lock()
	sess.taskCompleted = true
	sess.mu.Unlock()

	prompt := f.checkTriggers(context.Background(), sess, keyInfo, 5)
	require.NotNil(t, prompt)
	assert.Equal(t, TriggerTaskCompleted, prompt.Trigger)
	assert.Len(t, prompt.Options, 5, "task_completed should have 5 options including opt-out")
	assert.Equal(t, "E", prompt.Options[4].ID)
	assert.Equal(t, "Don't ask again", prompt.Options[4].Label)
}

func TestFeedback_Trigger_SavingsMilestone(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	sess.mu.Lock()
	sess.totalSavings = 5.50
	sess.mu.Unlock()

	prompt := f.checkTriggers(context.Background(), sess, keyInfo, 10)
	require.NotNil(t, prompt)
	assert.Equal(t, TriggerSavingsMilestone, prompt.Trigger)
	assert.Contains(t, prompt.Question, "$5.50")
}

func TestFeedback_Trigger_LongSession(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	prompt := f.checkTriggers(context.Background(), sess, keyInfo, 50)
	require.NotNil(t, prompt)
	assert.Equal(t, TriggerLongSession, prompt.Trigger)
	assert.Contains(t, prompt.Question, "50 interactions")
}

func TestFeedback_Trigger_AntiSpam_DailyLimit(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	// Set promptsToday to max
	sess.mu.Lock()
	sess.promptsToday = 3 // default MaxPerDay
	sess.promptDate = time.Now().Format("2006-01-02")
	sess.mu.Unlock()

	prompt := f.checkTriggers(context.Background(), sess, keyInfo, 50)
	assert.Nil(t, prompt, "should not trigger when daily limit reached")
}

func TestFeedback_Trigger_AntiSpam_Interval(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	// Set lastPromptAt to recent time
	sess.mu.Lock()
	sess.lastPromptAt = time.Now().Add(-5 * time.Minute) // within 20 min default interval
	sess.mu.Unlock()

	prompt := f.checkTriggers(context.Background(), sess, keyInfo, 50)
	assert.Nil(t, prompt, "should not trigger within min interval")
}

// --- extractToolResult tests ---

func TestExtractToolResult_Anthropic(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_feedback_123","content":"B"}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))

	result := extractToolResult(req)
	assert.Equal(t, "B", result)
}

func TestExtractToolResult_OpenAI(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_feedback_123","function":{"name":"ubiquum_feedback","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_feedback_123","content":"A"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))

	result := extractToolResult(req)
	assert.Equal(t, "A", result)
}

func TestExtractToolResult_NoToolResult(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))

	result := extractToolResult(req)
	assert.Equal(t, "", result)
}

// --- OptionsShown stored in record ---

func TestFeedback_OptionsShown_Stored(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()

	// The record is only created once the question actually reached the user,
	// so the upstream has to return something the question can ride on.
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	fireTrigger(t, f, assistantHandler(upstream, "application/json"),
		"/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)

	sess := f.getSession(context.Background(), keyInfo)
	sess.mu.Lock()
	record := sess.pendingRecord
	sess.mu.Unlock()

	require.NotNil(t, record, "should have a pending record")
	assert.NotEmpty(t, record.OptionsShown, "OptionsShown should be populated")

	// Parse the options JSON
	var options []feedbackOption
	require.NoError(t, json.Unmarshal([]byte(record.OptionsShown), &options))
	assert.Len(t, options, 4)
	assert.Equal(t, "A", options[0].ID)
	assert.Equal(t, "Smooth and fast, precise responses", options[0].Label)
}

// --- Anthropic streaming body parsing ---

func TestExtractToolResult_Anthropic_ContentString(t *testing.T) {
	// Tool result with string content
	body := `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_abc","content":"D"}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))

	result := extractToolResult(req)
	assert.Equal(t, "D", result)

	// Verify body was restored
	bodyBytes, _ := readAndRestoreBody(req)
	assert.True(t, len(bodyBytes) > 0, "body should be restored after reading")
}

func TestFeedback_ResponseContainsEnglishQuestions(t *testing.T) {
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	rec := fireTrigger(t, newTestFeedback(t, &mockStore{}),
		assistantHandler(upstream, "application/json"),
		"/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	content := msg["content"].(string)

	assert.Contains(t, content, "session is quite long")
	assert.Contains(t, content, "How is it going?")
	for _, italian := range []string{"nessun", "qualcosa", "risposte"} {
		assert.NotContains(t, content, italian)
	}
}

// --- Body restoration test ---

func TestReadAndRestoreBody(t *testing.T) {
	original := `{"model":"gpt-4","messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(original))

	data1, err := readAndRestoreBody(req)
	require.NoError(t, err)
	assert.Equal(t, original, string(data1))

	// Read again — body should be restored
	data2, err := readAndRestoreBody(req)
	require.NoError(t, err)
	assert.Equal(t, original, string(data2))
}

// --- Compression trigger with streaming ---

func TestFeedback_CompressionTrigger_AnthropicStreaming(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()

	// Build session with 5 high compression ratios
	sess := f.getSession(context.Background(), keyInfo)
	sess.mu.Lock()
	sess.recentRatios = []float64{0.35, 0.40, 0.38, 0.42, 0.36}
	sess.mu.Unlock()

	upstream := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ciao\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	handler := f.Middleware(assistantHandler(upstream, "text/event-stream"))

	body := `{"model":"claude-3","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req = req.WithContext(ctxWithKeyInfo(keyInfo))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	respBody := rec.Body.String()
	assert.Contains(t, respBody, "ciao", "the model's own answer must survive")
	assert.Contains(t, respBody, "event: content_block_start", "the question opens its own block")
	assert.Contains(t, respBody, "text_delta")
	assert.Less(t, strings.Index(respBody, "content_block_start"), strings.Index(respBody, "message_stop"),
		"the question must precede the terminal event")
}

// --- Verify task_completed is repeatable ---

func TestFeedback_TaskCompleted_Repeatable(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	keyInfo := testKeyInfo()
	sess := f.getSession(context.Background(), keyInfo)

	// First task completion
	sess.mu.Lock()
	sess.taskCompleted = true
	sess.mu.Unlock()

	prompt1 := f.checkTriggers(context.Background(), sess, keyInfo, 5)
	require.NotNil(t, prompt1)
	assert.Equal(t, TriggerTaskCompleted, prompt1.Trigger)

	// After firing, taskCompleted should be reset
	sess.mu.Lock()
	assert.False(t, sess.taskCompleted, "taskCompleted should be reset after firing")
	sess.mu.Unlock()

	// Set taskCompleted again — should fire again (not blocked by firedTriggers)
	sess.mu.Lock()
	sess.taskCompleted = true
	sess.mu.Unlock()

	prompt2 := f.checkTriggers(context.Background(), sess, keyInfo, 6)
	require.NotNil(t, prompt2, "task_completed should be repeatable")
	assert.Equal(t, TriggerTaskCompleted, prompt2.Trigger)
}

// --- Concurrent access test ---

func TestFeedback_ConcurrentRequests(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	handler := f.Middleware(nopHandler())
	keyInfo := testKeyInfo()

	// Launch concurrent requests
	var wg sync.WaitGroup
	const n = 20
	errors := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			req = req.WithContext(ctxWithKeyInfo(keyInfo))
			rec := httptest.NewRecorder()

			defer func() {
				if r := recover(); r != nil {
					errors <- fmt.Errorf("panic: %v", r)
				}
			}()

			handler.ServeHTTP(rec, req)
		}()
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent request error: %v", err)
	}
}

// A prompt that was never shown must not be spent: the trigger stays armed and
// no record is filed. Previously a stub row was written the moment the trigger
// fired, and captureAnswer wrote a second one later — so every completed cycle
// counted as two prompts and a key was silenced after roughly one question.
func TestFeedback_UnshownPromptIsNotSpent(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	// A tool-call turn: the user sees no text, so the question cannot ride on it.
	upstream := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}],"stop_reason":"tool_use"}`
	fireTrigger(t, f, assistantHandler(upstream, "application/json"),
		"/v1/messages", `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	ms.mu.Lock()
	records := len(ms.feedbackRecords)
	ms.mu.Unlock()
	assert.Zero(t, records, "nothing should be filed for a question the user never saw")

	sess := f.getSession(context.Background(), testKeyInfo())
	sess.mu.Lock()
	awaiting := sess.awaitingAnswer
	sess.mu.Unlock()
	assert.False(t, awaiting, "answer capture must not be armed for an unshown question")
}

// One row per prompt/answer cycle, not two.
func TestFeedback_OneRecordPerCycle(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	next := assistantHandler(upstream, "application/json")
	fireTrigger(t, f, next, "/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)

	handler := f.Middleware(next)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"A"}]}`))
	req = req.WithContext(ctxWithKeyInfo(testKeyInfo()))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Eventually(t, func() bool { return ms.answeredCount() > 0 }, time.Second, 10*time.Millisecond)
	ms.mu.Lock()
	total := len(ms.feedbackRecords)
	ms.mu.Unlock()
	assert.Equal(t, 1, total, "a completed cycle must leave exactly one record")
}

// Ignoring the question must not throw the pending record away: the answer is
// still accepted on a later turn.
func TestFeedback_IgnoredQuestionStaysPending(t *testing.T) {
	ms := &mockStore{}
	f := newTestFeedback(t, ms)
	upstream := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	next := assistantHandler(upstream, "application/json")
	fireTrigger(t, f, next, "/v1/chat/completions", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)

	handler := f.Middleware(next)
	// A normal, long request rather than an answer.
	long := strings.Repeat("scrivimi una funzione che ordina una lista ", 10)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"`+long+`"}]}`))
	req = req.WithContext(ctxWithKeyInfo(testKeyInfo()))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	sess := f.getSession(context.Background(), testKeyInfo())
	sess.mu.Lock()
	awaiting, pending := sess.awaitingAnswer, sess.pendingRecord
	sess.mu.Unlock()
	assert.True(t, awaiting, "the question is still unanswered")
	assert.NotNil(t, pending, "the record must survive an ignored question")
}

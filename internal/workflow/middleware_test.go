package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "workflow_test.db"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type capturingHandler struct {
	called bool
	body   []byte
}

func (c *capturingHandler) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.called = true
		c.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}
}

// backendStub spins up a fake backend that always returns the given
// executeResponse, and counts how many times it was called.
func backendStub(t *testing.T, resp executeResponse, status int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/workflows/execute", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func chatBody(model, userText string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": userText},
		},
	})
	return b
}

func newTestWorkflow(url string, failOpen bool, db store.Store) *Workflow {
	return New(config.WorkflowConfig{
		Enabled:  true,
		URL:      url,
		Timeout:  5 * time.Second,
		FailOpen: &failOpen,
	}, db, testLogger())
}

// ============================================================
// Disabled / passthrough behavior
// ============================================================

func TestMiddleware_NilWorkflow_Passthrough(t *testing.T) {
	mw := Middleware(nil)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req.Header.Set("x-ubiquum-workflow", "my-flow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
}

func TestMiddleware_NonMatchingPath_Passthrough(t *testing.T) {
	db := openTestStore(t)
	_, calls := backendStub(t, executeResponse{Decision: "allow", FinalPrompt: "x"}, http.StatusOK)
	wf := newTestWorkflow("http://unused", true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, 0, *calls)
}

func TestMiddleware_NoHeaderNoDefault_SkipsBackendCall(t *testing.T) {
	db := openTestStore(t)
	srv, calls := backendStub(t, executeResponse{Decision: "allow", FinalPrompt: "x"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hello")))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, 0, *calls)
}

func TestMiddleware_OptOutHeader_SkipsBackendCall(t *testing.T) {
	db := openTestStore(t)
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-1"}))
	defaultID := "some-workflow-id"
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-1", DefaultWorkflowID: &defaultID}))

	srv, calls := backendStub(t, executeResponse{Decision: "allow", FinalPrompt: "x"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hello")))
	req.Header.Set("x-ubiquum-workflow", "false")
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, 0, *calls, "explicit opt-out must skip the backend call even with a default workflow configured")
}

func TestMiddleware_DefaultWorkflow_TriggersBackendCall(t *testing.T) {
	db := openTestStore(t)
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-1"}))
	defaultID := "some-workflow-id"
	require.NoError(t, db.UpsertTenantSettings(context.Background(), &store.TenantSettings{
		TeamID: "team-1", DefaultWorkflowID: &defaultID}))

	srv, calls := backendStub(t, executeResponse{Decision: "allow", FinalPrompt: "transformed"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hello")))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, 1, *calls)
	assert.Equal(t, "default", rec.Header().Get("x-ubiquum-workflow-id"))
}

// ============================================================
// Decisions
// ============================================================

func TestMiddleware_Allow_RewritesPromptAndModel(t *testing.T) {
	db := openTestStore(t)
	srv, calls := backendStub(t, executeResponse{Decision: "allow", FinalPrompt: "aopple pppie", RouteModel: "routed-model"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "apple pie")))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, 1, *calls)
	assert.True(t, next.called)
	assert.Equal(t, "true", rec.Header().Get("x-ubiquum-workflow-applied"))
	assert.Equal(t, "test-workflow", rec.Header().Get("x-ubiquum-workflow-id"))

	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(next.body, &forwarded))
	assert.Equal(t, "routed-model", forwarded["model"])
	msgs := forwarded["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	assert.Equal(t, "aopple pppie", last["content"])
}

func TestMiddleware_Block_ReturnsForbidden(t *testing.T) {
	db := openTestStore(t)
	srv, _ := backendStub(t, executeResponse{Decision: "block", BlockMessage: "not allowed"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "bad stuff")))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, next.called)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "not allowed", errObj["message"])
}

func TestMiddleware_Block_AnthropicShape(t *testing.T) {
	db := openTestStore(t)
	srv, _ := backendStub(t, executeResponse{Decision: "block", BlockMessage: "nope"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	body, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, next.called)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	var respBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &respBody))
	assert.Equal(t, "error", respBody["type"])
}

func TestMiddleware_Respond_NonStreaming(t *testing.T) {
	db := openTestStore(t)
	srv, _ := backendStub(t, executeResponse{Decision: "respond", RespondMessage: "here is your answer"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "classify this")))
	req.Header.Set("x-ubiquum-workflow", "test-notion")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, next.called)
	assert.Equal(t, http.StatusOK, rec.Code)
	var respBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &respBody))
	choices := respBody["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "here is your answer", msg["content"])
}

func TestMiddleware_Respond_Streaming(t *testing.T) {
	db := openTestStore(t)
	srv, _ := backendStub(t, executeResponse{Decision: "respond", RespondMessage: "streamed answer"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "classify this"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-ubiquum-workflow", "test-notion")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, next.called)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "streamed answer")
	assert.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestMiddleware_ErrorDecision_FailOpen(t *testing.T) {
	db := openTestStore(t)
	srv, _ := backendStub(t, executeResponse{Decision: "error"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req.Header.Set("x-ubiquum-workflow", "broken-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
}

func TestMiddleware_BackendUnreachable_FailOpen(t *testing.T) {
	db := openTestStore(t)
	wf := newTestWorkflow("http://127.0.0.1:1", true, db) // nothing listening
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
}

func TestMiddleware_BackendUnreachable_FailClosed(t *testing.T) {
	db := openTestStore(t)
	wf := newTestWorkflow("http://127.0.0.1:1", false, db) // nothing listening
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, next.called)
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestMiddleware_ForwardsUserAPIKey(t *testing.T) {
	db := openTestStore(t)
	var gotReq executeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(executeResponse{Decision: "allow", FinalPrompt: "hi"})
	}))
	t.Cleanup(srv.Close)

	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	req.Header.Set("Authorization", "Bearer sk-abc123")
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{TeamID: "team-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "sk-abc123", gotReq.UserAPIKey)
	assert.Equal(t, "team-1", gotReq.TenantID)
	assert.Equal(t, "test-workflow", gotReq.WorkflowID)
	assert.Equal(t, "hi", gotReq.Prompt)
}

func TestMiddleware_NoUserMessage_Passthrough(t *testing.T) {
	db := openTestStore(t)
	srv, calls := backendStub(t, executeResponse{Decision: "allow", FinalPrompt: "x"}, http.StatusOK)
	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "system", "content": "you are a bot"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
	assert.Equal(t, 0, *calls)
}

func TestNew_DisabledReturnsNil(t *testing.T) {
	db := openTestStore(t)
	wf := New(config.WorkflowConfig{Enabled: false, URL: "http://x"}, db, testLogger())
	assert.Nil(t, wf)

	wf = New(config.WorkflowConfig{Enabled: true, URL: ""}, db, testLogger())
	assert.Nil(t, wf)
}

func TestIsStreamingRequest(t *testing.T) {
	assert.True(t, isStreamingRequest([]byte(`{"stream":true}`)))
	assert.False(t, isStreamingRequest([]byte(`{"stream":false}`)))
	assert.False(t, isStreamingRequest([]byte(`{}`)))
}

func TestRawBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-xyz")
	assert.Equal(t, "sk-xyz", rawBearerToken(req))

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.Equal(t, "", rawBearerToken(req2))
}

// The Anthropic SDK and Claude Code send the key in X-Api-Key and never set
// Authorization, and /v1/messages is this feature's primary target — without
// the fallback user_api_key reached the backend empty on every such request.
func TestRawBearerToken_Sources(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer sk-abc"}, "sk-abc"},
		{"bearer mixed case", map[string]string{"Authorization": "bEaReR sk-abc"}, "sk-abc"},
		{"x-api-key only", map[string]string{"X-Api-Key": "sk-anthropic"}, "sk-anthropic"},
		{"bearer wins over x-api-key", map[string]string{
			"Authorization": "Bearer sk-abc", "X-Api-Key": "sk-other"}, "sk-abc"},
		{"bare token with no scheme", map[string]string{"Authorization": "sk-bare"}, "sk-bare"},
		{"other scheme is not a key", map[string]string{"Authorization": "Basic dXNlcjpwdw=="}, ""},
		{"other scheme falls back", map[string]string{
			"Authorization": "Basic dXNlcjpwdw==", "X-Api-Key": "sk-fallback"}, "sk-fallback"},
		{"nothing", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := rawBearerToken(r); got != tc.want {
				t.Errorf("rawBearerToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

// fail_open is documented as defaulting to true. As a plain bool it zero-valued
// to fail-closed, so an operator who set workflow.enabled and nothing else
// turned every backend blip into a 502.
func TestWorkflowConfig_FailOpenDefaultsToTrue(t *testing.T) {
	cfg := config.WorkflowConfig{Enabled: true, URL: "http://backend:8000"}
	cfg.ApplyDefaults()
	if !cfg.ShouldFailOpen() {
		t.Error("an omitted fail_open must mean fail-open, as documented")
	}

	explicit := false
	closed := config.WorkflowConfig{Enabled: true, URL: "http://backend:8000", FailOpen: &explicit}
	closed.ApplyDefaults()
	if closed.ShouldFailOpen() {
		t.Error("an explicit fail_open:false must be honoured")
	}
}

// failingStore answers every tenant-settings lookup with an error, standing in
// for a transient DB problem.
type failingStore struct {
	store.Store
}

func (failingStore) GetTenantSettings(context.Context, string) (*store.TenantSettings, error) {
	return nil, errors.New("boom: tenant settings unavailable")
}

// A store error is not "no workflow configured". Folding it into a false meant
// a transient DB blip forwarded the untransformed prompt to the model even
// under fail_open:false, which exists precisely to stop that.
func TestMiddleware_DefaultWorkflowLookupError_RespectsFailClosed(t *testing.T) {
	wf := newTestWorkflow("http://unused", false, failingStore{Store: openTestStore(t)})
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	// No x-ubiquum-workflow header, so the default-workflow lookup runs.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyHash: "key-1", Active: true}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.False(t, next.called, "the prompt reached the model despite fail_open:false")
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// The same error under fail_open lets the request through, as configured.
func TestMiddleware_DefaultWorkflowLookupError_FailsOpen(t *testing.T) {
	wf := newTestWorkflow("http://unused", true, failingStore{Store: openTestStore(t)})
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyHash: "key-1", Active: true}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called)
}

// A respond decision with nothing to say is a misconfigured workflow, not an
// answer: returning an empty assistant turn looks to the caller like the model
// had nothing to add.
func TestMiddleware_EmptyRespondMessageIsNotAnAnswer(t *testing.T) {
	db := openTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(executeResponse{Decision: "respond", RespondMessage: "  "})
	}))
	t.Cleanup(srv.Close)

	wf := newTestWorkflow(srv.URL, true, db)
	mw := Middleware(wf)
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody("gpt-4o", "hi")))
	req.Header.Set("x-ubiquum-workflow", "test-workflow")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.True(t, next.called, "fail_open should have passed the request to the model")
	assert.NotContains(t, rec.Body.String(), `"content":""`)
}

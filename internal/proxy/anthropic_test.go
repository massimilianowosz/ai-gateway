package proxy

import (
	"context"
	"encoding/json"
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
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
)

// --- Mock provider for Anthropic tests ---

type mockAnthropicProvider struct {
	completeResp *provider.CompletionResponse
	completeErr  error
	streamChunks [][]byte
	streamErr    error
	lastReq      *provider.CompletionRequest
}

func (m *mockAnthropicProvider) Name() string { return "mock" }

func (m *mockAnthropicProvider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.lastReq = req
	return m.completeResp, m.completeErr
}

func (m *mockAnthropicProvider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	m.lastReq = req
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return &mockAnthropicStreamReader{chunks: m.streamChunks}, nil
}

type mockAnthropicStreamReader struct {
	chunks [][]byte
	idx    int
}

func (r *mockAnthropicStreamReader) Next() ([]byte, error) {
	if r.idx >= len(r.chunks) {
		return nil, io.EOF
	}
	chunk := r.chunks[r.idx]
	r.idx++
	return chunk, nil
}
func (r *mockAnthropicStreamReader) Close() error         { return nil }
func (r *mockAnthropicStreamReader) Headers() http.Header { return http.Header{} }

// --- Tests ---

func TestAnthropicHandler_Complete(t *testing.T) {
	stop := "stop"
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Model:   "gpt-4o",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "Hello!"}, FinishReason: &stop}},
			Usage:   &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}

	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":1024}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp anthropicResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "message", resp.Type)
	assert.Equal(t, "assistant", resp.Role)
	assert.Equal(t, "claude-3", resp.Model)
	assert.Equal(t, "end_turn", resp.StopReason)
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "text", resp.Content[0].Type)
	assert.Equal(t, "Hello!", resp.Content[0].Text)
	assert.Equal(t, 10, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.OutputTokens)
}

func TestAnthropicHandler_Complete_ToolCalls(t *testing.T) {
	toolCalls := "tool_calls"
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID:     "chatcmpl-456",
			Object: "chat.completion",
			Model:  "gpt-4o",
			Choices: []provider.Choice{{
				Index: 0,
				Message: &provider.Message{
					Role:    "assistant",
					Content: "Let me check that.",
					ToolCalls: []provider.ToolCall{{
						ID:   "call_123",
						Type: "function",
						Function: provider.FunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Rome"}`,
						},
					}},
				},
				FinishReason: &toolCalls,
			}},
			Usage: &provider.Usage{PromptTokens: 20, CompletionTokens: 15, TotalTokens: 35},
		},
	}

	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"What's the weather in Rome?"}],"max_tokens":1024,"tools":[{"name":"get_weather","description":"Get weather","input_schema":{"type":"object"}}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp anthropicResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "tool_use", resp.StopReason)
	require.Len(t, resp.Content, 2)
	assert.Equal(t, "text", resp.Content[0].Type)
	assert.Equal(t, "Let me check that.", resp.Content[0].Text)
	assert.Equal(t, "tool_use", resp.Content[1].Type)
	assert.Equal(t, "call_123", resp.Content[1].ID)
	assert.Equal(t, "get_weather", resp.Content[1].Name)
}

func TestAnthropicHandler_Validation(t *testing.T) {
	registry := newTestRegistry("claude-3", &mockAnthropicProvider{})
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	tests := []struct {
		name   string
		body   string
		errMsg string
	}{
		{"missing model", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`, "model is required"},
		{"missing messages", `{"model":"claude-3","max_tokens":100}`, "messages is required"},
		{"missing max_tokens", `{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`, "max_tokens is required"},
		{"invalid json", `{bad`, "invalid JSON body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), tt.errMsg)
		})
	}
}

func TestAnthropicHandler_ModelNotFound(t *testing.T) {
	registry := newTestRegistry("claude-3", &mockAnthropicProvider{})
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"nonexistent","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var errResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &errResp)
	assert.Equal(t, "error", errResp["type"])
}

func TestAnthropicHandler_Stream(t *testing.T) {
	// Simulate OpenAI-format streaming chunks
	chunks := [][]byte{
		[]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`),
		[]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`),
		[]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`),
		[]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}

	mock := &mockAnthropicProvider{streamChunks: chunks}
	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100,"stream":true}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))

	// Parse SSE events
	output := w.Body.String()
	assert.Contains(t, output, "event: message_start")
	assert.Contains(t, output, "event: content_block_start")
	assert.Contains(t, output, "event: content_block_delta")
	assert.Contains(t, output, "event: content_block_stop")
	assert.Contains(t, output, "event: message_delta")
	assert.Contains(t, output, "event: message_stop")
	assert.Contains(t, output, `"text_delta"`)
	assert.Contains(t, output, "Hello")
	assert.Contains(t, output, " world")
}

func TestAnthropicHandler_SystemMessage(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID: "test", Object: "chat.completion", Model: "gpt-4o",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: ptrStr("stop")}},
		},
	}

	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	// Test string system
	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100,"system":"You are helpful."}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)
	require.True(t, len(mock.lastReq.Messages) >= 2)
	assert.Equal(t, "system", mock.lastReq.Messages[0].Role)
	assert.Equal(t, "You are helpful.", mock.lastReq.Messages[0].Content)
}

// The catalogue name is ours, chosen to keep a pass-through model apart from a
// metered one. Anthropic has never heard of it, so what leaves the gateway has
// to be the provider's own name.
func TestAnthropicHandler_SendsTheProviderModelUpstream(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID: "test", Object: "chat.completion", Model: "claude-sonnet-5",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: ptrStr("stop")}},
		},
	}

	registry := newRenamingRegistry("claude-code-sonnet-5", "claude-sonnet-5", mock)
	handler := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	body := `{"model":"claude-code-sonnet-5","messages":[{"role":"user","content":"Hi"}],"max_tokens":16}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)
	assert.Equal(t, "claude-sonnet-5", mock.lastReq.Model)
}

// A client's model picker only knows the provider's names and refuses ours, so
// a caller paying with their own subscription may ask for either. The scoped
// token says which provider is answering, which makes the provider's name
// unambiguous — and keeps the rename our concern rather than the user's.
func TestAnthropicHandler_AcceptsTheProvidersOwnNameFromASubscriber(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID: "test", Object: "chat.completion", Model: "claude-opus-5",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: ptrStr("stop")}},
		},
	}
	registry, _ := provider.NewRegistry([]config.ModelConfig{{
		Name:          "claude-code-opus-5",
		Provider:      "mock",
		ProviderModel: "claude-opus-5",
		AuthMode:      config.AuthModeOAuthPassthrough,
	}}, &staticFactory{provider: mock})
	handler := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"Hi"}],"max_tokens":16}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithScopedUpstreamToken(req.Context(), "mock", "oauth-token"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)
	assert.Equal(t, "claude-opus-5", mock.lastReq.Model)
}

// Without a subscription the same name must not reach a pass-through model:
// that would move the bill from the platform to a caller who never offered it.
func TestAnthropicHandler_DoesNotRenameWithoutASubscription(t *testing.T) {
	mock := &mockAnthropicProvider{completeResp: &provider.CompletionResponse{ID: "t"}}
	registry, _ := provider.NewRegistry([]config.ModelConfig{{
		Name:          "claude-code-opus-5",
		Provider:      "mock",
		ProviderModel: "claude-opus-5",
		AuthMode:      config.AuthModeOAuthPassthrough,
	}}, &staticFactory{provider: mock})
	handler := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"Hi"}],"max_tokens":16}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Nil(t, mock.lastReq)
}

func TestAnthropicHandler_MultiBlockSystem(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID: "test", Object: "chat.completion", Model: "gpt-4o",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: ptrStr("stop")}},
		},
	}

	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100,"system":[{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)
	assert.Equal(t, "system", mock.lastReq.Messages[0].Role)
	assert.Equal(t, "Part 1\nPart 2", mock.lastReq.Messages[0].Content)
}

func TestAnthropicHandler_ConvertMessages(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID: "test", Object: "chat.completion", Model: "gpt-4o",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: ptrStr("stop")}},
		},
	}

	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	// Multi-turn with assistant tool_use and user tool_result
	body := `{
		"model": "claude-3",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": "What's the weather?"},
			{"role": "assistant", "content": [{"type": "text", "text": "Let me check."}, {"type": "tool_use", "id": "call_1", "name": "weather", "input": {"city": "Milan"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": "Sunny, 25C"}]},
			{"role": "user", "content": "Thanks!"}
		]
	}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)

	msgs := mock.lastReq.Messages
	// user, assistant (with tool_calls), user (tool_result as text), user
	require.True(t, len(msgs) >= 3)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "weather", msgs[1].ToolCalls[0].Function.Name)
}

func TestAnthropicHandler_UpstreamError(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeErr: &provider.UpstreamError{StatusCode: 429, Message: "rate limited"},
	}

	registry := newTestRegistry("claude-3", mock)
	rt := newTestRouter(registry)
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Router may wrap error; check it's a client error
	assert.True(t, w.Code >= 400)
	var errResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &errResp)
	assert.Equal(t, "error", errResp["type"])
	errObj := errResp["error"].(map[string]interface{})
	assert.NotEmpty(t, errObj["message"])
}

func TestAnthropicHandler_ErrorResponse_Format(t *testing.T) {
	w := httptest.NewRecorder()
	writeAnthropicError(w, 400, "invalid_request_error", "bad request")

	var resp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)

	assert.Equal(t, "error", resp["type"])
	errObj, ok := resp["error"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "invalid_request_error", errObj["type"])
	assert.Equal(t, "bad request", errObj["message"])
}

func TestMapOpenAIStopReason(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "end_turn"},
		{"", "end_turn"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, mapOpenAIStopReason(tt.input), "input: %s", tt.input)
	}
}

// --- Helpers ---

func ptrStr(s string) *string { return &s }

func newTestRegistry(model string, p provider.Provider) *provider.Registry {
	factory := &staticFactory{provider: p}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name:          model,
		Provider:      "mock",
		ProviderModel: model,
	}}, factory)
	return reg
}

func newRenamingRegistry(name, providerModel string, p provider.Provider) *provider.Registry {
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name:          name,
		Provider:      "mock",
		ProviderModel: providerModel,
	}}, &staticFactory{provider: p})
	return reg
}

func newTestRouter(registry *provider.Registry) *router.Router {
	return router.New(registry, config.RouterConfig{Retries: 0}, slog.Default())
}

type staticFactory struct {
	provider provider.Provider
}

func (f *staticFactory) Create(cfg config.ModelConfig) (provider.Provider, error) {
	return f.provider, nil
}

// PromptTokens is inclusive of cache reads and writes internally, but
// Anthropic's wire shape keeps them separate. Emitting the inclusive number as
// input_tokens with no cache fields made a client reading usage off a cached
// 100k prefix compute a ~10x overcharge.
func TestOpenAIToAnthropicResponse_SplitsCacheTokensBackOut(t *testing.T) {
	h := &AnthropicHandler{}
	usage := &provider.Usage{
		PromptTokens:     100200, // 200 uncached + 100000 read
		CompletionTokens: 300,
		TotalTokens:      100500,
	}
	usage.SetCacheUsage(100000, 0)

	got := h.openAIToAnthropicResponse(&provider.CompletionResponse{
		Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "hi"}}},
		Usage:   usage,
	}, "claude-sonnet-4")

	if got.Usage.InputTokens != 200 {
		t.Errorf("input_tokens = %d, want 200 (the tokens actually billed at full price)", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 100000 {
		t.Errorf("cache_read_input_tokens = %d, want 100000", got.Usage.CacheReadInputTokens)
	}
	if got.Usage.OutputTokens != 300 {
		t.Errorf("output_tokens = %d, want 300", got.Usage.OutputTokens)
	}
}

// A provider that reports no cache breakdown must produce the same body as
// before: no cache fields, input_tokens unchanged.
func TestOpenAIToAnthropicResponse_NoCacheBreakdownIsUnchanged(t *testing.T) {
	h := &AnthropicHandler{}
	got := h.openAIToAnthropicResponse(&provider.CompletionResponse{
		Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "hi"}}},
		Usage:   &provider.Usage{PromptTokens: 1200, CompletionTokens: 50, TotalTokens: 1250},
	}, "claude-sonnet-4")

	if got.Usage.InputTokens != 1200 {
		t.Errorf("input_tokens = %d, want 1200", got.Usage.InputTokens)
	}
	raw, err := json.Marshal(got.Usage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_") {
		t.Errorf("cache fields leaked into a response with no breakdown: %s", raw)
	}
}

// /v1/messages is the only surface where cache_creation_input_tokens exists at
// all, and the one the prefix-cache guard acts on, but RecordCacheTokens was
// wired only into /v1/chat/completions — so the documented cache series stayed
// at zero for exactly the traffic they were added to explain.
func TestAnthropicHandler_RecordsCacheTokens(t *testing.T) {
	stop := "stop"
	usage := &provider.Usage{PromptTokens: 5200, CompletionTokens: 40, TotalTokens: 5240}
	usage.SetCacheUsage(4000, 1000)

	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-cache",
			Model:   "claude-3",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "hi"}, FinishReason: &stop}},
			Usage:   usage,
		},
	}

	registry := newTestRegistry("claude-3", mock)
	spender := spend.NewBatchWriter(nil, slog.Default(), time.Hour)
	t.Cleanup(func() { _ = spender.Close() })
	tokens := &recordingTokenRecorder{}
	handler := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, spender, tokens)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":1024}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, 4000, tokens.cached, "cache reads must reach the metrics recorder")
	assert.Equal(t, 1000, tokens.created, "cache writes must reach the metrics recorder")
}

// sseEvents parses the SSE body into (event, decoded-data) pairs.
func sseEvents(t *testing.T, body string) []struct {
	Event string
	Data  map[string]interface{}
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]interface{}
	}
	var event string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]interface{}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data) != nil {
				continue
			}
			out = append(out, struct {
				Event string
				Data  map[string]interface{}
			}{event, data})
		}
	}
	return out
}

func streamHandler(t *testing.T, chunks [][]byte) *httptest.ResponseRecorder {
	t.Helper()
	mock := &mockAnthropicProvider{streamChunks: chunks}
	registry := newTestRegistry("claude-3", mock)
	handler := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	body := `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100,"stream":true}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// Every content block that opens must close exactly once, at its own index.
// The final text close was hardcoded to index 0, so text → tool → text
// stopped an already-closed block and left the real one dangling.
func TestAnthropicStream_BlocksOpenAndCloseAtTheirOwnIndex(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"delta":{"role":"assistant"}}]}`),
		[]byte(`{"choices":[{"delta":{"content":"prima"}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":"{\"x\":1}"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"content":"dopo"}}]}`),
		[]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
	}
	events := sseEvents(t, streamHandler(t, chunks).Body.String())

	opened, closed := map[float64]int{}, map[float64]int{}
	for _, ev := range events {
		idx, ok := ev.Data["index"].(float64)
		if !ok {
			continue
		}
		switch ev.Event {
		case "content_block_start":
			opened[idx]++
		case "content_block_stop":
			closed[idx]++
		}
	}
	for idx, n := range opened {
		if n != 1 {
			t.Errorf("block %v opened %d times", idx, n)
		}
		if closed[idx] != 1 {
			t.Errorf("block %v opened once but closed %d times", idx, closed[idx])
		}
	}
	for idx, n := range closed {
		if opened[idx] == 0 {
			t.Errorf("block %v was closed %d times but never opened", idx, n)
		}
	}
}

// A tool call opened between two text deltas advances the block counter. The
// text delta's index was derived from that counter, so the second run of text
// was emitted into the tool_use block.
func TestAnthropicStream_TextDeltaNeverLandsInAToolBlock(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"delta":{"content":"prima"}}]}`),
		// A tool call whose name has not arrived yet: state is created, the
		// block counter advances, but the text block is still open.
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"content":"dopo"}}]}`),
		[]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
	}
	events := sseEvents(t, streamHandler(t, chunks).Body.String())

	blockType := map[float64]string{}
	for _, ev := range events {
		if ev.Event != "content_block_start" {
			continue
		}
		idx, _ := ev.Data["index"].(float64)
		if cb, ok := ev.Data["content_block"].(map[string]interface{}); ok {
			blockType[idx], _ = cb["type"].(string)
		}
	}
	for _, ev := range events {
		if ev.Event != "content_block_delta" {
			continue
		}
		delta, _ := ev.Data["delta"].(map[string]interface{})
		if delta["type"] != "text_delta" {
			continue
		}
		idx, _ := ev.Data["index"].(float64)
		if got := blockType[idx]; got != "text" {
			t.Fatalf("text_delta emitted into block %v of type %q", idx, got)
		}
	}
}

// Parallel tool calls must land in separate blocks. A provider reporting
// index 0 for both had their argument streams concatenated into one block.
func TestAnthropicStream_ParallelToolCallsStaySeparate(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":"{\"x\":1}"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"beta","arguments":"{\"y\":2}"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
	}
	events := sseEvents(t, streamHandler(t, chunks).Body.String())

	args := map[float64]string{}
	names := map[float64]string{}
	for _, ev := range events {
		idx, ok := ev.Data["index"].(float64)
		if !ok {
			continue
		}
		if ev.Event == "content_block_start" {
			if cb, ok := ev.Data["content_block"].(map[string]interface{}); ok {
				names[idx], _ = cb["name"].(string)
			}
		}
		if ev.Event == "content_block_delta" {
			if d, ok := ev.Data["delta"].(map[string]interface{}); ok && d["type"] == "input_json_delta" {
				pj, _ := d["partial_json"].(string)
				args[idx] += pj
			}
		}
	}
	if len(names) != 2 {
		t.Fatalf("expected two tool blocks, got %d: %v", len(names), names)
	}
	for idx, a := range args {
		if !json.Valid([]byte(a)) {
			t.Errorf("block %v (%s) carries unparseable arguments %q — two payloads were concatenated",
				idx, names[idx], a)
		}
	}
}

// A provider that wraps tool arguments in a markdown fence must still deliver
// parseable input: streaming raw fragments removed the repair that existed for
// exactly this case.
func TestAnthropicStream_MalformedToolArgumentsAreRepaired(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":"` + "```json" + `\n"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\n` + "```" + `"}}]}}]}`),
		[]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`),
	}
	events := sseEvents(t, streamHandler(t, chunks).Body.String())

	args := ""
	for _, ev := range events {
		if ev.Event != "content_block_delta" {
			continue
		}
		if d, ok := ev.Data["delta"].(map[string]interface{}); ok && d["type"] == "input_json_delta" {
			pj, _ := d["partial_json"].(string)
			args += pj
		}
	}
	if !json.Valid([]byte(args)) {
		t.Fatalf("tool arguments reached the client unparseable: %q", args)
	}
}

package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
)

// Claude Code marks its cacheable prefix with cache_control breakpoints. The
// Anthropic → OpenAI → Anthropic round trip rebuilt every block without them,
// so Anthropic re-read the whole growing context at full price on each tool
// call — the difference between a cache hit and a miss is about tenfold.
func TestAnthropicToOpenAIRequest_KeepsCacheBreakpoints(t *testing.T) {
	h := &AnthropicHandler{}
	body := `{
		"model": "claude-3",
		"max_tokens": 100,
		"system": [
			{"type": "text", "text": "You are Claude Code."},
			{"type": "text", "text": "Long project brief", "cache_control": {"type": "ephemeral"}}
		],
		"tools": [
			{"name": "Read", "input_schema": {"type": "object"}},
			{"name": "Bash", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}}
		],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {}, "cache_control": {"type": "ephemeral"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "done", "cache_control": {"type": "ephemeral"}}]}
		]
	}`

	var req anthropicRequest
	require.NoError(t, json.NewDecoder(strings.NewReader(body)).Decode(&req))
	got := h.anthropicToOpenAIRequest(&req)

	byRole := map[string]provider.Message{}
	for _, m := range got.Messages {
		byRole[m.Role] = m
	}
	for _, role := range []string{"system", "user", "assistant", "tool"} {
		msg, ok := byRole[role]
		require.True(t, ok, "no %s message produced", role)
		assert.NotNil(t, msg.CacheControl, "%s message lost its cache breakpoint", role)
	}

	require.Len(t, got.Tools, 2)
	assert.Nil(t, got.Tools[0].CacheControl)
	assert.NotNil(t, got.Tools[1].CacheControl, "last tool lost its cache breakpoint")
}

// A request with no breakpoints must stay exactly as it was.
func TestAnthropicToOpenAIRequest_NoBreakpointsStaysEmpty(t *testing.T) {
	h := &AnthropicHandler{}
	var req anthropicRequest
	require.NoError(t, json.Unmarshal([]byte(
		`{"model":"claude-3","max_tokens":100,"system":"be brief","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`,
	), &req))

	got := h.anthropicToOpenAIRequest(&req)
	for _, m := range got.Messages {
		assert.Nil(t, m.CacheControl, "invented a breakpoint on the %s message", m.Role)
	}
	assert.Nil(t, got.Tools[0].CacheControl)
}

// message_start is where a client reads what the turn cost on the input side.
// It was hardcoded to zero, so Claude Code saw every cached prefix as free and
// every uncached one as free too.
func TestAnthropicHandler_StreamReportsUpstreamInputUsage(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}],"usage":{"prompt_tokens":100200,"completion_tokens":0,"total_tokens":100200,"cache_read_input_tokens":100000,"cache_creation_input_tokens":0}}`),
		[]byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`),
		[]byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100200,"completion_tokens":42,"total_tokens":100242,"cache_read_input_tokens":100000,"cache_creation_input_tokens":0}}`),
	}

	registry := newTestRegistry("claude-3", &mockAnthropicProvider{streamChunks: chunks})
	handler := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100,"stream":true}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	start := sseEventData(t, w.Body.String(), "message_start")
	usage := start["message"].(map[string]interface{})["usage"].(map[string]interface{})
	assert.EqualValues(t, 200, usage["input_tokens"], "input_tokens should exclude the cached prefix, not be zero")
	assert.EqualValues(t, 100000, usage["cache_read_input_tokens"])

	delta := sseEventData(t, w.Body.String(), "message_delta")
	assert.EqualValues(t, 42, delta["usage"].(map[string]interface{})["output_tokens"])
}

// The router wraps the failing attempt, so a type assertion on the upstream
// error missed: a real 429 reached the client as a 502, which agent clients
// read as transient and retry into an exhausted quota.
func TestAnthropicHandler_PreservesUpstreamRateLimit(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeErr: &provider.UpstreamError{StatusCode: http.StatusTooManyRequests, Message: "quota exhausted"},
	}
	registry := newTestRegistry("claude-3", mock)
	rt := router.New(registry, config.RouterConfig{Retries: 1}, slog.Default())
	handler := NewAnthropicHandler(registry, rt, slog.Default(), nil, nil, nil)

	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":100}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "rate_limit_error", body.Error.Type)
}

func sseEventData(t *testing.T, stream, event string) map[string]interface{} {
	t.Helper()
	for _, block := range strings.Split(stream, "\n\n") {
		if !strings.HasPrefix(block, "event: "+event+"\n") {
			continue
		}
		_, data, _ := strings.Cut(block, "data: ")
		var out map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(data), &out))
		return out
	}
	t.Fatal(fmt.Sprintf("event %q not found in stream:\n%s", event, stream))
	return nil
}

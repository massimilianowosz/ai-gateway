package copilot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestCompleteUsesScopedTokenAndCopilotPath(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedModel string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		assert.Equal(t, defaultUserAgent, r.Header.Get("User-Agent"))
		assert.Equal(t, defaultEditorVersion, r.Header.Get("Editor-Version"))
		assert.Equal(t, defaultEditorPluginVersion, r.Header.Get("Editor-Plugin-Version"))
		assert.Equal(t, defaultIntegrationID, r.Header.Get("Copilot-Integration-Id"))

		var body provider.CompletionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		capturedModel = body.Model

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
		}`))
	}))
	defer server.Close()

	p := New(server.URL+"/v1", "")
	ctx := auth.ContextWithScopedUpstreamToken(context.Background(), providerName, "tid_scoped")
	resp, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "/chat/completions", capturedPath)
	assert.Equal(t, "Bearer tid_scoped", capturedAuth)
	assert.Equal(t, "gpt-4o", capturedModel)
	assert.Equal(t, 4, resp.Usage.TotalTokens)
}

func TestCompleteFallsBackToConfiguredAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tid_configured", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]
		}`)
	}))
	defer server.Close()

	p := New(server.URL, "tid_configured")
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})

	require.NoError(t, err)
}

func TestCompleteRequiresToken(t *testing.T) {
	p := New("http://127.0.0.1:1", "")

	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})

	require.Error(t, err)
	upstreamErr, ok := err.(*provider.UpstreamError)
	require.True(t, ok)
	assert.Equal(t, http.StatusUnauthorized, upstreamErr.StatusCode)
	assert.Equal(t, "missing_upstream_token", upstreamErr.Code)
}

func TestStreamRelaysSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer tid_scoped", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"O\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	p := New(server.URL, "")
	ctx := auth.ContextWithScopedUpstreamToken(context.Background(), providerName, "tid_scoped")
	reader, err := p.Stream(ctx, &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.JSONEq(t, `{"choices":[{"delta":{"content":"O"}}]}`, string(chunk))

	_, err = reader.Next()
	assert.Equal(t, io.EOF, err)
}

func TestName(t *testing.T) {
	p := New("", "")
	assert.Equal(t, "github_copilot", p.Name())
}

func TestNew_DefaultsAPIBaseWhenEmpty(t *testing.T) {
	p := New("", "key")
	assert.Equal(t, defaultAPIBase+"/chat/completions", p.chatCompletionsURL())
}

func TestNew_StripsTrailingSlashAndV1Suffix(t *testing.T) {
	p := New("https://example.com/v1/", "key")
	assert.Equal(t, "https://example.com/chat/completions", p.chatCompletionsURL())
}

func TestNew_TrimsWhitespaceFromAPIKey(t *testing.T) {
	p := New("https://example.com", "  raw-key  ")
	assert.Equal(t, "raw-key", p.apiKey)
}

func TestScopedUpstreamTokenTakesPrecedenceOverConfiguredKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer scoped-wins", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	p := New(server.URL, "configured-key")
	ctx := auth.ContextWithScopedUpstreamToken(context.Background(), providerName, "scoped-wins")
	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
}

func TestComplete_ConvertsMaxTokensToMaxCompletionTokens(t *testing.T) {
	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	p := New(server.URL, "key")
	maxTokens := 128
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:     "gpt-4o",
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &maxTokens,
	})
	require.NoError(t, err)

	_, hasMaxTokens := captured["max_tokens"]
	assert.False(t, hasMaxTokens)
	assert.Equal(t, float64(128), captured["max_completion_tokens"])
}

func TestComplete_ErrorResponseNonStandardStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer server.Close()

	p := New(server.URL, "key")
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusTooManyRequests, upstreamErr.StatusCode)
	assert.Equal(t, "rate limited", upstreamErr.Message)
}

func TestStream_RequiresToken(t *testing.T) {
	p := New("http://127.0.0.1:1", "")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Nil(t, reader)
	require.Error(t, err)
	upstreamErr, ok := err.(*provider.UpstreamError)
	require.True(t, ok)
	assert.Equal(t, "missing_upstream_token", upstreamErr.Code)
}

func TestStream_ErrorResponseClosesBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"overloaded","type":"server_error"}}`)
	}))
	defer server.Close()

	p := New(server.URL, "key")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Nil(t, reader)
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusServiceUnavailable, upstreamErr.StatusCode)
}

func TestComplete_ContextCancelledBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	defer server.Close()

	p := New(server.URL, "key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
}

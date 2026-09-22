package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestOpenAI_Complete(t *testing.T) {
	expected := provider.CompletionResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1700000000,
		Model:   "gpt-4o-2024-11-20",
		Choices: []provider.Choice{
			{
				Index:        0,
				Message:      &provider.Message{Role: "assistant", Content: "Hello!"},
				FinishReason: strPtr("stop"),
			},
		},
		Usage: &provider.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		assert.Equal(t, "POST", r.Method)
		assert.Contains(t, r.URL.Path, "/openai/deployments/gpt-4o/chat/completions")
		assert.Equal(t, "test-key", r.Header.Get("api-key"))
		assert.Equal(t, "2024-10-21", r.URL.Query().Get("api-version"))

		var req provider.CompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		assert.False(t, req.Stream)
		assert.Equal(t, "gpt-4o", req.Model)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	resp, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "chatcmpl-123", resp.ID)
	assert.Equal(t, "Hello!", resp.Choices[0].Message.Content)
	assert.Equal(t, 15, resp.Usage.TotalTokens)
}

func TestOpenAI_Stream(t *testing.T) {
	sseData := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req provider.CompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		assert.True(t, req.Stream)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseData))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	// Read all chunks
	var chunks [][]byte
	for {
		chunk, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		chunks = append(chunks, chunk)
	}

	assert.Len(t, chunks, 3)
	// Verify first chunk
	var first struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	json.Unmarshal(chunks[0], &first)
	assert.Equal(t, "Hello", first.Choices[0].Delta.Content)
}

func TestOpenAI_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"message": "Rate limit exceeded",
				"type":    "rate_limit_error",
				"code":    "rate_limit",
			},
		})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	})

	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, 429, upstreamErr.StatusCode)
	assert.Equal(t, "Rate limit exceeded", upstreamErr.Message)
}

func TestOpenAI_DoFileRequestUsesAccountScopedAzureEndpoint(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/openai/files", r.URL.Path)
		assert.Equal(t, "2024-10-21", r.URL.Query().Get("api-version"))
		assert.Equal(t, "asc", r.URL.Query().Get("order"))
		assert.Equal(t, "test-key", r.Header.Get("api-key"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"file-provider"}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	resp, err := p.DoFileRequest(context.Background(), provider.FileRequest{
		Method:        http.MethodPost,
		Path:          "/files",
		RawQuery:      "order=asc",
		ContentType:   "multipart/form-data; boundary=test",
		ContentLength: int64(len("streamed-payload")),
		Body:          bytes.NewBufferString("streamed-payload"),
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "streamed-payload", capturedBody)
}

func TestOpenAI_DoResponsesRequestUsesNativeV1Endpoint(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/openai/v1/responses", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		assert.Equal(t, "test-key", r.Header.Get("api-key"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-provider"}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	body := `{"model":"gpt-4o-mini","input":"hello"}`
	resp, err := p.DoResponsesRequest(context.Background(), bytes.NewBufferString(body), int64(len(body)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.JSONEq(t, body, capturedBody)
}

func TestOpenAI_Name(t *testing.T) {
	p := NewOpenAI("https://example.com", "key", "v1")
	assert.Equal(t, "azure_openai", p.Name())
}

func TestNewOpenAI_TrimsTrailingSlashFromAPIBase(t *testing.T) {
	p := NewOpenAI("https://example.com/", "key", "v1")
	assert.Equal(t, "https://example.com", p.apiBase)
}

func TestOpenAI_Complete_ConvertsMaxTokensToMaxCompletionTokens(t *testing.T) {
	var captured map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "x"})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	maxTokens := 256
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:     "gpt-5",
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
		MaxTokens: &maxTokens,
	})
	require.NoError(t, err)

	_, hasMaxTokens := captured["max_tokens"]
	assert.False(t, hasMaxTokens, "max_tokens should be converted away")
	assert.Equal(t, float64(256), captured["max_completion_tokens"])
}

func TestOpenAI_Complete_DoesNotOverrideExplicitMaxCompletionTokens(t *testing.T) {
	var captured map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "x"})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	maxTokens := 256
	maxCompletionTokens := 512
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:               "gpt-5",
		Messages:            []provider.Message{{Role: "user", Content: "Hi"}},
		MaxTokens:           &maxTokens,
		MaxCompletionTokens: &maxCompletionTokens,
	})
	require.NoError(t, err)

	assert.Equal(t, float64(512), captured["max_completion_tokens"])
	assert.Equal(t, float64(256), captured["max_tokens"], "should not clear max_tokens when max_completion_tokens was already explicitly set")
}

func TestNewOpenAICompat_KeepsMaxTokensAsIs(t *testing.T) {
	var captured map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "x"})
	}))
	defer srv.Close()

	p := NewOpenAICompat(srv.URL, "test-key", "2024-10-21")
	maxTokens := 256
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:     "mistral-large",
		Messages:  []provider.Message{{Role: "user", Content: "Hi"}},
		MaxTokens: &maxTokens,
	})
	require.NoError(t, err)

	assert.Equal(t, float64(256), captured["max_tokens"])
	_, hasMaxCompletionTokens := captured["max_completion_tokens"]
	assert.False(t, hasMaxCompletionTokens, "legacy compat mode must not introduce max_completion_tokens")
}

func TestOpenAI_Complete_ContextCancelledBeforeRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "x"})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	})
	require.Error(t, err)
}

func TestOpenAI_Embed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Contains(t, r.URL.Path, "/openai/deployments/text-embedding-3-small/embeddings")
		assert.Equal(t, "test-key", r.Header.Get("api-key"))
		assert.Equal(t, "2024-10-21", r.URL.Query().Get("api-version"))

		var req provider.EmbeddingRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "text-embedding-3-small", req.Model)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.EmbeddingResponse{
			Object: "list",
			Model:  "text-embedding-3-small",
			Data:   []provider.EmbeddingObject{{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}}},
			Usage:  provider.EmbeddingUsage{PromptTokens: 4, TotalTokens: 4},
		})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	resp, err := p.Embed(context.Background(), &provider.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: "hello world",
	})

	require.NoError(t, err)
	assert.Len(t, resp.Data, 1)
	assert.Equal(t, 4, resp.Usage.TotalTokens)
}

func TestOpenAI_Embed_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "Invalid API key", "type": "invalid_request_error"},
		})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "bad-key", "2024-10-21")
	_, err := p.Embed(context.Background(), &provider.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: "hello world",
	})

	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusUnauthorized, upstreamErr.StatusCode)
}

func TestOpenAI_Stream_ErrorResponseClosesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "model overloaded", "type": "server_error"},
		})
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "test-key", "2024-10-21")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "Hi"}},
	})
	require.Nil(t, reader)
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusServiceUnavailable, upstreamErr.StatusCode)
}

func strPtr(s string) *string { return &s }

package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// --- Model Detail Tests ---

func TestModelDetailHandler_Found(t *testing.T) {
	reg := buildRegistry(t, "gpt-4o", "claude-3")
	h := NewModelDetailHandler(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4o", nil)
	req.SetPathValue("model", "gpt-4o")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "gpt-4o", resp["id"])
	assert.Equal(t, "model", resp["object"])
	assert.Equal(t, "ubiquum-ai-gateway", resp["owned_by"])
}

func TestModelDetailHandler_NotFound(t *testing.T) {
	reg := buildRegistry(t, "gpt-4o")
	h := NewModelDetailHandler(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/nonexistent", nil)
	req.SetPathValue("model", "nonexistent")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "does not exist")
}

func TestModelDetailHandler_EmptyID(t *testing.T) {
	reg := buildRegistry(t, "gpt-4o")
	h := NewModelDetailHandler(reg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)
	req.SetPathValue("model", "")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// --- Embeddings Tests ---

type mockEmbedder struct {
	resp    *provider.EmbeddingResponse
	err     error
	lastReq *provider.EmbeddingRequest
}

func (m *mockEmbedder) Name() string { return "mock_embedder" }
func (m *mockEmbedder) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, nil
}
func (m *mockEmbedder) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, nil
}
func (m *mockEmbedder) Embed(ctx context.Context, req *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	m.lastReq = req
	return m.resp, m.err
}

func buildEmbeddingRegistry(t *testing.T, model string, p provider.Provider) *provider.Registry {
	t.Helper()
	factory := &staticEmbedFactory{provider: p}
	reg, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          model,
		Provider:      "openai",
		ProviderModel: "text-embedding-3-small",
	}}, factory)
	require.NoError(t, err)
	return reg
}

type staticEmbedFactory struct {
	provider provider.Provider
}

func (f *staticEmbedFactory) Create(cfg config.ModelConfig) (provider.Provider, error) {
	return f.provider, nil
}

func TestEmbeddingsHandler_Success(t *testing.T) {
	mock := &mockEmbedder{
		resp: &provider.EmbeddingResponse{
			Object: "list",
			Data: []provider.EmbeddingObject{
				{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2, 0.3}},
			},
			Model: "text-embedding-3-small",
			Usage: provider.EmbeddingUsage{PromptTokens: 8, TotalTokens: 8},
		},
	}

	reg := buildEmbeddingRegistry(t, "text-embedding-3-small", mock)
	h := NewEmbeddingsHandler(reg, nil, slog.Default(), nil, nil)

	body := `{"model":"text-embedding-3-small","input":"Hello world"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp provider.EmbeddingResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "list", resp.Object)
	assert.Equal(t, "text-embedding-3-small", resp.Model)
	require.Len(t, resp.Data, 1)
	assert.Equal(t, 8, resp.Usage.PromptTokens)
}

func TestEmbeddingsHandler_BatchInput(t *testing.T) {
	mock := &mockEmbedder{
		resp: &provider.EmbeddingResponse{
			Object: "list",
			Data: []provider.EmbeddingObject{
				{Object: "embedding", Index: 0, Embedding: []float64{0.1}},
				{Object: "embedding", Index: 1, Embedding: []float64{0.2}},
			},
			Model: "text-embedding-3-small",
			Usage: provider.EmbeddingUsage{PromptTokens: 12, TotalTokens: 12},
		},
	}

	reg := buildEmbeddingRegistry(t, "text-embedding-3-small", mock)
	h := NewEmbeddingsHandler(reg, nil, slog.Default(), nil, nil)

	body := `{"model":"text-embedding-3-small","input":["Hello","World"]}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.NotNil(t, mock.lastReq)
}

func TestEmbeddingsHandler_Validation(t *testing.T) {
	mock := &mockEmbedder{}
	reg := buildEmbeddingRegistry(t, "text-embedding-3-small", mock)
	h := NewEmbeddingsHandler(reg, nil, slog.Default(), nil, nil)

	tests := []struct {
		name   string
		body   string
		errMsg string
	}{
		{"missing model", `{"input":"hello"}`, "model is required"},
		{"missing input", `{"model":"text-embedding-3-small"}`, "input is required"},
		{"invalid json", `{bad`, "invalid JSON body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Code)
			assert.Contains(t, rr.Body.String(), tt.errMsg)
		})
	}
}

func TestEmbeddingsHandler_ModelNotFound(t *testing.T) {
	mock := &mockEmbedder{}
	reg := buildEmbeddingRegistry(t, "text-embedding-3-small", mock)
	h := NewEmbeddingsHandler(reg, nil, slog.Default(), nil, nil)

	body := `{"model":"nonexistent","input":"hello"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestEmbeddingsHandler_ProviderNotEmbedder(t *testing.T) {
	// Provider that doesn't implement Embedder interface
	noEmbed := &mockAnthropicProvider{} // only implements Provider, not Embedder
	reg := buildEmbeddingRegistry(t, "gpt-4o", noEmbed)
	h := NewEmbeddingsHandler(reg, nil, slog.Default(), nil, nil)

	body := `{"model":"gpt-4o","input":"hello"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "does not support embeddings")
}

func TestEmbeddingsHandler_UpstreamError(t *testing.T) {
	mock := &mockEmbedder{
		err: &provider.UpstreamError{StatusCode: 500, Message: "internal server error"},
	}
	reg := buildEmbeddingRegistry(t, "text-embedding-3-small", mock)
	h := NewEmbeddingsHandler(reg, nil, slog.Default(), nil, nil)

	body := `{"model":"text-embedding-3-small","input":"hello"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

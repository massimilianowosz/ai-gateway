package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// mockProvider implements provider.Provider for testing.
type mockProvider struct {
	completeResp *provider.CompletionResponse
	completeErr  error
	streamChunks [][]byte
	streamErr    error
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if m.completeErr != nil {
		return nil, m.completeErr
	}
	return m.completeResp, nil
}

func (m *mockProvider) Stream(_ context.Context, _ *provider.CompletionRequest) (provider.StreamReader, error) {
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return &mockStreamReader{chunks: m.streamChunks}, nil
}

type mockStreamReader struct {
	chunks [][]byte
	idx    int
}

func (r *mockStreamReader) Next() ([]byte, error) {
	if r.idx >= len(r.chunks) {
		return nil, io.EOF
	}
	chunk := r.chunks[r.idx]
	r.idx++
	return chunk, nil
}

func (r *mockStreamReader) Close() error         { return nil }
func (r *mockStreamReader) Headers() http.Header { return http.Header{} }

type mockFactory struct {
	provider provider.Provider
}

func (f *mockFactory) Create(_ config.ModelConfig) (provider.Provider, error) {
	return f.provider, nil
}

func setupHandler(t *testing.T, mock *mockProvider) *CompletionHandler {
	t.Helper()
	registry, err := provider.NewRegistry(
		[]config.ModelConfig{{Name: "test-model", Provider: "mock", ProviderModel: "test-model-v1"}},
		&mockFactory{provider: mock},
	)
	require.NoError(t, err)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewCompletionHandler(registry, nil, logger, nil, nil, nil)
}

func TestCompletionHandler_NonStreaming(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1700000000,
			Model:   "test-model-v1",
			Choices: []provider.Choice{
				{
					Index:        0,
					Message:      &provider.Message{Role: "assistant", Content: "Hello!"},
					FinishReason: strPtr("stop"),
				},
			},
			Usage:   &provider.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
			Headers: http.Header{},
		},
	}

	handler := setupHandler(t, mock)
	body := `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp provider.CompletionResponse
	err := json.NewDecoder(rec.Body).Decode(&resp)
	require.NoError(t, err)
	assert.Equal(t, "chatcmpl-123", resp.ID)
	assert.Equal(t, "Hello!", resp.Choices[0].Message.Content)
}

func TestCompletionHandler_Streaming(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"id":"c1","choices":[{"delta":{"role":"assistant","content":"Hi"},"index":0}]}`),
		[]byte(`{"id":"c1","choices":[{"delta":{"content":" there"},"index":0}]}`),
		[]byte(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop","index":0}]}`),
	}

	mock := &mockProvider{streamChunks: chunks}
	handler := setupHandler(t, mock)

	body := `{"model":"test-model","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "data: {\"id\":\"c1\"")
	assert.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestStreamUsageFromChunk(t *testing.T) {
	usage := streamUsageFromChunk([]byte(`{"id":"c1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))

	require.NotNil(t, usage)
	assert.Equal(t, 11, usage.PromptTokens)
	assert.Equal(t, 7, usage.CompletionTokens)
	assert.Equal(t, 18, usage.TotalTokens)
}

func TestCompletionHandler_MissingModel(t *testing.T) {
	mock := &mockProvider{}
	handler := setupHandler(t, mock)

	body := `{"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "model is required")
}

func TestCompletionHandler_EmptyMessages(t *testing.T) {
	mock := &mockProvider{}
	handler := setupHandler(t, mock)

	body := `{"model":"test-model","messages":[]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "messages is required")
}

func TestCompletionHandler_UnknownModel(t *testing.T) {
	mock := &mockProvider{}
	handler := setupHandler(t, mock)

	body := `{"model":"nonexistent","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not available")
}

func TestCompletionHandler_UpstreamError(t *testing.T) {
	mock := &mockProvider{
		completeErr: &provider.UpstreamError{
			StatusCode: 429,
			Message:    "rate limited",
			Type:       "rate_limit_error",
		},
	}
	handler := setupHandler(t, mock)

	body := `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), "rate limited")
}

func TestCompletionHandler_InvalidJSON(t *testing.T) {
	mock := &mockProvider{}
	handler := setupHandler(t, mock)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func strPtr(s string) *string { return &s }

func TestDropParams(t *testing.T) {
	temp := 0.7
	topP := 0.9
	maxTokens := 100
	presP := 0.5
	freqP := 0.3
	n := 2

	req := &provider.CompletionRequest{
		Model:            "test",
		Temperature:      &temp,
		TopP:             &topP,
		MaxTokens:        &maxTokens,
		PresencePenalty:  &presP,
		FrequencyPenalty: &freqP,
		N:                &n,
		User:             "user-123",
		Stop:             "STOP",
		ResponseFormat:   map[string]interface{}{"type": "json_object"},
		StreamOptions:    &provider.StreamOptions{IncludeUsage: true},
	}

	dropParams(req, []string{"temperature", "top_p", "presence_penalty", "frequency_penalty", "n", "stop", "response_format", "stream_options", "user"})

	assert.Nil(t, req.Temperature)
	assert.Nil(t, req.TopP)
	assert.Nil(t, req.PresencePenalty)
	assert.Nil(t, req.FrequencyPenalty)
	assert.Nil(t, req.N)
	assert.Nil(t, req.Stop)
	assert.Nil(t, req.ResponseFormat)
	assert.Nil(t, req.StreamOptions)
	assert.Empty(t, req.User)
	// max_tokens should remain
	assert.Equal(t, &maxTokens, req.MaxTokens)
}

func TestDropParams_Empty(t *testing.T) {
	temp := 0.7
	req := &provider.CompletionRequest{
		Model:       "test",
		Temperature: &temp,
	}

	dropParams(req, nil)
	assert.Equal(t, &temp, req.Temperature)

	dropParams(req, []string{})
	assert.Equal(t, &temp, req.Temperature)
}

func TestDropParams_RemovesExtraFields(t *testing.T) {
	req := &provider.CompletionRequest{
		Model: "test",
		Extra: map[string]interface{}{
			"seed":       123,
			"logit_bias": map[string]interface{}{"42": -100},
		},
	}

	dropParams(req, []string{"seed"})

	assert.NotContains(t, req.Extra, "seed")
	assert.Contains(t, req.Extra, "logit_bias")
}

// --- Model allowlist (per-key access control) ---

func setupHandlerWithKeyInfo(t *testing.T, mock *mockProvider, keyInfo *store.APIKey) (*CompletionHandler, *http.Request) {
	t.Helper()
	handler := setupHandler(t, mock)
	body := `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	if keyInfo != nil {
		req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), keyInfo))
	}
	return handler, req
}

func TestCompletionHandler_ModelAllowlist_Allowed(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-1",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	// Key explicitly allows "test-model"
	keyInfo := &store.APIKey{Active: true, Budget: 100, Models: store.StringList{"test-model", "other-model"}}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCompletionHandler_ModelAllowlist_Denied(t *testing.T) {
	mock := &mockProvider{}
	// Key only allows "other-model", not "test-model"
	keyInfo := &store.APIKey{Active: true, Budget: 100, Models: store.StringList{"other-model", "another-model"}}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "access_denied")
	assert.Contains(t, rec.Body.String(), "test-model")
}

func TestCompletionHandler_ModelAllowlist_NilList_NoRestriction(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-2",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	// An absent Models list means no restriction was ever configured.
	keyInfo := &store.APIKey{Active: true, Budget: 100, Models: nil}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// An empty whitelist is a whitelist that grants nothing. Reading it as "no
// restriction" made revoking every model indistinguishable from never having
// set one, so the operator's revocation silently allowed everything.
func TestCompletionHandler_ModelAllowlist_EmptyList_DeniesEverything(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-2",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	keyInfo := &store.APIKey{Active: true, Budget: 100, Models: store.StringList{}}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "access_denied")
}

func TestCompletionHandler_ProviderAllowlist_Denied(t *testing.T) {
	mock := &mockProvider{}
	// test-model is registered on provider "mock"; the key only allows "azure".
	keyInfo := &store.APIKey{Active: true, Budget: 100, AllowedProviders: store.StringList{"azure"}}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "access_denied")
	assert.Contains(t, rec.Body.String(), "test-model")
}

func TestCompletionHandler_ProviderAllowlist_Allowed(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-3",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	keyInfo := &store.APIKey{Active: true, Budget: 100, AllowedProviders: store.StringList{"mock"}}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCompletionHandler_ProviderAllowlist_NilList_NoRestriction(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-4",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	keyInfo := &store.APIKey{Active: true, Budget: 100, AllowedProviders: nil}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthorizedDeploymentSelectionExcludesDeniedProvidersIncludingPinned(t *testing.T) {
	mock := &mockProvider{}
	registry, err := provider.NewRegistry([]config.ModelConfig{
		{Name: "mixed", Provider: "openai", ProviderModel: "m"},
		{Name: "mixed", Provider: "azure_openai", ProviderModel: "m"},
	}, &mockFactory{provider: mock})
	require.NoError(t, err)
	ctx := auth.ContextWithKeyInfo(context.Background(), &store.APIKey{
		DeniedProviders: store.StringList{"openai"},
	})

	var deniedID string
	for _, dep := range mustDeployments(t, registry, "mixed") {
		if dep.ProviderName == "openai" {
			deniedID = dep.ID
		}
	}
	require.NotEmpty(t, deniedID)
	for i := 0; i < 20; i++ {
		dep, getErr := getAuthorizedDeployment(ctx, registry, "mixed")
		require.NoError(t, getErr)
		assert.Equal(t, "azure_openai", dep.ProviderName)
	}
	_, err = getAuthorizedDeploymentByID(ctx, registry, "mixed", deniedID)
	assert.ErrorContains(t, err, "not authorized")
}

func mustDeployments(t *testing.T, registry *provider.Registry, model string) []*provider.Deployment {
	t.Helper()
	deployments, err := registry.GetDeployments(model)
	require.NoError(t, err)
	return deployments
}

// setupHandlerWithEUModel registers "test-model" as an EU-only deployment,
// for testing model_residency's require_eu enforcement.
func setupHandlerWithEUModel(t *testing.T, mock *mockProvider) *CompletionHandler {
	t.Helper()
	registry, err := provider.NewRegistry(
		[]config.ModelConfig{{Name: "test-model", Provider: "mock", ProviderModel: "test-model-v1", IsEU: true}},
		&mockFactory{provider: mock},
	)
	require.NoError(t, err)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewCompletionHandler(registry, nil, logger, nil, nil, nil)
}

func TestCompletionHandler_ModelResidency_Denied(t *testing.T) {
	mock := &mockProvider{}
	// test-model (setupHandler) is registered with no IsEU flag, i.e. not EU.
	keyInfo := &store.APIKey{Active: true, Budget: 100, RequireEUResidency: true}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "access_denied")
	assert.Contains(t, rec.Body.String(), "test-model")
}

func TestCompletionHandler_ModelResidency_Allowed(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-5",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	handler := setupHandlerWithEUModel(t, mock)
	body := `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	keyInfo := &store.APIKey{Active: true, Budget: 100, RequireEUResidency: true}
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(), keyInfo))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCompletionHandler_ModelResidency_NotRequired_NoRestriction(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-6",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	// test-model is not EU, but the key does not require EU residency.
	keyInfo := &store.APIKey{Active: true, Budget: 100, RequireEUResidency: false}
	handler, req := setupHandlerWithKeyInfo(t, mock, keyInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestCompletionHandler_NoKeyInfo_NoRestriction(t *testing.T) {
	mock := &mockProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-3",
			Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: strPtr("stop")}},
			Usage:   &provider.Usage{},
			Headers: http.Header{},
		},
	}
	// No key info in context (master key path) → no model restriction
	handler, req := setupHandlerWithKeyInfo(t, mock, nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

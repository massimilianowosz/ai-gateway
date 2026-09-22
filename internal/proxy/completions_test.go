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

func TestCompletionsHandler_Complete(t *testing.T) {
	stop := "stop"
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "cmpl-123",
			Object:  "chat.completion",
			Model:   "gpt-3.5-turbo-instruct",
			Created: 1234567890,
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "Hello world"}, FinishReason: &stop}},
			Usage:   &provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		},
	}

	registry := newTestRegistry("gpt-3.5-turbo-instruct", mock)
	rt := newTestRouter(registry)
	handler := NewCompletionsHandler(registry, rt, slog.Default(), nil, nil)

	body := `{"model":"gpt-3.5-turbo-instruct","prompt":"Say hello","max_tokens":100}`
	req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp completionsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "text_completion", resp.Object)
	assert.Equal(t, "gpt-3.5-turbo-instruct", resp.Model)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hello world", resp.Choices[0].Text)
	assert.Equal(t, "stop", *resp.Choices[0].FinishReason)
	assert.Equal(t, 8, resp.Usage.TotalTokens)
}

func TestCompletionsHandler_ArrayPrompt(t *testing.T) {
	mock := &mockAnthropicProvider{
		completeResp: &provider.CompletionResponse{
			ID: "cmpl-x", Object: "chat.completion", Model: "test",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: ptrStr("stop")}},
		},
	}

	registry := newTestRegistry("test-model", mock)
	rt := newTestRouter(registry)
	handler := NewCompletionsHandler(registry, rt, slog.Default(), nil, nil)

	body := `{"model":"test-model","prompt":["Hello","World"]}`
	req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	// Verify the prompt was joined
	require.NotNil(t, mock.lastReq)
	assert.Equal(t, "Hello\nWorld", mock.lastReq.Messages[0].Content)
}

func TestCompletionsHandler_Validation(t *testing.T) {
	mock := &mockAnthropicProvider{}
	registry := newTestRegistry("test", mock)
	rt := newTestRouter(registry)
	handler := NewCompletionsHandler(registry, rt, slog.Default(), nil, nil)

	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing model", `{"prompt":"hello"}`, http.StatusBadRequest},
		{"invalid json", `{bad`, http.StatusBadRequest},
		{"model not found", `{"model":"nonexistent","prompt":"hi"}`, http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, tt.code, w.Code)
		})
	}
}

func TestCompletionsHandler_Stream(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234,"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`),
		[]byte(`{"id":"cmpl-1","object":"chat.completion.chunk","created":1234,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}

	mock := &mockAnthropicProvider{streamChunks: chunks}
	registry := newTestRegistry("test", mock)
	rt := newTestRouter(registry)
	handler := NewCompletionsHandler(registry, rt, slog.Default(), nil, nil)

	body := `{"model":"test","prompt":"Hi","stream":true}`
	req := httptest.NewRequest("POST", "/v1/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "text_completion")
	assert.Contains(t, w.Body.String(), "[DONE]")
}

// --- Models Anthropic format ---

func TestModelsHandler_AnthropicFormat(t *testing.T) {
	reg := buildRegistry(t, "claude-3", "gpt-4o")
	h := NewModelsHandler(reg, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	// Should have Anthropic-format fields
	data := resp["data"].([]interface{})
	require.Len(t, data, 2)

	first := data[0].(map[string]interface{})
	assert.Equal(t, "model", first["type"])
	assert.Equal(t, "claude-3", first["id"])
	assert.Equal(t, "claude-3", first["display_name"])
	assert.NotEmpty(t, first["created_at"])

	assert.Equal(t, false, resp["has_more"])
}

// --- Moderations ---

type mockModerator struct {
	resp *provider.ModerationResponse
	err  error
}

func (m *mockModerator) Name() string { return "mock_moderator" }
func (m *mockModerator) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, nil
}
func (m *mockModerator) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, nil
}
func (m *mockModerator) Moderate(ctx context.Context, req *provider.ModerationRequest) (*provider.ModerationResponse, error) {
	return m.resp, m.err
}

func TestModerationsHandler_Success(t *testing.T) {
	mock := &mockModerator{
		resp: &provider.ModerationResponse{
			ID:    "modr-123",
			Model: "text-moderation-latest",
			Results: []provider.ModerationResult{{
				Flagged:        false,
				Categories:     map[string]bool{"hate": false, "violence": false},
				CategoryScores: map[string]float64{"hate": 0.001, "violence": 0.002},
			}},
		},
	}

	factory := &staticFactory{provider: mock}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name: "text-moderation-latest", Provider: "openai", ProviderModel: "text-moderation-latest",
	}}, factory)

	h := NewModerationsHandler(reg, slog.Default())

	body := `{"input":"Hello world"}`
	req := httptest.NewRequest("POST", "/v1/moderations", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var resp provider.ModerationResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "modr-123", resp.ID)
	assert.Len(t, resp.Results, 1)
	assert.False(t, resp.Results[0].Flagged)
}

func TestModerationsHandler_Validation(t *testing.T) {
	mock := &mockModerator{}
	factory := &staticFactory{provider: mock}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name: "text-moderation-latest", Provider: "openai", ProviderModel: "text-moderation-latest",
	}}, factory)
	h := NewModerationsHandler(reg, slog.Default())

	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing input", `{}`, http.StatusBadRequest},
		{"invalid json", `{bad`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/moderations", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			assert.Equal(t, tt.code, rr.Code)
		})
	}
}

func TestModerationsHandler_ProviderNotModerator(t *testing.T) {
	// Provider without Moderate interface
	mock := &mockAnthropicProvider{}
	registry := newTestRegistry("text-moderation-latest", mock)
	h := NewModerationsHandler(registry, slog.Default())

	body := `{"input":"hello","model":"text-moderation-latest"}`
	req := httptest.NewRequest("POST", "/v1/moderations", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "does not support moderations")
}

// --- Image generations ---

type mockImageGenerator struct {
	resp []byte
	err  error
}

func (m *mockImageGenerator) Name() string { return "mock_image" }
func (m *mockImageGenerator) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, nil
}
func (m *mockImageGenerator) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, nil
}
func (m *mockImageGenerator) GenerateImage(ctx context.Context, requestBody []byte) ([]byte, error) {
	return m.resp, m.err
}

func TestImageGenerationsHandler_Success(t *testing.T) {
	respJSON := `{"created":1234,"data":[{"url":"https://example.com/img.png"}]}`
	mock := &mockImageGenerator{resp: []byte(respJSON)}

	factory := &staticFactory{provider: mock}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name: "dall-e-3", Provider: "openai", ProviderModel: "dall-e-3",
	}}, factory)

	h := NewImageGenerationsHandler(reg, slog.Default())

	body := `{"prompt":"A cat","model":"dall-e-3"}`
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "example.com/img.png")
}

func TestImageGenerationsHandler_Validation(t *testing.T) {
	mock := &mockImageGenerator{}
	factory := &staticFactory{provider: mock}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name: "dall-e-3", Provider: "openai", ProviderModel: "dall-e-3",
	}}, factory)
	h := NewImageGenerationsHandler(reg, slog.Default())

	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing prompt", `{"model":"dall-e-3"}`, http.StatusBadRequest},
		{"invalid json", `{bad`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			assert.Equal(t, tt.code, rr.Code)
		})
	}
}

// --- Audio speech ---

type mockSpeaker struct {
	audio       []byte
	contentType string
	err         error
}

func (m *mockSpeaker) Name() string { return "mock_tts" }
func (m *mockSpeaker) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, nil
}
func (m *mockSpeaker) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, nil
}
func (m *mockSpeaker) Speak(ctx context.Context, requestBody []byte) ([]byte, string, error) {
	return m.audio, m.contentType, m.err
}

func TestAudioSpeechHandler_Success(t *testing.T) {
	mock := &mockSpeaker{audio: []byte("fake-audio-data"), contentType: "audio/mpeg"}

	factory := &staticFactory{provider: mock}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name: "tts-1", Provider: "openai", ProviderModel: "tts-1",
	}}, factory)

	h := NewAudioSpeechHandler(reg, slog.Default())

	body := `{"model":"tts-1","input":"Hello","voice":"alloy"}`
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "audio/mpeg", rr.Header().Get("Content-Type"))
	assert.Equal(t, "fake-audio-data", rr.Body.String())
}

func TestAudioSpeechHandler_Validation(t *testing.T) {
	mock := &mockSpeaker{}
	factory := &staticFactory{provider: mock}
	reg, _ := provider.NewRegistry([]config.ModelConfig{{
		Name: "tts-1", Provider: "openai", ProviderModel: "tts-1",
	}}, factory)
	h := NewAudioSpeechHandler(reg, slog.Default())

	body := `{"model":"tts-1","voice":"alloy"}`
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "input is required")
}

// --- Helper test ---

func TestExtractPromptText(t *testing.T) {
	tests := []struct {
		input    interface{}
		expected string
	}{
		{"hello", "hello"},
		{[]interface{}{"a", "b"}, "a\nb"},
		{123, "123"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, extractPromptText(tt.input))
	}
}

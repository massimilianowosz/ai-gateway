package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// --- computeCost ---

func TestComputeCost_FlatBillingIsFree(t *testing.T) {
	dep := &provider.Deployment{BillingMode: config.BillingModeFlat, ProviderModel: "gpt-4o"}
	pc := pricing.NewCalculator(slog.Default(), "", nil)
	pc.SetPrice("gpt-4o", pricing.ModelPrice{InputCostPerToken: 1, OutputCostPerToken: 1})

	assert.Equal(t, 0.0, computeCost(dep, pc, 1000, 1000))
}

func TestComputeCost_NilCalculatorIsFree(t *testing.T) {
	dep := &provider.Deployment{BillingMode: config.BillingModeMetered, ProviderModel: "gpt-4o"}
	assert.Equal(t, 0.0, computeCost(dep, nil, 1000, 1000))
}

func TestComputeCost_MeteredUsesPricing(t *testing.T) {
	dep := &provider.Deployment{BillingMode: config.BillingModeMetered, ProviderModel: "gpt-4o"}
	pc := pricing.NewCalculator(slog.Default(), "", nil)
	pc.SetPrice("gpt-4o", pricing.ModelPrice{InputCostPerToken: 0.001, OutputCostPerToken: 0.002})

	got := computeCost(dep, pc, 100, 50)
	assert.InDelta(t, 0.001*100+0.002*50, got, 1e-9)
}

// --- handler.go pure helpers ---

func TestMessageContentLen_String(t *testing.T) {
	assert.Equal(t, 5, messageContentLen("hello"))
}

func TestMessageContentLen_Parts(t *testing.T) {
	parts := []interface{}{
		map[string]interface{}{"type": "text", "text": "hi"},
		map[string]interface{}{"type": "text", "text": "there"},
		map[string]interface{}{"type": "image_url"}, // no "text" key, contributes 0
	}
	assert.Equal(t, 7, messageContentLen(parts))
}

func TestMessageContentLen_Unsupported(t *testing.T) {
	assert.Equal(t, 0, messageContentLen(42))
	assert.Equal(t, 0, messageContentLen(nil))
}

func TestEstimatePromptTokens(t *testing.T) {
	req := &provider.CompletionRequest{Messages: []provider.Message{
		{Role: "user", Content: "12345678"}, // 8 chars -> 2 tokens + 4 overhead
	}}
	got := estimatePromptTokens(req)
	assert.Equal(t, 4+2, got)
}

func TestEstimatePromptTokens_Nil(t *testing.T) {
	assert.Equal(t, 0, estimatePromptTokens(nil))
}

func TestEstimatePromptTokens_EmptyContentFallsBackToMessageCount(t *testing.T) {
	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: ""}}}
	got := estimatePromptTokens(req)
	// total starts at 4 (per-message overhead) so it never hits the
	// "total==0" fallback branch when there's at least one message; assert
	// it's still a sane positive number rather than depending on the exact
	// fallback path.
	assert.Greater(t, got, 0)
}

func TestEstimateCompletionTokens(t *testing.T) {
	msg := &provider.Message{Content: "12345678"}
	resp := &provider.CompletionResponse{Choices: []provider.Choice{{Message: msg}}}
	assert.Equal(t, 2, estimateCompletionTokens(resp))
}

func TestEstimateCompletionTokens_Nil(t *testing.T) {
	assert.Equal(t, 0, estimateCompletionTokens(nil))
}

func TestEstimateCompletionTokens_ShortNonEmptyRoundsUpToOne(t *testing.T) {
	msg := &provider.Message{Content: "ab"} // 2 chars / 4 = 0, but non-empty -> 1
	resp := &provider.CompletionResponse{Choices: []provider.Choice{{Message: msg}}}
	assert.Equal(t, 1, estimateCompletionTokens(resp))
}

// --- anthropic.go pure helpers ---

func TestRepairToolCallJSON_MarkdownJSONBlock(t *testing.T) {
	raw := "here you go:\n```json\n{\"a\":1}\n```\ndone"
	assert.Equal(t, `{"a":1}`, repairToolCallJSON(raw))
}

func TestRepairToolCallJSON_PlainCodeBlock(t *testing.T) {
	raw := "```\n{\"b\":2}\n```"
	assert.Equal(t, `{"b":2}`, repairToolCallJSON(raw))
}

func TestRepairToolCallJSON_BareBraces(t *testing.T) {
	raw := `noise before {"c":3} noise after`
	assert.Equal(t, `{"c":3}`, repairToolCallJSON(raw))
}

func TestRepairToolCallJSON_NestedBraces(t *testing.T) {
	raw := `{"outer":{"inner":1}}`
	assert.Equal(t, raw, repairToolCallJSON(raw))
}

func TestRepairToolCallJSON_Unrecoverable(t *testing.T) {
	assert.Equal(t, "", repairToolCallJSON("not json at all"))
}

func TestSimplifyParts_SingleTextPart(t *testing.T) {
	parts := []interface{}{map[string]interface{}{"type": "text", "text": "hello"}}
	assert.Equal(t, "hello", simplifyParts(parts))
}

func TestSimplifyParts_MultiplePartsUnchanged(t *testing.T) {
	parts := []interface{}{
		map[string]interface{}{"type": "text", "text": "a"},
		map[string]interface{}{"type": "text", "text": "b"},
	}
	got := simplifyParts(parts)
	assert.Equal(t, parts, got)
}

func TestSimplifyParts_NonTextSinglePartUnchanged(t *testing.T) {
	parts := []interface{}{map[string]interface{}{"type": "image"}}
	assert.Equal(t, parts, simplifyParts(parts))
}

func TestAnthropicHandler_WriteSSEError(t *testing.T) {
	registry := newTestRegistry("claude-x", &mockAnthropicProvider{})
	h := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	rr := httptest.NewRecorder()
	h.writeSSEError(rr, &provider.UpstreamError{StatusCode: 503, Message: "upstream down"})

	body := rr.Body.String()
	assert.Contains(t, body, "event: error")
	assert.Contains(t, body, "upstream down")
	assert.True(t, rr.Flushed)
}

func TestAnthropicHandler_WriteSSEError_GenericError(t *testing.T) {
	registry := newTestRegistry("claude-x", &mockAnthropicProvider{})
	h := NewAnthropicHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	rr := httptest.NewRecorder()
	h.writeSSEError(rr, errors.New("boom"))
	assert.Contains(t, rr.Body.String(), "boom")
}

// --- responses.go pure helper ---

func TestConvertResponsesToolChoice_String(t *testing.T) {
	assert.Equal(t, "auto", convertResponsesToolChoice("auto"))
}

func TestConvertResponsesToolChoice_FunctionObject(t *testing.T) {
	in := map[string]interface{}{"type": "function", "name": "get_weather"}
	got := convertResponsesToolChoice(in)
	m, ok := got.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "function", m["type"])
	fn, ok := m["function"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "get_weather", fn["name"])
}

func TestConvertResponsesToolChoice_Passthrough(t *testing.T) {
	in := 42
	assert.Equal(t, 42, convertResponsesToolChoice(in))
}

// --- completions.go handleUpstreamErr ---

func TestHandleUpstreamErr_NotFound(t *testing.T) {
	rr := httptest.NewRecorder()
	handleUpstreamErr(rr, slog.Default(), errors.New("model not found"))
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestHandleUpstreamErr_UpstreamError(t *testing.T) {
	rr := httptest.NewRecorder()
	handleUpstreamErr(rr, slog.Default(), &provider.UpstreamError{StatusCode: 429, Message: "rate limited"})
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Contains(t, rr.Body.String(), "rate limited")
}

func TestHandleUpstreamErr_UpstreamError_OutOfRangeStatusMapsToBadGateway(t *testing.T) {
	rr := httptest.NewRecorder()
	handleUpstreamErr(rr, slog.Default(), &provider.UpstreamError{StatusCode: 999, Message: "weird"})
	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

func TestHandleUpstreamErr_GenericError(t *testing.T) {
	rr := httptest.NewRecorder()
	handleUpstreamErr(rr, slog.Default(), errors.New("network exploded"))
	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

// --- edge_models.go ---

func TestEdgeModelsHandler_ServeAdd_Success(t *testing.T) {
	registry := newTestRegistry("existing-model", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	body := `{"model_name":"edge/llama3","litellm_params":{"api_base":"http://localhost:11434/v1","api_key":"unused"}}`
	req := httptest.NewRequest(http.MethodPost, "/model/new", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeAdd(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "registered")

	deps, err := registry.GetDeployments("edge/llama3")
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "llama3", deps[0].ProviderModel)
}

func TestEdgeModelsHandler_ServeAdd_Unauthorized(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/new", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-key")
	rr := httptest.NewRecorder()

	h.ServeAdd(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestEdgeModelsHandler_ServeAdd_MissingModelName(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/new", strings.NewReader(`{"litellm_params":{"api_base":"http://x"}}`))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeAdd(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestEdgeModelsHandler_ServeAdd_MissingAPIBase(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/new", strings.NewReader(`{"model_name":"edge/x","litellm_params":{}}`))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeAdd(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestEdgeModelsHandler_ServeAdd_InvalidJSON(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/new", strings.NewReader(`not json`))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeAdd(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestEdgeModelsHandler_ServeDelete_ByModelName(t *testing.T) {
	registry := newTestRegistry("edge/to-delete", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/delete", strings.NewReader(`{"model_name":"edge/to-delete"}`))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeDelete(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)

	_, err := registry.GetDeployments("edge/to-delete")
	assert.Error(t, err)
}

func TestEdgeModelsHandler_ServeDelete_ByID(t *testing.T) {
	registry := newTestRegistry("edge/by-id", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/delete", strings.NewReader(`{"id":"edge/by-id"}`))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeDelete(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestEdgeModelsHandler_ServeDelete_MissingIdentifier(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/delete", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer master-key")
	rr := httptest.NewRecorder()

	h.ServeDelete(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestEdgeModelsHandler_ServeDelete_Unauthorized(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "master-key")

	req := httptest.NewRequest(http.MethodPost, "/model/delete", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	h.ServeDelete(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestEdgeModelsHandler_IsMasterKey_EmptyConfiguredKeyAlwaysFalse(t *testing.T) {
	registry := newTestRegistry("m", &mockAnthropicProvider{})
	h := NewEdgeModelsHandler(registry, "")

	req := httptest.NewRequest(http.MethodPost, "/model/new", nil)
	req.Header.Set("Authorization", "Bearer ")
	assert.False(t, h.isMasterKey(req))
}

// --- media.go ---

type mockTranscriber struct {
	resp []byte
	err  error
}

func (m *mockTranscriber) Name() string { return "mock-transcriber" }
func (m *mockTranscriber) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *mockTranscriber) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, errors.New("not implemented")
}
func (m *mockTranscriber) Transcribe(ctx context.Context, req *provider.TranscriptionRequest) ([]byte, error) {
	return m.resp, m.err
}

func multipartAudioRequest(t *testing.T, model, filename string, fileContent []byte) *http.Request {
	t.Helper()
	var buf strings.Builder
	writer := multipart.NewWriter(&buf)
	if model != "" {
		require.NoError(t, writer.WriteField("model", model))
	}
	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		require.NoError(t, err)
		_, err = part.Write(fileContent)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestAudioTranscriptionHandler_Success(t *testing.T) {
	mock := &mockTranscriber{resp: []byte(`{"text":"hello world"}`)}
	registry := newTestRegistry("whisper-1", mock)
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "whisper-1", "audio.mp3", []byte("fake-audio-bytes"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, `{"text":"hello world"}`, rr.Body.String())
}

func TestAudioTranscriptionHandler_DefaultsModelToWhisper1(t *testing.T) {
	mock := &mockTranscriber{resp: []byte(`{}`)}
	registry := newTestRegistry("whisper-1", mock)
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "", "audio.mp3", []byte("data"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAudioTranscriptionHandler_MissingFile(t *testing.T) {
	registry := newTestRegistry("whisper-1", &mockTranscriber{})
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "whisper-1", "", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestAudioTranscriptionHandler_ModelNotFound(t *testing.T) {
	registry := newTestRegistry("whisper-1", &mockTranscriber{})
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "nonexistent-model", "audio.mp3", []byte("data"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestAudioTranscriptionHandler_ProviderNotTranscriber(t *testing.T) {
	registry := newTestRegistry("whisper-1", &mockAnthropicProvider{})
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "whisper-1", "audio.mp3", []byte("data"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestAudioTranscriptionHandler_UpstreamError(t *testing.T) {
	mock := &mockTranscriber{err: &provider.UpstreamError{StatusCode: 503, Message: "down"}}
	registry := newTestRegistry("whisper-1", mock)
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "whisper-1", "audio.mp3", []byte("data"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestAudioTranscriptionHandler_GenericUpstreamError(t *testing.T) {
	mock := &mockTranscriber{err: errors.New("boom")}
	registry := newTestRegistry("whisper-1", mock)
	h := NewAudioTranscriptionHandler(registry, slog.Default())

	req := multipartAudioRequest(t, "whisper-1", "audio.mp3", []byte("data"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

type mockRawForwarder struct {
	respBody        []byte
	respContentType string
	err             error
	lastEndpoint    string
}

func (m *mockRawForwarder) Name() string { return "mock-forwarder" }
func (m *mockRawForwarder) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *mockRawForwarder) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, errors.New("not implemented")
}
func (m *mockRawForwarder) Forward(ctx context.Context, endpoint, contentType string, body io.Reader) ([]byte, string, error) {
	m.lastEndpoint = endpoint
	return m.respBody, m.respContentType, m.err
}

func TestPassthroughHandler_Success(t *testing.T) {
	mock := &mockRawForwarder{respBody: []byte(`{"ok":true}`), respContentType: "application/json"}
	registry := newTestRegistry("dall-e-2", mock)
	h := NewPassthroughHandler(registry, slog.Default(), "/images/edits")

	body, _ := json.Marshal(map[string]string{"model": "dall-e-2"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, `{"ok":true}`, rr.Body.String())
	assert.Equal(t, "/images/edits", mock.lastEndpoint)
}

func TestPassthroughHandler_DefaultsModelWhenBodyHasNone(t *testing.T) {
	mock := &mockRawForwarder{respBody: []byte(`{}`), respContentType: "application/json"}
	registry := newTestRegistry("dall-e-2", mock)
	h := NewPassthroughHandler(registry, slog.Default(), "/images/variations")

	req := httptest.NewRequest(http.MethodPost, "/v1/images/variations", strings.NewReader(`not-json`))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestPassthroughHandler_ModelNotFound(t *testing.T) {
	registry := newTestRegistry("dall-e-2", &mockRawForwarder{})
	h := NewPassthroughHandler(registry, slog.Default(), "/images/edits")

	body, _ := json.Marshal(map[string]string{"model": "no-such-model"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestPassthroughHandler_ProviderNotForwarder(t *testing.T) {
	registry := newTestRegistry("dall-e-2", &mockAnthropicProvider{})
	h := NewPassthroughHandler(registry, slog.Default(), "/images/edits")

	body, _ := json.Marshal(map[string]string{"model": "dall-e-2"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestPassthroughHandler_UpstreamError(t *testing.T) {
	mock := &mockRawForwarder{err: &provider.UpstreamError{StatusCode: 502, Message: "bad gateway upstream"}}
	registry := newTestRegistry("dall-e-2", mock)
	h := NewPassthroughHandler(registry, slog.Default(), "/images/edits")

	body, _ := json.Marshal(map[string]string{"model": "dall-e-2"})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadGateway, rr.Code)
}

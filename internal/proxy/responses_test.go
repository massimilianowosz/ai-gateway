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
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

type mockResponsesProvider struct {
	completeResp *provider.CompletionResponse
	completeErr  error
	streamChunks [][]byte
	streamErr    error
	lastReq      *provider.CompletionRequest
	// completeGate, when set, holds Complete until the channel is closed and
	// then answers normally — a provider that finishes its work regardless of
	// the caller having given up.
	completeGate chan struct{}
}

func (m *mockResponsesProvider) Name() string { return "mock" }

func (m *mockResponsesProvider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.lastReq = req
	if m.completeGate != nil {
		<-m.completeGate
	}
	return m.completeResp, m.completeErr
}

func (m *mockResponsesProvider) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.StreamReader, error) {
	m.lastReq = req
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return &mockAnthropicStreamReader{chunks: m.streamChunks}, nil
}

func TestResponsesHandler_Complete_PlainStringInput(t *testing.T) {
	stop := "stop"
	mock := &mockResponsesProvider{
		completeResp: &provider.CompletionResponse{
			ID:      "chatcmpl-1",
			Object:  "chat.completion",
			Model:   "gpt-5.1-codex-max",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "Hello!"}, FinishReason: &stop}},
			Usage:   &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}

	registry := newTestRegistry("azure-gpt-5-1-codex-max", mock)
	rt := newTestRouter(registry)
	handler := NewResponsesHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"azure-gpt-5-1-codex-max","input":"Hi","instructions":"be nice"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)
	require.Len(t, mock.lastReq.Messages, 2)
	assert.Equal(t, "system", mock.lastReq.Messages[0].Role)
	assert.Equal(t, "be nice", mock.lastReq.Messages[0].Content)
	assert.Equal(t, "user", mock.lastReq.Messages[1].Role)
	assert.Equal(t, "Hi", mock.lastReq.Messages[1].Content)

	var resp responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "response", resp.Object)
	assert.Equal(t, "completed", resp.Status)
	require.Len(t, resp.Output, 1)
	assert.Equal(t, 10, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.OutputTokens)
}

func TestResponsesHandler_Complete_ToolCallRoundTrip(t *testing.T) {
	toolCalls := "tool_calls"
	mock := &mockResponsesProvider{
		completeResp: &provider.CompletionResponse{
			ID:     "chatcmpl-2",
			Object: "chat.completion",
			Model:  "gpt-5.1-codex-max",
			Choices: []provider.Choice{{
				Index: 0,
				Message: &provider.Message{
					Role: "assistant",
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
		},
	}

	registry := newTestRegistry("azure-gpt-5-1-codex-max", mock)
	rt := newTestRouter(registry)
	handler := NewResponsesHandler(registry, rt, slog.Default(), nil, nil, nil)

	// Input replays a prior function_call + function_call_output turn.
	body := `{"model":"azure-gpt-5-1-codex-max","input":[
		{"type":"message","role":"user","content":"What's the weather in Rome?"},
		{"type":"function_call","call_id":"call_123","name":"get_weather","arguments":"{\"city\":\"Rome\"}"},
		{"type":"function_call_output","call_id":"call_123","output":"Sunny, 25C"}
	],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)
	require.Len(t, mock.lastReq.Messages, 3)
	assert.Equal(t, "user", mock.lastReq.Messages[0].Role)
	assert.Equal(t, "assistant", mock.lastReq.Messages[1].Role)
	require.Len(t, mock.lastReq.Messages[1].ToolCalls, 1)
	assert.Equal(t, "get_weather", mock.lastReq.Messages[1].ToolCalls[0].Function.Name)
	assert.Equal(t, "tool", mock.lastReq.Messages[2].Role)
	assert.Equal(t, "call_123", mock.lastReq.Messages[2].ToolCallID)
	assert.Equal(t, "Sunny, 25C", mock.lastReq.Messages[2].Content)
	require.Len(t, mock.lastReq.Tools, 1)
	assert.Equal(t, "get_weather", mock.lastReq.Tools[0].Function.Name)

	var resp responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Output, 1)

	var fnCall responsesFunctionCallItem
	raw, _ := json.Marshal(resp.Output[0])
	require.NoError(t, json.Unmarshal(raw, &fnCall))
	assert.Equal(t, "function_call", fnCall.Type)
	assert.Equal(t, "get_weather", fnCall.Name)
}

func TestResponsesHandler_Stream_TextDelta(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{"content":"lo!"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
	}
	mock := &mockResponsesProvider{streamChunks: chunks}

	registry := newTestRegistry("azure-gpt-5-1-codex-max", mock)
	rt := newTestRouter(registry)
	handler := NewResponsesHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"azure-gpt-5-1-codex-max","input":"Hi","stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, "event: response.created")
	assert.Contains(t, out, "event: response.output_text.delta")
	assert.Contains(t, out, `"delta":"Hel"`)
	assert.Contains(t, out, "event: response.completed")
	assert.Contains(t, out, "Hello!")
}

// TestResponsesHandler_Stream_UpstreamError_EmitsResponseFailed guards
// against a regression where an upstream failure discovered after the SSE
// 200 response was already committed (e.g. a ChatGPT/Codex 429 usage-limit
// error) was surfaced as a non-standard `event: error` payload. OpenAI Codex
// CLI's SSE parser only treats response.completed/incomplete/failed as
// turn-terminating events, so that payload was silently ignored and the
// connection then closed with no terminal event — Codex reported the
// generic "stream disconnected before completion: stream closed before
// response.completed" instead of the real "usage limit reached" message.
func TestResponsesHandler_Stream_UpstreamError_EmitsResponseFailed(t *testing.T) {
	mock := &mockResponsesProvider{
		streamErr: &provider.UpstreamError{
			StatusCode: http.StatusTooManyRequests,
			Type:       "usage_limit_reached",
			Message:    "The usage limit has been reached",
		},
	}

	registry := newTestRegistry("codex-gpt-5.5", mock)
	rt := newTestRouter(registry)
	handler := NewResponsesHandler(registry, rt, slog.Default(), nil, nil, nil)

	body := `{"model":"codex-gpt-5.5","input":"Hi","stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, "event: response.failed")
	assert.Contains(t, out, `"status":"failed"`)
	assert.Contains(t, out, `"code":"usage_limit_reached"`)
	assert.Contains(t, out, `"message":"The usage limit has been reached"`)
	assert.NotContains(t, out, "event: error")
}

func TestResponsesHandler_TranslatesFullParameterSurface(t *testing.T) {
	stop := "stop"
	mock := &mockResponsesProvider{
		completeResp: &provider.CompletionResponse{
			ID: "chatcmpl-3", Object: "chat.completion", Model: "m",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "ok"}, FinishReason: &stop}},
		},
	}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	body := `{
		"model":"translated-model",
		"input":"Jane, 54 anni",
		"temperature":0.3,
		"top_p":0.9,
		"max_output_tokens":256,
		"parallel_tool_calls":false,
		"top_logprobs":3,
		"safety_identifier":"user-42",
		"service_tier":"flex",
		"prompt_cache_key":"cache-1",
		"metadata":{"tenant":"acme"},
		"reasoning":{"effort":"high"},
		"truncation":"auto",
		"text":{"format":{"type":"json_schema","name":"person","strict":true,
			"schema":{"type":"object","properties":{"name":{"type":"string"}}}}}
	}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mock.lastReq)

	assert.Equal(t, 0.3, *mock.lastReq.Temperature)
	assert.Equal(t, 256, *mock.lastReq.MaxTokens)
	assert.Equal(t, "user-42", mock.lastReq.User)
	assert.Equal(t, "high", mock.lastReq.Extra["reasoning_effort"])
	assert.Equal(t, "flex", mock.lastReq.Extra["service_tier"])
	assert.Equal(t, "cache-1", mock.lastReq.Extra["prompt_cache_key"])
	assert.Equal(t, false, mock.lastReq.Extra["parallel_tool_calls"])
	assert.Equal(t, true, mock.lastReq.Extra["logprobs"])
	assert.Equal(t, 3, mock.lastReq.Extra["top_logprobs"])

	// text.format is flat in Responses but nested under json_schema in chat.
	format, ok := mock.lastReq.ResponseFormat.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "json_schema", format["type"])
	schema, ok := format["json_schema"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "person", schema["name"])
	assert.Equal(t, true, schema["strict"])
	assert.NotNil(t, schema["schema"])

	// The response echoes the request parameters back, as the SDK expects.
	var resp responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.ParallelToolCalls)
	assert.Equal(t, "auto", resp.Truncation)
	assert.Equal(t, "flex", resp.ServiceTier)
	assert.Equal(t, map[string]string{"tenant": "acme"}, resp.Metadata)
	assert.Equal(t, "high", resp.Reasoning.Effort)
	require.NotNil(t, resp.OutputText)
	assert.Equal(t, "ok", *resp.OutputText)
	assert.True(t, resp.Store)
}

func TestResponsesHandler_UnsupportedBuiltinToolIsRejected(t *testing.T) {
	mock := &mockResponsesProvider{}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	body := `{"model":"translated-model","input":"cerca","tools":[{"type":"web_search"}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// A silent drop would leave the caller wondering why the tool was ignored.
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "web_search")
	assert.Nil(t, mock.lastReq)
}

func TestResponsesHandler_Complete_ReasoningAndRefusalItems(t *testing.T) {
	stop := "stop"
	mock := &mockResponsesProvider{
		completeResp: &provider.CompletionResponse{
			ID: "chatcmpl-4", Object: "chat.completion", Model: "m",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{
				Role:             "assistant",
				Content:          "",
				ReasoningContent: "L'utente chiede qualcosa di vietato.",
				Refusal:          "Non posso aiutarti con questo.",
			}, FinishReason: &stop}},
			Usage: &provider.Usage{
				PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20,
				PromptTokensDetails:     &provider.PromptTokensDetails{CachedTokens: 4},
				CompletionTokensDetails: &provider.CompletionTokensDetails{ReasoningTokens: 5},
			},
		},
	}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"translated-model","input":"..."}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Output []map[string]interface{} `json:"output"`
		Usage  responsesUsage           `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Output, 2)
	assert.Equal(t, "reasoning", resp.Output[0]["type"])
	assert.Equal(t, "message", resp.Output[1]["type"])

	content, ok := resp.Output[1]["content"].([]interface{})
	require.True(t, ok)
	require.Len(t, content, 1)
	assert.Equal(t, "refusal", content[0].(map[string]interface{})["type"])

	require.NotNil(t, resp.Usage.InputTokenDetails)
	assert.Equal(t, 4, resp.Usage.InputTokenDetails.CachedTokens)
	require.NotNil(t, resp.Usage.OutputTokenDetails)
	assert.Equal(t, 5, resp.Usage.OutputTokenDetails.ReasoningTokens)
}

func TestResponsesHandler_Stream_ReasoningAndIncompleteTerminalEvent(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"Sto pensando"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{"content":"Ciao"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
	}
	mock := &mockResponsesProvider{streamChunks: chunks}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"translated-model","input":"Hi","stream":true}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, "event: response.reasoning_summary_text.delta")
	assert.Contains(t, out, `"delta":"Sto pensando"`)
	assert.Contains(t, out, "event: response.reasoning_summary_text.done")
	// finish_reason "length" must terminate with response.incomplete, not
	// response.completed carrying an incomplete status.
	assert.Contains(t, out, "event: response.incomplete")
	assert.NotContains(t, out, "event: response.completed")
	assert.Contains(t, out, `"reason":"max_output_tokens"`)
}

func TestResponsesHandler_Stream_SequenceNumbersAreMonotonic(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"content":"a"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{"content":"b"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}
	mock := &mockResponsesProvider{streamChunks: chunks}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"translated-model","input":"Hi","stream":true}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var seqs []int
	for _, line := range strings.Split(w.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			SequenceNumber int `json:"sequence_number"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &event))
		seqs = append(seqs, event.SequenceNumber)
	}
	require.NotEmpty(t, seqs)
	for i, seq := range seqs {
		assert.Equal(t, i, seq, "sequence_number must increase by one per event")
	}
}

func TestResponsesHandler_Complete_CarriesLogprobs(t *testing.T) {
	stop := "stop"
	mock := &mockResponsesProvider{completeResp: &provider.CompletionResponse{
		ID: "chatcmpl-lp", Object: "chat.completion", Model: "m",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      &provider.Message{Role: "assistant", Content: "Ciao"},
			FinishReason: &stop,
			Logprobs: &provider.ChoiceLogprobs{Content: []json.RawMessage{
				json.RawMessage(`{"token":"Ciao","logprob":-0.25,"top_logprobs":[{"token":"Ciao","logprob":-0.25}]}`),
			}},
		}},
	}}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"translated-model","input":"Ciao","top_logprobs":1}`)))
	require.Equal(t, http.StatusOK, w.Code)

	// The caller asked for token probabilities; they belong on the output_text
	// part, not only on the upstream request.
	assert.Contains(t, mock.lastReq.Extra, "top_logprobs")
	assert.Contains(t, w.Body.String(), `"logprobs":[{"token":"Ciao"`)
}

func TestResponsesHandler_Stream_CarriesLogprobs(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"content":"Ciao"},"logprobs":{"content":[{"token":"Ciao","logprob":-0.25}]}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}
	mock := &mockResponsesProvider{streamChunks: chunks}
	registry := newTestRegistry("translated-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"translated-model","input":"Ciao","stream":true,"top_logprobs":1}`)))

	// The tokens ride the delta they belong to, and then travel with the
	// finished output_text part everywhere that part is echoed.
	var carrying []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || !strings.Contains(data, `"token":"Ciao"`) {
			continue
		}
		var event struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &event))
		carrying = append(carrying, event.Type)
	}

	assert.Equal(t, []string{
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}, carrying)
}

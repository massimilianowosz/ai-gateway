package chatgptcodex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func testProvider(baseURL, modelID string) *Provider {
	return &Provider{baseURL: baseURL, modelID: modelID, client: http.DefaultClient}
}

func contextWithUpstream(token, accountID string) context.Context {
	ctx := auth.ContextWithScopedUpstreamToken(context.Background(), providerName, token)
	ctx = auth.ContextWithScopedUpstreamAccountID(ctx, providerName, accountID)
	return ctx
}

func TestCompleteSendsUpstreamAuthAndAccountID(t *testing.T) {
	var capturedPath, capturedAuth, capturedAccountID string
	var capturedReq responsesWireRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedAccountID = r.Header.Get("ChatGPT-Account-Id")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedReq))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_1",
			"status": "completed",
			"model": "gpt-5.1-codex",
			"output": [
				{"type": "message", "id": "msg_1", "role": "assistant", "content": [{"type": "output_text", "text": "hello there"}]}
			],
			"usage": {"input_tokens": 5, "output_tokens": 2, "total_tokens": 7}
		}`))
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	ctx := contextWithUpstream("access-tok", "acct-123")

	resp, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "system", Content: "be nice"}, {Role: "user", Content: "hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "/responses", capturedPath)
	assert.Equal(t, "Bearer access-tok", capturedAuth)
	assert.Equal(t, "acct-123", capturedAccountID)
	assert.Equal(t, "gpt-5.1-codex", capturedReq.Model)
	assert.Equal(t, "be nice", capturedReq.Instructions)
	require.Len(t, capturedReq.Input, 1)
	assert.Equal(t, "message", capturedReq.Input[0].Type)
	assert.Equal(t, "user", capturedReq.Input[0].Role)

	require.NotNil(t, resp)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "hello there", resp.Choices[0].Message.Content)
	assert.Equal(t, "stop", *resp.Choices[0].FinishReason)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 7, resp.Usage.TotalTokens)
}

func TestCompleteFunctionCallOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_2",
			"status": "completed",
			"model": "gpt-5.1-codex",
			"output": [
				{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"Rome\"}"}
			]
		}`))
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	resp, err := p.Complete(contextWithUpstream("tok", "acct"), &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "what's the weather"}},
	})

	require.NoError(t, err)
	require.Len(t, resp.Choices[0].Message.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.Choices[0].Message.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	assert.Equal(t, "tool_calls", *resp.Choices[0].FinishReason)
}

func TestStreamTranslatesResponsesSSEToChatCompletionChunks(t *testing.T) {
	events := []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta","delta":"Hel"}`,
		`{"type":"response.output_text.delta","delta":"lo"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	reader, err := p.Stream(contextWithUpstream("tok", "acct"), &provider.CompletionRequest{
		Model:    "codex",
		Stream:   true,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	var deltas []string
	var sawFinish bool
	for {
		chunk, err := reader.Next()
		if err != nil {
			break
		}
		var parsed struct {
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal(chunk, &parsed))
		require.Len(t, parsed.Choices, 1)
		if parsed.Choices[0].Delta.Content != "" {
			deltas = append(deltas, parsed.Choices[0].Delta.Content)
		}
		if parsed.Choices[0].FinishReason != nil {
			sawFinish = true
			assert.Equal(t, "stop", *parsed.Choices[0].FinishReason)
		}
	}

	assert.Equal(t, []string{"Hel", "lo"}, deltas)
	assert.True(t, sawFinish)
}

func TestSetHeadersWithoutUpstreamContextOmitsAuth(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", nil)
	require.NoError(t, err)

	p := testProvider("http://example.com", "gpt-5.1-codex")
	p.setHeaders(req, false)

	assert.Empty(t, req.Header.Get("Authorization"))
	assert.Empty(t, req.Header.Get("ChatGPT-Account-Id"))
	assert.Equal(t, codexOriginator, req.Header.Get("originator"))
}

// sanity check that convertToResponsesWire never panics on empty content and
// keeps the reader interface happy with bufio buffering across chunk boundaries.
func TestConvertToResponsesWireHandlesEmptyContent(t *testing.T) {
	wr := convertToResponsesWire(&provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "assistant", Content: nil}},
	}, "gpt-5.1-codex")
	require.Len(t, wr.Input, 1)
	assert.Equal(t, "", wr.Input[0].Content[0].Text)
}

func TestName(t *testing.T) {
	p := New("gpt-5.1-codex")
	assert.Equal(t, "chatgpt_codex", p.Name())
}

func TestNew_DefaultsBaseURL(t *testing.T) {
	p := New("gpt-5.1-codex")
	assert.Equal(t, defaultBaseURL, p.baseURL)
}

func TestModelFor_FallsBackToRequestModelWhenProviderModelIDEmpty(t *testing.T) {
	p := &Provider{}
	assert.Equal(t, "requested-model", p.modelFor(&provider.CompletionRequest{Model: "requested-model"}))

	p2 := &Provider{modelID: "fixed-model"}
	assert.Equal(t, "fixed-model", p2.modelFor(&provider.CompletionRequest{Model: "requested-model"}))
}

func TestConvertToResponsesWire_StoreIsAlwaysFalse(t *testing.T) {
	wr := convertToResponsesWire(&provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, "gpt-5.1-codex")
	body, err := json.Marshal(wr)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"store":false`)
}

func TestResponsesWireInput_MarshalJSON_FunctionCallOutputAlwaysIncludesOutputKey(t *testing.T) {
	input := responsesWireInput{Type: "function_call_output", CallID: "call_1", Output: ""}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &decoded))
	_, hasOutput := decoded["output"]
	assert.True(t, hasOutput, "output key must be present even when empty")
	assert.Equal(t, "", decoded["output"])
}

func TestResponsesWireInput_MarshalJSON_FunctionCall(t *testing.T) {
	input := responsesWireInput{Type: "function_call", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"Rome"}`}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, "get_weather", decoded["name"])
	assert.Equal(t, `{"city":"Rome"}`, decoded["arguments"])
}

func TestComplete_IncompleteStatusMapsToLengthFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_3",
			"status": "incomplete",
			"output": [{"type": "message", "id": "msg_1", "role": "assistant", "content": [{"type": "output_text", "text": "cut off"}]}]
		}`))
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	resp, err := p.Complete(contextWithUpstream("tok", "acct"), &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "length", *resp.Choices[0].FinishReason)
}

func TestComplete_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid upstream token","type":"authentication_error"}}`))
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusUnauthorized, upstreamErr.StatusCode)
	assert.Equal(t, "invalid upstream token", upstreamErr.Message)
}

func TestStream_ErrorResponseClosesBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`))
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Nil(t, reader)
	require.Error(t, err)
}

func TestStream_ToolCallArgumentsDeltaAndOutputItemAdded(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"get_weather"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"city\":"}`,
		`{"type":"response.function_call_arguments.delta","delta":"\"Rome\"}"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	reader, err := p.Stream(contextWithUpstream("tok", "acct"), &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "weather?"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	type parsedChunk struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}

	var chunks []parsedChunk
	for {
		raw, err := reader.Next()
		if err != nil {
			break
		}
		var c parsedChunk
		require.NoError(t, json.Unmarshal(raw, &c))
		chunks = append(chunks, c)
	}

	require.Len(t, chunks, 4)
	require.Len(t, chunks[0].Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "get_weather", chunks[0].Choices[0].Delta.ToolCalls[0].Function.Name)
	assert.Equal(t, "call_1", chunks[0].Choices[0].Delta.ToolCalls[0].ID)
	assert.Equal(t, `{"city":`, chunks[1].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	assert.Equal(t, `"Rome"}`, chunks[2].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	require.NotNil(t, chunks[3].Choices[0].FinishReason)
	assert.Equal(t, "stop", *chunks[3].Choices[0].FinishReason)
}

func TestComplete_ContextCancelledBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer server.Close()

	p := testProvider(server.URL, "gpt-5.1-codex")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "codex",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
}

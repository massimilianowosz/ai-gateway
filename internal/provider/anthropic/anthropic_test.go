package anthropic

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

func TestProvider_Name(t *testing.T) {
	p := New("key", "claude-3-opus")
	assert.Equal(t, "anthropic", p.Name())
}

func TestComplete_DirectAPIKey_UsesXAPIKeyHeader(t *testing.T) {
	var capturedXAPIKey, capturedAuth, capturedVersion string
	var capturedReq anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedXAPIKey = r.Header.Get("x-api-key")
		capturedAuth = r.Header.Get("Authorization")
		capturedVersion = r.Header.Get("anthropic-version")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedReq))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			Content:    []anthropicContentBlock{{Type: "text", Text: "hello"}},
			Model:      "claude-3-opus",
			StopReason: "end_turn",
			Usage:      &anthropicUsage{InputTokens: 10, OutputTokens: 5},
		})
	}))
	defer srv.Close()

	p := New("test-key", "claude-3-opus")
	p.baseURL = srv.URL

	resp, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "test-key", capturedXAPIKey)
	assert.Empty(t, capturedAuth, "direct API key mode must not set Authorization Bearer")
	assert.Equal(t, defaultVersion, capturedVersion)
	assert.Equal(t, defaultMaxTokens, capturedReq.MaxTokens)
	assert.Equal(t, "hello", resp.Choices[0].Message.Content)
	assert.Equal(t, "stop", *resp.Choices[0].FinishReason)
	assert.Equal(t, 15, resp.Usage.TotalTokens)
}

func TestComplete_OAuthPassthrough_SendsBillingAndIdentityBlocksFirst(t *testing.T) {
	var capturedAuth, capturedBeta, capturedDirectAccess string
	var capturedReq anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedBeta = r.Header.Get("anthropic-beta")
		capturedDirectAccess = r.Header.Get("anthropic-dangerous-direct-browser-access")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedReq))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{
			ID:         "msg_1",
			Content:    []anthropicContentBlock{{Type: "text", Text: "hi"}},
			StopReason: "end_turn",
		})
	}))
	defer srv.Close()

	p := New("unused-key", "claude-3-opus")
	p.baseURL = srv.URL

	ctx := auth.ContextWithScopedUpstreamToken(context.Background(), "anthropic", "oauth-access-token")
	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "system", Content: "be nice"}, {Role: "user", Content: "hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "Bearer oauth-access-token", capturedAuth)
	assert.Contains(t, capturedBeta, "oauth-2025-04-20")
	assert.Equal(t, "true", capturedDirectAccess)

	// System must be a []anthropicSystemBlock with billing block first,
	// identity block second, and the user-supplied system prompt last.
	blocks, ok := capturedReq.System.([]interface{})
	require.True(t, ok, "system must be an array of blocks in OAuth passthrough mode")
	require.Len(t, blocks, 3)
	first := blocks[0].(map[string]interface{})
	second := blocks[1].(map[string]interface{})
	third := blocks[2].(map[string]interface{})
	assert.Contains(t, first["text"], "x-anthropic-billing-header")
	assert.Contains(t, second["text"], "Claude Code")
	assert.Equal(t, "be nice", third["text"])
}

func TestComplete_NoOAuthToken_UsesXAPIKeyEvenWithSystemPrompt(t *testing.T) {
	var capturedReq anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedReq))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{Content: []anthropicContentBlock{{Type: "text", Text: "hi"}}, StopReason: "end_turn"})
	}))
	defer srv.Close()

	p := New("test-key", "claude-3-opus")
	p.baseURL = srv.URL
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "system", Content: "be nice"}, {Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)

	// Plain string system, not billing-wrapped blocks.
	sysStr, ok := capturedReq.System.(string)
	require.True(t, ok)
	assert.Equal(t, "be nice", sysStr)
}

func TestNewAzure_UsesBearerAuthAndModelMeshHeader_IgnoringUpstreamToken(t *testing.T) {
	var capturedAuth, capturedXAPIKey, capturedModelMesh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedXAPIKey = r.Header.Get("x-api-key")
		capturedModelMesh = r.Header.Get("x-ms-model-mesh-model-name")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{Content: []anthropicContentBlock{{Type: "text", Text: "hi"}}, StopReason: "end_turn"})
	}))
	defer srv.Close()

	p := NewAzure("azure-key", srv.URL, "claude-3-opus-on-azure")

	// Even if a scoped upstream OAuth token is present, Azure mode must
	// ignore it and always use the configured API key as a Bearer token.
	ctx := auth.ContextWithScopedUpstreamToken(context.Background(), "anthropic", "oauth-access-token")
	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "Bearer azure-key", capturedAuth)
	assert.Empty(t, capturedXAPIKey)
	assert.Equal(t, "claude-3-opus-on-azure", capturedModelMesh)
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"stop_sequence": "stop",
		"unknown_thing": "stop",
	}
	for in, want := range cases {
		assert.Equal(t, want, mapStopReason(in), "reason=%s", in)
	}
}

func TestComplete_MaxTokensStopReason_MapsToLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{
			Content:    []anthropicContentBlock{{Type: "text", Text: "truncated"}},
			StopReason: "max_tokens",
		})
	}))
	defer srv.Close()

	p := New("key", "claude-3-opus")
	p.baseURL = srv.URL
	resp, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "length", *resp.Choices[0].FinishReason)
}

func TestParseErrorResponse_StructuredJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"type": "rate_limit_error", "message": "slow down"},
		})
	}))
	defer srv.Close()

	p := New("key", "claude-3-opus")
	p.baseURL = srv.URL
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusTooManyRequests, upstreamErr.StatusCode)
	assert.Equal(t, "slow down", upstreamErr.Message)
}

func TestParseErrorResponse_UnstructuredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("upstream is on fire"))
	}))
	defer srv.Close()

	p := New("key", "claude-3-opus")
	p.baseURL = srv.URL
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusBadGateway, upstreamErr.StatusCode)
	assert.Contains(t, upstreamErr.Message, "upstream is on fire")
}

func TestStream_ErrorResponseClosesBodyAndReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"type": "authentication_error", "message": "invalid key"},
		})
	}))
	defer srv.Close()

	p := New("key", "claude-3-opus")
	p.baseURL = srv.URL
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Nil(t, reader)
	require.Error(t, err)
}

func TestStream_TextDeltaChunks(t *testing.T) {
	sseBody := "" +
		"data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseBody))
	}))
	defer srv.Close()

	p := New("key", "claude-3-opus")
	p.baseURL = srv.URL
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	var texts []string
	var sawRoleChunk, sawFinish bool
	for {
		chunk, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		var decoded struct {
			Choices []struct {
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal(chunk, &decoded))
		c := decoded.Choices[0]
		if c.Delta.Role == "assistant" {
			sawRoleChunk = true
		}
		if c.Delta.Content != "" {
			texts = append(texts, c.Delta.Content)
		}
		if c.FinishReason != nil {
			assert.Equal(t, "stop", *c.FinishReason)
			sawFinish = true
		}
	}

	assert.True(t, sawRoleChunk)
	assert.Equal(t, []string{"Hello", " world"}, texts)
	assert.True(t, sawFinish)
}

func TestStream_ToolUseIncrementalArguments(t *testing.T) {
	sseBody := "" +
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"get_weather\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"loc\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"ation\\\":\\\"NYC\\\"}\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseBody))
	}))
	defer srv.Close()

	p := New("key", "claude-3-opus")
	p.baseURL = srv.URL
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "weather in NYC"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	var toolCallStarted bool
	var argFragments []string
	for {
		chunk, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		var decoded struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id,omitempty"`
						Function struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal(chunk, &decoded))
		if len(decoded.Choices[0].Delta.ToolCalls) == 0 {
			continue
		}
		tc := decoded.Choices[0].Delta.ToolCalls[0]
		if tc.Function.Name == "get_weather" {
			toolCallStarted = true
		}
		if tc.Function.Arguments != "" {
			argFragments = append(argFragments, tc.Function.Arguments)
		}
	}

	assert.True(t, toolCallStarted)
	require.Len(t, argFragments, 2)
	// Reassembling the incremental partial_json fragments must yield valid JSON.
	full := argFragments[0] + argFragments[1]
	var args map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(full), &args))
	assert.Equal(t, "NYC", args["location"])
}

func TestStream_ContextCancelledMidRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := New("key", "claude-3-opus")
	p.baseURL = "http://127.0.0.1:0"
	_, err := p.Stream(ctx, &provider.CompletionRequest{
		Model:    "claude-3-opus",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
}

func TestExtractStopSequences(t *testing.T) {
	assert.Equal(t, []string{"STOP"}, extractStopSequences("STOP"))
	assert.Equal(t, []string{"A", "B"}, extractStopSequences([]interface{}{"A", "B"}))
	assert.Nil(t, extractStopSequences(42))
}

func TestParseDataURI(t *testing.T) {
	mime, data, ok := parseDataURI("data:image/png;base64,AAAA")
	assert.True(t, ok)
	assert.Equal(t, "image/png", mime)
	assert.Equal(t, "AAAA", data)

	_, _, ok = parseDataURI("https://example.com/image.png")
	assert.False(t, ok)
}

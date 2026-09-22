package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// mockNativeResponsesProvider implements provider.ResponsesAPI so /v1/responses
// takes the native passthrough path.
type mockNativeResponsesProvider struct {
	upstreamBody []byte
	status       int
	contentType  string
	payload      string
}

func (m *mockNativeResponsesProvider) Name() string { return "mock-native" }

func (m *mockNativeResponsesProvider) Complete(context.Context, *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, assertUnreachable
}

func (m *mockNativeResponsesProvider) Stream(context.Context, *provider.CompletionRequest) (provider.StreamReader, error) {
	return nil, assertUnreachable
}

func (m *mockNativeResponsesProvider) DoResponsesRequest(_ context.Context, body io.Reader, _ int64) (*http.Response, error) {
	m.upstreamBody, _ = io.ReadAll(body)
	status := m.status
	if status == 0 {
		status = http.StatusOK
	}
	contentType := m.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	return jsonHTTPResponse(status, contentType, m.payload), nil
}

// assertUnreachable marks provider methods the native path must never call.
var assertUnreachable = errNativePathOnly{}

type errNativePathOnly struct{}

func (errNativePathOnly) Error() string {
	return "translated path used for a provider that speaks Responses natively"
}

func newNativeResponsesRegistry(t *testing.T, model, providerModel string, p provider.Provider) *provider.Registry {
	t.Helper()
	reg, err := provider.NewRegistry([]config.ModelConfig{{
		Name:          model,
		Provider:      "mock",
		ProviderModel: providerModel,
	}}, &staticFactory{provider: p})
	require.NoError(t, err)
	return reg
}

func TestResponsesHandler_NativePassthroughPreservesUnmodelledFields(t *testing.T) {
	mock := &mockNativeResponsesProvider{payload: `{
		"id":"resp-native","object":"response","created_at":1700000000,
		"status":"completed","model":"upstream-model","parallel_tool_calls":true,
		"output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}
	}`}
	registry := newNativeResponsesRegistry(t, "gw-model", "upstream-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	// Every field below is absent from responsesRequest: the point of the test
	// is that they survive because the raw body is forwarded, not re-marshalled.
	body := `{
		"model":"gw-model",
		"input":"ciao",
		"reasoning":{"effort":"high","summary":"auto"},
		"text":{"format":{"type":"json_schema","name":"p","strict":true,"schema":{"type":"object"}}},
		"background":true,
		"store":false,
		"include":["reasoning.encrypted_content"],
		"metadata":{"tenant":"acme"},
		"truncation":"auto",
		"tools":[{"type":"web_search"}]
	}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, mock.upstreamBody)

	var upstream map[string]interface{}
	require.NoError(t, json.Unmarshal(mock.upstreamBody, &upstream))

	// Only the model is rewritten; everything else reaches the provider intact.
	assert.Equal(t, "upstream-model", upstream["model"])
	assert.Equal(t, map[string]interface{}{"effort": "high", "summary": "auto"}, upstream["reasoning"])
	assert.Equal(t, true, upstream["background"])
	assert.Equal(t, false, upstream["store"])
	assert.Equal(t, "auto", upstream["truncation"])
	assert.Equal(t, []interface{}{"reasoning.encrypted_content"}, upstream["include"])
	assert.Equal(t, map[string]interface{}{"tenant": "acme"}, upstream["metadata"])
	assert.NotNil(t, upstream["text"])
	require.Len(t, upstream["tools"], 1)
	assert.Equal(t, "web_search", upstream["tools"].([]interface{})[0].(map[string]interface{})["type"])

	// The gateway model name is restored on the way back to the client.
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "gw-model", resp["model"])
}

func TestResponsesHandler_NativeStreamRelaysVerbatimAndRecordsUsage(t *testing.T) {
	// The stream deliberately contains an event type the gateway does not
	// model: it must still reach the client untouched.
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp-1\"}}\n\n" +
		"event: response.web_search_call.searching\n" +
		"data: {\"type\":\"response.web_search_call.searching\",\"sequence_number\":1,\"item_id\":\"ws_1\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp-1\",\"model\":\"upstream-model\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":4,\"total_tokens\":15}}}\n\n"

	mock := &mockNativeResponsesProvider{contentType: "text/event-stream", payload: stream}
	registry := newNativeResponsesRegistry(t, "gw-model", "upstream-model", mock)
	tokens := &recordingTokenRecorder{}
	spender := spend.NewBatchWriter(nil, slog.Default(), time.Hour)
	t.Cleanup(func() { _ = spender.Close() })

	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, spender, tokens)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"ciao","stream":true}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()

	// Unmodelled events relayed byte-for-byte.
	assert.Contains(t, out, "event: response.web_search_call.searching\n")
	assert.Contains(t, out, `"item_id":"ws_1"`)
	assert.Contains(t, out, "event: response.completed\n")
	// Terminal event carries the gateway model name, not the upstream one.
	assert.Contains(t, out, `"model":"gw-model"`)
	assert.NotContains(t, out, `"model":"upstream-model"`)
	// sequence_number is preserved even though the event was re-encoded.
	assert.Contains(t, out, `"sequence_number":2`)

	// Usage extracted from the terminal event feeds spend and metrics, which a
	// plain io.Copy relay would have lost.
	assert.Equal(t, 11, tokens.prompt)
	assert.Equal(t, 4, tokens.completion)
}

// The cache breakdown is what says whether a long conversation is being
// re-read at full price every turn or served from the provider's prefix cache.
// It arrives nested under input_tokens_details, and a parser that reads only
// the top-level counters reports every turn as a full miss — which is both a
// wrong cost and a blind spot exactly where a caching problem would show.
func TestResponsesHandler_NativeStreamRecordsCachedPromptTokens(t *testing.T) {
	stream := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"upstream-model\"," +
		"\"usage\":{\"input_tokens\":10000,\"output_tokens\":50,\"total_tokens\":10050," +
		"\"input_tokens_details\":{\"cached_tokens\":9500}}}}\n\n"

	mock := &mockNativeResponsesProvider{contentType: "text/event-stream", payload: stream}
	registry := newNativeResponsesRegistry(t, "gw-model", "upstream-model", mock)
	tokens := &recordingTokenRecorder{}
	spender := spend.NewBatchWriter(nil, slog.Default(), time.Hour)
	t.Cleanup(func() { _ = spender.Close() })

	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, spender, tokens)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gw-model","input":"ciao","stream":true}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 9500, tokens.cached, "cached prompt tokens were dropped on the way through")
}

// On a reasoning model the thinking is most of what the turn produced and is
// billed at the output rate, but it arrives nested under
// output_tokens_details. Reading only the top-level output_tokens makes a turn
// that thought for minutes look like a handful of tokens.
func TestNativeResponsesUsage_CarriesReasoningTokens(t *testing.T) {
	raw := []byte(`{"input_tokens":10000,"output_tokens":8000,"total_tokens":18000,
		"input_tokens_details":{"cached_tokens":9500},
		"output_tokens_details":{"reasoning_tokens":7800}}`)

	usage := nativeResponsesUsage(raw)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.CompletionTokens)
	assert.Equal(t, 9500, usage.CachedTokens())
	require.NotNil(t, usage.CompletionTokensDetails, "reasoning tokens were dropped")
	assert.Equal(t, 7800, usage.CompletionTokensDetails.ReasoningTokens)
}

type recordingTokenRecorder struct {
	prompt     int
	completion int
	cached     int
	created    int
}

func (r *recordingTokenRecorder) RecordCacheTokens(cached, created int) {
	r.cached += cached
	r.created += created
}

func (r *recordingTokenRecorder) RecordTokens(promptTokens, completionTokens int) {
	r.prompt += promptTokens
	r.completion += completionTokens
}

// mockChatOnlyResponsesProvider is the shape that caused the bug this test
// guards: a client type that implements provider.ResponsesAPI (because it also
// serves the real OpenAI endpoint) while pointing at a backend that only
// speaks chat completions.
type mockChatOnlyResponsesProvider struct {
	mockResponsesProvider
	nativeCalled bool
}

func (m *mockChatOnlyResponsesProvider) DoResponsesRequest(context.Context, io.Reader, int64) (*http.Response, error) {
	m.nativeCalled = true
	return jsonHTTPResponse(http.StatusNotFound, "application/json", `{"error":{"message":"Unknown request URL"}}`), nil
}

func (m *mockChatOnlyResponsesProvider) SupportsNativeResponses() bool { return false }

func TestResponsesHandler_ChatOnlyProviderIsNotTreatedAsNative(t *testing.T) {
	mock := &mockChatOnlyResponsesProvider{
		mockResponsesProvider: mockResponsesProvider{completeResp: textCompletion("tradotto")},
	}
	registry := newNativeResponsesRegistry(t, "compat-model", "upstream-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), nil, nil, nil)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"compat-model","input":"Ciao"}`))

	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, mock.nativeCalled, "a chat-only backend must not receive a Responses request")
	require.NotNil(t, mock.lastReq, "the request must go through the chat completions translation")

	var out responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.NotNil(t, out.OutputText)
	assert.Equal(t, "tradotto", *out.OutputText)
}

// mockStatefulNativeProvider speaks both halves of the native Responses API,
// so retrieve/delete/cancel are proxied upstream.
type mockStatefulNativeProvider struct {
	mockNativeResponsesProvider
	resourceCalls []string
}

func (m *mockStatefulNativeProvider) SupportsNativeResponses() bool { return true }

func (m *mockStatefulNativeProvider) DoResponsesResourceRequest(_ context.Context, method, path string, _ io.Reader) (*http.Response, error) {
	m.resourceCalls = append(m.resourceCalls, method+" "+path)
	return jsonHTTPResponse(http.StatusOK, "application/json",
		`{"id":"resp-native","object":"response","deleted":true}`), nil
}

func TestResponsesHandler_DeletingANativeResponseDropsTheLocalRow(t *testing.T) {
	mock := &mockStatefulNativeProvider{mockNativeResponsesProvider: mockNativeResponsesProvider{payload: `{
		"id":"resp-native","object":"response","created_at":1700000000,
		"status":"completed","model":"upstream-model","output":[]
	}`}}
	db := openResponsesTestStore(t)
	registry := newNativeResponsesRegistry(t, "native-model", "upstream-model", mock)
	handler := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), db, nil, nil)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"native-model","input":"Ciao"}`))
	require.Equal(t, http.StatusOK, w.Code)

	rs, ok := db.(store.ResponseStore)
	require.True(t, ok)
	stored, err := rs.GetResponse(context.Background(), "resp-native", "key:key-hash-1")
	require.NoError(t, err)
	require.NotNil(t, stored)

	del := httptest.NewRecorder()
	delReq := authedResponsesRequest("DELETE", "/v1/responses/resp-native", "")
	delReq.SetPathValue("response_id", "resp-native")
	handler.ServeObject(del, delReq)

	require.Equal(t, http.StatusOK, del.Code)
	assert.Equal(t, []string{"DELETE /resp-native"}, mock.resourceCalls)
	assert.Contains(t, del.Body.String(), `"deleted":true`)

	// The gateway's own copy must go too, otherwise it outlives the delete and
	// no later call can reach it.
	gone, err := rs.GetResponse(context.Background(), "resp-native", "key:key-hash-1")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

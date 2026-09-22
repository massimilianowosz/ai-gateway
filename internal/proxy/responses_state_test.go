package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func openResponsesTestStore(t *testing.T) store.Store {
	t.Helper()
	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "responses.db"),
	})
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background()))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// authedResponsesRequest carries key info so the handler can scope stored
// responses to a tenant, the same way the auth middleware does in production.
// testResponsesOwnerID is the owner fileOwnerID derives from the key that
// authedResponsesRequest attaches.
const testResponsesOwnerID = "key:key-hash-1"

func authedResponsesRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	ctx := auth.ContextWithKeyInfo(req.Context(), &store.APIKey{
		KeyHash: "key-hash-1", KeyPrefix: "sk-test", Active: true, Budget: 100,
	})
	return req.WithContext(ctx)
}

func newStatefulResponsesHandler(t *testing.T, model string, p provider.Provider) (*ResponsesHandler, store.Store) {
	t.Helper()
	db := openResponsesTestStore(t)
	registry := newTestRegistry(model, p)
	return NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), db, nil, nil), db
}

func textCompletion(text string) *provider.CompletionResponse {
	stop := "stop"
	return &provider.CompletionResponse{
		ID: "chatcmpl-x", Object: "chat.completion", Model: "m",
		Choices: []provider.Choice{{
			Index: 0, Message: &provider.Message{Role: "assistant", Content: text}, FinishReason: &stop,
		}},
		Usage: &provider.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}
}

func TestResponsesHandler_StoresAndRetrievesResponse(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Ciao!")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Ciao"}`))
	require.Equal(t, http.StatusOK, w.Code)

	var created responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)

	// Retrieve
	get := httptest.NewRecorder()
	req := authedResponsesRequest("GET", "/v1/responses/"+created.ID, "")
	req.SetPathValue("response_id", created.ID)
	handler.ServeObject(get, req)

	require.Equal(t, http.StatusOK, get.Code)
	var retrieved responsesResponse
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &retrieved))
	assert.Equal(t, created.ID, retrieved.ID)
	assert.Equal(t, "completed", retrieved.Status)
	require.NotNil(t, retrieved.OutputText)
	assert.Equal(t, "Ciao!", *retrieved.OutputText)

	// Input items
	items := httptest.NewRecorder()
	itemsReq := authedResponsesRequest("GET", "/v1/responses/"+created.ID+"/input_items", "")
	itemsReq.SetPathValue("response_id", created.ID)
	handler.ServeInputItems(items, itemsReq)

	require.Equal(t, http.StatusOK, items.Code)
	var list responsesItemList
	require.NoError(t, json.Unmarshal(items.Body.Bytes(), &list))
	assert.Equal(t, "list", list.Object)
	require.Len(t, list.Data, 1)
	assert.Equal(t, "message", list.Data[0].(map[string]interface{})["type"])
	assert.NotEmpty(t, list.FirstID)

	// Delete, then the response is gone
	del := httptest.NewRecorder()
	delReq := authedResponsesRequest("DELETE", "/v1/responses/"+created.ID, "")
	delReq.SetPathValue("response_id", created.ID)
	handler.ServeObject(del, delReq)
	require.Equal(t, http.StatusOK, del.Code)
	assert.Contains(t, del.Body.String(), `"deleted":true`)

	missing := httptest.NewRecorder()
	missingReq := authedResponsesRequest("GET", "/v1/responses/"+created.ID, "")
	missingReq.SetPathValue("response_id", created.ID)
	handler.ServeObject(missing, missingReq)
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

func TestResponsesHandler_StoreFalseIsNotPersisted(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Ciao!")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Ciao","store":false}`))
	require.Equal(t, http.StatusOK, w.Code)

	var created responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	assert.False(t, created.Store)

	get := httptest.NewRecorder()
	req := authedResponsesRequest("GET", "/v1/responses/"+created.ID, "")
	req.SetPathValue("response_id", created.ID)
	handler.ServeObject(get, req)
	assert.Equal(t, http.StatusNotFound, get.Code)
}

func TestResponsesHandler_PreviousResponseIDReplaysConversation(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Sono Claude.")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Come ti chiami?"}`))
	require.Equal(t, http.StatusOK, first.Code)

	var created responsesResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &created))

	mock.completeResp = textCompletion("Te l'ho appena detto.")
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Ripeti","previous_response_id":"`+created.ID+`"}`))
	require.Equal(t, http.StatusOK, second.Code)

	// The provider must see the whole conversation, not just the new turn:
	// prior user message, prior assistant answer, new user message.
	require.NotNil(t, mock.lastReq)
	require.Len(t, mock.lastReq.Messages, 3)
	assert.Equal(t, "user", mock.lastReq.Messages[0].Role)
	assert.Equal(t, "Come ti chiami?", mock.lastReq.Messages[0].Content)
	assert.Equal(t, "assistant", mock.lastReq.Messages[1].Role)
	assert.Equal(t, "Sono Claude.", mock.lastReq.Messages[1].Content)
	assert.Equal(t, "user", mock.lastReq.Messages[2].Role)
	assert.Equal(t, "Ripeti", mock.lastReq.Messages[2].Content)

	var chained responsesResponse
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &chained))
	assert.Equal(t, created.ID, chained.PreviousResponseID)
}

func TestResponsesHandler_UnknownPreviousResponseIDIsRejected(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("x")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Ciao","previous_response_id":"resp_missing"}`))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "resp_missing")
	assert.Nil(t, mock.lastReq)
}

func TestResponsesHandler_BackgroundResponseQueuesThenCompletes(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Fatto.")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Lavora","background":true}`))
	require.Equal(t, http.StatusOK, w.Code)

	var queued responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &queued))
	assert.Equal(t, "queued", queued.Status)
	assert.True(t, queued.Background)

	// Poll until the detached turn finishes.
	var final responsesResponse
	require.Eventually(t, func() bool {
		get := httptest.NewRecorder()
		req := authedResponsesRequest("GET", "/v1/responses/"+queued.ID, "")
		req.SetPathValue("response_id", queued.ID)
		handler.ServeObject(get, req)
		if get.Code != http.StatusOK {
			return false
		}
		final = responsesResponse{}
		if json.Unmarshal(get.Body.Bytes(), &final) != nil {
			return false
		}
		return final.Status == "completed"
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, queued.ID, final.ID)
	require.NotNil(t, final.OutputText)
	assert.Equal(t, "Fatto.", *final.OutputText)
}

func TestResponsesHandler_ResponsesAreScopedToTheirOwner(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("segreto")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Ciao"}`))
	require.Equal(t, http.StatusOK, w.Code)

	var created responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// Another tenant must not be able to read it.
	other := httptest.NewRequest("GET", "/v1/responses/"+created.ID, nil)
	other = other.WithContext(auth.ContextWithKeyInfo(other.Context(), &store.APIKey{
		Budget:  100,
		KeyHash: "key-hash-2", KeyPrefix: "sk-other", Active: true,
	}))
	other.SetPathValue("response_id", created.ID)

	get := httptest.NewRecorder()
	handler.ServeObject(get, other)
	assert.Equal(t, http.StatusNotFound, get.Code)
}

func TestResponsesHandler_RequestWithoutInputIsAccepted(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Sono Claude.")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Come ti chiami?"}`))
	require.Equal(t, http.StatusOK, first.Code)

	var created responsesResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &created))

	// "input" is optional: a turn may carry only previous_response_id, which is
	// how a client asks the model to continue from a stored conversation.
	mock.lastReq = nil
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","previous_response_id":"`+created.ID+`"}`))

	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.NotNil(t, mock.lastReq)
	require.Len(t, mock.lastReq.Messages, 2)
	assert.Equal(t, "Come ti chiami?", mock.lastReq.Messages[0].Content)
	assert.Equal(t, "Sono Claude.", mock.lastReq.Messages[1].Content)
}

func TestResponsesHandler_InstructionsOnlyRequestIsAccepted(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","instructions":"Rispondi ok"}`))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, mock.lastReq)
	require.Len(t, mock.lastReq.Messages, 1)
	assert.Equal(t, "system", mock.lastReq.Messages[0].Role)
}

func TestResponsesHandler_StoredResponseCarriesAnExpiry(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Ciao!")}
	handler, db := newStatefulResponsesHandler(t, "translated-model", mock)
	handler.SetResponseTTL(2 * time.Hour)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Ciao"}`))
	require.Equal(t, http.StatusOK, w.Code)

	var created responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	rs, ok := db.(store.ResponseStore)
	require.True(t, ok)
	record, err := rs.GetResponse(context.Background(), created.ID, "key:key-hash-1")
	require.NoError(t, err)
	require.NotNil(t, record, "the response must be stored under the caller's owner id")
	require.NotNil(t, record.ExpiresAt, "a stored response without an expiry would live forever")
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), *record.ExpiresAt, time.Minute)
}

// storedItemIDs reads the ids the gateway assigned to a response's input items,
// which are the cursors the pagination parameters take.
func storedItemIDs(t *testing.T, handler *ResponsesHandler, responseID string) []string {
	t.Helper()
	list := listInputItems(t, handler, responseID, "?limit=100")
	ids := make([]string, 0, len(list.Data))
	for _, item := range list.Data {
		ids = append(ids, responsesItemID(item))
	}
	return ids
}

func listInputItems(t *testing.T, handler *ResponsesHandler, responseID, query string) responsesItemList {
	t.Helper()
	w := httptest.NewRecorder()
	req := authedResponsesRequest("GET", "/v1/responses/"+responseID+"/input_items"+query, "")
	req.SetPathValue("response_id", responseID)
	handler.ServeInputItems(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var list responsesItemList
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	return list
}

// threeItemResponse stores one turn whose input carries three message items.
func threeItemResponse(t *testing.T) (*ResponsesHandler, string) {
	t.Helper()
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses", `{
		"model":"translated-model",
		"input":[
			{"type":"message","role":"user","content":"uno"},
			{"type":"message","role":"user","content":"due"},
			{"type":"message","role":"user","content":"tre"}
		]}`))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var created responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	return handler, created.ID
}

func TestResponsesHandler_InputItemsPagination(t *testing.T) {
	handler, id := threeItemResponse(t)
	ids := storedItemIDs(t, handler, id)
	require.Len(t, ids, 3)

	t.Run("limit truncates and reports has_more", func(t *testing.T) {
		list := listInputItems(t, handler, id, "?limit=2")
		require.Len(t, list.Data, 2)
		assert.True(t, list.HasMore)
		assert.Equal(t, ids[0], list.FirstID)
		assert.Equal(t, ids[1], list.LastID)
	})

	t.Run("last page reports no more", func(t *testing.T) {
		list := listInputItems(t, handler, id, "?after="+ids[1])
		require.Len(t, list.Data, 1)
		assert.False(t, list.HasMore)
		assert.Equal(t, ids[2], list.FirstID)
	})

	t.Run("before excludes the cursor", func(t *testing.T) {
		list := listInputItems(t, handler, id, "?before="+ids[2])
		require.Len(t, list.Data, 2)
		assert.Equal(t, ids[0], list.FirstID)
		assert.Equal(t, ids[1], list.LastID)
	})

	t.Run("desc reverses before the cursor applies", func(t *testing.T) {
		list := listInputItems(t, handler, id, "?order=desc&limit=2")
		require.Len(t, list.Data, 2)
		assert.Equal(t, ids[2], list.FirstID)
		assert.Equal(t, ids[1], list.LastID)
		assert.True(t, list.HasMore)
	})
}

func TestResponsesHandler_InputItemsRejectsBadPagination(t *testing.T) {
	handler, id := threeItemResponse(t)

	for _, query := range []string{"?limit=0", "?limit=1000", "?limit=abc", "?order=sideways", "?after=msg_nope"} {
		w := httptest.NewRecorder()
		req := authedResponsesRequest("GET", "/v1/responses/"+id+"/input_items"+query, "")
		req.SetPathValue("response_id", id)
		handler.ServeInputItems(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "query %s must be rejected", query)
	}
}

func TestResponsesHandler_PromptTemplateIsRejected(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","prompt":{"id":"pmpt_123","version":"2"}}`))

	// Expanding the template is the provider's job: translating without it
	// would silently send an empty brief.
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "prompt")
	assert.Nil(t, mock.lastReq)
}

func TestResponsesHandler_CancelMarksAQueuedTurnCancelled(t *testing.T) {
	release := make(chan struct{})
	mock := &mockResponsesProvider{completeResp: textCompletion("tardi"), completeGate: release}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Lavora","background":true}`))
	require.Equal(t, http.StatusOK, w.Code)

	var queued responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &queued))
	require.Equal(t, "queued", queued.Status)

	cancel := httptest.NewRecorder()
	cancelReq := authedResponsesRequest("POST", "/v1/responses/"+queued.ID+"/cancel", "")
	cancelReq.SetPathValue("response_id", queued.ID)
	handler.ServeCancel(cancel, cancelReq)

	require.Equal(t, http.StatusOK, cancel.Code)
	var cancelled responsesResponse
	require.NoError(t, json.Unmarshal(cancel.Body.Bytes(), &cancelled))
	assert.Equal(t, "cancelled", cancelled.Status)

	// Let the upstream call return: a cancelled turn must not overwrite its own
	// final state with the late answer.
	close(release)
	require.Eventually(t, func() bool {
		get := httptest.NewRecorder()
		req := authedResponsesRequest("GET", "/v1/responses/"+queued.ID, "")
		req.SetPathValue("response_id", queued.ID)
		handler.ServeObject(get, req)
		var current responsesResponse
		return json.Unmarshal(get.Body.Bytes(), &current) == nil && current.Status == "cancelled"
	}, 2*time.Second, 10*time.Millisecond)
}

// `before` truncates the page, so the cursor item and everything past it are
// still out there. Reporting has_more:false made a client paginating backwards
// stop a page early.
func TestResponsesHandler_InputItemsBeforeReportsHasMore(t *testing.T) {
	handler, id := threeItemResponse(t)
	ids := storedItemIDs(t, handler, id)
	require.Len(t, ids, 3)

	list := listInputItems(t, handler, id, "?before="+ids[2])
	require.Len(t, list.Data, 2)
	assert.True(t, list.HasMore, "items exist at and after the cursor")

	// The last page still reports no more: nothing was cut.
	full := listInputItems(t, handler, id, "?limit=100")
	require.Len(t, full.Data, 3)
	assert.False(t, full.HasMore)
}

// A detached turn belongs to its conversation exactly as a foreground one does.
// Writing only the response row left the thread empty, so the next turn on that
// conversation replayed an incomplete history.
func TestResponsesHandler_BackgroundTurnJoinsItsConversation(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Fatto.")}
	handler, db := newStatefulResponsesHandler(t, "translated-model", mock)

	cs, ok := db.(store.ConversationStore)
	require.True(t, ok)
	conv := &store.StoredConversation{
		ID:        "conv_bg",
		OwnerID:   testResponsesOwnerID,
		CreatedAt: time.Now(),
	}
	require.NoError(t, cs.CreateConversation(context.Background(), conv))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses",
		`{"model":"translated-model","input":"Lavora","background":true,"conversation":"conv_bg"}`))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var queued responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &queued))

	require.Eventually(t, func() bool {
		items, err := cs.ListConversationItems(context.Background(), "conv_bg", testResponsesOwnerID)
		if err != nil {
			return false
		}
		// The caller's input and the model's answer.
		return len(items) >= 2
	}, 2*time.Second, 10*time.Millisecond,
		"the background turn never joined its conversation")

	items, err := cs.ListConversationItems(context.Background(), "conv_bg", testResponsesOwnerID)
	require.NoError(t, err)
	var joined strings.Builder
	for _, item := range items {
		joined.WriteString(item.Payload)
	}
	assert.Contains(t, joined.String(), "Lavora", "the caller's input is missing from the thread")
	assert.Contains(t, joined.String(), "Fatto.", "the model's answer is missing from the thread")
}

// The thread forwarded to a provider that speaks Responses natively must not
// carry the gateway's own item ids: it never issued them, a real Responses item
// id is hex, and a synthetic one is at best ignored and at worst rejected. The
// internally resolved input keeps them, since input_items listings expose them.
func TestResponsesHandler_ConversationBodyDropsGatewayItemIDs(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, db := newStatefulResponsesHandler(t, "translated-model", mock)

	cs, ok := db.(store.ConversationStore)
	require.True(t, ok)
	ctx := context.Background()
	require.NoError(t, cs.CreateConversation(ctx, &store.StoredConversation{
		ID: "conv_ids", OwnerID: testResponsesOwnerID, CreatedAt: time.Now(),
	}))
	_, err := cs.AppendConversationItems(ctx, "conv_ids", testResponsesOwnerID, []string{
		`{"type":"message","role":"user","content":"storico"}`,
	})
	require.NoError(t, err)

	rawBody := []byte(`{"model":"native-model","conversation":"conv_ids","input":"nuovo"}`)
	req := &responsesRequest{
		Model:        "native-model",
		Conversation: json.RawMessage(`"conv_ids"`),
		Input:        json.RawMessage(`"nuovo"`),
	}
	httpReq := authedResponsesRequest("POST", "/v1/responses", "")
	require.NoError(t, handler.applyConversation(httpReq, req, &rawBody))

	// What goes on the wire carries no gateway ids.
	var wire struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	require.NoError(t, json.Unmarshal(rawBody, &wire))
	require.Len(t, wire.Input, 2)
	for i, item := range wire.Input {
		_, hasID := item["id"]
		assert.False(t, hasID, "forwarded item %d carries a gateway-minted id: %v", i, item)
	}

	// The internally resolved input still does, for input_items listings.
	var resolved []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(req.Input, &resolved))
	require.Len(t, resolved, 2)
	for i, item := range resolved {
		_, hasID := item["id"]
		assert.True(t, hasID, "resolved item %d lost its id", i)
	}
}

// A caller's own item id is theirs and must survive: it may be a real provider
// id from a thread the client is resuming.
func TestResponsesHandler_ConversationBodyKeepsCallerItemIDs(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, db := newStatefulResponsesHandler(t, "translated-model", mock)

	cs, _ := db.(store.ConversationStore)
	require.NoError(t, cs.CreateConversation(context.Background(), &store.StoredConversation{
		ID: "conv_keep", OwnerID: testResponsesOwnerID, CreatedAt: time.Now(),
	}))

	rawBody := []byte(`{"model":"native-model","conversation":"conv_keep","input":[{"id":"msg_abc123","type":"message","role":"user","content":"mio"}]}`)
	req := &responsesRequest{
		Model:        "native-model",
		Conversation: json.RawMessage(`"conv_keep"`),
		Input:        json.RawMessage(`[{"id":"msg_abc123","type":"message","role":"user","content":"mio"}]`),
	}
	httpReq := authedResponsesRequest("POST", "/v1/responses", "")
	require.NoError(t, handler.applyConversation(httpReq, req, &rawBody))

	assert.Contains(t, string(rawBody), "msg_abc123", "the caller's own item id was stripped")
}

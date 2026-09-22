package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func newConversationsHandler(t *testing.T) (*ConversationsHandler, store.Store) {
	t.Helper()
	db := openResponsesTestStore(t)
	return NewConversationsHandler(slog.Default(), db), db
}

func conversationRequestWithPath(method, target, body string, values map[string]string) *http.Request {
	req := authedResponsesRequest(method, target, body)
	for key, value := range values {
		req.SetPathValue(key, value)
	}
	return req
}

func createConversation(t *testing.T, handler *ConversationsHandler, body string) conversationObject {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeCollection(w, authedResponsesRequest("POST", "/v1/conversations", body))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var created conversationObject
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	assert.Equal(t, "conversation", created.Object)
	return created
}

func listConversationItems(t *testing.T, handler *ConversationsHandler, id, query string) conversationItemList {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeItems(w, conversationRequestWithPath("GET", "/v1/conversations/"+id+"/items"+query, "",
		map[string]string{"conversation_id": id}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var list conversationItemList
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	return list
}

func TestConversationsHandler_Lifecycle(t *testing.T) {
	handler, _ := newConversationsHandler(t)

	created := createConversation(t, handler, `{"metadata":{"topic":"support"}}`)
	assert.Equal(t, map[string]string{"topic": "support"}, created.Metadata)

	// Retrieve
	get := httptest.NewRecorder()
	handler.ServeObject(get, conversationRequestWithPath("GET", "/v1/conversations/"+created.ID, "",
		map[string]string{"conversation_id": created.ID}))
	require.Equal(t, http.StatusOK, get.Code)

	// Update metadata
	update := httptest.NewRecorder()
	handler.ServeObject(update, conversationRequestWithPath("POST", "/v1/conversations/"+created.ID,
		`{"metadata":{"topic":"billing"}}`, map[string]string{"conversation_id": created.ID}))
	require.Equal(t, http.StatusOK, update.Code)

	var updated conversationObject
	require.NoError(t, json.Unmarshal(update.Body.Bytes(), &updated))
	assert.Equal(t, map[string]string{"topic": "billing"}, updated.Metadata)

	// Delete
	del := httptest.NewRecorder()
	handler.ServeObject(del, conversationRequestWithPath("DELETE", "/v1/conversations/"+created.ID, "",
		map[string]string{"conversation_id": created.ID}))
	require.Equal(t, http.StatusOK, del.Code)
	assert.Contains(t, del.Body.String(), `"deleted":true`)

	gone := httptest.NewRecorder()
	handler.ServeObject(gone, conversationRequestWithPath("GET", "/v1/conversations/"+created.ID, "",
		map[string]string{"conversation_id": created.ID}))
	assert.Equal(t, http.StatusNotFound, gone.Code)
}

func TestConversationsHandler_Items(t *testing.T) {
	handler, _ := newConversationsHandler(t)

	created := createConversation(t, handler, `{"items":[
		{"type":"message","role":"user","content":"uno"}
	]}`)

	// Append two more items.
	add := httptest.NewRecorder()
	handler.ServeItems(add, conversationRequestWithPath("POST", "/v1/conversations/"+created.ID+"/items", `{"items":[
		{"type":"message","role":"assistant","content":"due"},
		{"type":"message","role":"user","content":"tre"}
	]}`, map[string]string{"conversation_id": created.ID}))
	require.Equal(t, http.StatusOK, add.Code, add.Body.String())

	list := listConversationItems(t, handler, created.ID, "")
	require.Len(t, list.Data, 3, "items must come back in the order they were appended")

	ids := make([]string, 0, 3)
	for _, raw := range list.Data {
		var item struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		}
		require.NoError(t, json.Unmarshal(raw, &item))
		require.NotEmpty(t, item.ID, "every stored item must carry a gateway id")
		ids = append(ids, item.ID)
	}

	// Retrieve one item
	get := httptest.NewRecorder()
	handler.ServeItem(get, conversationRequestWithPath("GET", "/v1/conversations/"+created.ID+"/items/"+ids[1], "",
		map[string]string{"conversation_id": created.ID, "item_id": ids[1]}))
	require.Equal(t, http.StatusOK, get.Code)
	assert.Contains(t, get.Body.String(), `"due"`)

	// Delete it; the rest keeps its order
	del := httptest.NewRecorder()
	handler.ServeItem(del, conversationRequestWithPath("DELETE", "/v1/conversations/"+created.ID+"/items/"+ids[1], "",
		map[string]string{"conversation_id": created.ID, "item_id": ids[1]}))
	require.Equal(t, http.StatusOK, del.Code)

	remaining := listConversationItems(t, handler, created.ID, "")
	require.Len(t, remaining.Data, 2)
	assert.Equal(t, ids[0], remaining.FirstID)
	assert.Equal(t, ids[2], remaining.LastID)
}

func TestConversationsHandler_ItemsPagination(t *testing.T) {
	handler, _ := newConversationsHandler(t)
	created := createConversation(t, handler, `{"items":[
		{"type":"message","role":"user","content":"uno"},
		{"type":"message","role":"user","content":"due"},
		{"type":"message","role":"user","content":"tre"}
	]}`)

	all := listConversationItems(t, handler, created.ID, "?limit=100")
	require.Len(t, all.Data, 3)
	first, last := all.FirstID, all.LastID

	page := listConversationItems(t, handler, created.ID, "?limit=2")
	require.Len(t, page.Data, 2)
	assert.True(t, page.HasMore)
	assert.Equal(t, first, page.FirstID)

	tail := listConversationItems(t, handler, created.ID, "?after="+page.LastID)
	require.Len(t, tail.Data, 1)
	assert.False(t, tail.HasMore)
	assert.Equal(t, last, tail.FirstID)

	desc := listConversationItems(t, handler, created.ID, "?order=desc")
	require.Len(t, desc.Data, 3)
	assert.Equal(t, last, desc.FirstID)
	assert.Equal(t, first, desc.LastID)
}

func TestConversationsHandler_AreScopedToTheirOwner(t *testing.T) {
	handler, _ := newConversationsHandler(t)
	created := createConversation(t, handler, `{}`)

	other := httptest.NewRequest("GET", "/v1/conversations/"+created.ID, nil)
	other = other.WithContext(auth.ContextWithKeyInfo(other.Context(), &store.APIKey{
		Budget:  100,
		KeyHash: "key-hash-2", KeyPrefix: "sk-other", Active: true,
	}))
	other.SetPathValue("conversation_id", created.ID)

	w := httptest.NewRecorder()
	handler.ServeObject(w, other)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- integration with /v1/responses ---

// newConversationBoundHandlers wires a responses handler and a conversations
// handler onto the same store, the way the router does.
func newConversationBoundHandlers(t *testing.T, mock *mockResponsesProvider) (*ResponsesHandler, *ConversationsHandler) {
	t.Helper()
	db := openResponsesTestStore(t)
	registry := newTestRegistry("translated-model", mock)
	responses := NewResponsesHandler(registry, newTestRouter(registry), slog.Default(), db, nil, nil)
	return responses, NewConversationsHandler(slog.Default(), db)
}

func TestResponsesHandler_ConversationCarriesHistoryAndRecordsTheTurn(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Sono Claude.")}
	responses, conversations := newConversationBoundHandlers(t, mock)

	conversation := createConversation(t, conversations, `{"items":[
		{"type":"message","role":"user","content":"Ricorda: mi chiamo Max."}
	]}`)

	w := postResponses(t, responses, `{"model":"translated-model","input":"Come mi chiamo?","conversation":"`+conversation.ID+`"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The provider must see the stored history ahead of the new turn.
	require.NotNil(t, mock.lastReq)
	require.Len(t, mock.lastReq.Messages, 2)
	assert.Equal(t, "Ricorda: mi chiamo Max.", mock.lastReq.Messages[0].Content)
	assert.Equal(t, "Come mi chiamo?", mock.lastReq.Messages[1].Content)

	// The turn is appended to the conversation: the new input, then the output.
	items := listConversationItems(t, conversations, conversation.ID, "?limit=100")
	require.Len(t, items.Data, 3)
	assert.Contains(t, string(items.Data[1]), "Come mi chiamo?")
	assert.Contains(t, string(items.Data[2]), "Sono Claude.")
}

func TestResponsesHandler_ConversationHistoryIsNotDuplicatedAcrossTurns(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("uno")}
	responses, conversations := newConversationBoundHandlers(t, mock)
	conversation := createConversation(t, conversations, `{}`)

	body := `{"model":"translated-model","input":"domanda","conversation":"` + conversation.ID + `"}`
	require.Equal(t, http.StatusOK, postResponses(t, responses, body).Code)

	mock.completeResp = textCompletion("due")
	require.Equal(t, http.StatusOK, postResponses(t, responses, body).Code)

	// Two turns of one input and one output each: the history merged into the
	// request must not be written back on top of itself.
	items := listConversationItems(t, conversations, conversation.ID, "?limit=100")
	assert.Len(t, items.Data, 4)
}

func TestResponsesHandler_ConversationAndPreviousResponseIDConflict(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("x")}
	responses, conversations := newConversationBoundHandlers(t, mock)
	conversation := createConversation(t, conversations, `{}`)

	w := postResponses(t, responses,
		`{"model":"translated-model","input":"Ciao","conversation":"`+conversation.ID+`","previous_response_id":"resp_1"}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "cannot be used together")
	assert.Nil(t, mock.lastReq)
}

func TestResponsesHandler_UnknownConversationIsRejected(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("x")}
	responses, _ := newConversationBoundHandlers(t, mock)

	w := postResponses(t, responses, `{"model":"translated-model","input":"Ciao","conversation":"conv_missing"}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "conv_missing")
	assert.Nil(t, mock.lastReq)
}

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ConversationsHandler serves /v1/conversations. Conversations are held by the
// gateway itself rather than forwarded, so they behave the same on every
// provider — including the ones with no conversation store of their own — and
// so a conversation can outlive the deployment that answered its last turn.
type ConversationsHandler struct {
	logger *slog.Logger
	store  store.Store
	ttl    time.Duration
}

func NewConversationsHandler(logger *slog.Logger, db store.Store) *ConversationsHandler {
	return &ConversationsHandler{logger: logger, store: db}
}

// SetConversationTTL overrides how long conversations are kept. A non-positive
// duration restores the default.
func (h *ConversationsHandler) SetConversationTTL(ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultResponseTTL
	}
	h.ttl = ttl
}

func (h *ConversationsHandler) conversationStore() store.ConversationStore {
	if h.store == nil {
		return nil
	}
	cs, _ := h.store.(store.ConversationStore)
	return cs
}

func (h *ConversationsHandler) expiry() *time.Time {
	ttl := h.ttl
	if ttl <= 0 {
		ttl = defaultResponseTTL
	}
	expiry := time.Now().Add(ttl)
	return &expiry
}

// --- wire types ---

type conversationObject struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Metadata  map[string]string `json:"metadata"`
}

type conversationItemList struct {
	Object  string            `json:"object"`
	Data    []json.RawMessage `json:"data"`
	FirstID string            `json:"first_id,omitempty"`
	LastID  string            `json:"last_id,omitempty"`
	HasMore bool              `json:"has_more"`
}

type conversationRequest struct {
	Metadata map[string]string `json:"metadata,omitempty"`
	Items    []json.RawMessage `json:"items,omitempty"`
}

func newConversationID() string { return newResponsesItemID("conv") }

func conversationToObject(record *store.StoredConversation) conversationObject {
	object := conversationObject{
		ID:        record.ID,
		Object:    "conversation",
		CreatedAt: record.CreatedAt.Unix(),
		Metadata:  map[string]string{},
	}
	if record.Metadata != "" {
		_ = json.Unmarshal([]byte(record.Metadata), &object.Metadata)
	}
	return object
}

// --- collection ---

// ServeCollection handles POST /v1/conversations.
func (h *ConversationsHandler) ServeCollection(w http.ResponseWriter, r *http.Request) {
	cs, ownerID, ok := h.scope(w, r)
	if !ok {
		return
	}

	req, ok := decodeConversationRequest(w, r)
	if !ok {
		return
	}

	record := &store.StoredConversation{
		ID:        newConversationID(),
		OwnerID:   ownerID,
		Metadata:  encodeConversationMetadata(req.Metadata),
		CreatedAt: time.Now(),
		ExpiresAt: h.expiry(),
	}
	if err := cs.CreateConversation(r.Context(), record); err != nil {
		h.logger.Error("conversation create failed", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "failed to create conversation")
		return
	}

	if len(req.Items) > 0 {
		if _, err := cs.AppendConversationItems(r.Context(), record.ID, ownerID, rawItemPayloads(req.Items)); err != nil {
			h.logger.Error("conversation seed items failed", "error", err, "conversation_id", record.ID)
			writeError(w, http.StatusInternalServerError, "server_error", "failed to add conversation items")
			return
		}
	}

	writeJSON(w, http.StatusOK, conversationToObject(record))
}

// --- single conversation ---

// ServeObject handles GET, POST (metadata update) and DELETE on
// /v1/conversations/{conversation_id}.
func (h *ConversationsHandler) ServeObject(w http.ResponseWriter, r *http.Request) {
	cs, ownerID, ok := h.scope(w, r)
	if !ok {
		return
	}
	record, ok := h.lookup(w, r, cs, ownerID)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, conversationToObject(record))

	case http.MethodPost:
		req, ok := decodeConversationRequest(w, r)
		if !ok {
			return
		}
		record.Metadata = encodeConversationMetadata(req.Metadata)
		if err := cs.UpdateConversationMetadata(r.Context(), record.ID, ownerID, record.Metadata); err != nil {
			h.logger.Error("conversation update failed", "error", err, "conversation_id", record.ID)
			writeError(w, http.StatusInternalServerError, "server_error", "failed to update conversation")
			return
		}
		writeJSON(w, http.StatusOK, conversationToObject(record))

	case http.MethodDelete:
		if err := cs.DeleteConversation(r.Context(), record.ID, ownerID); err != nil {
			h.logger.Error("conversation delete failed", "error", err, "conversation_id", record.ID)
			writeError(w, http.StatusInternalServerError, "server_error", "failed to delete conversation")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id": record.ID, "object": "conversation.deleted", "deleted": true,
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// --- items ---

// ServeItems handles GET and POST on /v1/conversations/{conversation_id}/items.
func (h *ConversationsHandler) ServeItems(w http.ResponseWriter, r *http.Request) {
	cs, ownerID, ok := h.scope(w, r)
	if !ok {
		return
	}
	record, ok := h.lookup(w, r, cs, ownerID)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		items, err := cs.ListConversationItems(r.Context(), record.ID, ownerID)
		if err != nil {
			h.logger.Error("conversation items list failed", "error", err, "conversation_id", record.ID)
			writeError(w, http.StatusInternalServerError, "server_error", "failed to list conversation items")
			return
		}
		list, err := paginateConversationItems(items, r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		req, ok := decodeConversationRequest(w, r)
		if !ok {
			return
		}
		if len(req.Items) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "items is required")
			return
		}
		added, err := cs.AppendConversationItems(r.Context(), record.ID, ownerID, rawItemPayloads(req.Items))
		if err != nil {
			h.logger.Error("conversation items append failed", "error", err, "conversation_id", record.ID)
			writeError(w, http.StatusInternalServerError, "server_error", "failed to add conversation items")
			return
		}
		writeJSON(w, http.StatusOK, conversationItemListOf(added))

	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ServeItem handles GET and DELETE on
// /v1/conversations/{conversation_id}/items/{item_id}.
func (h *ConversationsHandler) ServeItem(w http.ResponseWriter, r *http.Request) {
	cs, ownerID, ok := h.scope(w, r)
	if !ok {
		return
	}
	record, ok := h.lookup(w, r, cs, ownerID)
	if !ok {
		return
	}

	itemID := r.PathValue("item_id")
	if itemID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "item id is required")
		return
	}

	item, err := cs.GetConversationItem(r.Context(), record.ID, ownerID, itemID)
	if err != nil {
		h.logger.Error("conversation item lookup failed", "error", err, "item_id", itemID)
		writeError(w, http.StatusInternalServerError, "server_error", "conversation item lookup failed")
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("item %q not found", itemID))
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeRawJSON(w, http.StatusOK, storedItemWithID(*item))

	case http.MethodDelete:
		if err := cs.DeleteConversationItem(r.Context(), record.ID, ownerID, itemID); err != nil {
			h.logger.Error("conversation item delete failed", "error", err, "item_id", itemID)
			writeError(w, http.StatusInternalServerError, "server_error", "failed to delete conversation item")
			return
		}
		// OpenAI answers a item deletion with the conversation itself.
		writeJSON(w, http.StatusOK, conversationToObject(record))

	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// --- helpers ---

// scope resolves the store capability and the caller's tenant, writing the
// appropriate error when either is missing.
func (h *ConversationsHandler) scope(w http.ResponseWriter, r *http.Request) (store.ConversationStore, string, bool) {
	cs := h.conversationStore()
	ownerID := fileOwnerID(r.Context())
	if cs == nil || ownerID == "" {
		writeError(w, http.StatusInternalServerError, "server_error", "conversation store is unavailable")
		return nil, "", false
	}
	return cs, ownerID, true
}

func (h *ConversationsHandler) lookup(w http.ResponseWriter, r *http.Request, cs store.ConversationStore, ownerID string) (*store.StoredConversation, bool) {
	id := r.PathValue("conversation_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "conversation id is required")
		return nil, false
	}

	record, err := cs.GetConversation(r.Context(), id, ownerID)
	if err != nil {
		h.logger.Error("conversation lookup failed", "error", err, "conversation_id", id)
		writeError(w, http.StatusInternalServerError, "server_error", "conversation lookup failed")
		return nil, false
	}
	if record == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("conversation %q not found", id))
		return nil, false
	}
	return record, true
}

// decodeConversationRequest parses a body that may legitimately be empty:
// creating a conversation with neither metadata nor items is valid.
func decodeConversationRequest(w http.ResponseWriter, r *http.Request) (conversationRequest, bool) {
	var req conversationRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBody caps the reader, so an oversized body surfaces here rather
		// than as an allocation. Report it as the limit it is: a 400 sends the
		// caller looking for a syntax error that is not there.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds the %d byte limit", tooLarge.Limit))
			return req, false
		}
		writeDecodeError(w, err)
		return req, false
	}
	if len(body) == 0 {
		return req, true
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeDecodeError(w, err)
		return req, false
	}
	return req, true
}

func encodeConversationMetadata(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// rawItemPayloads normalises incoming items so every stored item carries an id,
// which is what listings and item lookups address them by.
func rawItemPayloads(items []json.RawMessage) []string {
	payloads := make([]string, 0, len(items))
	for _, item := range items {
		payloads = append(payloads, string(item))
	}
	return payloads
}

// storedItemWithID returns an item's payload carrying the id the store gave it,
// so clients address items by an id the gateway controls rather than one the
// caller may or may not have supplied.
func storedItemWithID(item store.StoredConversationItem) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(item.Payload), &fields) != nil {
		return item.Payload
	}
	id, err := json.Marshal(item.ID)
	if err != nil {
		return item.Payload
	}
	fields["id"] = id
	encoded, err := json.Marshal(fields)
	if err != nil {
		return item.Payload
	}
	return string(encoded)
}

func conversationItemListOf(items []store.StoredConversationItem) conversationItemList {
	list := conversationItemList{Object: "list", Data: []json.RawMessage{}}
	for _, item := range items {
		list.Data = append(list.Data, json.RawMessage(storedItemWithID(item)))
	}
	if len(items) > 0 {
		list.FirstID = items[0].ID
		list.LastID = items[len(items)-1].ID
	}
	return list
}

// paginateConversationItems applies the list parameters to a conversation's
// items, using the same rules as the response input-item listing.
func paginateConversationItems(items []store.StoredConversationItem, query url.Values) (conversationItemList, error) {
	list := conversationItemList{Object: "list", Data: []json.RawMessage{}}

	page, err := paginateByID(len(items), query, func(i int) string { return items[i].ID })
	if err != nil {
		return list, err
	}
	if page.reversed {
		reversed := make([]store.StoredConversationItem, len(items))
		for i, item := range items {
			reversed[len(items)-1-i] = item
		}
		items = reversed
	}
	items = items[page.from:page.to]

	list.HasMore = page.hasMore
	for _, item := range items {
		list.Data = append(list.Data, json.RawMessage(storedItemWithID(item)))
	}
	if len(items) > 0 {
		list.FirstID = items[0].ID
		list.LastID = items[len(items)-1].ID
	}
	return list, nil
}

// --- integration with /v1/responses ---

// applyConversation expands a request's conversation into its input and takes
// the parameter back out of the body forwarded upstream. Conversations belong
// to the gateway, so no provider is ever asked to resolve one — that keeps a
// thread working across deployments, and across providers that have no
// conversation store at all.
//
// rawBody is rewritten in place so the native passthrough forwards the expanded
// request rather than an id its provider has never seen.
func (h *ResponsesHandler) applyConversation(r *http.Request, req *responsesRequest, rawBody *[]byte) error {
	conversationID := responsesConversationID(req.Conversation)
	if conversationID == "" {
		return nil
	}
	if req.PreviousResponseID != "" {
		return fmt.Errorf("conversation and previous_response_id cannot be used together")
	}

	cs, _ := h.store.(store.ConversationStore)
	ownerID := fileOwnerID(r.Context())
	if cs == nil || ownerID == "" {
		return fmt.Errorf("conversation requires a configured conversation store")
	}

	conversation, err := cs.GetConversation(r.Context(), conversationID, ownerID)
	if err != nil {
		return fmt.Errorf("conversation lookup failed")
	}
	if conversation == nil {
		return fmt.Errorf("conversation %q not found", conversationID)
	}

	stored, err := cs.ListConversationItems(r.Context(), conversationID, ownerID)
	if err != nil {
		return fmt.Errorf("conversation items lookup failed")
	}

	// Two views of the same thread. `history` carries the gateway's item ids,
	// which input_items listings and conversation reads expose as stable
	// handles. `forwarded` is what goes on the wire to a provider that speaks
	// Responses natively, and it carries none of them: those ids are ours, the
	// provider never issued them, and a real Responses item id is hex — a
	// synthetic one is at best ignored and at worst rejected outright. Ids the
	// caller sent are left alone, since those are theirs to use.
	incoming := normalizeResponsesInput(req.Input)
	history := make([]json.RawMessage, 0, len(stored)+len(incoming))
	forwarded := make([]json.RawMessage, 0, len(stored)+len(incoming))
	for _, item := range stored {
		history = append(history, json.RawMessage(storedItemWithID(item)))
		forwarded = append(forwarded, json.RawMessage(item.Payload))
	}
	history = append(history, incoming...)
	forwarded = append(forwarded, responsesInputAsSent(req.Input)...)

	merged, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("conversation expansion failed")
	}
	wire, err := json.Marshal(forwarded)
	if err != nil {
		return fmt.Errorf("conversation expansion failed")
	}

	req.Input = merged
	req.resolvedConversation = conversationID
	req.pendingConversationItems = incoming

	rewritten, err := rewriteConversationBody(*rawBody, wire)
	if err != nil {
		return err
	}
	*rawBody = rewritten
	return nil
}

// rewriteConversationBody replaces the input with the expanded thread and drops
// the conversation reference, leaving every other field of the client's request
// untouched.
func rewriteConversationBody(rawBody []byte, input json.RawMessage) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &fields); err != nil {
		return nil, fmt.Errorf("invalid Responses request: %w", err)
	}
	fields["input"] = input
	delete(fields, "conversation")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("conversation expansion failed")
	}
	return encoded, nil
}

// appendTurnToConversation adds what this turn contributed — the caller's new
// input items, then the model's output — to the conversation it belongs to.
// Failures are logged and never fail the request: the client already has its
// answer.
func (h *ResponsesHandler) appendTurnToConversation(ctx context.Context, req *responsesRequest, output []json.RawMessage) {
	if req.resolvedConversation == "" {
		return
	}
	cs, _ := h.store.(store.ConversationStore)
	ownerID := fileOwnerID(ctx)
	if cs == nil || ownerID == "" {
		return
	}

	payloads := make([]string, 0, len(req.pendingConversationItems)+len(output))
	for _, item := range req.pendingConversationItems {
		payloads = append(payloads, string(item))
	}
	for _, item := range output {
		payloads = append(payloads, string(item))
	}
	if len(payloads) == 0 {
		return
	}

	if _, err := cs.AppendConversationItems(ctx, req.resolvedConversation, ownerID, payloads); err != nil {
		h.logger.Warn("conversation append failed", "error", err, "conversation_id", req.resolvedConversation)
	}
}

// appendStoredTurnToConversation adds a finished turn to its conversation using
// only what the stored row holds, for the paths where the original request is
// long gone — a native background turn finishing on a later poll.
func (h *ResponsesHandler) appendStoredTurnToConversation(ctx context.Context, record *store.StoredResponse) {
	if record.ConversationID == "" {
		return
	}
	cs, _ := h.store.(store.ConversationStore)
	if cs == nil || record.OwnerID == "" {
		return
	}

	var payloads []string
	var input []json.RawMessage
	if record.InputItems != "" && json.Unmarshal([]byte(record.InputItems), &input) == nil {
		for _, item := range input {
			payloads = append(payloads, string(item))
		}
	}
	var payload struct {
		Output []json.RawMessage `json:"output"`
	}
	if record.Payload != "" && json.Unmarshal([]byte(record.Payload), &payload) == nil {
		for _, item := range payload.Output {
			payloads = append(payloads, string(item))
		}
	}
	if len(payloads) == 0 {
		return
	}

	if _, err := cs.AppendConversationItems(ctx, record.ConversationID, record.OwnerID, payloads); err != nil {
		h.logger.Warn("conversation append failed", "error", err, "conversation_id", record.ConversationID)
	}
}

// responsesOutputItems re-encodes a finished response's output for storage in a
// conversation.
func responsesOutputItems(resp *responsesResponse) []json.RawMessage {
	if resp == nil {
		return nil
	}
	items := make([]json.RawMessage, 0, len(resp.Output))
	for _, item := range resp.Output {
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		items = append(items, encoded)
	}
	return items
}

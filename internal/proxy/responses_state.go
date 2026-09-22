package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// responsesItemList is the OpenAI list envelope used by
// GET /v1/responses/{id}/input_items.
type responsesItemList struct {
	Object  string        `json:"object"`
	Data    []interface{} `json:"data"`
	FirstID string        `json:"first_id,omitempty"`
	LastID  string        `json:"last_id,omitempty"`
	HasMore bool          `json:"has_more"`
}

// backgroundResponses tracks in-flight background turns so cancel can stop
// them. Entries are per-process: a cancel issued against another replica falls
// back to marking the stored response cancelled.
var backgroundResponses sync.Map // response id -> context.CancelFunc

// responseStore returns the optional persistence capability, or nil when the
// configured store cannot hold responses (e.g. the lightweight test stores).
func (h *ResponsesHandler) responseStore() store.ResponseStore {
	if h.store == nil {
		return nil
	}
	rs, _ := h.store.(store.ResponseStore)
	return rs
}

// shouldStoreResponse mirrors the OpenAI default: responses are stored unless
// the caller opts out with store:false.
func shouldStoreResponse(req *responsesRequest) bool {
	return req.Store == nil || *req.Store
}

// --- persistence ---

// persistResponse saves a finished turn. Storage failures are logged and never
// fail the request: the client already has its answer, and losing the ability
// to retrieve it later is not worth turning a successful call into an error.
func (h *ResponsesHandler) persistResponse(
	ctx context.Context,
	req *responsesRequest,
	resp *responsesResponse,
	dep *provider.Deployment,
	upstreamID string,
) {
	// A turn joins its conversation whether or not it is itself stored: the
	// caller asked for it to be part of that thread, and store:false is about
	// retrieving the response later, not about the thread it belongs to.
	h.appendTurnToConversation(ctx, req, responsesOutputItems(resp))

	rs := h.responseStore()
	ownerID := fileOwnerID(ctx)
	if rs == nil || ownerID == "" || !shouldStoreResponse(req) {
		return
	}

	record, err := h.newStoredResponse(ownerID, req, resp, dep, upstreamID)
	if err != nil {
		h.logger.Warn("response persist failed", "error", err, "response_id", resp.ID)
		return
	}
	if err := rs.CreateResponse(ctx, record); err != nil {
		h.logger.Warn("response persist failed", "error", err, "response_id", resp.ID)
	}
}

func (h *ResponsesHandler) newStoredResponse(
	ownerID string,
	req *responsesRequest,
	resp *responsesResponse,
	dep *provider.Deployment,
	upstreamID string,
) (*store.StoredResponse, error) {
	payload, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("encode response payload: %w", err)
	}
	// The stored input is the fully resolved conversation, so a chain of
	// previous_response_id lookups stays O(1) instead of walking backwards.
	items, err := json.Marshal(resolvedResponsesInput(req))
	if err != nil {
		return nil, fmt.Errorf("encode response input: %w", err)
	}

	record := &store.StoredResponse{
		ID:                 resp.ID,
		OwnerID:            ownerID,
		Model:              req.Model,
		UpstreamID:         upstreamID,
		Status:             resp.Status,
		PreviousResponseID: req.PreviousResponseID,
		ConversationID:     responsesConversationID(req.Conversation),
		Payload:            string(payload),
		InputItems:         string(items),
		CreatedAt:          time.Now(),
		ExpiresAt:          h.responseExpiry(),
	}
	if dep != nil {
		record.DeploymentID = dep.ID
	}
	return record, nil
}

// resolvedResponsesInput returns the request input as a list of items, which is
// both what input_items listing serves and what previous_response_id replays.
func resolvedResponsesInput(req *responsesRequest) []json.RawMessage {
	return normalizeResponsesInput(req.Input)
}

// normalizeResponsesInput turns the polymorphic "input" field into a list of
// items, so plain-string and item-array requests are stored identically. Items
// without an id are given one: input_items listings expose stable handles.
func normalizeResponsesInput(raw json.RawMessage) []json.RawMessage {
	return responsesInputItems(raw, true)
}

// responsesInputAsSent normalizes the same field without minting ids, for the
// bytes a native provider will actually receive.
//
// A gateway-minted id is bookkeeping: the provider never issued it, and a real
// Responses item id is hex, so a synthetic one is at best ignored and at worst
// rejected. Ids the caller sent are theirs and are left in place.
func responsesInputAsSent(raw json.RawMessage) []json.RawMessage {
	return responsesInputItems(raw, false)
}

func responsesInputItems(raw json.RawMessage, mintIDs bool) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}

	var text string
	if json.Unmarshal(raw, &text) == nil {
		fields := map[string]interface{}{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": text}},
		}
		if mintIDs {
			fields["id"] = newResponsesItemID("msg")
		}
		item, err := json.Marshal(fields)
		if err != nil {
			return nil
		}
		return []json.RawMessage{item}
	}

	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	if !mintIDs {
		return items
	}
	for i, item := range items {
		items[i] = withResponsesItemID(item)
	}
	return items
}

// withResponsesItemID gives an input item an id when the client did not send
// one; input_items listings are expected to expose stable ids.
func withResponsesItemID(item json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return item
	}
	if _, ok := fields["id"]; ok {
		return item
	}
	id, err := json.Marshal(newResponsesItemID("msg"))
	if err != nil {
		return item
	}
	fields["id"] = id
	encoded, err := json.Marshal(fields)
	if err != nil {
		return item
	}
	return encoded
}

func responsesConversationID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var object struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.ID
	}
	return ""
}

// --- previous_response_id ---

// resolvePreviousResponse expands previous_response_id into the request input
// so providers without server-side state still see the whole conversation.
// Native passthrough never calls this: those providers resolve the chain
// themselves.
func (h *ResponsesHandler) resolvePreviousResponse(ctx context.Context, req *responsesRequest) error {
	if req.PreviousResponseID == "" {
		return nil
	}
	rs := h.responseStore()
	ownerID := fileOwnerID(ctx)
	if rs == nil || ownerID == "" {
		return fmt.Errorf("previous_response_id requires a configured response store")
	}

	previous, err := rs.GetResponse(ctx, req.PreviousResponseID, ownerID)
	if err != nil {
		return fmt.Errorf("previous_response_id lookup failed")
	}
	if previous == nil {
		return fmt.Errorf("previous response %q not found", req.PreviousResponseID)
	}

	history := storedResponseItems(previous)
	history = append(history, normalizeResponsesInput(req.Input)...)

	merged, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("previous_response_id expansion failed")
	}
	req.Input = merged
	return nil
}

// storedResponseItems replays a stored turn as input items: everything that
// went in, followed by everything the model produced.
func storedResponseItems(record *store.StoredResponse) []json.RawMessage {
	var items []json.RawMessage
	if record.InputItems != "" {
		var stored []json.RawMessage
		if json.Unmarshal([]byte(record.InputItems), &stored) == nil {
			items = append(items, stored...)
		}
	}
	if record.Payload != "" {
		var payload struct {
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal([]byte(record.Payload), &payload) == nil {
			items = append(items, payload.Output...)
		}
	}
	return items
}

// --- stateful endpoints ---

// ServeObject handles GET and DELETE on /v1/responses/{response_id}.
func (h *ResponsesHandler) ServeObject(w http.ResponseWriter, r *http.Request) {
	record, ok := h.lookupResponse(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.writeStoredResponse(w, r, record)
	case http.MethodDelete:
		h.deleteResponse(w, r, record)
	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ServeCancel handles POST /v1/responses/{response_id}/cancel.
func (h *ResponsesHandler) ServeCancel(w http.ResponseWriter, r *http.Request) {
	record, ok := h.lookupResponse(w, r)
	if !ok {
		return
	}

	if h.proxyResponsesResource(w, r, record, http.MethodPost, "/cancel") {
		return
	}

	// Local background turn: stop the goroutine, then mark it cancelled.
	if cancel, ok := backgroundResponses.LoadAndDelete(record.ID); ok {
		cancel.(context.CancelFunc)()
	} else if rs := h.responseStore(); rs != nil {
		// The turn belongs to another replica, whose context this process
		// cannot reach. Leave the mark its worker polls for.
		if err := rs.RequestResponseCancel(r.Context(), record.ID, record.OwnerID); err != nil {
			h.logger.Warn("response cancel request failed", "error", err, "response_id", record.ID)
		}
	}
	if !isTerminalResponsesStatus(record.Status) {
		if !h.updateStoredResponseStatus(r.Context(), record, "cancelled") {
			// The turn reached its answer between the lookup and this write.
			// Serve what actually happened rather than a cancellation that did
			// not take — and re-read, because this snapshot is now stale.
			if rs := h.responseStore(); rs != nil {
				if fresh, err := rs.GetResponse(r.Context(), record.ID, record.OwnerID); err == nil && fresh != nil {
					record = fresh
				}
			}
		}
	}
	h.writeStoredResponse(w, r, record)
}

// defaultResponsesItemLimit and maxResponsesItemLimit mirror the OpenAI list
// endpoints: 20 items per page by default, 100 at most.
const (
	defaultResponsesItemLimit = 20
	maxResponsesItemLimit     = 100
)

// ServeInputItems handles GET /v1/responses/{response_id}/input_items.
func (h *ResponsesHandler) ServeInputItems(w http.ResponseWriter, r *http.Request) {
	record, ok := h.lookupResponse(w, r)
	if !ok {
		return
	}

	items := make([]interface{}, 0)
	if record.InputItems != "" {
		var stored []interface{}
		_ = json.Unmarshal([]byte(record.InputItems), &stored)
		items = append(items, stored...)
	}

	list, err := paginateResponsesItems(items, r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// paginateResponsesItems applies the OpenAI list parameters (order, after,
// before, limit) to a stored item list. Items are held in chronological order,
// so "desc" reverses them before the cursors are applied and the cursor always
// means "the item after this id in the order being returned".
func paginateResponsesItems(items []interface{}, query url.Values) (responsesItemList, error) {
	list := responsesItemList{Object: "list", Data: []interface{}{}}

	page, err := paginateByID(len(items), query, func(i int) string { return responsesItemID(items[i]) })
	if err != nil {
		return list, err
	}
	if page.reversed {
		items = reverseItems(items)
	}
	items = items[page.from:page.to]

	list.HasMore = page.hasMore
	list.Data = append(list.Data, items...)
	if len(items) > 0 {
		list.FirstID = responsesItemID(items[0])
		list.LastID = responsesItemID(items[len(items)-1])
	}
	return list, nil
}

func reverseItems(items []interface{}) []interface{} {
	reversed := make([]interface{}, len(items))
	for i, item := range items {
		reversed[len(items)-1-i] = item
	}
	return reversed
}

// listPage is the slice of a list one page request selects, expressed against
// the list already put in the requested order.
type listPage struct {
	from     int
	to       int
	reversed bool
	hasMore  bool
}

// paginateByID resolves the OpenAI list parameters (order, after, before,
// limit) against a list of that many items, reading each item's id through idAt
// so the same rules serve response input items and conversation items alike.
// Cursors are applied after the ordering, so "after" always means "the item
// following this one in the order being returned".
func paginateByID(count int, query url.Values, idAt func(int) string) (listPage, error) {
	page := listPage{to: count}

	switch order := query.Get("order"); order {
	case "", "asc":
	case "desc":
		page.reversed = true
	default:
		return page, fmt.Errorf("invalid order %q: expected \"asc\" or \"desc\"", order)
	}

	limit := defaultResponsesItemLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxResponsesItemLimit {
			return page, fmt.Errorf("invalid limit %q: expected an integer between 1 and %d", raw, maxResponsesItemLimit)
		}
		limit = parsed
	}

	// idAt indexes the original list, so a reversed page has to translate.
	position := func(i int) int {
		if page.reversed {
			return count - 1 - i
		}
		return i
	}
	indexOf := func(id string) int {
		for i := 0; i < count; i++ {
			if idAt(position(i)) == id {
				return i
			}
		}
		return -1
	}

	if after := query.Get("after"); after != "" {
		index := indexOf(after)
		if index < 0 {
			return page, fmt.Errorf("item %q not found in this list", after)
		}
		page.from = index + 1
	}
	if before := query.Get("before"); before != "" {
		index := indexOf(before)
		if index < 0 {
			return page, fmt.Errorf("item %q not found in this list", before)
		}
		if index < page.to {
			page.to = index
			// The cursor item and everything past it exist, so the listing is
			// not exhausted; without this a client paginating backwards stops
			// one page early.
			page.hasMore = true
		}
	}
	if page.from > page.to {
		page.from = page.to
	}
	if page.to-page.from > limit {
		page.to = page.from + limit
		page.hasMore = true
	}
	return page, nil
}

func responsesItemID(item interface{}) string {
	object, ok := item.(map[string]interface{})
	if !ok {
		return ""
	}
	id, _ := object["id"].(string)
	return id
}

// lookupResponse resolves the path id within the caller's tenant scope and
// writes the appropriate error when it cannot.
func (h *ResponsesHandler) lookupResponse(w http.ResponseWriter, r *http.Request) (*store.StoredResponse, bool) {
	id := r.PathValue("response_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "response id is required")
		return nil, false
	}

	rs := h.responseStore()
	ownerID := fileOwnerID(r.Context())
	if rs == nil || ownerID == "" {
		writeError(w, http.StatusInternalServerError, "server_error", "response store is unavailable")
		return nil, false
	}

	record, err := rs.GetResponse(r.Context(), id, ownerID)
	if err != nil {
		h.logger.Error("response lookup failed", "error", err, "response_id", id)
		writeError(w, http.StatusInternalServerError, "server_error", "response lookup failed")
		return nil, false
	}
	if record == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("response %q not found", id))
		return nil, false
	}
	return record, true
}

// writeStoredResponse serves a response object, refreshing it from the
// provider first when the turn is still running upstream.
func (h *ResponsesHandler) writeStoredResponse(w http.ResponseWriter, r *http.Request, record *store.StoredResponse) {
	// A retrieve with stream=true follows the turn as SSE rather than returning
	// the object once, which is how a client resumes a background turn whose
	// connection dropped.
	if r.URL.Query().Get("stream") == "true" {
		startingAfter, err := responsesStartingAfter(r.URL.Query().Get("starting_after"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		h.tailStoredResponse(w, r, record, startingAfter)
		return
	}

	if h.proxyResponsesResource(w, r, record, http.MethodGet, "") {
		return
	}
	writeRawJSON(w, http.StatusOK, record.Payload)
}

func (h *ResponsesHandler) deleteResponse(w http.ResponseWriter, r *http.Request, record *store.StoredResponse) {
	// The local row is dropped whether or not the provider owns a copy: leaving
	// it behind would keep serving a response the client asked to forget, and
	// would strand a row that no later delete can reach once the upstream copy
	// is gone.
	if rs := h.responseStore(); rs != nil {
		if err := rs.DeleteResponse(r.Context(), record.ID, record.OwnerID); err != nil {
			h.logger.Warn("response delete failed", "error", err, "response_id", record.ID)
		}
	}
	if h.proxyResponsesResource(w, r, record, http.MethodDelete, "") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id": record.ID, "object": "response", "deleted": true,
	})
}

// proxyResponsesResource forwards a stateful operation to the provider that
// owns the response, reporting whether it has already written the reply. False
// means the caller should fall back to the local record: that is the case for
// translated turns and for providers that do not expose stateful endpoints.
func (h *ResponsesHandler) proxyResponsesResource(
	w http.ResponseWriter,
	r *http.Request,
	record *store.StoredResponse,
	method, suffix string,
) bool {
	if record.UpstreamID == "" || record.DeploymentID == "" {
		return false
	}
	dep, err := getAuthorizedDeploymentByID(r.Context(), h.registry, record.Model, record.DeploymentID)
	if err != nil {
		return false
	}
	api, ok := dep.Provider.(provider.ResponsesResourceAPI)
	if !ok {
		return false
	}

	// The query string rides along on a retrieve so include[] reaches the
	// provider that actually holds the extra data.
	path := "/" + record.UpstreamID + suffix
	if method == http.MethodGet && r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}

	resp, err := api.DoResponsesResourceRequest(r.Context(), method, path, nil)
	if err != nil {
		handleUpstreamErr(w, h.logger, err)
		return true
	}
	defer resp.Body.Close()

	payload, err := readUpstreamResponsesPayload(resp)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return true
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeRawJSON(w, resp.StatusCode, string(payload))
		return true
	}

	// A delete reply is a {"deleted": true} envelope, not a response object, and
	// the row it would refresh is already gone — relay it as-is.
	if method == http.MethodDelete {
		writeRawJSON(w, http.StatusOK, string(payload))
		return true
	}

	// Keep the local record in step so a later poll works even if the provider
	// becomes unreachable.
	h.refreshStoredResponse(r.Context(), record, payload)
	writeRawJSON(w, http.StatusOK, record.Payload)
	return true
}

// readUpstreamResponsesPayload reads a provider reply, capped so a misbehaving
// upstream cannot exhaust memory.
func readUpstreamResponsesPayload(resp *http.Response) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponsesPayloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read provider response payload")
	}
	return payload, nil
}

const maxResponsesPayloadBytes = 32 << 20

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeRawJSON relays an already-encoded JSON document without re-parsing it.
func writeRawJSON(w http.ResponseWriter, status int, payload string) {
	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, payload)
}

func (h *ResponsesHandler) refreshStoredResponse(ctx context.Context, record *store.StoredResponse, payload []byte) {
	var object map[string]interface{}
	if json.Unmarshal(payload, &object) != nil {
		return
	}
	object["model"] = record.Model
	encoded, err := json.Marshal(object)
	if err != nil {
		return
	}
	status, _ := object["status"].(string)

	wasTerminal := isTerminalResponsesStatus(record.Status)
	record.Payload = string(encoded)
	if status != "" {
		record.Status = status
	}
	applied := false
	if rs := h.responseStore(); rs != nil {
		var err error
		applied, err = rs.UpdateResponse(ctx, record)
		if err != nil {
			h.logger.Warn("response refresh failed", "error", err, "response_id", record.ID)
		}
	}

	// The poll that carries the answer is the one moment a native background
	// turn can join its conversation: it was queued with an empty output when
	// it started, so appending then filed a question with no reply.
	//
	// Gated on the write having landed, not just on the transition this poller
	// observed. Two concurrent pollers both read a non-terminal row and both
	// see it finish; only one of them actually writes, and only that one may
	// append — otherwise the turn enters the conversation twice.
	if applied && !wasTerminal && isTerminalResponsesStatus(record.Status) {
		h.appendStoredTurnToConversation(ctx, record)
	}
}

// updateStoredResponseStatus moves the stored turn to status and reports
// whether the write landed. It does not land when the turn reached a terminal
// state first — the answer wins over a cancellation that arrived late.
//
// Only the status and payload columns are written. Writing the whole row from
// this snapshot would roll the event log back to whatever the caller happens to
// hold, and a streaming worker is appending to that log concurrently.
func (h *ResponsesHandler) updateStoredResponseStatus(ctx context.Context, record *store.StoredResponse, status string) bool {
	rs := h.responseStore()
	if rs == nil {
		return false
	}
	payload := patchResponsesPayloadStatus(record.Payload, status)
	applied, err := rs.UpdateResponseStatus(ctx, record.ID, record.OwnerID, status, payload)
	if err != nil {
		h.logger.Warn("response status update failed", "error", err, "response_id", record.ID)
		return false
	}
	if !applied {
		return false
	}
	record.Status = status
	if payload != "" {
		record.Payload = payload
	}
	return true
}

// patchResponsesPayloadStatus keeps the stored payload's status in step with
// the row's column. The store owns the implementation because the stale-turn
// sweep needs it too, and two copies of this would be two ways for the payload
// and the column to disagree.
func patchResponsesPayloadStatus(payload, status string) string {
	return store.PatchResponsePayload(payload, status, "", "")
}

// --- background turns ---

// startBackgroundResponse answers immediately with a queued response and runs
// the turn detached, so the client polls GET /v1/responses/{id}. Only the
// translated path needs this; native providers run background turns upstream.
func (h *ResponsesHandler) startBackgroundResponse(
	w http.ResponseWriter,
	r *http.Request,
	rReq *responsesRequest,
	openaiReq *provider.CompletionRequest,
) {
	record, ok := h.queueBackgroundResponse(w, r, rReq)
	if !ok {
		return
	}

	// context.WithoutCancel: the turn must outlive the HTTP request that
	// started it, but stay cancellable through POST .../cancel.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.backgroundDeadline())
	backgroundResponses.Store(record.ID, cancel)

	// Read the queued payload before the worker starts: from that point the
	// record belongs to the goroutine.
	queued := record.Payload
	go h.runBackgroundResponse(ctx, cancel, r, rReq, openaiReq, record)

	writeRawJSON(w, http.StatusOK, queued)
}

// queueBackgroundResponse stores the queued placeholder a background turn is
// polled through, and reports whether it managed to. Both background paths
// (streamed and not) start here, so a client sees the same queued object.
func (h *ResponsesHandler) queueBackgroundResponse(
	w http.ResponseWriter,
	r *http.Request,
	rReq *responsesRequest,
) (*store.StoredResponse, bool) {
	rs := h.responseStore()
	ownerID := fileOwnerID(r.Context())
	if rs == nil || ownerID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"background responses require a configured response store")
		return nil, false
	}

	queued := newResponsesResponse(newResponseID(), time.Now().Unix(), rReq)
	queued.Status = "queued"
	setResponsesOutput(queued, nil)

	record, err := h.newStoredResponse(ownerID, rReq, queued, nil, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "failed to queue background response")
		return nil, false
	}
	if err := rs.CreateResponse(r.Context(), record); err != nil {
		h.logger.Error("background response queue failed", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "failed to queue background response")
		return nil, false
	}
	return record, true
}

func (h *ResponsesHandler) runBackgroundResponse(
	ctx context.Context,
	cancel context.CancelFunc,
	r *http.Request,
	rReq *responsesRequest,
	openaiReq *provider.CompletionRequest,
	record *store.StoredResponse,
) {
	defer cancel()
	defer backgroundResponses.Delete(record.ID)

	start := time.Now()
	resp, dep, err := h.completeResponses(ctx, openaiReq)

	// The cancellation check has to survive the cancelled context it is
	// reacting to, or the status write is rejected along with the call it is
	// recording.
	persistCtx := context.WithoutCancel(ctx)
	if err != nil {
		if ctx.Err() != nil {
			h.updateStoredResponseStatus(persistCtx, record, "cancelled")
			return
		}
		h.logger.Error("background response failed", "error", err, "response_id", record.ID)
		h.failStoredResponse(persistCtx, record, rReq, err)
		return
	}
	// A provider that ignores cancellation still answers eventually. That late
	// answer must not overwrite the cancelled state the client has already been
	// shown.
	if ctx.Err() != nil {
		h.updateStoredResponseStatus(persistCtx, record, "cancelled")
		return
	}

	final := h.openAIToResponses(resp, rReq)
	final.ID = record.ID
	final.Background = true

	payload, err := json.Marshal(final)
	if err != nil {
		h.failStoredResponse(persistCtx, record, rReq, err)
		return
	}
	record.Payload = string(payload)
	record.Status = final.Status
	if dep != nil {
		record.DeploymentID = dep.ID
	}
	persisted := true
	if rs := h.responseStore(); rs != nil {
		var err error
		persisted, err = rs.UpdateResponse(persistCtx, record)
		if err != nil {
			h.logger.Warn("background response persist failed", "error", err, "response_id", record.ID)
			persisted = false
		}
	}
	// A detached turn belongs to its conversation exactly as a foreground one
	// does. Writing only the response row left the conversation missing both
	// the input and the answer, so the next turn replayed an incomplete thread.
	//
	// Only when the row actually took the answer, though: a cancel or the stale
	// sweep can close the turn during this window, and the write is then
	// refused — appending anyway would file an answer the client was told never
	// arrived.
	if persisted {
		h.appendTurnToConversation(persistCtx, rReq, responsesOutputItems(final))
	}
	if dep != nil {
		h.logSpend(r, rReq.Model, dep, resp, time.Since(start))
	}
}

func (h *ResponsesHandler) failStoredResponse(
	ctx context.Context,
	record *store.StoredResponse,
	rReq *responsesRequest,
	cause error,
) {
	failed := newResponsesResponse(record.ID, time.Now().Unix(), rReq)
	failed.Status = "failed"
	failed.Error = &responsesError{Code: "upstream_error", Message: cause.Error()}
	setResponsesOutput(failed, nil)

	payload, err := json.Marshal(failed)
	if err != nil {
		return
	}
	record.Payload = string(payload)
	record.Status = "failed"
	if rs := h.responseStore(); rs != nil {
		if _, err := rs.UpdateResponse(ctx, record); err != nil {
			h.logger.Warn("background response failure persist failed", "error", err, "response_id", record.ID)
		}
	}
}

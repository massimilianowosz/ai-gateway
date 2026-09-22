package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// sseEvent is one parsed Responses SSE frame.
type sseEvent struct {
	Type           string          `json:"type"`
	SequenceNumber int             `json:"sequence_number"`
	Response       json.RawMessage `json:"response"`
}

func parseResponsesSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event sseEvent
		require.NoError(t, json.Unmarshal([]byte(data), &event), "unparseable SSE frame: %s", data)
		events = append(events, event)
	}
	return events
}

func eventTypes(events []sseEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func postResponses(t *testing.T, handler *ResponsesHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedResponsesRequest("POST", "/v1/responses", body))
	return w
}

func retrieveResponse(t *testing.T, handler *ResponsesHandler, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := authedResponsesRequest("GET", "/v1/responses/"+id+query, "")
	req.SetPathValue("response_id", id)
	handler.ServeObject(w, req)
	return w
}

func helloStreamChunks() [][]byte {
	return [][]byte{
		[]byte(`{"choices":[{"index":0,"delta":{"content":"Ciao"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{"content":" mondo"}}]}`),
		[]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}
}

func TestResponsesHandler_BackgroundStreamDeliversAndPersistsTheTurn(t *testing.T) {
	mock := &mockResponsesProvider{streamChunks: helloStreamChunks()}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := postResponses(t, handler, `{"model":"translated-model","input":"Ciao","background":true,"stream":true}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))

	events := parseResponsesSSE(t, w.Body.String())
	require.NotEmpty(t, events, "a background stream must reach the client that opened it")
	types := eventTypes(events)
	// A detached turn is accepted before it starts, so it opens on queued.
	assert.Equal(t, "response.queued", types[0])
	assert.Equal(t, "response.created", types[1])
	assert.Contains(t, types, "response.output_text.delta")
	assert.Equal(t, "response.completed", types[len(types)-1])

	for i, event := range events {
		assert.Equal(t, i, event.SequenceNumber, "sequence numbers must be dense and ordered")
	}

	// The same turn is retrievable afterwards, with the text it streamed.
	get := retrieveResponse(t, handler, storedResponseID(t, events), "")
	require.Equal(t, http.StatusOK, get.Code)
	var stored responsesResponse
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &stored))
	assert.Equal(t, "completed", stored.Status)
	require.NotNil(t, stored.OutputText)
	assert.Equal(t, "Ciao mondo", *stored.OutputText)
}

// storedResponseID reads the response id out of the stream's first event.
func storedResponseID(t *testing.T, events []sseEvent) string {
	t.Helper()
	require.NotEmpty(t, events)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(events[0].Response, &created))
	require.NotEmpty(t, created.ID)
	return created.ID
}

func TestResponsesHandler_BackgroundStreamResumesFromSequenceNumber(t *testing.T) {
	mock := &mockResponsesProvider{streamChunks: helloStreamChunks()}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	first := postResponses(t, handler, `{"model":"translated-model","input":"Ciao","background":true,"stream":true}`)
	require.Equal(t, http.StatusOK, first.Code)
	events := parseResponsesSSE(t, first.Body.String())
	require.True(t, len(events) > 3)
	id := storedResponseID(t, events)

	// A client that dropped after event 2 resumes from there and must see the
	// rest of the turn exactly once, terminal event included.
	resumed := retrieveResponse(t, handler, id, "?stream=true&starting_after=2")
	require.Equal(t, http.StatusOK, resumed.Code)

	replayed := parseResponsesSSE(t, resumed.Body.String())
	require.NotEmpty(t, replayed)
	assert.Equal(t, 3, replayed[0].SequenceNumber, "resume must continue after the given sequence number")
	assert.Equal(t, "response.completed", replayed[len(replayed)-1].Type)
	assert.Len(t, replayed, len(events)-3)
}

func TestResponsesHandler_RetrieveWithStreamReplaysTheWholeTurn(t *testing.T) {
	mock := &mockResponsesProvider{streamChunks: helloStreamChunks()}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	first := postResponses(t, handler, `{"model":"translated-model","input":"Ciao","background":true,"stream":true}`)
	require.Equal(t, http.StatusOK, first.Code)
	original := parseResponsesSSE(t, first.Body.String())
	id := storedResponseID(t, original)

	replayed := parseResponsesSSE(t, retrieveResponse(t, handler, id, "?stream=true").Body.String())
	assert.Equal(t, eventTypes(original), eventTypes(replayed),
		"a retrieve with stream=true and no cursor replays the turn from the start")
}

func TestResponsesHandler_RetrieveWithStreamOnANonStreamedTurnStillTerminates(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("Fatto.")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := postResponses(t, handler, `{"model":"translated-model","input":"Lavora","background":true}`)
	require.Equal(t, http.StatusOK, w.Code)
	var queued responsesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &queued))

	// The turn was never streamed, so there is no recorded event log — the
	// follower still has to receive a terminal event or it would hang.
	require.Eventually(t, func() bool {
		events := parseResponsesSSE(t, retrieveResponse(t, handler, queued.ID, "?stream=true").Body.String())
		return len(events) > 0 && events[len(events)-1].Type == "response.completed"
	}, 3*time.Second, 50*time.Millisecond)
}

func TestResponsesHandler_BackgroundStreamReportsUpstreamFailure(t *testing.T) {
	mock := &mockResponsesProvider{streamErr: assertUnreachable}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	w := postResponses(t, handler, `{"model":"translated-model","input":"Ciao","background":true,"stream":true}`)
	require.Equal(t, http.StatusOK, w.Code)

	events := parseResponsesSSE(t, w.Body.String())
	require.NotEmpty(t, events, "a turn that never opened must still terminate the stream")
	assert.Equal(t, "response.failed", events[len(events)-1].Type)
}

func TestResponsesHandler_StartingAfterIsValidated(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	created := postResponses(t, handler, `{"model":"translated-model","input":"Ciao"}`)
	require.Equal(t, http.StatusOK, created.Code)
	var response responsesResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &response))

	for _, query := range []string{"?stream=true&starting_after=abc", "?stream=true&starting_after=-2"} {
		w := retrieveResponse(t, handler, response.ID, query)
		assert.Equal(t, http.StatusBadRequest, w.Code, "query %s must be rejected", query)
	}
}

func TestStoredResponsesSink_StopsOnACancelFromAnotherReplica(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, db := newStatefulResponsesHandler(t, "translated-model", mock)
	rs := db.(store.ResponseStore)

	record := &store.StoredResponse{
		ID: "resp_remote", OwnerID: "key:key-hash-1", Model: "translated-model",
		Status: "in_progress", CreatedAt: time.Now(),
	}
	require.NoError(t, rs.CreateResponse(context.Background(), record))

	cancelled := false
	sink := newStoredResponsesSink(context.Background(), handler, record, func() { cancelled = true })

	// A cancel that landed on another replica leaves only this mark; the worker
	// has to notice it while streaming.
	require.NoError(t, rs.RequestResponseCancel(context.Background(), record.ID, record.OwnerID))

	sink.writeEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","sequence_number":1}`))
	sink.lastFlush = time.Now().Add(-time.Second) // due for a flush
	sink.writeEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","sequence_number":2}`))

	assert.True(t, cancelled, "the worker must stop when a cancel is marked in the store")
}

func TestResponsesHandler_BackgroundRequiresStore(t *testing.T) {
	mock := &mockResponsesProvider{completeResp: textCompletion("ok")}
	handler, _ := newStatefulResponsesHandler(t, "translated-model", mock)

	// On a translated model the gateway runs the turn, so its record is the
	// only way back to it: opting out of storage must be refused rather than
	// silently ignored. Native providers keep their own copy and are exempt.
	w := postResponses(t, handler, `{"model":"translated-model","input":"Ciao","background":true,"store":false}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "store")
	assert.Nil(t, mock.lastReq)
}

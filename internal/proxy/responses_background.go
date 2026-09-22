package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const (
	// responsesEventFlushInterval bounds how often a streaming background turn
	// writes its event log. Every event would mean a database round-trip per
	// token; batching keeps the write rate sane while staying well inside the
	// latency a background turn is expected to have.
	responsesEventFlushInterval = 250 * time.Millisecond

	// responsesTailPollInterval is how often a client following a background
	// turn re-reads its event log. The turn runs detached — possibly on another
	// replica — so the store is the only channel between the two.
	responsesTailPollInterval = 100 * time.Millisecond

	// maxResponsesTailDuration caps a single follow. Hitting it drops the
	// connection rather than holding a goroutine forever; the client resumes
	// with starting_after, which is what that parameter exists for.
	maxResponsesTailDuration = 30 * time.Minute
)

// terminalResponsesStatuses are the states a turn does not leave.
var terminalResponsesStatuses = map[string]struct{}{
	"completed":  {},
	"incomplete": {},
	"failed":     {},
	"cancelled":  {},
}

func isTerminalResponsesStatus(status string) bool {
	_, terminal := terminalResponsesStatuses[status]
	return terminal
}

// storedResponsesSink records the events of a background turn on its stored
// response instead of writing them to a client. It is the only writer of that
// row while the turn runs: it tracks the status the events imply, keeps the
// payload in step with the terminal event, and watches for a cancel raised
// elsewhere.
type storedResponsesSink struct {
	handler *ResponsesHandler
	ctx     context.Context
	record  *store.StoredResponse
	cancel  context.CancelFunc

	events    []json.RawMessage
	lastFlush time.Time
	// terminalPersisted records whether the write that closed the turn actually
	// landed. A cancel or the stale sweep can close the row first, and the
	// worker's answer is then refused — appending it to the conversation anyway
	// would file a reply the client was told never arrived.
	terminalPersisted bool
}

func newStoredResponsesSink(ctx context.Context, h *ResponsesHandler, record *store.StoredResponse, cancel context.CancelFunc) *storedResponsesSink {
	return &storedResponsesSink{handler: h, ctx: ctx, record: record, cancel: cancel, lastFlush: time.Now()}
}

func (s *storedResponsesSink) writeEvent(eventType string, payload []byte) {
	s.events = append(s.events, append(json.RawMessage(nil), payload...))

	// The terminal case comes first: a turn that fails before it produces
	// anything emits its terminal event as the very first one, and treating
	// that as an opening event would leave the row in_progress forever.
	switch eventType {
	case "response.queued":
		s.record.Status = "queued"
	case "response.created", "response.in_progress":
		s.record.Status = "in_progress"
	case "response.completed", "response.incomplete", "response.failed":
		s.record.Status = responsesStatusForEvent(eventType)
		s.adoptTerminalPayload(payload)
		s.terminalPersisted = s.flush()
		return
	}

	// The opening event is published immediately: a follower would otherwise
	// sit on an empty log for a whole flush interval before seeing the turn
	// start.
	if len(s.events) == 1 || time.Since(s.lastFlush) >= responsesEventFlushInterval {
		_ = s.flush()
		s.checkCancelRequested()
	}
}

func responsesStatusForEvent(eventType string) string {
	switch eventType {
	case "response.incomplete":
		return "incomplete"
	case "response.failed":
		return "failed"
	default:
		return "completed"
	}
}

// adoptTerminalPayload lifts the response object out of the terminal event, so
// a later GET serves the same object the stream ended on.
func (s *storedResponsesSink) adoptTerminalPayload(payload []byte) {
	var event struct {
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil || len(event.Response) == 0 {
		return
	}
	s.record.Payload = string(event.Response)
}

// flush writes the event log, reporting whether the write landed.
func (s *storedResponsesSink) flush() bool {
	rs := s.handler.responseStore()
	if rs == nil {
		return false
	}
	encoded, err := json.Marshal(s.events)
	if err != nil {
		return false
	}
	s.record.Events = string(encoded)
	s.lastFlush = time.Now()
	applied, err := rs.UpdateResponse(s.ctx, s.record)
	if err != nil {
		s.handler.logger.Warn("background stream persist failed", "error", err, "response_id", s.record.ID)
		return false
	}
	return applied
}

// checkCancelRequested stops the turn when a cancel landed on another replica,
// which has no way to reach this worker's context directly.
func (s *storedResponsesSink) checkCancelRequested() {
	rs := s.handler.responseStore()
	if rs == nil || s.cancel == nil {
		return
	}
	requested, err := rs.ResponseCancelRequested(s.ctx, s.record.ID)
	if err != nil || !requested {
		return
	}
	s.cancel()
}

// --- background streaming ---

// startBackgroundStream answers a background:true, stream:true turn. The turn
// runs detached, writing its events to the stored response, and this request
// then follows that log like any other reader — so a client that drops the
// connection and reconnects with starting_after sees exactly the same stream.
func (h *ResponsesHandler) startBackgroundStream(
	w http.ResponseWriter,
	r *http.Request,
	rReq *responsesRequest,
	openaiReq *provider.CompletionRequest,
) {
	record, ok := h.queueBackgroundResponse(w, r, rReq)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.backgroundDeadline())
	backgroundResponses.Store(record.ID, cancel)

	// The worker gets its own copy of the row: it mutates the record as the
	// turn advances, while this request reads its own and re-reads from the
	// store on every poll.
	worker := *record
	go h.runBackgroundStream(ctx, cancel, r, rReq, openaiReq, &worker)

	h.tailStoredResponse(w, r, record, -1)
}

func (h *ResponsesHandler) runBackgroundStream(
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
	openaiReq.Stream = true

	sink := newStoredResponsesSink(ctx, h, record, cancel)
	out := newResponsesStreamWriterTo(sink)

	// A detached turn opens on response.queued, which is what tells a client
	// the work is accepted but has not started yet. The live path has no such
	// state and opens on response.created.
	queued := newResponsesResponse(record.ID, time.Now().Unix(), rReq)
	queued.Status = "queued"
	setResponsesOutput(queued, nil)
	out.send("response.queued", map[string]interface{}{"response": queued})

	dep, reader, err := h.openResponsesStream(ctx, openaiReq)
	if err != nil {
		h.logger.Error("background stream failed", "error", err, "response_id", record.ID)
		h.emitStoredStreamFailure(out, rReq, record, err)
		return
	}

	result := h.runResponsesTurn(ctx, out, record.ID, rReq, reader)

	// A turn cancelled mid-flight ends cancelled, whatever the upstream stream
	// managed to produce before it stopped.
	if ctx.Err() != nil {
		h.updateStoredResponseStatus(context.WithoutCancel(ctx), record, "cancelled")
		return
	}
	// A detached turn belongs to its conversation exactly as a foreground one
	// does; the sink writes the response row but knows nothing about threads.
	// Only when the row took the answer — see terminalPersisted.
	if sink.terminalPersisted {
		h.appendTurnToConversation(context.WithoutCancel(ctx), rReq, responsesOutputItems(result.final))
	}
	h.logStreamSpend(r, rReq.Model, dep, openaiReq, result.usage, result.completionChars, time.Since(start))
}

// emitStoredStreamFailure closes out a background turn that never opened,
// through the same event log a client is following.
// The sink persists on the terminal event, so emitting it is all this needs.
func (h *ResponsesHandler) emitStoredStreamFailure(
	out *responsesStreamWriter,
	rReq *responsesRequest,
	record *store.StoredResponse,
	cause error,
) {
	failed := newResponsesResponse(record.ID, time.Now().Unix(), rReq)
	failed.Status = "failed"
	failed.Error = responsesErrorFrom(cause)
	setResponsesOutput(failed, nil)
	out.send("response.failed", map[string]interface{}{"response": failed})
}

// --- following a stored turn ---

// tailStoredResponse streams a stored turn's events to the client, following
// the turn until it reaches a terminal state. startingAfter is the last
// sequence number the client already has; -1 means "from the beginning".
func (h *ResponsesHandler) tailStoredResponse(
	w http.ResponseWriter,
	r *http.Request,
	record *store.StoredResponse,
	startingAfter int,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming is not supported on this connection")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sent := startingAfter
	deadline := time.Now().Add(maxResponsesTailDuration)
	rs := h.responseStore()

	for {
		sent = writeResponsesEventsAfter(w, flusher, record.Events, sent)

		if isTerminalResponsesStatus(record.Status) {
			// A turn that was not streamed has no event log to replay; the
			// client still needs one terminal event to close the stream.
			if record.Events == "" {
				writeSyntheticTerminalEvent(w, flusher, record, sent+1)
			}
			return
		}
		if rs == nil || time.Now().After(deadline) {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-time.After(responsesTailPollInterval):
		}

		refreshed, err := rs.GetResponse(r.Context(), record.ID, record.OwnerID)
		if err != nil || refreshed == nil {
			return
		}
		record = refreshed
	}
}

// writeResponsesEventsAfter relays the recorded events the client has not seen
// yet and returns the highest sequence number written.
func writeResponsesEventsAfter(w http.ResponseWriter, flusher http.Flusher, events string, sent int) int {
	if events == "" {
		return sent
	}
	var recorded []json.RawMessage
	if json.Unmarshal([]byte(events), &recorded) != nil {
		return sent
	}

	for _, event := range recorded {
		var header struct {
			Type           string `json:"type"`
			SequenceNumber int    `json:"sequence_number"`
		}
		if json.Unmarshal(event, &header) != nil || header.SequenceNumber <= sent {
			continue
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", header.Type, event)
		sent = header.SequenceNumber
	}
	flusher.Flush()
	return sent
}

// writeSyntheticTerminalEvent ends the stream for a turn that finished without
// producing an event log, e.g. a non-streamed background turn a client chose to
// follow with stream=true.
func writeSyntheticTerminalEvent(w http.ResponseWriter, flusher http.Flusher, record *store.StoredResponse, sequence int) {
	eventType := "response." + record.Status
	if record.Status == "cancelled" {
		// There is no response.cancelled event in the protocol; a cancelled
		// turn is reported as failed so the client's parser terminates.
		eventType = "response.failed"
	}

	payload := map[string]interface{}{
		"type":            eventType,
		"sequence_number": sequence,
	}
	if record.Payload != "" {
		payload["response"] = json.RawMessage(record.Payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, encoded)
	flusher.Flush()
}

// responsesStartingAfter reads the resume cursor from a retrieve request. The
// absence of the parameter means "replay everything".
func responsesStartingAfter(query string) (int, error) {
	if query == "" {
		return -1, nil
	}
	value, err := strconv.Atoi(query)
	if err != nil || value < 0 {
		return -1, fmt.Errorf("invalid starting_after %q: expected a non-negative sequence number", query)
	}
	return value, nil
}

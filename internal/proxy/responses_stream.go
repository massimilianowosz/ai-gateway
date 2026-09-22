package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

var responsesIDCounter atomic.Uint64

// newResponseID / newResponsesItemID mint the ids the Responses API exposes.
// The counter guards against collisions when two items are created inside the
// same nanosecond, which a plain timestamp does not.
func newResponseID() string { return newResponsesItemID("resp") }

func newResponsesItemID(prefix string) string {
	return fmt.Sprintf("%s_%d%03d", prefix, time.Now().UnixNano(), responsesIDCounter.Add(1)%1000)
}

// responsesEventSink receives the events of one streamed turn. The live path
// writes them straight to the client as SSE; a background turn records them so
// a client that connects late, or reconnects after a drop, can replay them.
type responsesEventSink interface {
	writeEvent(eventType string, payload []byte)
}

// httpResponsesSink streams events to the client as they happen.
type httpResponsesSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	buf     *bytes.Buffer
}

func (s *httpResponsesSink) writeEvent(eventType string, payload []byte) {
	s.buf.Reset()
	s.buf.WriteString("event: ")
	s.buf.WriteString(eventType)
	s.buf.WriteString("\ndata: ")
	s.buf.Write(payload)
	s.buf.WriteString("\n\n")
	_, _ = s.w.Write(s.buf.Bytes())
	s.flusher.Flush()
}

// responsesStreamWriter serialises Responses SSE events with the monotonic
// sequence_number the protocol requires.
type responsesStreamWriter struct {
	sink responsesEventSink
	seq  int
}

func newResponsesStreamWriter(w http.ResponseWriter, flusher http.Flusher, buf *bytes.Buffer) *responsesStreamWriter {
	return &responsesStreamWriter{sink: &httpResponsesSink{w: w, flusher: flusher, buf: buf}}
}

func newResponsesStreamWriterTo(sink responsesEventSink) *responsesStreamWriter {
	return &responsesStreamWriter{sink: sink}
}

// send emits an event, injecting "type" and "sequence_number" so callers never
// have to repeat them.
func (s *responsesStreamWriter) send(event string, fields map[string]interface{}) {
	payload := make(map[string]interface{}, len(fields)+2)
	for k, v := range fields {
		payload[k] = v
	}
	payload["type"] = event
	payload["sequence_number"] = s.seq
	s.seq++

	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.sink.writeEvent(event, encoded)
}

// responsesStreamState accumulates the output items of one streamed turn.
type responsesStreamState struct {
	out *responsesStreamWriter

	outputIndex int
	items       []interface{}

	reasoningItemID string
	reasoningOpen   bool
	reasoningBuf    strings.Builder

	textItemID   string
	textOpen     bool
	textBuf      strings.Builder
	textLogprobs []json.RawMessage

	refusalBuf strings.Builder

	toolCalls map[int]*responsesToolCallState
	toolOrder []int
}

type responsesToolCallState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	started     bool
	args        strings.Builder
}

func newResponsesStreamState(out *responsesStreamWriter) *responsesStreamState {
	return &responsesStreamState{out: out, toolCalls: make(map[int]*responsesToolCallState)}
}

// --- reasoning ---

func (s *responsesStreamState) appendReasoning(delta string) {
	if !s.reasoningOpen {
		s.reasoningItemID = newResponsesItemID("rs")
		s.reasoningOpen = true
		s.reasoningBuf.Reset()
		s.out.send("response.output_item.added", map[string]interface{}{
			"output_index": s.outputIndex,
			"item": responsesReasoningItem{
				Type: "reasoning", ID: s.reasoningItemID, Status: "in_progress",
				Summary: []responsesReasoningSummaryPart{},
			},
		})
		s.out.send("response.reasoning_summary_part.added", map[string]interface{}{
			"item_id": s.reasoningItemID, "output_index": s.outputIndex, "summary_index": 0,
			"part": responsesReasoningSummaryPart{Type: "summary_text", Text: ""},
		})
	}
	s.reasoningBuf.WriteString(delta)
	s.out.send("response.reasoning_summary_text.delta", map[string]interface{}{
		"item_id": s.reasoningItemID, "output_index": s.outputIndex, "summary_index": 0,
		"delta": delta,
	})
}

func (s *responsesStreamState) closeReasoning() {
	if !s.reasoningOpen {
		return
	}
	text := s.reasoningBuf.String()
	part := responsesReasoningSummaryPart{Type: "summary_text", Text: text}
	s.out.send("response.reasoning_summary_text.done", map[string]interface{}{
		"item_id": s.reasoningItemID, "output_index": s.outputIndex, "summary_index": 0, "text": text,
	})
	s.out.send("response.reasoning_summary_part.done", map[string]interface{}{
		"item_id": s.reasoningItemID, "output_index": s.outputIndex, "summary_index": 0, "part": part,
	})
	item := responsesReasoningItem{
		Type: "reasoning", ID: s.reasoningItemID, Status: "completed",
		Summary: []responsesReasoningSummaryPart{part},
	}
	s.out.send("response.output_item.done", map[string]interface{}{
		"output_index": s.outputIndex, "item": item,
	})
	s.items = append(s.items, item)
	s.reasoningOpen = false
	s.outputIndex++
}

// --- assistant message (text + refusal) ---

func (s *responsesStreamState) openText() {
	if s.textOpen {
		return
	}
	s.closeReasoning()
	s.textItemID = newResponsesItemID("msg")
	s.textOpen = true
	s.textBuf.Reset()
	s.textLogprobs = nil
	s.refusalBuf.Reset()
	s.out.send("response.output_item.added", map[string]interface{}{
		"output_index": s.outputIndex,
		"item": responsesMessageItem{
			Type: "message", ID: s.textItemID, Status: "in_progress",
			Role: "assistant", Content: []interface{}{},
		},
	})
}

func (s *responsesStreamState) appendText(delta string, logprobs []json.RawMessage) {
	first := !s.textOpen
	s.openText()
	if first {
		s.out.send("response.content_part.added", map[string]interface{}{
			"item_id": s.textItemID, "output_index": s.outputIndex, "content_index": 0,
			"part": responsesOutputTextContent{Type: "output_text", Text: "", Annotations: []interface{}{}},
		})
	}
	s.textBuf.WriteString(delta)
	s.textLogprobs = append(s.textLogprobs, logprobs...)

	event := map[string]interface{}{
		"item_id": s.textItemID, "output_index": s.outputIndex, "content_index": 0,
		"delta": delta,
	}
	// Only the tokens of this delta ride along with it; the whole set is
	// repeated on the part when the text closes.
	if len(logprobs) > 0 {
		event["logprobs"] = logprobs
	}
	s.out.send("response.output_text.delta", event)
}

func (s *responsesStreamState) appendRefusal(delta string) {
	first := !s.textOpen
	s.openText()
	if first {
		s.out.send("response.content_part.added", map[string]interface{}{
			"item_id": s.textItemID, "output_index": s.outputIndex, "content_index": 0,
			"part": responsesRefusalContent{Type: "refusal", Refusal: ""},
		})
	}
	s.refusalBuf.WriteString(delta)
	s.out.send("response.refusal.delta", map[string]interface{}{
		"item_id": s.textItemID, "output_index": s.outputIndex, "content_index": 0,
		"delta": delta,
	})
}

func (s *responsesStreamState) closeText() {
	if !s.textOpen {
		return
	}
	var content []interface{}
	if text := s.textBuf.String(); text != "" || s.refusalBuf.Len() == 0 {
		part := responsesOutputTextContent{
			Type: "output_text", Text: text, Annotations: []interface{}{}, Logprobs: s.textLogprobs,
		}
		done := map[string]interface{}{
			"item_id": s.textItemID, "output_index": s.outputIndex, "content_index": 0, "text": text,
		}
		if len(s.textLogprobs) > 0 {
			done["logprobs"] = s.textLogprobs
		}
		s.out.send("response.output_text.done", done)
		s.out.send("response.content_part.done", map[string]interface{}{
			"item_id": s.textItemID, "output_index": s.outputIndex, "content_index": 0, "part": part,
		})
		content = append(content, part)
	}
	if refusal := s.refusalBuf.String(); refusal != "" {
		part := responsesRefusalContent{Type: "refusal", Refusal: refusal}
		s.out.send("response.refusal.done", map[string]interface{}{
			"item_id": s.textItemID, "output_index": s.outputIndex,
			"content_index": len(content), "refusal": refusal,
		})
		s.out.send("response.content_part.done", map[string]interface{}{
			"item_id": s.textItemID, "output_index": s.outputIndex,
			"content_index": len(content), "part": part,
		})
		content = append(content, part)
	}

	item := responsesMessageItem{
		Type: "message", ID: s.textItemID, Status: "completed",
		Role: "assistant", Content: content,
	}
	s.out.send("response.output_item.done", map[string]interface{}{
		"output_index": s.outputIndex, "item": item,
	})
	s.items = append(s.items, item)
	s.textOpen = false
	s.outputIndex++
}

// --- tool calls ---

func (s *responsesStreamState) appendToolCall(index int, id, name, arguments string) {
	state, exists := s.toolCalls[index]
	if !exists {
		s.closeText()
		s.closeReasoning()
		// outputIndex is deliberately not claimed here. A provider may open a
		// call's state with arguments before its name arrives, and the index is
		// only advanced when a call actually starts — so two states created
		// before either could start would both hold the same index, and their
		// response.output_item.added events would collide on it.
		state = &responsesToolCallState{callID: id, name: name}
		s.toolCalls[index] = state
		s.toolOrder = append(s.toolOrder, index)
	} else {
		if id != "" {
			state.callID = id
		}
		if name != "" {
			state.name = name
		}
	}

	if !state.started && state.callID != "" && state.name != "" {
		state.outputIndex = s.outputIndex
		state.itemID = newResponsesItemID("fc")
		s.out.send("response.output_item.added", map[string]interface{}{
			"output_index": state.outputIndex,
			"item": responsesFunctionCallItem{
				Type: "function_call", ID: state.itemID, CallID: state.callID,
				Name: state.name, Arguments: "", Status: "in_progress",
			},
		})
		state.started = true
		s.outputIndex = state.outputIndex + 1
	}

	if arguments != "" && state.started {
		state.args.WriteString(arguments)
		s.out.send("response.function_call_arguments.delta", map[string]interface{}{
			"item_id": state.itemID, "output_index": state.outputIndex, "delta": arguments,
		})
	}
}

func (s *responsesStreamState) closeToolCalls() {
	for _, index := range s.toolOrder {
		state := s.toolCalls[index]
		if state == nil || !state.started {
			continue
		}
		args := state.args.String()
		if args == "" {
			args = "{}"
		}
		s.out.send("response.function_call_arguments.done", map[string]interface{}{
			"item_id": state.itemID, "output_index": state.outputIndex,
			"name": state.name, "arguments": args,
		})
		item := responsesFunctionCallItem{
			Type: "function_call", ID: state.itemID, CallID: state.callID,
			Name: state.name, Arguments: args, Status: "completed",
		}
		s.out.send("response.output_item.done", map[string]interface{}{
			"output_index": state.outputIndex, "item": item,
		})
		s.items = append(s.items, item)
	}
}

func (s *responsesStreamState) finish() {
	s.closeReasoning()
	s.closeText()
	s.closeToolCalls()
}

// --- stream relay ---

// streamChunkDelta is the slice of an upstream chat-completions chunk the
// Responses translation needs.
type streamChunkDelta struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			Refusal          string `json:"refusal"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Logprobs *struct {
			Content []json.RawMessage `json:"content"`
		} `json:"logprobs"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// relayResponsesStream translates OpenAI chat-completions SSE chunks into
// Responses API streaming events.
func (h *ResponsesHandler) relayResponsesStream(w http.ResponseWriter, r *http.Request, rReq *responsesRequest, dep *provider.Deployment, req *provider.CompletionRequest, reader provider.StreamReader, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		reader.Close()
		return
	}

	buf := perf.GetBuffer()
	defer perf.PutBuffer(buf)

	out := newResponsesStreamWriter(w, flusher, buf)
	result := h.runResponsesTurn(r.Context(), out, newResponseID(), rReq, reader)

	h.logStreamSpend(r, rReq.Model, dep, req, result.usage, result.completionChars, time.Since(start))
	// The client has disconnected by now in the cancel case, so persistence
	// must not inherit the request context.
	h.persistResponse(context.WithoutCancel(r.Context()), rReq, result.final, dep, "")
}

// responsesTurnResult is what one translated streaming turn produced, for the
// caller to bill and persist.
type responsesTurnResult struct {
	final           *responsesResponse
	usage           *provider.Usage
	completionChars int
}

// runResponsesTurn translates an upstream chat-completions stream into Responses
// events, emitting them through out. It owns the reader and always emits a
// terminal event, so whatever is attached to the sink sees a complete turn.
func (h *ResponsesHandler) runResponsesTurn(
	ctx context.Context,
	out *responsesStreamWriter,
	respID string,
	rReq *responsesRequest,
	reader provider.StreamReader,
) responsesTurnResult {
	defer reader.Close()

	state := newResponsesStreamState(out)

	inProgress := newResponsesResponse(respID, time.Now().Unix(), rReq)
	inProgress.Status = "in_progress"

	out.send("response.created", map[string]interface{}{"response": inProgress})
	out.send("response.in_progress", map[string]interface{}{"response": inProgress})

	var usage *provider.Usage
	var finishReason string
	var completionChars int

	for {
		chunk, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() == context.Canceled {
				h.logger.Debug("stream closed by client", "error", err)
			} else {
				h.logger.Error("stream read error", "error", err)
			}
			break
		}

		if chunkUsage := streamUsageFromChunk(chunk); chunkUsage != nil {
			usage = chunkUsage
		}

		var parsed streamChunkDelta
		if err := json.Unmarshal(chunk, &parsed); err != nil {
			continue
		}
		if len(parsed.Choices) == 0 {
			continue
		}
		choice := parsed.Choices[0]

		// Providers spell the chain-of-thought delta either way.
		if reasoning := choice.Delta.ReasoningContent + choice.Delta.Reasoning; reasoning != "" {
			state.appendReasoning(reasoning)
		}
		if choice.Delta.Content != "" {
			completionChars += len(choice.Delta.Content)
			var logprobs []json.RawMessage
			if choice.Logprobs != nil {
				logprobs = choice.Logprobs.Content
			}
			state.appendText(choice.Delta.Content, logprobs)
		}
		if choice.Delta.Refusal != "" {
			state.appendRefusal(choice.Delta.Refusal)
		}
		for _, tc := range choice.Delta.ToolCalls {
			state.appendToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	state.finish()

	final := newResponsesResponse(respID, time.Now().Unix(), rReq)
	applyResponsesFinishReason(final, finishReason)
	setResponsesOutput(final, state.items)
	if usage != nil {
		final.Usage = newResponsesUsage(usage)
	}

	// The terminal event must match the status: Codex CLI and the official SDK
	// only end a turn on completed / incomplete / failed.
	terminal := "response.completed"
	if final.Status == "incomplete" {
		terminal = "response.incomplete"
	}
	out.send(terminal, map[string]interface{}{"response": final})

	return responsesTurnResult{final: final, usage: usage, completionChars: completionChars}
}

// responsesErrorFrom turns an upstream failure into the response.error object
// clients read to learn why a turn stopped.
func responsesErrorFrom(err error) *responsesError {
	errMsg := "internal server error"
	errCode := "internal_error"
	// Use errors.As (not a plain type assertion): router.Route wraps the
	// deployment error via fmt.Errorf("all %d attempts failed ...: %w", ...),
	// so the *provider.UpstreamError is one level deep, not the top-level err.
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		errMsg = ue.Message
		switch {
		case ue.Type != "":
			errCode = ue.Type
		case ue.Code != "":
			errCode = ue.Code
		}
	} else if err != nil {
		errMsg = err.Error()
	}
	return &responsesError{Code: errCode, Message: errMsg}
}

// writeSSEError terminates an in-progress Responses API SSE stream after an
// upstream failure (e.g. a ChatGPT/Codex 429 usage-limit error surfaced once
// we're already committed to a 200 SSE response).
//
// IMPORTANT: this must emit a `response.failed` event, not a bare `error`
// event. OpenAI Codex CLI's SSE parser (codex-rs) only treats
// response.completed / response.incomplete / response.failed as
// turn-terminating events; any other event type/shape is silently ignored.
// Previously this sent `event: error` with a non-standard payload, which
// Codex ignored — the handler then returned and closed the connection with
// no terminal event at all, so Codex reported the generic
// "stream disconnected before completion: stream closed before
// response.completed" instead of the real upstream message (e.g. "The
// usage limit has been reached").
func (h *ResponsesHandler) writeSSEError(w http.ResponseWriter, req *responsesRequest, err error) {
	h.logger.Error("upstream error", "error", err)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	failed := newResponsesResponse(newResponseID(), time.Now().Unix(), req)
	failed.Status = "failed"
	failed.Error = responsesErrorFrom(err)

	data, _ := json.Marshal(map[string]interface{}{
		"type":            "response.failed",
		"sequence_number": 0,
		"response":        failed,
	})
	fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", data)
	flusher.Flush()
}

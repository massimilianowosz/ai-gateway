package workflow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

const (
	headerContentType     = "Content-Type"
	headerCacheControl    = "Cache-Control"
	headerConnection      = "Connection"
	headerXAccelBuffering = "X-Accel-Buffering"

	contentTypeJSON        = "application/json"
	contentTypeEventStream = "text/event-stream"

	cacheControlNoCache = "no-cache"
	connectionKeepAlive = "keep-alive"
	noBuffering         = "no"
)

// writeRespond short-circuits the request with a synthetic assistant reply
// built from a workflow's "respond" decision — no LLM call is made.
// respID is used only on the Responses surface, where the caller has already
// minted it so the turn could be persisted under that same id.
func writeRespond(w http.ResponseWriter, isAnthropic, isResponses, streaming bool, respID, model, message string) {
	switch {
	case isAnthropic:
		if streaming {
			writeAnthropicRespondStream(w, model, message)
			return
		}
		writeAnthropicRespond(w, model, message)
	case isResponses:
		if streaming {
			writeResponsesRespondStream(w, respID, model, message)
			return
		}
		writeResponsesRespond(w, respID, model, message)
	default:
		if streaming {
			writeOpenAIRespondStream(w, model, message)
			return
		}
		writeOpenAIRespond(w, model, message)
	}
}

// respondID mints the id of a short-circuited turn. The separator matches the
// gateway's own ids (resp_, msg_, chatcmpl-) so a client cannot tell a
// workflow answer apart by shape alone.
func respondID(prefix string) string {
	sep := "_"
	if prefix == "chatcmpl" {
		sep = "-"
	}
	return fmt.Sprintf("%s%sworkflow%d", prefix, sep, time.Now().UnixNano())
}

// writeJSON writes a single non-streaming JSON response.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeSSEHeaders opens an SSE response, common to all three streaming formats.
func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set(headerContentType, contentTypeEventStream)
	w.Header().Set(headerCacheControl, cacheControlNoCache)
	w.Header().Set(headerConnection, connectionKeepAlive)
	w.Header().Set(headerXAccelBuffering, noBuffering)
	w.WriteHeader(http.StatusOK)
}

// --- OpenAI chat.completions ---

func writeOpenAIRespond(w http.ResponseWriter, model, message string) {
	stop := "stop"
	resp := provider.CompletionResponse{
		ID:      respondID("chatcmpl"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      &provider.Message{Role: "assistant", Content: message},
			FinishReason: &stop,
		}},
		Usage: &provider.Usage{},
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeOpenAIRespondStream(w http.ResponseWriter, model, message string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIRespond(w, model, message)
		return
	}
	writeSSEHeaders(w)

	id := respondID("chatcmpl")
	created := time.Now().Unix()
	stop := "stop"

	// delta is a map, not a provider.Message: that type tags Role and Content
	// without omitempty, so the terminating chunk serialized
	// {"role":"","content":null} where OpenAI sends {}. Strictly typed SDKs
	// fail to decode an empty role.
	writeChunk := func(delta map[string]interface{}, finishReason *string) {
		chunk := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	writeChunk(map[string]interface{}{"role": "assistant", "content": message}, nil)
	writeChunk(map[string]interface{}{}, &stop)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// --- Anthropic /v1/messages ---

func writeAnthropicRespond(w http.ResponseWriter, model, message string) {
	resp := map[string]interface{}{
		"id":   respondID("msg"),
		"type": "message",
		"role": "assistant",
		"content": []map[string]string{
			{"type": "text", "text": message},
		},
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeAnthropicRespondStream(w http.ResponseWriter, model, message string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicRespond(w, model, message)
		return
	}
	writeSSEHeaders(w)

	msgID := respondID("msg")
	writeSSE := func(event string, data interface{}) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		flusher.Flush()
	}

	writeSSE("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []interface{}{},
			"model":   model,
			"usage":   map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	writeSSE("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	writeSSE("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": message},
	})
	writeSSE("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})
	writeSSE("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]int{"output_tokens": 0},
	})
	writeSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// --- OpenAI Responses API ---

// responsesRespondPayload builds the Responses object a short-circuited turn
// returns. It is split out so the same bytes can be persisted before they are
// written: a client whose next turn chains on previous_response_id needs the
// row to exist, and returning an id nothing stored made that turn 404.
func responsesRespondPayload(respID, itemID, model, message string) map[string]interface{} {
	return map[string]interface{}{
		"id":                  respID,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               model,
		"parallel_tool_calls": true,
		"output": []map[string]interface{}{{
			"type":   "message",
			"id":     itemID,
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        message,
				"annotations": []interface{}{},
			}},
		}},
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
	}
}

func writeResponsesRespond(w http.ResponseWriter, respID, model, message string) {
	writeJSON(w, http.StatusOK, responsesRespondPayload(respID, respondID("msg"), model, message))
}

func writeResponsesRespondStream(w http.ResponseWriter, respID, model, message string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeResponsesRespond(w, respID, model, message)
		return
	}
	writeSSEHeaders(w)

	itemID := respondID("msg")
	seq := 0
	nextSeq := func() int {
		seq++
		return seq - 1
	}
	writeSSE := func(event string, data interface{}) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
		flusher.Flush()
	}

	baseResponse := map[string]interface{}{
		"id":     respID,
		"object": "response",
		"status": "in_progress",
		"model":  model,
		"output": []interface{}{},
	}
	writeSSE("response.created", map[string]interface{}{
		"type":            "response.created",
		"sequence_number": nextSeq(),
		"response":        baseResponse,
	})
	writeSSE("response.in_progress", map[string]interface{}{
		"type":            "response.in_progress",
		"sequence_number": nextSeq(),
		"response":        baseResponse,
	})
	writeSSE("response.output_item.added", map[string]interface{}{
		"type":            "response.output_item.added",
		"sequence_number": nextSeq(),
		"output_index":    0,
		"item": map[string]interface{}{
			"type": "message", "id": itemID, "status": "in_progress", "role": "assistant",
			"content": []interface{}{},
		},
	})
	writeSSE("response.content_part.added", map[string]interface{}{
		"type":            "response.content_part.added",
		"sequence_number": nextSeq(),
		"item_id":         itemID,
		"output_index":    0,
		"content_index":   0,
		"part":            map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
	})
	writeSSE("response.output_text.delta", map[string]interface{}{
		"type":            "response.output_text.delta",
		"sequence_number": nextSeq(),
		"item_id":         itemID,
		"output_index":    0,
		"content_index":   0,
		"delta":           message,
	})
	writeSSE("response.output_text.done", map[string]interface{}{
		"type":            "response.output_text.done",
		"sequence_number": nextSeq(),
		"item_id":         itemID,
		"output_index":    0,
		"content_index":   0,
		"text":            message,
	})
	writeSSE("response.content_part.done", map[string]interface{}{
		"type":            "response.content_part.done",
		"sequence_number": nextSeq(),
		"item_id":         itemID,
		"output_index":    0,
		"content_index":   0,
		"part":            map[string]interface{}{"type": "output_text", "text": message, "annotations": []interface{}{}},
	})
	writeSSE("response.output_item.done", map[string]interface{}{
		"type":            "response.output_item.done",
		"sequence_number": nextSeq(),
		"output_index":    0,
		"item": map[string]interface{}{
			"type": "message", "id": itemID, "status": "completed", "role": "assistant",
			"content": []map[string]interface{}{{"type": "output_text", "text": message, "annotations": []interface{}{}}},
		},
	})
	writeSSE("response.completed", map[string]interface{}{
		"type":            "response.completed",
		"sequence_number": nextSeq(),
		"response": map[string]interface{}{
			"id": respID, "object": "response", "status": "completed", "model": model,
			"parallel_tool_calls": true,
			"output": []map[string]interface{}{{
				"type": "message", "id": itemID, "status": "completed", "role": "assistant",
				"content": []map[string]interface{}{{"type": "output_text", "text": message, "annotations": []interface{}{}}},
			}},
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		},
	})
}

// writeWorkflowErrorStatus writes a block/error response in the shape the
// caller's API format expects.
func writeWorkflowErrorStatus(w http.ResponseWriter, isAnthropic bool, status int, errType, message string) {
	if isAnthropic {
		writeJSON(w, status, map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    errType,
				"message": message,
			},
		})
		return
	}
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}

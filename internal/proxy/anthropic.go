package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/perf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// AnthropicHandler handles POST /v1/messages requests in Anthropic API format.
// It translates between the Anthropic Messages API and the internal OpenAI format,
// enabling Claude Code, Cursor (Anthropic mode), and any Anthropic SDK client.
type AnthropicHandler struct {
	registry *provider.Registry
	router   *router.Router
	logger   *slog.Logger
	pricing  *pricing.Calculator
	spender  *spend.BatchWriter
	tokens   tokenRecorder
}

// NewAnthropicHandler creates a new Anthropic messages handler.
func NewAnthropicHandler(registry *provider.Registry, rt *router.Router, logger *slog.Logger, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, tokenRecorders ...tokenRecorder) *AnthropicHandler {
	var tokens tokenRecorder
	if len(tokenRecorders) > 0 {
		tokens = tokenRecorders[0]
	}
	return &AnthropicHandler{
		registry: registry,
		router:   rt,
		logger:   logger,
		pricing:  pc,
		spender:  spender,
		tokens:   tokens,
	}
}

// --- Anthropic API Types ---

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        interface{}        `json:"system,omitempty"` // string or []content block
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    interface{}        `json:"tool_choice,omitempty"`
	Metadata      interface{}        `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []content block
}

type anthropicContentBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     interface{}      `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   interface{}      `json:"content,omitempty"` // string or []block for tool_result
	Source    *anthropicSource `json:"source,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Name         string      `json:"name"`
	Description  string      `json:"description,omitempty"`
	InputSchema  interface{} `json:"input_schema"`
	CacheControl interface{} `json:"cache_control,omitempty"`
}

type anthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence,omitempty"`
	Usage        anthropicUsage          `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Anthropic reports cache usage beside input_tokens, not inside it. The
	// internal Usage model is inclusive instead, so these are split back out
	// on the way to the client — otherwise a cached 100k prefix is reported as
	// 100k full-price input tokens and the caller computes a ~10x overcharge.
	// Omitted when the provider reported no cache breakdown, which is how a
	// non-caching provider's response stays byte-shaped as before.
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// --- Handler ---

// resolveUpstreamAlias accepts the provider's own model name from a caller who
// is paying with that provider's subscription.
//
// Our catalogue renames a pass-through model so it cannot be confused with a
// metered one, but the client's model picker only knows the provider's name and
// refuses what it does not recognise. Since a scoped upstream token already
// says which provider is answering, the name it uses is unambiguous here — and
// the rename stays an internal concern rather than something the user has to
// know about.
func resolveUpstreamAlias(ctx context.Context, registry *provider.Registry, model string) string {
	upstream := auth.ScopedUpstreamProvider(ctx)
	if upstream == "" || registry == nil {
		return model
	}
	if name, ok := registry.UpstreamAlias(upstream, model); ok {
		return name
	}
	return model
}

func (h *AnthropicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if limit, ok := bodyTooLarge(err); ok {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds the %d byte limit", limit))
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}

	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages is required and must not be empty")
		return
	}
	if req.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens is required and must be > 0")
		return
	}

	req.Model = resolveUpstreamAlias(r.Context(), h.registry, req.Model)

	// Anthropic bills an OAuth subscription against the agent identity on the
	// request, so the caller's own is relayed rather than a pinned one.
	if id := provider.ParseClientIdentity(r.Header.Get("User-Agent"), r.Header.Get("x-app")); id.Version != "" || id.App != "" {
		r = r.WithContext(provider.WithClientIdentity(r.Context(), id))
	}

	// Enforce per-key model access control
	if !auth.IsModelAllowed(r.Context(), req.Model, h.registry.IsRestricted(req.Model)) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error",
			fmt.Sprintf("this API key does not have access to %s", deniedModelPhrase(r.Context(), req.Model)))
		return
	}
	if !auth.IsProviderAllowed(r.Context(), h.registry.ProvidersFor(req.Model)) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error",
			fmt.Sprintf("this API key does not have access to the provider serving %s", deniedModelPhrase(r.Context(), req.Model)))
		return
	}
	if allEU, hasDeployments := h.registry.AllDeploymentsEU(req.Model); !auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error", fmt.Sprintf("this API key requires an EU-only deployment for model %q", req.Model))
		return
	}
	// A key that cannot pay — never funded, or funded and spent — may still
	// call a model the gateway does not pay for. Anything it does pay for needs
	// a budget with something left in it. Checked here, where the model is
	// known and authentication could not see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(req.Model)); status != auth.BudgetOK {
		// 402, not 429: a rate limit and an exhausted budget need different
		// fixes, and a client checking only the status code should be able
		// to tell them apart, same as every other route's budget refusal.
		writeAnthropicError(w, http.StatusPaymentRequired, "rate_limit_error", budgetMessage(status, req.Model))
		return
	}

	// Convert Anthropic request to internal OpenAI format
	openaiReq := h.anthropicToOpenAIRequest(&req)

	injectContextPackInstructions(r.Context(), openaiReq)

	if req.Stream {
		h.handleStream(w, r, &req, openaiReq)
	} else {
		h.handleComplete(w, r, &req, openaiReq)
	}
}

func (h *AnthropicHandler) handleComplete(w http.ResponseWriter, r *http.Request, aReq *anthropicRequest, req *provider.CompletionRequest) {
	start := time.Now()

	// Verify model exists before routing
	if _, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model); err != nil {
		h.logger.Warn("model not found", "model", req.Model, "handler", "complete")
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", unavailableModelPhrase(h.registry, req.Model))
		return
	}

	var resp *provider.CompletionResponse
	var served *provider.Deployment

	if h.router != nil {
		result, err := h.router.RouteEligible(r.Context(), req.Model, deploymentEligible(r.Context()), func(dep *provider.Deployment) error {
			upstreamReq := *req
			upstreamReq.Model = dep.ProviderModel
			upstreamReq.Stream = false
			dropParams(&upstreamReq, dep.DropParams)

			var completeErr error
			resp, completeErr = dep.Provider.Complete(r.Context(), &upstreamReq)
			return completeErr
		})

		if err != nil {
			h.handleUpstreamError(w, err)
			return
		}

		served = result.Deployment
		h.logSpend(r, req.Model, result.Deployment, resp, time.Since(start))
	} else {
		dep, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
		if err != nil {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model %q is not available", req.Model))
			return
		}

		upstreamReq := *req
		upstreamReq.Model = dep.ProviderModel
		dropParams(&upstreamReq, dep.DropParams)

		resp, err = dep.Provider.Complete(r.Context(), &upstreamReq)
		if err != nil {
			h.handleUpstreamError(w, err)
			return
		}

		served = dep
		h.logSpend(r, req.Model, dep, resp, time.Since(start))
	}

	// Convert OpenAI response back to Anthropic format
	anthropicResp := h.openAIToAnthropicResponse(resp, aReq.Model)

	setDeploymentHeaders(w, served)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(anthropicResp)
}

func (h *AnthropicHandler) handleStream(w http.ResponseWriter, r *http.Request, aReq *anthropicRequest, req *provider.CompletionRequest) {
	start := time.Now()
	req.Stream = true

	// Verify model exists before routing
	if _, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model); err != nil {
		h.logger.Warn("model not found", "model", req.Model, "handler", "stream")
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	if _, ok := w.(http.Flusher); !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error",
			"streaming is not supported on this connection")
		return
	}

	var streamReader provider.StreamReader
	var dep *provider.Deployment

	if h.router != nil {
		result, err := h.router.RouteEligible(r.Context(), req.Model, deploymentEligible(r.Context()), func(d *provider.Deployment) error {
			upstreamReq := *req
			upstreamReq.Model = d.ProviderModel
			upstreamReq.Stream = true
			// Ask for usage, before dropParams can take it away again. Without
			// it the estimate below is the normal path rather than a fallback,
			// and it under-counts every streamed turn.
			withStreamUsage(&upstreamReq)
			dropParams(&upstreamReq, d.DropParams)

			reader, err := d.Provider.Stream(r.Context(), &upstreamReq)
			if err != nil {
				return err
			}
			streamReader = reader
			dep = d
			return nil
		})
		if err != nil {
			h.handleUpstreamError(w, err)
			return
		}
		dep = result.Deployment
	} else {
		var err error
		dep, err = getAuthorizedDeployment(r.Context(), h.registry, req.Model)
		if err != nil {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error",
				fmt.Sprintf("model %q is not available", req.Model))
			return
		}

		upstreamReq := *req
		upstreamReq.Model = dep.ProviderModel
		withStreamUsage(&upstreamReq)
		dropParams(&upstreamReq, dep.DropParams)

		streamReader, err = dep.Provider.Stream(r.Context(), &upstreamReq)
		if err != nil {
			h.handleUpstreamError(w, err)
			return
		}
	}

	// The upstream accepted, so 200 is now the truth. Writing it before routing
	// turned every upstream failure — 401, 429, exhausted deployments — into a
	// 200 carrying an SSE error event, so clients that back off on status never
	// did, and hiveStateStatusWriter could not see a failed routed call.
	// The headers still go out ahead of the first token, which is the long wait
	// this was meant to cover.
	setDeploymentHeaders(w, dep)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	h.relayAnthropicStream(w, r, aReq.Model, dep, req, streamReader, start)
}

func (h *AnthropicHandler) relayAnthropicStream(w http.ResponseWriter, r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest, reader provider.StreamReader, start time.Time) {
	defer reader.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	buf := perf.GetBuffer()
	defer perf.PutBuffer(buf)

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	var usage *provider.Usage
	var finishReason string
	var completionChars int

	// message_start carries the input counters, and a client sizing its context
	// window reads them from there. Emitting it before the first upstream chunk
	// reported a hardcoded zero, so it waits for one.
	messageStarted := false
	startMessage := func() {
		if messageStarted {
			return
		}
		messageStarted = true
		h.writeSSE(w, buf, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []interface{}{},
				"model":   model,
				"usage":   anthropicStreamUsage(usage),
			},
		})
	}

	// Track content blocks: text block (always index 0 if present), tool blocks follow
	textBlockStarted := false
	textBlockIndex := 0 // index of the currently open text block
	contentIndex := 0   // next available content block index

	// Track active tool calls by their OpenAI index
	type toolCallState struct {
		contentIndex int
		id           string
		name         string
		started      bool // whether content_block_start has been emitted
		// Arguments stream through untouched once the payload proves itself to
		// be bare JSON. Some providers (GLM, DeepSeek) wrap it in a markdown
		// fence or prepend thinking text, and those need the buffered
		// repair path that repairToolCallJSON exists for — streaming raw
		// fragments removed that safety net for everyone.
		argsJudged bool
		buffered   bool
		argBuf     strings.Builder
	}
	toolCalls := make(map[int]*toolCallState)

	// Relay OpenAI SSE chunks as Anthropic events
	for {
		chunk, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if r.Context().Err() == context.Canceled {
				h.logger.Debug("stream closed by client", "error", err)
			} else {
				h.logger.Error("stream read error", "error", err)
			}
			break
		}

		if chunkUsage := streamUsageFromChunk(chunk); chunkUsage != nil {
			usage = chunkUsage
		}
		startMessage()

		// Parse OpenAI chunk to extract content
		var openaiChunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Role      string `json:"role"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id,omitempty"`
						Type     string `json:"type,omitempty"`
						Function struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk, &openaiChunk); err != nil {
			continue
		}

		if len(openaiChunk.Choices) == 0 {
			continue
		}

		choice := openaiChunk.Choices[0]

		// Handle text content — stream immediately (safe, no pairing requirement)
		if choice.Delta.Content != "" {
			completionChars += len(choice.Delta.Content)
			if !textBlockStarted {
				// The text block's index is recorded, not derived. A tool call
				// whose state is created between two text deltas advances
				// contentIndex without closing the text block, so
				// "contentIndex - 1" pointed at the tool block and sent a
				// text_delta into a tool_use.
				textBlockIndex = contentIndex
				h.writeSSE(w, buf, flusher, "content_block_start", map[string]interface{}{
					"type":          "content_block_start",
					"index":         textBlockIndex,
					"content_block": map[string]string{"type": "text", "text": ""},
				})
				textBlockStarted = true
				contentIndex++
			}
			h.writeSSE(w, buf, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]string{"type": "text_delta", "text": choice.Delta.Content},
			})
		}

		// Handle tool calls — stream incrementally
		for _, tc := range choice.Delta.ToolCalls {
			state, exists := toolCalls[tc.Index]

			if !exists {
				state = &toolCallState{
					contentIndex: contentIndex,
					id:           tc.ID,
					name:         tc.Function.Name,
				}
				toolCalls[tc.Index] = state
				contentIndex++
			} else {
				if tc.ID != "" {
					state.id = tc.ID
				}
				if tc.Function.Name != "" {
					state.name = tc.Function.Name
				}
			}

			// Emit content_block_start as soon as we have id + name
			if !state.started && state.id != "" && state.name != "" {
				// Close text block before first tool block
				if textBlockStarted {
					h.writeSSE(w, buf, flusher, "content_block_stop", map[string]interface{}{
						"type":  "content_block_stop",
						"index": textBlockIndex,
					})
					textBlockStarted = false
				}
				h.writeSSE(w, buf, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": state.contentIndex,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    state.id,
						"name":  state.name,
						"input": map[string]interface{}{},
					},
				})
				state.started = true
			}

			// Tool arguments are output the model produced, so they count
			// toward the completion estimate; leaving them out billed a
			// tool-heavy turn as though it had written nothing.
			completionChars += len(tc.Function.Arguments) + len(tc.Function.Name)

			// Stream argument chunks immediately, once the opening bytes show
			// the provider is sending bare JSON.
			if tc.Function.Arguments != "" && state.started {
				switch {
				case state.buffered:
					state.argBuf.WriteString(tc.Function.Arguments)
				case !state.argsJudged:
					state.argBuf.WriteString(tc.Function.Arguments)
					trimmed := strings.TrimSpace(state.argBuf.String())
					if trimmed == "" {
						break // nothing to judge yet
					}
					state.argsJudged = true
					if trimmed[0] == '{' || trimmed[0] == '[' {
						h.writeSSE(w, buf, flusher, "content_block_delta", map[string]interface{}{
							"type":  "content_block_delta",
							"index": state.contentIndex,
							"delta": map[string]string{"type": "input_json_delta", "partial_json": state.argBuf.String()},
						})
						state.argBuf.Reset()
					} else {
						state.buffered = true
					}
				default:
					h.writeSSE(w, buf, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": state.contentIndex,
						"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
					})
				}
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	// Close the text block if it is still open. Not necessarily index 0: after
	// a tool call the text reopens at a later index, and stopping 0 both
	// repeated a stop the client already had and left the real block dangling.
	startMessage()
	if textBlockStarted {
		h.writeSSE(w, buf, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		})
	}

	// Flush any tool call that had to be buffered, repaired in one delta.
	for _, state := range toolCalls {
		if !state.started || state.argBuf.Len() == 0 {
			continue
		}
		h.writeSSE(w, buf, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": state.contentIndex,
			"delta": map[string]string{
				"type":         "input_json_delta",
				"partial_json": repairToolCallJSON(state.argBuf.String()),
			},
		})
		state.argBuf.Reset()
	}

	// Close all open tool_use blocks
	for _, state := range toolCalls {
		if state.started {
			h.writeSSE(w, buf, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": state.contentIndex,
			})
		}
	}

	// If no content blocks were emitted at all, send an empty text block
	if !textBlockStarted && len(toolCalls) == 0 {
		h.writeSSE(w, buf, flusher, "content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		h.writeSSE(w, buf, flusher, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
	}

	// Send message_delta with stop reason
	stopReason := mapOpenAIStopReason(finishReason)
	h.writeSSE(w, buf, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason},
		"usage": anthropicStreamUsage(usage),
	})

	// Send message_stop
	h.writeSSE(w, buf, flusher, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})

	// Log spend
	h.logStreamSpend(r, model, dep, req, usage, completionChars, time.Since(start))
}

func (h *AnthropicHandler) writeSSE(w http.ResponseWriter, buf *bytes.Buffer, flusher http.Flusher, event string, data interface{}) {
	buf.Reset()
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\ndata: ")
	encoded, _ := json.Marshal(data)
	buf.Write(encoded)
	buf.WriteString("\n\n")
	_, _ = w.Write(buf.Bytes())
	flusher.Flush()
}

func (h *AnthropicHandler) writeSSEError(w http.ResponseWriter, err error) {
	h.logger.Error("upstream error", "error", err)
	setFailureHeaders(w, err)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	errMsg := "internal server error"
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		errMsg = ue.Message
	} else if err != nil {
		errMsg = err.Error()
	}
	data, _ := json.Marshal(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    "api_error",
			"message": errMsg,
		},
	})
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

// repairToolCallJSON attempts to extract valid JSON from malformed tool call arguments.
// Some providers (e.g., GLM, DeepSeek) wrap JSON in markdown or include thinking text.
func repairToolCallJSON(raw string) string {
	// Try extracting JSON from markdown code block
	if idx := strings.Index(raw, "```json"); idx >= 0 {
		start := idx + 7
		if end := strings.Index(raw[start:], "```"); end >= 0 {
			candidate := strings.TrimSpace(raw[start : start+end])
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}
	if idx := strings.Index(raw, "```"); idx >= 0 {
		start := idx + 3
		// Skip optional language tag on same line
		if nl := strings.IndexByte(raw[start:], '\n'); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(raw[start:], "```"); end >= 0 {
			candidate := strings.TrimSpace(raw[start : start+end])
			if json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}

	// Try finding first { ... } or [ ... ] in the string
	braceStart := strings.IndexByte(raw, '{')
	if braceStart >= 0 {
		// Find matching closing brace
		depth := 0
		for i := braceStart; i < len(raw); i++ {
			switch raw[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					candidate := raw[braceStart : i+1]
					if json.Valid([]byte(candidate)) {
						return candidate
					}
				}
			}
		}
	}

	return ""
}

// --- Conversion: Anthropic → OpenAI ---

func (h *AnthropicHandler) anthropicToOpenAIRequest(req *anthropicRequest) *provider.CompletionRequest {
	openaiReq := &provider.CompletionRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	maxTokens := req.MaxTokens
	openaiReq.MaxTokens = &maxTokens

	if len(req.StopSequences) > 0 {
		openaiReq.Stop = req.StopSequences
	}

	// System message
	if req.System != nil {
		sysText := extractSystemText(req.System)
		if sysText != "" {
			openaiReq.Messages = append(openaiReq.Messages, provider.Message{
				Role:         "system",
				Content:      sysText,
				CacheControl: lastCacheControl(req.System),
			})
		}
	}

	// Convert messages
	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			openaiReq.Messages = append(openaiReq.Messages, convertAnthropicUser(msg.Content)...)
		case "assistant":
			openaiMsg := convertAnthropicAssistant(msg.Content)
			openaiReq.Messages = append(openaiReq.Messages, openaiMsg)
		}
	}

	// Convert tools
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]interface{}{"type": "object"}
			}
			openaiReq.Tools = append(openaiReq.Tools, provider.Tool{
				Type: "function",
				Function: provider.Function{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  schema,
				},
				CacheControl: t.CacheControl,
			})
		}
	}

	return openaiReq
}

// lastCacheControl returns the prompt-cache breakpoint furthest into a list of
// content blocks. The internal message model has one slot per message, and the
// last block is where the boundary actually falls.
func lastCacheControl(content interface{}) interface{} {
	blocks, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var cc interface{}
	for _, block := range blocks {
		if found := provider.PartCacheControl(block); found != nil {
			cc = found
		}
	}
	return cc
}

// --- Conversion: OpenAI → Anthropic ---

func (h *AnthropicHandler) openAIToAnthropicResponse(resp *provider.CompletionResponse, model string) *anthropicResponse {
	aResp := &anthropicResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.Message != nil {
			// Text content
			if content, ok := choice.Message.Content.(string); ok && content != "" {
				aResp.Content = append(aResp.Content, anthropicContentBlock{
					Type: "text",
					Text: content,
				})
			}

			// Tool calls
			for _, tc := range choice.Message.ToolCalls {
				argsJSON := tc.Function.Arguments
				if argsJSON == "" {
					argsJSON = "{}"
				}
				var input interface{}
				if err := json.Unmarshal([]byte(argsJSON), &input); err != nil {
					// Attempt repair
					if repaired := repairToolCallJSON(argsJSON); repaired != "" {
						_ = json.Unmarshal([]byte(repaired), &input)
					}
					if input == nil {
						input = map[string]interface{}{"raw_input": argsJSON}
					}
				}
				aResp.Content = append(aResp.Content, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
		}
		if choice.FinishReason != nil {
			aResp.StopReason = mapOpenAIStopReason(*choice.FinishReason)
		}
	}

	if resp.Usage != nil {
		cacheRead := resp.Usage.CachedTokens()
		cacheWrite := resp.Usage.CacheCreation()
		aResp.Usage = anthropicUsage{
			// PromptTokens is inclusive of both cache counters; Anthropic's
			// wire shape keeps them separate, so subtract them back out.
			InputTokens:              resp.Usage.PromptTokens - cacheRead - cacheWrite,
			OutputTokens:             resp.Usage.CompletionTokens,
			CacheReadInputTokens:     cacheRead,
			CacheCreationInputTokens: cacheWrite,
		}
		if aResp.Usage.InputTokens < 0 {
			// A provider that reports a breakdown larger than its own total is
			// not worth trusting into a negative; report what it charged.
			aResp.Usage.InputTokens = resp.Usage.PromptTokens
		}
	}

	if len(aResp.Content) == 0 {
		aResp.Content = []anthropicContentBlock{{Type: "text", Text: ""}}
	}

	return aResp
}

// --- Helpers ---

func extractSystemText(system interface{}) string {
	switch v := system.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if text, _ := m["text"].(string); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertAnthropicUser converts an Anthropic user message to one or more OpenAI messages.
// tool_result blocks become separate role:"tool" messages; other content stays as role:"user".
func convertAnthropicUser(content interface{}) []provider.Message {
	// Simple string content
	if s, ok := content.(string); ok {
		return []provider.Message{{Role: "user", Content: s}}
	}

	blocks, ok := content.([]interface{})
	if !ok {
		return []provider.Message{{Role: "user", Content: content}}
	}

	var msgs []provider.Message
	var userParts []interface{}
	var userCC interface{}

	for _, item := range blocks {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := m["type"].(string)
		switch blockType {
		case "tool_result":
			// Flush any pending user parts before tool messages
			if len(userParts) > 0 {
				msgs = append(msgs, provider.Message{Role: "user", Content: simplifyParts(userParts), CacheControl: userCC})
				userParts = nil
				userCC = nil
			}
			toolCallID, _ := m["tool_use_id"].(string)
			msgs = append(msgs, provider.Message{
				Role:         "tool",
				ToolCallID:   toolCallID,
				Content:      extractToolResultContent(m["content"]),
				CacheControl: provider.PartCacheControl(m),
			})
		case "text":
			text, _ := m["text"].(string)
			userParts = append(userParts, map[string]interface{}{
				"type": "text",
				"text": text,
			})
			if cc := provider.PartCacheControl(m); cc != nil {
				userCC = cc
			}
		case "image":
			if source, ok := m["source"].(map[string]interface{}); ok {
				mediaType, _ := source["media_type"].(string)
				data, _ := source["data"].(string)
				userParts = append(userParts, map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]string{
						"url": "data:" + mediaType + ";base64," + data,
					},
				})
				if cc := provider.PartCacheControl(m); cc != nil {
					userCC = cc
				}
			}
		}
	}

	// Flush remaining user parts
	if len(userParts) > 0 {
		msgs = append(msgs, provider.Message{Role: "user", Content: simplifyParts(userParts), CacheControl: userCC})
	}

	// If nothing was produced (shouldn't happen), return empty user message
	if len(msgs) == 0 {
		return []provider.Message{{Role: "user", Content: ""}}
	}

	return msgs
}

// extractToolResultContent extracts text from a tool_result's content field,
// which can be a string or an array of content blocks.
func extractToolResultContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var texts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if m["type"] == "text" {
					if text, ok := m["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// simplifyParts returns a string if there's a single text part, otherwise the array.
func simplifyParts(parts []interface{}) interface{} {
	if len(parts) == 1 {
		if p, ok := parts[0].(map[string]interface{}); ok {
			if p["type"] == "text" {
				return p["text"]
			}
		}
	}
	return parts
}

func convertAnthropicAssistant(content interface{}) provider.Message {
	msg := provider.Message{Role: "assistant"}

	switch v := content.(type) {
	case string:
		msg.Content = v
	case []interface{}:
		msg.CacheControl = lastCacheControl(v)
		var textParts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				blockType, _ := m["type"].(string)
				switch blockType {
				case "text":
					text, _ := m["text"].(string)
					textParts = append(textParts, text)
				case "tool_use":
					id, _ := m["id"].(string)
					name, _ := m["name"].(string)
					input := m["input"]
					args, _ := json.Marshal(input)
					msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
						ID:   id,
						Type: "function",
						Function: provider.FunctionCall{
							Name:      name,
							Arguments: string(args),
						},
					})
				}
			}
		}
		if len(textParts) > 0 {
			msg.Content = strings.Join(textParts, "")
		}
	}

	return msg
}

// anthropicStreamUsage renders a turn's usage in Anthropic's wire shape, where
// the cache counters sit beside input_tokens rather than inside it. A provider
// that reported no breakdown yields only the two mandatory keys.
func anthropicStreamUsage(u *provider.Usage) map[string]int {
	out := map[string]int{"input_tokens": 0, "output_tokens": 0}
	if u == nil {
		return out
	}
	read, created := u.CachedTokens(), u.CacheCreation()
	input := u.PromptTokens - read - created
	if input < 0 {
		// A provider whose breakdown exceeds its own total is not worth
		// trusting into a negative; report what it charged.
		input = u.PromptTokens
	}
	out["input_tokens"] = input
	out["output_tokens"] = u.CompletionTokens
	if u.CacheReported() {
		out["cache_read_input_tokens"] = read
		out["cache_creation_input_tokens"] = created
	}
	return out
}

func mapOpenAIStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func (h *AnthropicHandler) handleUpstreamError(w http.ResponseWriter, err error) {
	h.logger.Error("upstream error", "error", err)
	setFailureHeaders(w, err)

	// The router wraps the last attempt's error, so the upstream status only
	// survives an unwrapping match. A type assertion turned every 429 into a
	// 502, which agent clients read as transient and retry into the ground.
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		status := ue.StatusCode
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		errType := "api_error"
		if status == 429 {
			errType = "rate_limit_error"
		} else if status >= 500 {
			errType = "api_error"
		} else if status >= 400 {
			errType = "invalid_request_error"
		}
		writeAnthropicError(w, status, errType, ue.Message)
		return
	}

	writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
}

func (h *AnthropicHandler) logSpend(r *http.Request, model string, dep *provider.Deployment, resp *provider.CompletionResponse, duration time.Duration) {
	if h.spender == nil || resp == nil {
		return
	}

	record := store.SpendRecord{
		Model:       model,
		Provider:    dep.ProviderName,
		Duration:    duration.Milliseconds(),
		Status:      200,
		IsEU:        dep.IsEU,
		AuthMode:    dep.AuthMode,
		BillingMode: dep.BillingMode,
	}

	if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo != nil {
		record.ApplyIdentity(keyInfo)
	}

	if resp.Usage != nil {
		applyUsageTokens(&record, dep, resp.Usage)
		if h.pricing != nil {
			record.Cost = computeCostUsage(dep, h.pricing, resp.Usage)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
			h.tokens.RecordCacheTokens(resp.Usage.CachedTokens(), resp.Usage.CacheCreation())
		}
	}

	h.recordSpend(r, record)
}

func (h *AnthropicHandler) logStreamSpend(r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest, usage *provider.Usage, completionChars int, duration time.Duration) {
	if h.spender == nil {
		return
	}

	record := store.SpendRecord{
		Model:       model,
		Provider:    dep.ProviderName,
		Duration:    duration.Milliseconds(),
		Status:      200,
		IsEU:        dep.IsEU,
		AuthMode:    dep.AuthMode,
		BillingMode: dep.BillingMode,
	}

	if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo != nil {
		record.ApplyIdentity(keyInfo)
	}

	if usage != nil && usage.TotalTokens > 0 {
		applyUsageTokens(&record, dep, usage)
		if h.pricing != nil {
			record.Cost = computeCostUsage(dep, h.pricing, usage)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(usage.PromptTokens, usage.CompletionTokens)
			h.tokens.RecordCacheTokens(usage.CachedTokens(), usage.CacheCreation())
		}
	} else {
		// Fallback: estimate tokens when upstream drops before sending usage chunk
		promptTok := estimatePromptTokens(req)
		completionTok := completionChars / 4
		if completionTok == 0 && completionChars > 0 {
			completionTok = 1
		}
		record.PromptTokens = promptTok
		record.CompletionTokens = completionTok
		record.TotalTokens = promptTok + completionTok
		if h.pricing != nil {
			record.Cost = computeCost(dep, h.pricing, promptTok, completionTok)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(promptTok, completionTok)
		}
	}

	h.recordSpend(r, record)
}

func (h *AnthropicHandler) recordSpend(r *http.Request, record store.SpendRecord) {
	// Fill UsageCapture for upstream middleware (HiveState)
	if uc := store.GetUsageCapture(r.Context()); uc != nil && record.PromptTokens > 0 {
		uc.PromptTokens = record.PromptTokens
		uc.CompletionTokens = record.CompletionTokens
		uc.TotalTokens = record.TotalTokens
		uc.CachedPromptTokens = record.CachedPromptTokens
		uc.CacheCreationTokens = record.CacheCreationTokens
		uc.Cost = record.Cost
		uc.Filled = true
	}

	keyInfo := auth.KeyInfoFromContext(r.Context())
	if keyInfo != nil && (keyInfo.Budget > 0 || keyInfo.TeamID != "") {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.spender.RecordSync(ctx, record); err != nil {
			h.logger.Warn("spend log failed", "error", err, "key_prefix", record.KeyPrefix, "model", record.Model)
		}
		return
	}
	h.spender.Record(record)
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

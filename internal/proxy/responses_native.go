package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/sse"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

type nativeResponseFileRef struct {
	part    map[string]interface{}
	mapping *store.ProviderFile
}

type nativeResponseFileCollector struct {
	ctx          context.Context
	fileStore    store.FileStore
	ownerID      string
	model        string
	deploymentID string
	refs         []nativeResponseFileRef
}

func (c *nativeResponseFileCollector) collect(value interface{}) error {
	switch node := value.(type) {
	case []interface{}:
		return c.collectSlice(node)
	case map[string]interface{}:
		return c.collectMap(node)
	default:
		return nil
	}
}

func (c *nativeResponseFileCollector) collectSlice(nodes []interface{}) error {
	for _, child := range nodes {
		if err := c.collect(child); err != nil {
			return err
		}
	}
	return nil
}

func (c *nativeResponseFileCollector) collectMap(node map[string]interface{}) error {
	if err := c.collectFileRef(node); err != nil {
		return err
	}
	for _, child := range node {
		if err := c.collect(child); err != nil {
			return err
		}
	}
	return nil
}

func (c *nativeResponseFileCollector) collectFileRef(part map[string]interface{}) error {
	partType, _ := part["type"].(string)
	if partType != "input_file" && partType != "input_image" {
		return nil
	}
	fileID, _ := part["file_id"].(string)
	if fileID == "" {
		return nil
	}
	mapping, err := resolveProviderFile(c.ctx, c.fileStore, c.ownerID, fileID, c.model)
	if err != nil {
		return err
	}
	if c.deploymentID != "" && c.deploymentID != mapping.DeploymentID {
		return fmt.Errorf("all uploaded files in one request must belong to the same provider deployment")
	}
	c.deploymentID = mapping.DeploymentID
	c.refs = append(c.refs, nativeResponseFileRef{part: part, mapping: mapping})
	return nil
}

// resolveNativeResponseFiles replaces Ubiquum ids in Responses input parts
// with the provider ids that own them. It enables native Responses forwarding
// only when the selected provider implements provider.ResponsesAPI.
func resolveNativeResponseFiles(
	r *http.Request,
	db store.Store,
	registry *provider.Registry,
	model string,
	rawInput json.RawMessage,
) (json.RawMessage, *provider.Deployment, bool, error) {
	fileStore, ok := db.(store.FileStore)
	if !ok {
		return rawInput, nil, false, nil
	}
	// "input" is optional: a turn can carry only previous_response_id, a prompt
	// template, or instructions. Unmarshalling an absent field would fail on
	// empty JSON and reject the request before it ever reached a provider.
	if len(rawInput) == 0 {
		return rawInput, nil, false, nil
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return nil, nil, false, fmt.Errorf("invalid Responses input: %w", err)
	}

	collector := nativeResponseFileCollector{
		ctx:       r.Context(),
		fileStore: fileStore,
		ownerID:   fileOwnerID(r.Context()),
		model:     model,
	}
	if err := collector.collect(input); err != nil {
		return nil, nil, false, err
	}
	if len(collector.refs) == 0 {
		return rawInput, nil, false, nil
	}

	dep, err := getAuthorizedDeploymentByID(r.Context(), registry, model, collector.deploymentID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("provider deployment for uploaded file is unavailable")
	}
	// The deployment is returned even when it cannot speak Responses natively,
	// so the caller can tell "no uploaded files" from "uploaded files on a
	// provider that needs the translated path".
	if nativeResponsesProvider(dep) == nil {
		return rawInput, dep, false, nil
	}

	for _, ref := range collector.refs {
		ref.part["file_id"] = ref.mapping.UpstreamID
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, nil, false, fmt.Errorf("encode native Responses input: %w", err)
	}
	return encoded, dep, true, nil
}

// nativeResponsesProvider returns the deployment's native Responses client, or
// nil when the provider cannot speak the protocol directly.
func nativeResponsesProvider(dep *provider.Deployment) provider.ResponsesAPI {
	if dep == nil {
		return nil
	}
	api, _ := dep.Provider.(provider.ResponsesAPI)
	if api == nil {
		return nil
	}
	// Implementing the interface is not enough: one client type backs both the
	// real OpenAI endpoint and OpenAI-compatible backends that have no
	// /responses at all.
	if capability, ok := dep.Provider.(provider.NativeResponsesCapability); ok && !capability.SupportsNativeResponses() {
		return nil
	}
	return api
}

// modelSpeaksResponses reports whether every deployment behind a model can be
// handed a Responses request verbatim. A mixed list falls back to the
// translated path, which works on every provider, rather than making the
// protocol depend on which deployment the router happens to pick.
func (h *ResponsesHandler) modelSpeaksResponses(model string) bool {
	deployments, err := h.registry.GetDeployments(model)
	if err != nil || len(deployments) == 0 {
		return false
	}
	for _, dep := range deployments {
		if nativeResponsesProvider(dep) == nil {
			return false
		}
	}
	return true
}

// nativeResponsesBody rewrites only what the gateway owns — the upstream model
// name, the input when file ids had to be remapped, and the governed context
// pack bundle merged into instructions — leaving every other field of the
// client's request byte-identical.
func nativeResponsesBody(rawBody []byte, providerModel string, input json.RawMessage, contextInstructions string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &fields); err != nil {
		return nil, fmt.Errorf("invalid Responses request: %w", err)
	}
	model, err := json.Marshal(providerModel)
	if err != nil {
		return nil, err
	}
	fields["model"] = model
	if len(input) > 0 {
		fields["input"] = input
	}
	if contextInstructions != "" {
		merged, err := mergeInstructionsField(fields["instructions"], contextInstructions)
		if err != nil {
			return nil, err
		}
		fields["instructions"] = merged
	}
	return json.Marshal(fields)
}

func (h *ResponsesHandler) handleNativeResponse(
	w http.ResponseWriter,
	r *http.Request,
	req *responsesRequest,
	rawBody []byte,
	nativeInput json.RawMessage,
	pinned *provider.Deployment,
) {
	start := time.Now()

	var contextInstructions string
	if key := auth.KeyInfoFromContext(r.Context()); key != nil {
		contextInstructions = key.ContextInstructions
	}

	var (
		dep  *provider.Deployment
		resp *http.Response
	)
	// A non-2xx upstream reply is turned into an error so the router can retry
	// on another deployment and the circuit breaker sees the failure; without
	// this, native passthrough would silently lose failover.
	attempt := func(candidate *provider.Deployment) error {
		body, err := nativeResponsesBody(rawBody, candidate.ProviderModel, nativeInput, contextInstructions)
		if err != nil {
			return err
		}
		upstream, err := nativeResponsesProvider(candidate).DoResponsesRequest(
			r.Context(), bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return err
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			return sse.ParseOpenAIError(upstream)
		}
		dep, resp = candidate, upstream
		return nil
	}

	switch {
	case pinned != nil:
		if err := attempt(pinned); err != nil {
			handleUpstreamErr(w, h.logger, err)
			return
		}
	case h.router != nil:
		if _, err := h.router.RouteEligible(r.Context(), req.Model, deploymentEligible(r.Context()), attempt); err != nil {
			handleUpstreamErr(w, h.logger, err)
			return
		}
	default:
		candidate, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
		if err != nil {
			writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
			return
		}
		if err := attempt(candidate); err != nil {
			handleUpstreamErr(w, h.logger, err)
			return
		}
	}
	defer resp.Body.Close()

	setDeploymentHeaders(w, dep)
	if contentType := resp.Header.Get(headerContentType); contentType != "" {
		w.Header().Set(headerContentType, contentType)
	}
	if requestID := resp.Header.Get("x-request-id"); requestID != "" {
		w.Header().Set("X-Upstream-Request-ID", requestID)
	}

	if isNativeResponseStream(req, resp) {
		w.WriteHeader(resp.StatusCode)
		usage, final, relayErr := relayNativeResponsesStream(w, resp.Body, req.Model)
		// A stream that ends without its terminal event was cut: the client is
		// left holding a partial answer. The status line went out as 200 long
		// before this point and cannot be taken back, so the log is the only
		// place a truncation is distinguishable from a clean completion.
		if final == nil {
			h.logger.Warn("native Responses stream ended before its terminal event",
				"model", req.Model, "elapsed", time.Since(start).String(),
				"cause", relayCause(relayErr), "error", relayErr,
				"client_gone", r.Context().Err() != nil)
		}
		h.logSpend(r, req.Model, dep, &provider.CompletionResponse{Usage: usage}, time.Since(start))
		h.persistNativeResponse(context.WithoutCancel(r.Context()), req, final, dep)
		return
	}

	// Bounded: this is a provider's response, and an unbounded ReadAll turns a
	// misbehaving or hostile upstream into an out-of-memory kill of the whole
	// gateway.
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxNativeResponseBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "failed to read native Responses payload")
		return
	}
	if int64(len(responseBody)) >= maxNativeResponseBytes {
		writeError(w, http.StatusBadGateway, "upstream_error", "native Responses payload exceeds the size limit")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "provider returned an invalid Responses payload")
		return
	}
	payload["model"] = req.Model

	h.recordNativeResponseUsage(w, r, req.Model, dep, payload, start)
	if encoded, err := json.Marshal(payload); err == nil {
		h.persistNativeResponse(r.Context(), req, encoded, dep)
	}

	w.WriteHeader(resp.StatusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

// maxNativeResponseBytes caps a single native Responses payload. Generous
// enough for any real turn, small enough that one upstream cannot exhaust the
// process.
const maxNativeResponseBytes = 64 << 20

// persistNativeResponse records a natively-served turn under the id the
// provider assigned, which is the id the client holds and will use to retrieve
// or cancel it. The upstream id is kept so those calls can be proxied back.
func (h *ResponsesHandler) persistNativeResponse(
	ctx context.Context,
	req *responsesRequest,
	payload []byte,
	dep *provider.Deployment,
) {
	var object struct {
		ID     string            `json:"id"`
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(payload, &object) != nil || object.ID == "" {
		return
	}
	// Only a finished turn joins its conversation. A native background turn
	// answers "queued" with an empty output, and the poll that eventually
	// carries the answer updates the response row alone — so appending here
	// filed the input under a turn whose reply never arrived, and the next turn
	// replayed a thread with a question and no answer.
	if isTerminalResponsesStatus(object.Status) {
		h.appendTurnToConversation(ctx, req, object.Output)
	}

	rs := h.responseStore()
	ownerID := fileOwnerID(ctx)
	if rs == nil || ownerID == "" || len(payload) == 0 || !shouldStoreResponse(req) {
		return
	}

	items, err := json.Marshal(resolvedResponsesInput(req))
	if err != nil {
		return
	}
	record := &store.StoredResponse{
		ID:                 object.ID,
		OwnerID:            ownerID,
		Model:              req.Model,
		UpstreamID:         object.ID,
		Status:             object.Status,
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
	if err := rs.CreateResponse(ctx, record); err != nil {
		h.logger.Warn("native response persist failed", "error", err, "response_id", object.ID)
	}
}

func isNativeResponseStream(req *responsesRequest, resp *http.Response) bool {
	return req.Stream || strings.Contains(resp.Header.Get(headerContentType), "text/event-stream")
}

// terminalResponsesEvents carry the final response object, and with it the
// usage the gateway needs for spend and metrics.
var terminalResponsesEvents = map[string]struct{}{
	"response.completed":  {},
	"response.incomplete": {},
	"response.failed":     {},
}

// relayCause reduces a relay failure to the one thing worth knowing when a
// turn comes back truncated: which side gave up. "Upstream" and "client" call
// for opposite investigations, and the raw error alone reads the same either
// way once it has been wrapped a few times.
func relayCause(err error) string {
	switch {
	case err == nil:
		return "upstream_eof_without_terminal_event"
	case errors.Is(err, context.Canceled):
		return "client_disconnected"
	case strings.Contains(err.Error(), "client stopped reading"):
		return "client_disconnected"
	default:
		return "upstream_closed"
	}
}

// relayNativeResponsesStream forwards a provider SSE stream to the client and
// returns the usage and final response object carried by its terminal event.
// Lines are relayed byte-for-byte — so every event type the provider emits
// reaches the client, including ones this gateway does not model — except the
// terminal event, whose model field is rewritten to the Ubiquum model name.
//
// The last return value says why the relay stopped. A truncated stream and a
// completed one are otherwise indistinguishable after the fact: the 200 went
// out with the first byte, so the only remaining evidence of a cut is which
// side closed, and that is worth carrying out of here rather than discarding.
func relayNativeResponsesStream(w http.ResponseWriter, body io.Reader, model string) (*provider.Usage, []byte, error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(body)

	var usage *provider.Usage
	var final []byte
	for {
		line, readErr := sse.ReadLine(reader)
		if len(line) > 0 {
			if terminal := rewriteTerminalResponsesEvent(line, model); terminal != nil {
				if terminal.line != nil {
					line = terminal.line
				}
				if terminal.usage != nil {
					usage = terminal.usage
				}
				if terminal.response != nil {
					final = terminal.response
				}
			}
			if _, err := w.Write(line); err != nil {
				return usage, final, fmt.Errorf("client stopped reading: %w", err)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return usage, final, nil
			}
			return usage, final, fmt.Errorf("upstream stream ended: %w", readErr)
		}
	}
}

// terminalResponsesEvent is what a rewritten terminal SSE line yields: the line
// to relay in place of the original, the usage to bill, and the final response
// object to persist.
type terminalResponsesEvent struct {
	line     []byte
	usage    *provider.Usage
	response []byte
}

// rewriteTerminalResponsesEvent re-encodes a terminal event with response.model
// set to the Ubiquum model name. It returns nil for every other line, meaning
// "relay unchanged".
func rewriteTerminalResponsesEvent(line []byte, model string) *terminalResponsesEvent {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data: "))
	if !ok {
		return nil
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil
	}

	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return nil
	}
	var eventType string
	if json.Unmarshal(event["type"], &eventType) != nil {
		return nil
	}
	if _, terminal := terminalResponsesEvents[eventType]; !terminal {
		return nil
	}

	var response map[string]json.RawMessage
	if json.Unmarshal(event["response"], &response) != nil {
		return nil
	}
	out := &terminalResponsesEvent{usage: nativeResponsesUsage(response["usage"])}

	encodedModel, err := json.Marshal(model)
	if err != nil {
		return out
	}
	response["model"] = encodedModel
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		return out
	}
	out.response = encodedResponse

	event["response"] = encodedResponse
	encodedEvent, err := json.Marshal(event)
	if err != nil {
		return out
	}

	rewritten := make([]byte, 0, len(encodedEvent)+8)
	rewritten = append(rewritten, "data: "...)
	rewritten = append(rewritten, encodedEvent...)
	rewritten = append(rewritten, '\n')
	out.line = rewritten
	return out
}

func nativeResponsesUsage(raw json.RawMessage) *provider.Usage {
	if len(raw) == 0 {
		return nil
	}
	// input_tokens_details.cached_tokens is the only report of how much of the
	// prompt the provider served from its cache. Dropping it made every turn
	// look like a full-price cache miss to both the cost calculation and the
	// metrics, which is also what made a cache problem impossible to see here.
	var usage struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		TotalTokens        int `json:"total_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 {
		return nil
	}
	out := &provider.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if cached := usage.InputTokensDetails.CachedTokens; cached > 0 {
		// This API reports no cache-creation counter, so the write side stays 0.
		out.SetCacheUsage(cached, 0)
	}
	// On a reasoning model this is most of what the turn actually produced,
	// and it is billed at the output rate. Without it there is no way to tell
	// a long turn spent thinking from a short one.
	if reasoning := usage.OutputTokensDetails.ReasoningTokens; reasoning > 0 {
		out.CompletionTokensDetails = &provider.CompletionTokensDetails{ReasoningTokens: reasoning}
	}
	return out
}

func (h *ResponsesHandler) recordNativeResponseUsage(
	w http.ResponseWriter,
	r *http.Request,
	model string,
	dep *provider.Deployment,
	payload map[string]interface{},
	start time.Time,
) {
	usageMap, ok := payload["usage"].(map[string]interface{})
	if !ok {
		return
	}
	promptTokens := jsonNumberToInt(usageMap["input_tokens"])
	completionTokens := jsonNumberToInt(usageMap["output_tokens"])
	totalTokens := jsonNumberToInt(usageMap["total_tokens"])
	usage := &provider.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
	}
	if details, ok := usageMap["input_tokens_details"].(map[string]interface{}); ok {
		if cached := jsonNumberToInt(details["cached_tokens"]); cached > 0 {
			usage.SetCacheUsage(cached, 0)
		}
	}
	if details, ok := usageMap["output_tokens_details"].(map[string]interface{}); ok {
		if reasoning := jsonNumberToInt(details["reasoning_tokens"]); reasoning > 0 {
			usage.CompletionTokensDetails = &provider.CompletionTokensDetails{ReasoningTokens: reasoning}
		}
	}
	completion := &provider.CompletionResponse{Usage: usage}
	h.logSpend(r, model, dep, completion, time.Since(start))
	if h.pricing == nil || totalTokens <= 0 {
		return
	}
	cost := computeCost(dep, h.pricing, promptTokens, completionTokens)
	if cost > 0 {
		w.Header().Set("X-Ubiquum-Cost", fmt.Sprintf("%.10f", cost))
	}
}

func jsonNumberToInt(value interface{}) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case json.Number:
		result, _ := number.Int64()
		return int(result)
	default:
		return 0
	}
}

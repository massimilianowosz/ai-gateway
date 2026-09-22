// Package workflow implements the x-ubiquum-workflow integration: an optional
// callout to the backend's /workflows/execute endpoint that lets a tenant
// transform, block, or directly answer a chat/message request before it
// reaches the LLM.
package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const (
	headerWorkflowRequest = "x-ubiquum-workflow"
	headerApplied         = "x-ubiquum-workflow-applied"
	headerWorkflowID      = "x-ubiquum-workflow-id"
	headerDurationMs      = "x-ubiquum-workflow-duration-ms"

	pathAnthropicMessages = "/v1/messages"
	pathChatCompletions   = "/v1/chat/completions"
	pathResponses         = "/v1/responses"

	defaultWorkflowLabel = "default"

	decisionBlock   = "block"
	decisionRespond = "respond"
	decisionAllow   = "allow"
	decisionError   = "workflow_error"
)

// executeRequest is the payload sent to the backend's /workflows/execute endpoint.
type executeRequest struct {
	TenantID   string      `json:"tenant_id"`
	WorkflowID string      `json:"workflow_id,omitempty"`
	Prompt     string      `json:"prompt"`
	Messages   []wfMessage `json:"messages,omitempty"`
	UserAPIKey string      `json:"user_api_key,omitempty"`
}

// executeResponse is the backend's response for a workflow execution.
type executeResponse struct {
	Decision        string  `json:"decision"` // "allow", "block", "respond", "error"
	BlockMessage    string  `json:"block_message"`
	BlockReason     string  `json:"block_reason"`
	RespondMessage  string  `json:"respond_message"`
	FinalPrompt     string  `json:"final_prompt"`
	RouteModel      string  `json:"route_model"`
	TotalDurationMs float64 `json:"total_duration_ms"`
}

type allowRequest struct {
	bodyBytes    []byte
	lastUserText string
	// promptIdx is where extractPrompt read lastUserText from, so the rewrite
	// lands on that same turn rather than on whatever the last user-role
	// message happens to be.
	promptIdx   int
	isAnthropic bool
	isResponses bool
}

// Workflow calls out to the backend's /workflows/execute endpoint to run a
// tenant's request through an x-ubiquum-workflow before it reaches the LLM.
type Workflow struct {
	cfg    config.WorkflowConfig
	store  store.Store
	client *http.Client
	// ttl is the Responses retention the gateway is configured with, so a
	// short-circuited turn expires on the same schedule as a real one.
	ttl    time.Duration
	logger *slog.Logger
}

// New creates a Workflow integration. Returns nil if disabled or unconfigured,
// in which case Middleware becomes a no-op passthrough.
// SetResponseTTL overrides how long a short-circuited Responses turn is kept.
// A non-positive duration restores the default.
func (wf *Workflow) SetResponseTTL(ttl time.Duration) {
	if wf == nil {
		return
	}
	wf.ttl = ttl
}

func New(cfg config.WorkflowConfig, db store.Store, logger *slog.Logger) *Workflow {
	if !cfg.Enabled || cfg.URL == "" {
		return nil
	}
	return &Workflow{
		cfg:    cfg,
		store:  db,
		client: &http.Client{Timeout: cfg.Timeout},
		logger: logger,
	}
}

// Middleware returns the x-ubiquum-workflow HTTP middleware. If wf is nil
// (disabled), it returns a plain passthrough.
func Middleware(wf *Workflow) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if wf == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wf.handle(w, r, next)
		})
	}
}

func (wf *Workflow) handle(w http.ResponseWriter, r *http.Request, next http.Handler) {
	isAnthropic, isOpenAI, isResponses := detectAPIFormat(r)
	if !isAnthropic && !isOpenAI && !isResponses {
		next.ServeHTTP(w, r)
		return
	}

	headerWorkflow := strings.TrimSpace(r.Header.Get(headerWorkflowRequest))
	if strings.EqualFold(headerWorkflow, "false") {
		next.ServeHTTP(w, r)
		return
	}

	teamID := teamIDFromContext(r.Context())
	if headerWorkflow == "" {
		hasDefault, err := wf.hasDefaultWorkflow(r.Context(), teamID)
		if err != nil {
			// A store error is not "no workflow configured". Swallowing it
			// forwarded the untransformed prompt to the model even under
			// fail_open:false, which exists precisely to stop that.
			wf.logger.Warn("workflow: default workflow lookup failed", "error", err, "team_id", teamID)
			w.Header().Set(headerApplied, "false")
			wf.failOpenOrError(w, r, next, isAnthropic, "workflow configuration unavailable")
			return
		}
		if !hasDefault {
			next.ServeHTTP(w, r)
			return
		}
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBody sits above us, so an oversized body fails here. Answer with
		// the status that says so rather than forwarding a truncated reader for
		// the handler to report as a decode error.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeWorkflowErrorStatus(w, isAnthropic, http.StatusRequestEntityTooLarge, decisionError,
				fmt.Sprintf("request body exceeds the %d byte limit", tooLarge.Limit))
			return
		}
		wf.logger.Warn("workflow: request body read failed, forwarding as-is", "error", err)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		next.ServeHTTP(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	messages, lastUserText, promptIdx := extractPrompt(bodyBytes, isAnthropic, isResponses)
	if lastUserText == "" {
		next.ServeHTTP(w, r)
		return
	}

	execResp, latencyMs, callErr := wf.execute(r, teamID, headerWorkflow, messages, lastUserText)
	if callErr != nil {
		wf.logger.Warn("workflow: execute call failed", "error", callErr)
		w.Header().Set(headerApplied, "false")
		wf.failOpenOrError(w, r, next, isAnthropic, "workflow service unavailable")
		return
	}

	workflowID := workflowIDOrDefault(headerWorkflow)
	w.Header().Set(headerWorkflowID, workflowID)
	w.Header().Set(headerDurationMs, strconv.FormatInt(latencyMs, 10))

	switch execResp.Decision {
	case decisionBlock:
		wf.handleBlock(w, execResp, isAnthropic, workflowID)
	case decisionRespond:
		streaming := isStreamingRequest(bodyBytes)
		wf.handleRespond(w, r, next, execResp, isAnthropic, isResponses, streaming, bodyBytes, workflowID)
	case decisionAllow:
		wf.handleAllow(w, r, next, execResp, allowRequest{
			bodyBytes:    bodyBytes,
			lastUserText: lastUserText,
			promptIdx:    promptIdx,
			isAnthropic:  isAnthropic,
			isResponses:  isResponses,
		}, workflowID)
	default:
		wf.handleNonActionable(w, r, next, execResp, isAnthropic, workflowID)
	}
}

// detectAPIFormat identifies which of the three chat/message API shapes a
// request uses, based on its path and method.
func detectAPIFormat(r *http.Request) (isAnthropic, isOpenAI, isResponses bool) {
	if r.Method != http.MethodPost {
		return false, false, false
	}
	switch r.URL.Path {
	case pathAnthropicMessages:
		return true, false, false
	case pathChatCompletions:
		return false, true, false
	case pathResponses:
		return false, false, true
	default:
		return false, false, false
	}
}

func teamIDFromContext(ctx context.Context) string {
	if ki := auth.KeyInfoFromContext(ctx); ki != nil {
		return ki.TeamID
	}
	return ""
}

// hasDefaultWorkflow reports whether the tenant has a default workflow
// configured, used to decide whether a header-less request is still worth a
// backend round-trip. The error is returned rather than folded into the bool:
// "no default workflow" and "we could not find out" are different answers, and
// only the caller knows whether fail-open lets the second one through.
func (wf *Workflow) hasDefaultWorkflow(ctx context.Context, teamID string) (bool, error) {
	if teamID == "" || wf.store == nil {
		return false, nil
	}
	settings, err := wf.store.GetTenantSettings(ctx, teamID)
	if err != nil {
		return false, err
	}
	if settings == nil || settings.DefaultWorkflowID == nil {
		return false, nil
	}
	return *settings.DefaultWorkflowID != "", nil
}

func requestModel(body []byte) string {
	var doc struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &doc)
	return doc.Model
}

// execute builds the backend request and times the round-trip.
func (wf *Workflow) execute(r *http.Request, teamID, workflowID string, messages []wfMessage, prompt string) (*executeResponse, int64, error) {
	execReq := executeRequest{
		TenantID:   teamID,
		WorkflowID: workflowID,
		Prompt:     prompt,
		Messages:   messages,
		UserAPIKey: rawBearerToken(r),
	}
	start := time.Now()
	resp, err := wf.call(r.Context(), execReq)
	return resp, time.Since(start).Milliseconds(), err
}

func workflowIDOrDefault(headerWorkflow string) string {
	if headerWorkflow == "" {
		return defaultWorkflowLabel
	}
	return headerWorkflow
}

// failOpenOrError either passes the request through unmodified or writes an
// error response, depending on the workflow's configured fail-open policy.
func (wf *Workflow) failOpenOrError(w http.ResponseWriter, r *http.Request, next http.Handler, isAnthropic bool, message string) {
	if wf.cfg.ShouldFailOpen() {
		next.ServeHTTP(w, r)
		return
	}
	writeWorkflowErrorStatus(w, isAnthropic, http.StatusBadGateway, decisionError, message)
}

func (wf *Workflow) handleBlock(w http.ResponseWriter, resp *executeResponse, isAnthropic bool, workflowID string) {
	w.Header().Set(headerApplied, "true")
	reason := resp.BlockMessage
	if reason == "" {
		reason = resp.BlockReason
	}
	if reason == "" {
		reason = "request blocked by workflow"
	}
	wf.logger.Info("workflow: request blocked", "reason", reason, "workflow", workflowID)
	writeWorkflowErrorStatus(w, isAnthropic, http.StatusForbidden, "workflow_blocked", reason)
}

func (wf *Workflow) handleRespond(w http.ResponseWriter, r *http.Request, next http.Handler, resp *executeResponse, isAnthropic, isResponses, streaming bool, bodyBytes []byte, workflowID string) {
	model := requestModel(bodyBytes)
	w.Header().Set(headerApplied, "true")
	wf.logger.Info("workflow: responded without calling the LLM", "workflow", workflowID)

	// The handler's own validation never runs on a short-circuited turn, so the
	// checks that decide whether the request was even coherent have to happen
	// here — otherwise an invalid request is answered as though it were fine.
	if isResponses {
		if msg := wf.validateRespondRequest(r, bodyBytes); msg != "" {
			writeWorkflowErrorStatus(w, isAnthropic, http.StatusBadRequest, decisionError, msg)
			return
		}
	}

	if strings.TrimSpace(resp.RespondMessage) == "" {
		// A respond decision with nothing to say is a misconfigured workflow,
		// not an answer. Returning an empty assistant turn looks to the caller
		// like the model had nothing to add.
		wf.logger.Warn("workflow: respond decision carried no message", "workflow", workflowID)
		w.Header().Set(headerApplied, "false")
		wf.failOpenOrError(w, r, next, isAnthropic, "workflow returned an empty response")
		return
	}

	respID := respondID("resp")
	if isResponses {
		// The turn has to exist in the responses store, or the client's next
		// request chaining on previous_response_id is rejected as unknown.
		wf.persistRespondTurn(r, respID, model, resp.RespondMessage, bodyBytes)
	}
	writeRespond(w, isAnthropic, isResponses, streaming, respID, model, resp.RespondMessage)
}

// persistRespondTurn stores a short-circuited Responses turn so it can be
// chained from. Failures are logged and never fail the request: the caller
// already has its answer, and losing the ability to chain is not worth turning
// a successful call into an error.
func (wf *Workflow) persistRespondTurn(r *http.Request, respID, model, message string, bodyBytes []byte) {
	rs, ok := wf.store.(store.ResponseStore)
	if !ok || wf.store == nil {
		return
	}
	ownerID := auth.OwnerID(r.Context())
	if ownerID == "" {
		return
	}

	// The request's own semantics apply to a short-circuited turn exactly as
	// they do to a real one.
	var req struct {
		Store              *bool           `json:"store"`
		Conversation       json.RawMessage `json:"conversation"`
		Input              json.RawMessage `json:"input"`
		PreviousResponseID string          `json:"previous_response_id"`
	}
	_ = json.Unmarshal(bodyBytes, &req)

	conversationID := conversationIDFrom(req.Conversation)
	inputItems := normalizeRespondInput(req.Input)
	outputItem := respondOutputItem(message)

	// The stored input is the *resolved* thread, matching what the Responses
	// path stores. Nothing walks previous_response_id at read time —
	// storedResponseItems reads InputItems and the payload's output and stops —
	// so filing only this turn's input broke the chain here: the turn after a
	// short-circuited one saw this exchange and nothing before it.
	if req.PreviousResponseID != "" {
		previous, err := rs.GetResponse(r.Context(), req.PreviousResponseID, ownerID)
		if err != nil || previous == nil {
			wf.logger.Warn("workflow: previous_response_id not found, storing an unchained turn",
				"error", err, "previous_response_id", req.PreviousResponseID)
		} else {
			inputItems = append(storedRespondHistory(previous), inputItems...)
		}
	}

	// The conversation is joined whether or not the turn itself is stored:
	// store:false is about retrieving the response later, not about the thread
	// it belongs to — the real Responses path draws exactly that line.
	wf.appendRespondTurnToConversation(r, conversationID, ownerID, inputItems, outputItem)

	if req.Store != nil && !*req.Store {
		return
	}
	payload, err := json.Marshal(responsesRespondPayload(respID, respondID("msg"), model, message))
	if err != nil {
		wf.logger.Warn("workflow: respond turn encode failed", "error", err, "response_id", respID)
		return
	}
	storedInput := "[]"
	if encoded, err := json.Marshal(inputItems); err == nil {
		storedInput = string(encoded)
	}
	expiry := time.Now().Add(wf.responseTTL())
	record := &store.StoredResponse{
		ID:             respID,
		OwnerID:        ownerID,
		Model:          model,
		Status:         "completed",
		ConversationID: conversationID,
		// Without this the chain broke at a short-circuited turn: the next
		// request naming this response as its previous_response_id found a row
		// with no ancestor, so everything said before it was lost.
		PreviousResponseID: req.PreviousResponseID,
		Payload:            string(payload),
		InputItems:         storedInput,
		CreatedAt:          time.Now(),
		ExpiresAt:          &expiry,
	}
	if err := rs.CreateResponse(r.Context(), record); err != nil {
		wf.logger.Warn("workflow: respond turn persist failed", "error", err, "response_id", respID)
	}
}

// storedRespondHistory replays a stored turn as input items: everything that
// went in, then everything that came out. Mirrors the Responses path's own
// expansion, which is what makes the chain O(1) to read back.
func storedRespondHistory(record *store.StoredResponse) []json.RawMessage {
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

// validateRespondRequest reports why a Responses request is malformed, or "".
// It mirrors what the Responses handler would have rejected.
func (wf *Workflow) validateRespondRequest(r *http.Request, bodyBytes []byte) string {
	var req struct {
		Conversation       json.RawMessage `json:"conversation"`
		PreviousResponseID string          `json:"previous_response_id"`
	}
	if json.Unmarshal(bodyBytes, &req) != nil {
		return ""
	}
	conversationID := conversationIDFrom(req.Conversation)
	if conversationID != "" && req.PreviousResponseID != "" {
		return "conversation and previous_response_id cannot be used together"
	}

	ownerID := auth.OwnerID(r.Context())
	if ownerID == "" {
		return ""
	}
	if conversationID != "" {
		if cs, ok := wf.store.(store.ConversationStore); ok {
			conversation, err := cs.GetConversation(r.Context(), conversationID, ownerID)
			if err != nil || conversation == nil {
				return fmt.Sprintf("conversation %q not found", conversationID)
			}
		}
	}
	if req.PreviousResponseID != "" {
		if rs, ok := wf.store.(store.ResponseStore); ok {
			previous, err := rs.GetResponse(r.Context(), req.PreviousResponseID, ownerID)
			if err != nil || previous == nil {
				return fmt.Sprintf("previous response %q not found", req.PreviousResponseID)
			}
		}
	}
	return ""
}

// normalizeRespondInput turns the polymorphic "input" field into a list of
// items, the way the Responses path stores it. Filing the raw string meant a
// later replay of this turn saw something no other turn looks like.
func normalizeRespondInput(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		item, err := json.Marshal(map[string]interface{}{
			"id":   newRespondItemID(),
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": text}},
		})
		if err != nil {
			return nil
		}
		return []json.RawMessage{item}
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	for i, item := range items {
		items[i] = withRespondItemID(item)
	}
	return items
}

// withRespondItemID gives an input item an id when the caller sent none.
// /input_items paginates with after/before cursors over these ids, so an item
// stored without one cannot be paged past.
func withRespondItemID(item json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return item
	}
	if _, ok := fields["id"]; ok {
		return item
	}
	id, err := json.Marshal(newRespondItemID())
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

// newRespondItemID mints an item id in the shape the Responses surface uses.
func newRespondItemID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	return "msg_" + hex.EncodeToString(raw[:])
}

// respondOutputItem renders the workflow's answer as a Responses output item.
func respondOutputItem(message string) json.RawMessage {
	encoded, err := json.Marshal(map[string]interface{}{
		"type": "message", "role": "assistant", "status": "completed",
		"content": []map[string]interface{}{{
			"type": "output_text", "text": message, "annotations": []interface{}{},
		}},
	})
	if err != nil {
		return nil
	}
	return encoded
}

// appendRespondTurnToConversation files what this turn contributed — the
// caller's input, then the workflow's answer — in the thread it belongs to.
func (wf *Workflow) appendRespondTurnToConversation(
	r *http.Request, conversationID, ownerID string,
	input []json.RawMessage, output json.RawMessage,
) {
	if conversationID == "" {
		return
	}
	cs, ok := wf.store.(store.ConversationStore)
	if !ok {
		return
	}
	payloads := make([]string, 0, len(input)+1)
	for _, item := range input {
		payloads = append(payloads, string(item))
	}
	if len(output) > 0 {
		payloads = append(payloads, string(output))
	}
	if len(payloads) == 0 {
		return
	}
	if _, err := cs.AppendConversationItems(r.Context(), conversationID, ownerID, payloads); err != nil {
		wf.logger.Warn("workflow: conversation append failed", "error", err, "conversation_id", conversationID)
	}
}

// defaultRespondTTL matches the Responses API's own retention default. It only
// applies when the gateway was given no explicit setting.
const defaultRespondTTL = 30 * 24 * time.Hour

// responseTTL is the configured retention, so a short-circuited turn expires on
// the same schedule as a real one rather than always after thirty days.
func (wf *Workflow) responseTTL() time.Duration {
	if wf.ttl > 0 {
		return wf.ttl
	}
	return defaultRespondTTL
}

// conversationIDFrom reads the conversation reference, which is either a bare
// id or an object carrying one.
func conversationIDFrom(raw json.RawMessage) string {
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

func (wf *Workflow) handleAllow(w http.ResponseWriter, r *http.Request, next http.Handler, resp *executeResponse, req allowRequest, workflowID string) {
	w.Header().Set(headerApplied, "true")
	newBody := wf.rewritePrompt(req.bodyBytes, req.lastUserText, resp.FinalPrompt, req.promptIdx, req.isAnthropic, req.isResponses)
	newBody = wf.rewriteModel(newBody, resp.RouteModel, workflowID)

	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	next.ServeHTTP(w, r)
}

func (wf *Workflow) rewritePrompt(bodyBytes []byte, lastUserText, finalPrompt string, promptIdx int, isAnthropic, isResponses bool) []byte {
	if finalPrompt == "" || finalPrompt == lastUserText {
		return bodyBytes
	}
	rewritten, err := rewriteLastUserText(bodyBytes, finalPrompt, promptIdx, isAnthropic, isResponses)
	if err != nil {
		wf.logger.Warn("workflow: body rewrite failed, passing through original prompt", "error", err)
		return bodyBytes
	}
	return rewritten
}

func (wf *Workflow) rewriteModel(bodyBytes []byte, routeModel, workflowID string) []byte {
	if routeModel == "" {
		return bodyBytes
	}
	rewritten, err := overrideModel(bodyBytes, routeModel)
	if err != nil {
		return bodyBytes
	}
	wf.logger.Info("workflow: model routed", "model", routeModel, "workflow", workflowID)
	return rewritten
}

func (wf *Workflow) handleNonActionable(w http.ResponseWriter, r *http.Request, next http.Handler, resp *executeResponse, isAnthropic bool, workflowID string) {
	w.Header().Set(headerApplied, "false")
	wf.logger.Warn("workflow: execute returned a non-actionable decision", "decision", resp.Decision, "workflow", workflowID)
	wf.failOpenOrError(w, r, next, isAnthropic, "workflow execution failed")
}

func (wf *Workflow) call(ctx context.Context, execReq executeRequest) (*executeResponse, error) {
	body, err := json.Marshal(execReq)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(wf.cfg.URL, "/") + "/workflows/execute"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set(headerContentType, contentTypeJSON)

	resp, err := wf.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("workflow execute returned status %d: %s", resp.StatusCode, truncateBody(respBody))
	}

	var execResp executeResponse
	if err := json.Unmarshal(respBody, &execResp); err != nil {
		return nil, fmt.Errorf("invalid workflow execute response: %w", err)
	}
	return &execResp, nil
}

// rawBearerToken recovers the caller's API key the same way auth does.
//
// The X-Api-Key fallback is not optional here: the Anthropic SDK and Claude
// Code send the key in that header and never set Authorization, and
// /v1/messages is the primary target of this feature — without it user_api_key
// reached the backend empty on every such request. A non-Bearer Authorization
// scheme yields nothing rather than the whole header value, which was being
// passed off as the key.
func rawBearerToken(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		if token := strings.TrimSpace(authHeader[7:]); token != "" {
			return token
		}
	} else if authHeader != "" && !strings.Contains(authHeader, " ") {
		// A bare token with no scheme, which some clients send.
		return authHeader
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func truncateBody(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

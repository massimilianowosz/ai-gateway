package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// guardRequest is the payload sent to the legacy LLM Guard batch endpoint.
type guardRequest struct {
	Messages   []guardMessage `json:"messages"`
	Guardrails []string       `json:"guardrails,omitempty"`
	TeamID     string         `json:"team_id,omitempty"`
	KeyHash    string         `json:"user_api_key_hash,omitempty"`
}

type guardMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// guardResponse is the JSON response from the LLM Guard service.
type guardResponse struct {
	Action              string   `json:"action"`
	BlockedReason       string   `json:"blocked_reason,omitempty"`
	TriggeredScanners   []string `json:"triggered_scanners,omitempty"`
	TriggeredGuardrails []string `json:"triggered_guardrails,omitempty"`
}

// Guardrail scans chat/message requests through inline scanners or an external service.
type Guardrail struct {
	cfg    config.GuardrailConfig
	engine *guardrail.Engine // inline engine (nil if using legacy HTTP)
	store  store.Store
	client *http.Client
	logger *slog.Logger
}

// anonymizationConfigStore is optional so the hotfix remains compatible with
// test doubles and alternative Store implementations. GormStore implements it
// by reading the portal-owned anonymization_configs table.
type anonymizationConfigStore interface {
	GetAnonymizationEntities(ctx context.Context, teamID string) (*[]string, error)
}

type requestGuardrailState struct {
	enabled              []string
	piiEntities          *[]string
	keyInfo              *store.APIKey
	disabled             bool
	sensitiveDataHandled bool
}

type requestGuardrailStateKey struct{}

// NewGuardrail creates a guardrail middleware.
// If inline scanners are configured, uses the local Engine.
// Otherwise falls back to the legacy external LLM Guard HTTP service.
// Returns nil if not enabled.
func NewGuardrail(cfg config.GuardrailConfig, engine *guardrail.Engine, db store.Store, logger *slog.Logger) *Guardrail {
	if !cfg.Enabled {
		return nil
	}
	// Must have either inline engine or legacy URL
	if engine == nil && cfg.URL == "" {
		return nil
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Guardrail{
		cfg:    cfg,
		engine: engine,
		store:  db,
		client: &http.Client{
			Timeout: timeout,
		},
		logger: logger,
	}
}

// SensitiveDataMiddleware handles PII and secrets immediately after
// authentication, before workflow, cache, request logging and HiveState can
// observe the request. The regular Middleware remains later in the chain for
// prompt-injection, toxicity and any future non-sensitive-data scanners.
//
// When the inline engine is unavailable this is deliberately a no-op: the
// legacy HTTP guardrail path can block, but cannot safely rewrite a request.
func (g *Guardrail) SensitiveDataMiddleware(next http.Handler) http.Handler {
	if g == nil || g.engine == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := readAndRestoreGuardrailBody(r)
		if err != nil {
			g.logger.Warn("guardrail: failed to read request body for sensitive-data preprocessing", "error", err)
			next.ServeHTTP(w, r)
			return
		}

		messages := extractMessages(bodyBytes)
		if len(messages) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		state := g.resolveRequestState(r)
		if state.disabled {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestGuardrailStateKey{}, state)))
			return
		}

		early, _ := splitSensitiveDataGuardrails(state.enabled)
		if len(early) == 0 {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestGuardrailStateKey{}, state)))
			return
		}

		result := g.engine.ScanWithPIIEntities(r.Context(), toGuardrailMessages(messages), early, state.piiEntities, state.keyInfo)
		if result.Action == "BLOCKED" {
			g.recordBlocked(r.Context(), state.keyInfo, result)
			g.writeBlockedResult(w, result)
			return
		}

		if hasGuardrail(state.enabled, guardPIIRedact) {
			if entities := g.redactPII(r, state.piiEntities, bodyBytes); len(entities) > 0 {
				g.recordRedactions(r.Context(), state.keyInfo, entities)
			}
		}

		state.sensitiveDataHandled = true
		ctx := context.WithValue(r.Context(), requestGuardrailStateKey{}, state)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Middleware returns a middleware function that scans requests.
// If the Guardrail is nil (disabled), it returns a no-op pass-through.
func (g *Guardrail) Middleware(next http.Handler) http.Handler {
	if g == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := readAndRestoreGuardrailBody(r)
		if err != nil {
			g.logger.Warn("guardrail: failed to read request body", "error", err)
			next.ServeHTTP(w, r)
			return
		}

		// Parse model-visible text from OpenAI Chat/Responses/Completions and
		// Anthropic Messages request bodies.
		messages := extractMessages(bodyBytes)
		if len(messages) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		state := g.stateForRequest(r)
		if state.disabled {
			next.ServeHTTP(w, r)
			return
		}

		enabledGuardrails := state.enabled
		allowPIIRedaction := true
		if state.sensitiveDataHandled {
			_, enabledGuardrails = splitSensitiveDataGuardrails(enabledGuardrails)
			allowPIIRedaction = false
			if len(enabledGuardrails) == 0 {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Route to inline engine or legacy HTTP
		if g.engine != nil {
			g.scanInline(w, r, next, messages, enabledGuardrails, state.piiEntities, state.keyInfo, bodyBytes, allowPIIRedaction)
		} else {
			g.scanLegacy(w, r, next, messages, enabledGuardrails, state.keyInfo)
		}
	})
}

func readAndRestoreGuardrailBody(r *http.Request) ([]byte, error) {
	bodyBytes, err := io.ReadAll(r.Body)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes, nil
}

func (g *Guardrail) stateForRequest(r *http.Request) requestGuardrailState {
	if state, ok := r.Context().Value(requestGuardrailStateKey{}).(requestGuardrailState); ok {
		return state
	}
	return g.resolveRequestState(r)
}

func (g *Guardrail) resolveRequestState(r *http.Request) requestGuardrailState {
	state := requestGuardrailState{
		enabled: append([]string(nil), g.cfg.Guardrails...),
		keyInfo: auth.KeyInfoFromContext(r.Context()),
	}

	// A guardrail_set policy bound to this agent environment only ever adds
	// to what the global config already scans, the same "policies narrow,
	// never widen" rule spend_budget and model_allowlist follow — narrowing
	// here means requiring more, not less. Read once, applied at every exit
	// point below via requireNonDerogable, so a header cannot remove what a
	// non-derogable policy requires.
	var keyRequired []string
	var keyNonDerogable bool
	if state.keyInfo != nil && state.keyInfo.KeyHash != "" && g.store != nil {
		ks, captured := auth.KeyRouteSettingsFromContext(r.Context())
		var err error
		if !captured {
			ks, err = g.store.GetKeyRouteSettings(r.Context(), state.keyInfo.KeyHash)
		}
		if err == nil && ks != nil && ks.GuardrailOverride != nil {
			keyRequired = ks.GuardrailOverride.RequiredGuardrails
			keyNonDerogable = ks.GuardrailOverride.NonDerogable
		}
	}
	requireNonDerogable := func(s requestGuardrailState) requestGuardrailState {
		if !keyNonDerogable || len(keyRequired) == 0 {
			return s
		}
		s.enabled = unionStrings(s.enabled, keyRequired)
		s.disabled = false
		return s
	}
	state.enabled = unionStrings(state.enabled, keyRequired)

	guardHeader := r.Header.Get("x-ubiquum-guard")
	if guardHeader == "" {
		guardHeader = r.Header.Get("x-guardrails")
	}
	hasHeaderOverride := false
	if guardHeader != "" {
		if strings.EqualFold(strings.TrimSpace(guardHeader), "none") {
			state.disabled = true
			state.enabled = nil
			return requireNonDerogable(state)
		}
		var headerGuardrails []string
		for _, name := range strings.Split(guardHeader, ",") {
			if name = strings.TrimSpace(name); name != "" {
				headerGuardrails = append(headerGuardrails, name)
			}
		}
		if len(headerGuardrails) > 0 {
			state.enabled = headerGuardrails
			hasHeaderOverride = true
		}
	}

	if !hasHeaderOverride && len(state.enabled) > 0 {
		// Missing tenant settings still apply policy defaults. In particular,
		// sensitive-data-block must remain off when an older tenant has never
		// stored a value for the newly introduced opt-in policy.
		var tenantGuardrails map[string]bool
		if state.keyInfo != nil && state.keyInfo.TeamID != "" && g.store != nil {
			settings, err := g.store.GetTenantSettings(r.Context(), state.keyInfo.TeamID)
			if err != nil {
				g.logger.Warn("guardrail: failed to load tenant guardrails; applying safe defaults", "error", err, "team_id", state.keyInfo.TeamID)
			} else if settings != nil {
				tenantGuardrails = settings.GuardrailsConfig
			}
		}
		state.enabled = filterTenantGuardrails(state.enabled, tenantGuardrails)
		if len(state.enabled) == 0 {
			state.disabled = true
			return requireNonDerogable(state)
		}
	}

	if state.keyInfo == nil || state.keyInfo.TeamID == "" || g.store == nil {
		return requireNonDerogable(state)
	}

	// The entity selection is read whatever the header says: a request can turn
	// a policy off, but cannot widen the entities either policy covers.
	if configStore, ok := g.store.(anonymizationConfigStore); ok {
		entities, err := configStore.GetAnonymizationEntities(r.Context(), state.keyInfo.TeamID)
		if err != nil {
			// A lookup failure keeps the full built-in detector set.
			g.logger.Warn("guardrail: failed to load anonymization entities", "error", err, "team_id", state.keyInfo.TeamID)
		} else {
			state.piiEntities = entities
		}
	}

	return requireNonDerogable(state)
}

// unionStrings returns the deduplicated union of a and b, preserving a's
// order and appending anything from b not already present.
func unionStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a))
	out := append([]string(nil), a...)
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

// guardPIIRedact is the policy that rewrites PII and forwards the request;
// guardPIIBlock is the one that refuses it. Both read the same entity
// selection, and when a tenant runs both, blocking decides first.
const (
	guardPIIRedact = "sensitive-data"
	guardPIIBlock  = "sensitive-data-block"
)

// optInGuardrails must be switched on explicitly by a tenant: for these,
// "absent from guardrails_config" means off, not "inherit the default".
// PII Blocking is opt-in so that shipping it enabled in the gateway config
// cannot silently turn an existing tenant's redaction into a refusal.
var optInGuardrails = map[string]bool{guardPIIBlock: true}

var sensitiveDataGuardrails = map[string]bool{
	guardPIIRedact:              true,
	guardPIIBlock:               true,
	"sensitive-data-protection": true,
	"anonymization":             true,
	"pii":                       true,
	"secrets":                   true,
}

// filterTenantGuardrails keeps the guardrails a tenant has enabled. Policies
// missing from the tenant config are kept (they default on) unless they are
// opt-in.
func filterTenantGuardrails(guardrails []string, cfg map[string]bool) []string {
	var out []string
	for _, gr := range guardrails {
		if enabled, exists := cfg[gr]; exists {
			if enabled {
				out = append(out, gr)
			}
			continue
		}
		if !optInGuardrails[gr] {
			out = append(out, gr)
		}
	}
	return out
}

func hasGuardrail(guardrails []string, name string) bool {
	for _, gr := range guardrails {
		if gr == name {
			return true
		}
	}
	return false
}

// splitSensitiveDataGuardrails separates policies that must run before any
// component which can inspect or persist request text. An empty configured list
// historically means "all scanners", so represent that explicitly on both
// sides of the split.
func splitSensitiveDataGuardrails(guardrails []string) (early, remaining []string) {
	if len(guardrails) == 0 {
		return []string{"pii", "secrets"}, []string{"injection", "moderation"}
	}
	for _, name := range guardrails {
		if sensitiveDataGuardrails[name] {
			early = append(early, name)
		} else {
			remaining = append(remaining, name)
		}
	}
	return early, remaining
}

func toGuardrailMessages(messages []guardMessage) []guardrail.Message {
	out := make([]guardrail.Message, len(messages))
	for i, message := range messages {
		out[i] = guardrail.Message{Role: message.Role, Content: message.Content}
	}
	return out
}

// scanInline uses the local guardrail.Engine. It applies the two PII policies
// in the order that makes enabling both coherent: blocking refuses the request
// outright, and only what survives is redacted and forwarded.
func (g *Guardrail) scanInline(w http.ResponseWriter, r *http.Request, next http.Handler, messages []guardMessage, enabledGuardrails []string, piiEntities *[]string, keyInfo *store.APIKey, bodyBytes []byte, allowPIIRedaction bool) {
	result := g.engine.ScanWithPIIEntities(r.Context(), toGuardrailMessages(messages), enabledGuardrails, piiEntities, keyInfo)

	if result.Action == "BLOCKED" {
		g.recordBlocked(r.Context(), keyInfo, result)
		g.writeBlockedResult(w, result)
		return
	}

	if allowPIIRedaction && hasGuardrail(enabledGuardrails, guardPIIRedact) {
		if entities := g.redactPII(r, piiEntities, bodyBytes); len(entities) > 0 {
			g.recordRedactions(r.Context(), keyInfo, entities)
		}
	}

	next.ServeHTTP(w, r)
}

func (g *Guardrail) writeBlockedResult(w http.ResponseWriter, result guardrail.Result) {
	g.logger.Info("guardrail: request blocked",
		"reason", result.BlockedReason,
		"scanners", result.TriggeredScanners,
		"guardrails", result.TriggeredGuardrails,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	msg, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": result.BlockedReason,
			"type":    "content_policy_violation",
			"code":    "guardrail_blocked",
		},
	})
	_, _ = w.Write(msg)
}

// redactPII rewrites the request body in place, replacing the tenant's PII
// entities with [ENTITY] placeholders. The request is forwarded either way:
// refusing is the other policy's job.
// It returns the kinds of data it replaced, so the caller can record them.
func (g *Guardrail) redactPII(r *http.Request, piiEntities *[]string, bodyBytes []byte) []string {
	scanner := g.engine.PIIScanner()
	if scanner == nil {
		return nil
	}

	redacted, entities := redactBody(bodyBytes, func(text string) (string, []string) {
		return scanner.Redact(text, piiEntities)
	})
	if len(entities) == 0 {
		return nil
	}

	g.logger.Info("guardrail: pii redacted",
		"entities", entities,
		"path", r.URL.Path,
	)
	r.Body = io.NopCloser(bytes.NewReader(redacted))
	r.ContentLength = int64(len(redacted))
	r.Header.Set("Content-Length", strconv.Itoa(len(redacted)))
	return entities
}

// scanLegacy calls the external LLM Guard HTTP service (backward compatibility).
func (g *Guardrail) scanLegacy(w http.ResponseWriter, r *http.Request, next http.Handler, messages []guardMessage, enabledGuardrails []string, keyInfo *store.APIKey) {
	gReq := guardRequest{
		Messages:   messages,
		Guardrails: enabledGuardrails,
	}
	if keyInfo != nil {
		gReq.KeyHash = keyInfo.KeyHash
		gReq.TeamID = keyInfo.TeamID
	}

	guardBody, _ := json.Marshal(gReq)
	url := strings.TrimRight(g.cfg.URL, "/") + "/analyze/batch/beta/litellm_basic_guardrail_api"

	hookReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(guardBody))
	if err != nil {
		g.logger.Warn("guardrail: failed to create request", "error", err)
		if g.cfg.FailOpen {
			next.ServeHTTP(w, r)
		} else {
			writeGuardError(w, "guardrail service unavailable")
		}
		return
	}
	hookReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(hookReq)
	if err != nil {
		g.logger.Warn("guardrail: callout failed", "url", url, "error", err)
		if g.cfg.FailOpen {
			next.ServeHTTP(w, r)
		} else {
			writeGuardError(w, "guardrail service unavailable")
		}
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode >= 500 {
		g.logger.Warn("guardrail: service error", "status", resp.StatusCode)
		if g.cfg.FailOpen {
			next.ServeHTTP(w, r)
		} else {
			writeGuardError(w, "guardrail service error")
		}
		return
	}

	var gResp guardResponse
	if err := json.Unmarshal(respBody, &gResp); err != nil {
		g.logger.Warn("guardrail: invalid response", "error", err, "body", string(respBody))
		if g.cfg.FailOpen {
			next.ServeHTTP(w, r)
		} else {
			writeGuardError(w, "guardrail service returned invalid response")
		}
		return
	}

	if gResp.Action == "BLOCKED" {
		reason := gResp.BlockedReason
		if reason == "" {
			reason = "request blocked by content guardrail"
		}
		// Refusals from the external service go out through the same door as
		// the inline ones, so a blocked request is recorded once, in one place,
		// whichever engine decided it.
		result := guardrail.Result{
			Action:              "BLOCKED",
			BlockedReason:       reason,
			TriggeredScanners:   gResp.TriggeredScanners,
			TriggeredGuardrails: gResp.TriggeredGuardrails,
		}
		g.recordBlocked(r.Context(), keyInfo, result)
		g.writeBlockedResult(w, result)
		return
	}

	next.ServeHTTP(w, r)
}

// extractMessages returns every piece of text the target model can see across
// OpenAI Chat, Anthropic Messages, OpenAI Responses and legacy Completions.
func extractMessages(body []byte) []guardMessage {
	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		System       json.RawMessage `json:"system"`
		Instructions json.RawMessage `json:"instructions"`
		Input        json.RawMessage `json:"input"`
		Prompt       json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}

	out := make([]guardMessage, 0, len(parsed.Messages)+3)
	for _, m := range parsed.Messages {
		if text := extractContentText(m.Content); text != "" {
			out = append(out, guardMessage{Role: m.Role, Content: text})
		}
	}

	// Anthropic carries the system prompt outside the messages array.
	if text := extractContentText(parsed.System); text != "" {
		out = append(out, guardMessage{Role: "system", Content: text})
	}
	for _, instructions := range extractStringValues(parsed.Instructions) {
		out = append(out, guardMessage{Role: "system", Content: instructions})
	}
	out = append(out, extractResponsesInput(parsed.Input)...)
	for _, prompt := range extractStringValues(parsed.Prompt) {
		out = append(out, guardMessage{Role: "user", Content: prompt})
	}

	return out
}

func extractResponsesInput(raw json.RawMessage) []guardMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if values := extractStringValues(raw); len(values) > 0 {
		out := make([]guardMessage, 0, len(values))
		for _, value := range values {
			out = append(out, guardMessage{Role: "user", Content: value})
		}
		return out
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]guardMessage, 0, len(items))
	for _, item := range items {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Output  json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(item, &message); err != nil {
			continue
		}
		role := message.Role
		if role == "" {
			role = "user"
		}
		if text := extractContentText(message.Content); text != "" {
			out = append(out, guardMessage{Role: role, Content: text})
			continue
		}
		switch message.Type {
		case "text", "input_text", "output_text":
			if message.Text != "" {
				out = append(out, guardMessage{Role: role, Content: message.Text})
			}
		case "function_call_output":
			// A tool's result returning into the conversation, which is how
			// file contents reach the model in an agent loop.
			if text := extractContentText(message.Output); text != "" {
				out = append(out, guardMessage{Role: role, Content: text})
			}
		}
	}
	return out
}

func extractStringValues(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if value == "" {
			return nil
		}
		return []string{value}
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		var nonEmpty []string
		for _, value := range values {
			if value != "" {
				nonEmpty = append(nonEmpty, value)
			}
		}
		return nonEmpty
	}
	return nil
}

// maxContentBlockDepth bounds the recursion through nested content blocks. A
// tool result holding text blocks is two levels; anything deeper is a body
// shaped to make the scanner work, not a prompt.
const maxContentBlockDepth = 4

// extractContentText handles plain strings and content-block arrays, including
// the blocks that carry a tool's output back into the conversation. Those are
// not decoration: a file an agent reads reaches the model through a
// tool_result, and text the scanner cannot see is text neither policy protects.
func extractContentText(raw json.RawMessage) string {
	return extractContentTextDepth(raw, 0)
}

func extractContentTextDepth(raw json.RawMessage, depth int) string {
	if len(raw) == 0 || string(raw) == "null" || depth > maxContentBlockDepth {
		return ""
	}
	// Try as plain string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}

	// Try as array of content blocks (Anthropic and Responses formats)
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text", "input_text", "output_text":
				if b.Text == "" {
					continue
				}
				parts = append(parts, b.Text)
			case "tool_result":
				if text := extractContentTextDepth(b.Content, depth+1); text != "" {
					parts = append(parts, text)
				}
			case "function_call_output":
				if text := extractContentTextDepth(b.Output, depth+1); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

func writeGuardError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	msg, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "guardrail_error",
		},
	})
	_, _ = w.Write(msg)
}

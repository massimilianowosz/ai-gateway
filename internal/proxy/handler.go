package proxy

import (
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

// CompletionHandler handles /v1/chat/completions requests.
type CompletionHandler struct {
	registry *provider.Registry
	router   *router.Router
	logger   *slog.Logger
	pricing  *pricing.Calculator
	spender  *spend.BatchWriter
	tokens   tokenRecorder
	store    store.Store
}

type tokenRecorder interface {
	RecordTokens(promptTokens, completionTokens int)
	// RecordCacheTokens reports the provider's prompt-cache breakdown, so
	// cache hit rate can be tracked against the prefix-cache guard's
	// decisions. Implementations may ignore zero values.
	RecordCacheTokens(cached, created int)
}

// NewCompletionHandler creates a new completion handler.
func NewCompletionHandler(registry *provider.Registry, rt *router.Router, logger *slog.Logger, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, tokenRecorders ...tokenRecorder) *CompletionHandler {
	var tokens tokenRecorder
	if len(tokenRecorders) > 0 {
		tokens = tokenRecorders[0]
	}
	return &CompletionHandler{
		registry: registry,
		router:   rt,
		logger:   logger,
		pricing:  pc,
		spender:  spender,
		tokens:   tokens,
		store:    db,
	}
}

func (h *CompletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req provider.CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required and must not be empty")
		return
	}

	// Enforce per-key and per-team model access control
	if !auth.IsModelAllowed(r.Context(), req.Model, h.registry.IsRestricted(req.Model)) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied for model %q", req.Model))
		return
	}
	if !auth.IsProviderAllowed(r.Context(), h.registry.ProvidersFor(req.Model)) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied for the provider serving model %q", req.Model))
		return
	}
	if allEU, hasDeployments := h.registry.AllDeploymentsEU(req.Model); !auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied: model %q is not available in an EU-only deployment", req.Model))
		return
	}
	// A key that cannot pay — never funded, or funded and spent — may still
	// call a model the gateway does not pay for. Anything it does pay for needs
	// a budget with something left in it. Checked here, where the model is
	// known and authentication could not see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(req.Model)); status != auth.BudgetOK {
		writeBudgetError(w, status, req.Model)
		return
	}

	if err := resolveFileReferences(r, h.store, h.registry, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	injectContextPackInstructions(r.Context(), &req)

	if req.PinnedDeploymentID != "" {
		h.servePinnedDeployment(w, r, &req)
		return
	}

	// Use router if available (retry + circuit breaker)
	if h.router != nil {
		h.serveWithRouter(w, r, &req)
		return
	}

	// Fallback: direct routing (no retry)
	dep, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	upstreamReq := req
	upstreamReq.Model = dep.ProviderModel
	if req.Stream {
		// Always request usage in stream so we get real token counts. This has
		// to run *before* dropParams: injecting after it silently re-added the
		// field a deployment had configured away, and the upstreams that
		// declare drop_params for it reject the request outright.
		if upstreamReq.StreamOptions == nil {
			upstreamReq.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
		} else {
			upstreamReq.StreamOptions.IncludeUsage = true
		}
	}
	dropParams(&upstreamReq, dep.DropParams)

	if req.Stream {
		h.handleStream(w, r, req.Model, dep, &upstreamReq)
	} else {
		h.handleComplete(w, r, req.Model, dep, &upstreamReq)
	}
}

func (h *CompletionHandler) servePinnedDeployment(w http.ResponseWriter, r *http.Request, req *provider.CompletionRequest) {
	dep, err := getAuthorizedDeploymentByID(r.Context(), h.registry, req.Model, req.PinnedDeploymentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	upstreamReq := *req
	upstreamReq.Model = dep.ProviderModel
	if req.Stream {
		// Inject before dropping; see serveDirect above.
		if upstreamReq.StreamOptions == nil {
			upstreamReq.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
		} else {
			upstreamReq.StreamOptions.IncludeUsage = true
		}
	}
	dropParams(&upstreamReq, dep.DropParams)
	w.Header().Set(headerUbiquumProvider, dep.ProviderName)

	if req.Stream {
		h.handleStream(w, r, req.Model, dep, &upstreamReq)
		return
	}
	h.handleComplete(w, r, req.Model, dep, &upstreamReq)
}

func (h *CompletionHandler) serveWithRouter(w http.ResponseWriter, r *http.Request, req *provider.CompletionRequest) {
	start := time.Now()

	if req.Stream {
		// For streaming, we can't retry mid-stream, so route first then stream
		var streamDep *provider.Deployment
		var streamReader provider.StreamReader

		result, err := h.router.RouteEligible(r.Context(), req.Model, deploymentEligible(r.Context()), func(dep *provider.Deployment) error {
			upstreamReq := *req
			upstreamReq.Model = dep.ProviderModel
			upstreamReq.Stream = true
			// Always request usage in stream so we get real token counts
			if upstreamReq.StreamOptions == nil {
				upstreamReq.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
			} else {
				upstreamReq.StreamOptions.IncludeUsage = true
			}
			dropParams(&upstreamReq, dep.DropParams)

			reader, err := dep.Provider.Stream(r.Context(), &upstreamReq)
			if err != nil {
				return err
			}
			streamDep = dep
			streamReader = reader
			return nil
		})

		if err != nil {
			h.handleUpstreamError(w, err)
			return
		}

		// Set routing info headers for streaming responses
		w.Header().Set(headerUbiquumProvider, result.Deployment.ProviderName)
		if result.Attempts > 1 {
			w.Header().Set("X-Ubiquum-Attempts", fmt.Sprintf("%d", result.Attempts))
		}
		if len(result.FailedDeployments) > 0 {
			w.Header().Set("X-Ubiquum-Failed", strings.Join(result.FailedDeployments, ","))
		}

		h.relayStream(w, r, req.Model, streamDep, req, streamReader, start)
	} else {
		var resp *provider.CompletionResponse

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

		h.logger.Debug("request routed",
			"model", req.Model,
			"deployment", result.Deployment.ID,
			"attempts", result.Attempts,
		)

		// Set routing info headers
		w.Header().Set(headerUbiquumProvider, result.Deployment.ProviderName)
		if result.Attempts > 1 {
			w.Header().Set("X-Ubiquum-Attempts", fmt.Sprintf("%d", result.Attempts))
		}
		if len(result.FailedDeployments) > 0 {
			w.Header().Set("X-Ubiquum-Failed", strings.Join(result.FailedDeployments, ","))
		}

		// Calculate cost and set header
		var cost float64
		if resp != nil && resp.Usage != nil && h.pricing != nil {
			cost = computeCostUsage(result.Deployment, h.pricing, resp.Usage)
		}
		if cost > 0 {
			w.Header().Set("X-Ubiquum-Cost", fmt.Sprintf("%.10f", cost))
		}

		// Log spend asynchronously
		h.logSpend(r, req.Model, result.Deployment, req, resp, time.Since(start))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (h *CompletionHandler) handleComplete(w http.ResponseWriter, r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest) {
	start := time.Now()
	resp, err := dep.Provider.Complete(r.Context(), req)
	if err != nil {
		h.handleUpstreamError(w, err)
		return
	}

	if resp != nil && resp.Usage != nil && h.pricing != nil {
		cost := computeCostUsage(dep, h.pricing, resp.Usage)
		if cost > 0 {
			w.Header().Set("X-Ubiquum-Cost", fmt.Sprintf("%.10f", cost))
		}
	}
	h.logSpend(r, model, dep, req, resp, time.Since(start))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *CompletionHandler) handleStream(w http.ResponseWriter, r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest) {
	start := time.Now()
	reader, err := dep.Provider.Stream(r.Context(), req)
	if err != nil {
		h.handleUpstreamError(w, err)
		return
	}

	h.relayStream(w, r, model, dep, req, reader, start)
}

func (h *CompletionHandler) relayStream(w http.ResponseWriter, r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest, reader provider.StreamReader, start time.Time) {
	defer reader.Close()

	// Set streaming headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	// Use pooled buffer for SSE formatting (reduces GC pressure at high concurrency)
	buf := perf.GetBuffer()
	defer perf.PutBuffer(buf)

	var usage *provider.Usage
	completionChars := 0

	// Relay SSE chunks
	for {
		chunk, err := reader.Next()
		if err == io.EOF {
			buf.Reset()
			buf.WriteString("data: [DONE]\n\n")
			_, _ = w.Write(buf.Bytes())
			flusher.Flush()

			h.logStreamSpend(r, model, dep, req, usage, completionChars, time.Since(start))
			return
		}
		if err != nil {
			if r.Context().Err() == context.Canceled {
				h.logger.Debug("stream closed by client", "error", err)
			} else {
				h.logger.Error("stream read error", "error", err)
			}
			return
		}

		if chunkUsage := streamUsageFromChunk(chunk); chunkUsage != nil {
			usage = chunkUsage
		}

		// Track completion content length for token estimation fallback
		completionChars += streamContentLen(chunk)

		buf.Reset()
		buf.WriteString("data: ")
		buf.Write(chunk)
		buf.WriteString("\n\n")
		_, _ = w.Write(buf.Bytes())
		flusher.Flush()
	}
}

func (h *CompletionHandler) handleUpstreamError(w http.ResponseWriter, err error) {
	h.logger.Error("upstream error", "error", err)

	// Model not found → 404
	if strings.Contains(err.Error(), "not found") {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	// The router wraps the last attempt's error, so the upstream status only
	// survives an unwrapping match. A type assertion turned every 429 into a
	// 502, which clients read as transient and retry into the ground.
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		// Preserve upstream status code for client errors
		status := ue.StatusCode
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		writeError(w, status, "upstream_error", ue.Message)
		return
	}

	writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

// dropParams removes parameters from the request that are not supported by the provider.
func dropParams(req *provider.CompletionRequest, params []string) {
	if len(params) == 0 {
		return
	}

	for _, p := range params {
		switch p {
		case "temperature":
			req.Temperature = nil
		case "top_p":
			req.TopP = nil
		case "max_tokens":
			req.MaxTokens = nil
		case "max_completion_tokens":
			req.MaxCompletionTokens = nil
		case "stop":
			req.Stop = nil
		case "presence_penalty":
			req.PresencePenalty = nil
		case "frequency_penalty":
			req.FrequencyPenalty = nil
		case "n":
			req.N = nil
		case "tools":
			req.Tools = nil
		case "tool_choice":
			req.ToolChoice = nil
		case "response_format":
			req.ResponseFormat = nil
		case "stream_options":
			req.StreamOptions = nil
		case "stream_options.include_usage":
			if req.StreamOptions != nil {
				req.StreamOptions.IncludeUsage = false
			}
		case "user":
			req.User = ""
		default:
			delete(req.Extra, p)
		}
	}
}

func streamUsageFromChunk(chunk []byte) *provider.Usage {
	var payload struct {
		Usage *provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		return nil
	}
	return payload.Usage
}

// streamContentLen extracts the delta content length from a streaming chunk.
func streamContentLen(chunk []byte) int {
	var payload struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunk, &payload); err != nil {
		return 0
	}
	n := 0
	for _, c := range payload.Choices {
		n += len(c.Delta.Content)
	}
	return n
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
		},
	})
}

// logSpend records token usage and cost via the batch writer.
func (h *CompletionHandler) logSpend(r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest, resp *provider.CompletionResponse, duration time.Duration) {
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

	// Extract key info from context
	if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo != nil {
		record.ApplyIdentity(keyInfo)
	}

	// Extract token usage and calculate cost
	if resp.Usage != nil && resp.Usage.TotalTokens > 0 {
		applyUsageTokens(&record, dep, resp.Usage)
		if h.pricing != nil {
			record.Cost = computeCostUsage(dep, h.pricing, resp.Usage)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
			h.tokens.RecordCacheTokens(resp.Usage.CachedTokens(), resp.Usage.CacheCreation())
		}
	} else {
		// Fallback: estimate tokens (chars/4) when provider doesn't return usage
		promptTok := estimatePromptTokens(req)
		completionTok := estimateCompletionTokens(resp)
		record.PromptTokens = promptTok
		record.CompletionTokens = completionTok
		record.TotalTokens = promptTok + completionTok
		record.CostEstimated = true
		if h.pricing != nil {
			record.Cost = computeCost(dep, h.pricing, promptTok, completionTok)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(promptTok, completionTok)
		}
	}

	h.recordSpend(r, record)
}

// logStreamSpend logs a spend record for streaming requests.
// Token counts are available when providers emit stream usage chunks.
func (h *CompletionHandler) logStreamSpend(r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest, usage *provider.Usage, completionChars int, duration time.Duration) {
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
		// Fallback: estimate tokens when provider doesn't return usage in stream
		promptTok := estimatePromptTokens(req)
		completionTok := completionChars / 4
		if completionTok == 0 && completionChars > 0 {
			completionTok = 1
		}
		record.PromptTokens = promptTok
		record.CompletionTokens = completionTok
		record.TotalTokens = promptTok + completionTok
		record.CostEstimated = true
		if h.pricing != nil {
			record.Cost = computeCost(dep, h.pricing, promptTok, completionTok)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(promptTok, completionTok)
		}
	}

	h.recordSpend(r, record)
}

func (h *CompletionHandler) recordSpend(r *http.Request, record store.SpendRecord) {
	// Fill UsageCapture for upstream middleware (HiveState)
	if uc := store.GetUsageCapture(r.Context()); uc != nil && record.PromptTokens > 0 {
		uc.PromptTokens = record.PromptTokens
		uc.CompletionTokens = record.CompletionTokens
		uc.TotalTokens = record.TotalTokens
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

// estimatePromptTokens estimates prompt token count from request messages (chars/4).
func estimatePromptTokens(req *provider.CompletionRequest) int {
	if req == nil {
		return 0
	}
	total := 0
	for _, m := range req.Messages {
		total += 4 // per-message overhead (role, formatting)
		total += messageContentLen(m.Content) / 4
	}
	if total == 0 && len(req.Messages) > 0 {
		total = len(req.Messages)
	}
	return total
}

// estimateCompletionTokens estimates completion token count from response choices (chars/4).
func estimateCompletionTokens(resp *provider.CompletionResponse) int {
	if resp == nil {
		return 0
	}
	total := 0
	for _, c := range resp.Choices {
		if c.Message != nil {
			total += messageContentLen(c.Message.Content)
		}
	}
	tok := total / 4
	if tok == 0 && total > 0 {
		tok = 1
	}
	return tok
}

// messageContentLen returns the character length of message content (string or []ContentPart).
func messageContentLen(content interface{}) int {
	switch v := content.(type) {
	case string:
		return len(v)
	case []interface{}:
		n := 0
		for _, part := range v {
			if m, ok := part.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					n += len(text)
				}
			}
		}
		return n
	default:
		return 0
	}
}

// withStreamUsage asks the upstream to report token usage in the stream. Call
// it before dropParams, so a deployment that declares stream_options in
// drop_params still gets it removed.
func withStreamUsage(req *provider.CompletionRequest) {
	if req.StreamOptions == nil {
		req.StreamOptions = &provider.StreamOptions{IncludeUsage: true}
		return
	}
	req.StreamOptions.IncludeUsage = true
}

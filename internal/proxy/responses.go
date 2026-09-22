package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ResponsesHandler handles POST /v1/responses requests in OpenAI Responses API
// format. Providers with a native Responses endpoint receive the original
// protocol when required by uploaded files; all other requests are translated
// to the internal Chat Completions format so Responses clients (e.g. OpenAI
// Codex CLI/Desktop) can use any model deployment configured on the gateway.
type ResponsesHandler struct {
	registry *provider.Registry
	router   *router.Router
	logger   *slog.Logger
	pricing  *pricing.Calculator
	spender  *spend.BatchWriter
	tokens   tokenRecorder
	store    store.Store
	// ttl is how long a persisted turn stays retrievable. Zero means the
	// package default; see SetResponseTTL.
	ttl time.Duration
	// backgroundTimeout bounds how long a detached worker may run. Zero means
	// the package default; see SetBackgroundTimeout.
	backgroundTimeout time.Duration
}

// defaultResponseTTL matches OpenAI's own retention for stored responses.
const defaultResponseTTL = 30 * 24 * time.Hour

// defaultBackgroundTimeout bounds a detached turn. It matches the sweep's own
// cutoff, so a worker is stopped rather than merely declared abandoned.
const defaultBackgroundTimeout = time.Hour

// SetBackgroundTimeout overrides how long a detached worker may run.
func (h *ResponsesHandler) SetBackgroundTimeout(d time.Duration) {
	h.backgroundTimeout = d
}

// backgroundDeadline returns the timeout a detached worker runs under.
//
// Without one the worker had no deadline at all: the stale-turn sweep marked
// its row failed but could not stop the goroutine, so it stayed parked on an
// upstream connection and, if the provider eventually answered, wrote its
// result over the terminal state the client had already been shown.
func (h *ResponsesHandler) backgroundDeadline() time.Duration {
	if h.backgroundTimeout > 0 {
		return h.backgroundTimeout
	}
	return defaultBackgroundTimeout
}

// SetResponseTTL overrides how long stored responses are kept. A non-positive
// duration restores the default; responses are never stored indefinitely,
// because the gateway holds the full payload of every turn.
func (h *ResponsesHandler) SetResponseTTL(ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultResponseTTL
	}
	h.ttl = ttl
}

// responseExpiry returns the expiry stamp for a turn persisted now.
func (h *ResponsesHandler) responseExpiry() *time.Time {
	ttl := h.ttl
	if ttl <= 0 {
		ttl = defaultResponseTTL
	}
	expiry := time.Now().Add(ttl)
	return &expiry
}

// NewResponsesHandler creates a new Responses API handler.
func NewResponsesHandler(registry *provider.Registry, rt *router.Router, logger *slog.Logger, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, tokenRecorders ...tokenRecorder) *ResponsesHandler {
	var tokens tokenRecorder
	if len(tokenRecorders) > 0 {
		tokens = tokenRecorders[0]
	}
	return &ResponsesHandler{
		registry: registry,
		router:   rt,
		logger:   logger,
		pricing:  pc,
		spender:  spender,
		tokens:   tokens,
		store:    db,
	}
}

// --- Handler ---

func (h *ResponsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The raw body is kept intact: providers that speak Responses natively are
	// handed the client's own JSON, so parameters this gateway does not model
	// (reasoning, text.format, include, background, built-in tools, ...) reach
	// them untouched instead of being dropped by a round-trip through our
	// structs.
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeDecodeError(w, err)
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	// Enforce per-key model access control
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

	// Conversations are expanded before anything else looks at the request: the
	// native passthrough forwards rawBody, so the expansion has to be part of
	// it rather than something applied later on the translated path only.
	if err := h.applyConversation(r, &req, &rawBody); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	nativeInput, fileDep, useNative, err := resolveNativeResponseFiles(
		r,
		h.store,
		h.registry,
		req.Model,
		req.Input,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	switch {
	case useNative:
		// Uploaded files on a provider that speaks Responses natively: pinned
		// to the deployment owning the file ids, so no failover across accounts.
		req.Input = nativeInput
		h.handleNativeResponse(w, r, &req, rawBody, nativeInput, fileDep)
		return
	case fileDep != nil:
		// Uploaded files on a translated provider: fall through so
		// resolveFileReferences can inline the contents.
	case h.modelSpeaksResponses(req.Model):
		h.handleNativeResponse(w, r, &req, rawBody, nil, nil)
		return
	}

	// Providers reached through the translation have no server-side state, so
	// the gateway replays the referenced turn into the input itself.
	if err := h.resolvePreviousResponse(r.Context(), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	openaiReq, err := h.responsesToOpenAIRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	openaiReq.Model = req.Model
	if err := resolveFileReferences(r, h.store, h.registry, openaiReq); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	injectContextPackInstructions(r.Context(), openaiReq)

	background := req.Background != nil && *req.Background

	// On this path the gateway runs the detached turn itself, so its stored
	// record is the only thing the client can come back to. Native providers
	// keep their own copy and are left to decide for themselves.
	if background && !shouldStoreResponse(&req) {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"background responses require store to be true on this model: the turn is only retrievable from the record the gateway keeps")
		return
	}

	switch {
	case background && req.Stream:
		// Detached *and* streamed: the turn is recorded as it runs and this
		// request follows that recording, so a dropped connection can be
		// resumed instead of losing the turn.
		h.startBackgroundStream(w, r, &req, openaiReq)
	case background:
		h.startBackgroundResponse(w, r, &req, openaiReq)
	case req.Stream:
		h.handleStream(w, r, &req, openaiReq)
	default:
		h.handleComplete(w, r, &req, openaiReq)
	}
}

func (h *ResponsesHandler) handleComplete(w http.ResponseWriter, r *http.Request, rReq *responsesRequest, req *provider.CompletionRequest) {
	start := time.Now()

	if _, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model); err != nil {
		h.logger.Warn("model not found", "model", req.Model, "handler", "responses.complete")
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	resp, dep, err := h.completeResponses(r.Context(), req)
	if err != nil {
		handleUpstreamErr(w, h.logger, err)
		return
	}
	if dep != nil {
		w.Header().Set(headerUbiquumProvider, dep.ProviderName)
		h.logSpend(r, req.Model, dep, resp, time.Since(start))
	}

	respObj := h.openAIToResponses(resp, rReq)
	h.persistResponse(r.Context(), rReq, respObj, dep, "")

	writeJSON(w, http.StatusOK, respObj)
}

// completeResponses runs one non-streaming turn through the same routing rules
// as the streaming path: pinned deployment when provider files are involved,
// router with failover otherwise.
func (h *ResponsesHandler) completeResponses(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, *provider.Deployment, error) {
	call := func(dep *provider.Deployment) (*provider.CompletionResponse, error) {
		upstreamReq := *req
		upstreamReq.Model = dep.ProviderModel
		upstreamReq.Stream = false
		dropParams(&upstreamReq, dep.DropParams)
		return dep.Provider.Complete(ctx, &upstreamReq)
	}

	if req.PinnedDeploymentID != "" {
		dep, err := getAuthorizedDeploymentByID(ctx, h.registry, req.Model, req.PinnedDeploymentID)
		if err != nil {
			return nil, nil, err
		}
		resp, err := call(dep)
		return resp, dep, err
	}

	if h.router != nil {
		var resp *provider.CompletionResponse
		result, err := h.router.RouteEligible(ctx, req.Model, deploymentEligible(ctx), func(dep *provider.Deployment) error {
			var completeErr error
			resp, completeErr = call(dep)
			return completeErr
		})
		if err != nil {
			return nil, nil, err
		}
		return resp, result.Deployment, nil
	}

	dep, err := getAuthorizedDeployment(ctx, h.registry, req.Model)
	if err != nil {
		return nil, nil, fmt.Errorf("model %q is not available", req.Model)
	}
	resp, err := call(dep)
	return resp, dep, err
}

func (h *ResponsesHandler) handleStream(w http.ResponseWriter, r *http.Request, rReq *responsesRequest, req *provider.CompletionRequest) {
	start := time.Now()
	req.Stream = true

	if _, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model); err != nil {
		h.logger.Warn("model not found", "model", req.Model, "handler", "responses.stream")
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	// Write SSE headers immediately so the client knows the connection is
	// alive while we wait for the upstream provider.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	dep, streamReader, err := h.openResponsesStream(r.Context(), req)
	if err != nil {
		h.writeSSEError(w, rReq, err)
		return
	}

	h.relayResponsesStream(w, r, rReq, dep, req, streamReader, start)
}

func (h *ResponsesHandler) openResponsesStream(ctx context.Context, req *provider.CompletionRequest) (*provider.Deployment, provider.StreamReader, error) {
	if req.PinnedDeploymentID != "" {
		dep, err := getAuthorizedDeploymentByID(ctx, h.registry, req.Model, req.PinnedDeploymentID)
		if err != nil {
			return nil, nil, err
		}
		reader, err := openDeploymentStream(ctx, dep, req)
		return dep, reader, err
	}

	if h.router != nil {
		var reader provider.StreamReader
		result, err := h.router.RouteEligible(ctx, req.Model, deploymentEligible(ctx), func(dep *provider.Deployment) error {
			var streamErr error
			reader, streamErr = openDeploymentStream(ctx, dep, req)
			return streamErr
		})
		if err != nil {
			return nil, nil, err
		}
		return result.Deployment, reader, nil
	}

	dep, err := getAuthorizedDeployment(ctx, h.registry, req.Model)
	if err != nil {
		return nil, nil, fmt.Errorf("model %q is not available", req.Model)
	}
	reader, err := openDeploymentStream(ctx, dep, req)
	return dep, reader, err
}

func openDeploymentStream(ctx context.Context, dep *provider.Deployment, req *provider.CompletionRequest) (provider.StreamReader, error) {
	upstreamReq := *req
	upstreamReq.Model = dep.ProviderModel
	upstreamReq.Stream = true
	dropParams(&upstreamReq, dep.DropParams)
	return dep.Provider.Stream(ctx, &upstreamReq)
}

// --- Spend logging ---

func (h *ResponsesHandler) logSpend(r *http.Request, model string, dep *provider.Deployment, resp *provider.CompletionResponse, duration time.Duration) {
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
		record.PromptTokens = resp.Usage.PromptTokens
		record.CompletionTokens = resp.Usage.CompletionTokens
		record.TotalTokens = resp.Usage.TotalTokens
		record.CachedPromptTokens = resp.Usage.CachedTokens()
		record.CacheCreationTokens = resp.Usage.CacheCreation()
		if d := resp.Usage.CompletionTokensDetails; d != nil {
			record.ReasoningTokens = d.ReasoningTokens
		}
		if h.pricing != nil {
			record.Cost = computeCost(dep, h.pricing, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
			h.tokens.RecordCacheTokens(resp.Usage.CachedTokens(), resp.Usage.CacheCreation())
		}
	}

	h.recordSpend(r, record)
}

func (h *ResponsesHandler) logStreamSpend(r *http.Request, model string, dep *provider.Deployment, req *provider.CompletionRequest, usage *provider.Usage, completionChars int, duration time.Duration) {
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
		record.PromptTokens = usage.PromptTokens
		record.CompletionTokens = usage.CompletionTokens
		record.TotalTokens = usage.TotalTokens
		if h.pricing != nil {
			record.Cost = computeCost(dep, h.pricing, usage.PromptTokens, usage.CompletionTokens)
		}
		if h.tokens != nil {
			h.tokens.RecordTokens(usage.PromptTokens, usage.CompletionTokens)
			h.tokens.RecordCacheTokens(usage.CachedTokens(), usage.CacheCreation())
		}
	} else {
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

func (h *ResponsesHandler) recordSpend(r *http.Request, record store.SpendRecord) {
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

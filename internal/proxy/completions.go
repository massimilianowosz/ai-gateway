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

// CompletionsHandler handles POST /v1/completions (legacy text completions).
// Converts legacy format to chat completions internally.
type CompletionsHandler struct {
	registry *provider.Registry
	router   *router.Router
	logger   *slog.Logger
	pricing  *pricing.Calculator
	spender  *spend.BatchWriter
	tokens   tokenRecorder
}

// NewCompletionsHandler creates a handler for legacy completions.
func NewCompletionsHandler(registry *provider.Registry, rt *router.Router, logger *slog.Logger, pc *pricing.Calculator, spender *spend.BatchWriter, tokenRecorders ...tokenRecorder) *CompletionsHandler {
	var tokens tokenRecorder
	if len(tokenRecorders) > 0 {
		tokens = tokenRecorders[0]
	}
	return &CompletionsHandler{
		registry: registry,
		router:   rt,
		logger:   logger,
		pricing:  pc,
		spender:  spender,
		tokens:   tokens,
	}
}

// Legacy completions request
type completionsRequest struct {
	Model            string      `json:"model"`
	Prompt           interface{} `json:"prompt"` // string or []string
	MaxTokens        *int        `json:"max_tokens,omitempty"`
	Temperature      *float64    `json:"temperature,omitempty"`
	TopP             *float64    `json:"top_p,omitempty"`
	N                *int        `json:"n,omitempty"`
	Stream           bool        `json:"stream,omitempty"`
	Stop             interface{} `json:"stop,omitempty"`
	PresencePenalty  *float64    `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64    `json:"frequency_penalty,omitempty"`
	User             string      `json:"user,omitempty"`
	Suffix           string      `json:"suffix,omitempty"`
}

// Legacy completions response
type completionsResponse struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []completionsChoice `json:"choices"`
	Usage   *provider.Usage     `json:"usage,omitempty"`
}

type completionsChoice struct {
	Text         string  `json:"text"`
	Index        int     `json:"index"`
	FinishReason *string `json:"finish_reason"`
}

func (h *CompletionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req completionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
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

	// Convert prompt to chat messages
	prompt := extractPromptText(req.Prompt)
	chatReq := &provider.CompletionRequest{
		Model:            req.Model,
		Messages:         []provider.Message{{Role: "user", Content: prompt}},
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		N:                req.N,
		Stop:             req.Stop,
		PresencePenalty:  req.PresencePenalty,
		FrequencyPenalty: req.FrequencyPenalty,
		User:             req.User,
		Stream:           req.Stream,
	}

	injectContextPackInstructions(r.Context(), chatReq)

	if req.Stream {
		h.handleStream(w, r, &req, chatReq)
	} else {
		h.handleComplete(w, r, &req, chatReq)
	}
}

func (h *CompletionsHandler) handleComplete(w http.ResponseWriter, r *http.Request, legacyReq *completionsRequest, req *provider.CompletionRequest) {
	start := time.Now()

	if _, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model); err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	var resp *provider.CompletionResponse

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
			handleUpstreamErr(w, h.logger, err)
			return
		}
		h.logSpend(r, req.Model, result.Deployment, resp, time.Since(start))
	} else {
		dep, _ := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
		upstreamReq := *req
		upstreamReq.Model = dep.ProviderModel
		dropParams(&upstreamReq, dep.DropParams)

		var err error
		resp, err = dep.Provider.Complete(r.Context(), &upstreamReq)
		if err != nil {
			handleUpstreamErr(w, h.logger, err)
			return
		}
		h.logSpend(r, req.Model, dep, resp, time.Since(start))
	}

	// Convert chat response to legacy completions format
	legacyResp := h.chatToLegacyResponse(resp, legacyReq.Model)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(legacyResp)
}

func (h *CompletionsHandler) handleStream(w http.ResponseWriter, r *http.Request, legacyReq *completionsRequest, req *provider.CompletionRequest) {
	start := time.Now()

	if _, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model); err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	var streamReader provider.StreamReader
	var dep *provider.Deployment

	if h.router != nil {
		result, err := h.router.RouteEligible(r.Context(), req.Model, deploymentEligible(r.Context()), func(d *provider.Deployment) error {
			upstreamReq := *req
			upstreamReq.Model = d.ProviderModel
			upstreamReq.Stream = true
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
			handleUpstreamErr(w, h.logger, err)
			return
		}
		dep = result.Deployment
	} else {
		var err error
		dep, _ = getAuthorizedDeployment(r.Context(), h.registry, req.Model)
		upstreamReq := *req
		upstreamReq.Model = dep.ProviderModel
		dropParams(&upstreamReq, dep.DropParams)

		streamReader, err = dep.Provider.Stream(r.Context(), &upstreamReq)
		if err != nil {
			handleUpstreamErr(w, h.logger, err)
			return
		}
	}

	defer streamReader.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	buf := perf.GetBuffer()
	defer perf.PutBuffer(buf)

	var usage *provider.Usage

	for {
		chunk, err := streamReader.Next()
		if err == io.EOF {
			buf.Reset()
			buf.WriteString("data: [DONE]\n\n")
			_, _ = w.Write(buf.Bytes())
			flusher.Flush()
			h.logStreamSpend(r, req.Model, dep, usage, time.Since(start))
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

		// Convert chat chunk to legacy format
		legacyChunk := convertChatChunkToLegacy(chunk, legacyReq.Model)
		buf.Reset()
		buf.WriteString("data: ")
		buf.Write(legacyChunk)
		buf.WriteString("\n\n")
		_, _ = w.Write(buf.Bytes())
		flusher.Flush()
	}
}

func (h *CompletionsHandler) chatToLegacyResponse(resp *provider.CompletionResponse, model string) *completionsResponse {
	legacy := &completionsResponse{
		ID:      resp.ID,
		Object:  "text_completion",
		Created: resp.Created,
		Model:   model,
		Usage:   resp.Usage,
	}

	for _, choice := range resp.Choices {
		text := ""
		if choice.Message != nil {
			if s, ok := choice.Message.Content.(string); ok {
				text = s
			}
		}
		legacy.Choices = append(legacy.Choices, completionsChoice{
			Text:         text,
			Index:        choice.Index,
			FinishReason: choice.FinishReason,
		})
	}

	return legacy
}

func convertChatChunkToLegacy(chunk []byte, model string) []byte {
	var chatChunk struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Index        int                      `json:"index"`
			Delta        struct{ Content string } `json:"delta"`
			FinishReason *string                  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunk, &chatChunk); err != nil {
		return chunk // pass through
	}

	legacy := map[string]interface{}{
		"id":      chatChunk.ID,
		"object":  "text_completion",
		"created": chatChunk.Created,
		"model":   model,
	}

	var choices []completionsChoice
	for _, c := range chatChunk.Choices {
		choices = append(choices, completionsChoice{
			Text:         c.Delta.Content,
			Index:        c.Index,
			FinishReason: c.FinishReason,
		})
	}
	legacy["choices"] = choices

	out, _ := json.Marshal(legacy)
	return out
}

func extractPromptText(prompt interface{}) string {
	switch v := prompt.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, p := range v {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprintf("%v", prompt)
}

func handleUpstreamErr(w http.ResponseWriter, logger *slog.Logger, err error) {
	logger.Error("upstream error", "error", err)
	if strings.Contains(err.Error(), "not found") {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	// errors.As, not a type assertion: router.Route wraps the deployment error
	// via fmt.Errorf("all %d attempts failed ...: %w", ...), so the
	// *provider.UpstreamError sits one level deep and a bare assertion would
	// miss it — collapsing every routed upstream failure to a generic 502.
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		status := ue.StatusCode
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		writeError(w, status, "upstream_error", ue.Message)
		return
	}
	writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

func (h *CompletionsHandler) logSpend(r *http.Request, model string, dep *provider.Deployment, resp *provider.CompletionResponse, duration time.Duration) {
	if h.spender == nil || resp == nil {
		return
	}
	record := store.SpendRecord{
		Model: model, Provider: dep.ProviderName, Duration: duration.Milliseconds(), Status: 200, IsEU: dep.IsEU, AuthMode: dep.AuthMode, BillingMode: dep.BillingMode,
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
		}
		// Fill UsageCapture for upstream middleware (HiveState)
		if uc := store.GetUsageCapture(r.Context()); uc != nil {
			uc.PromptTokens = resp.Usage.PromptTokens
			uc.CompletionTokens = resp.Usage.CompletionTokens
			uc.TotalTokens = resp.Usage.TotalTokens
			uc.Cost = record.Cost
			uc.Filled = true
		}
	}
	h.spender.Record(record)
}

func (h *CompletionsHandler) logStreamSpend(r *http.Request, model string, dep *provider.Deployment, usage *provider.Usage, duration time.Duration) {
	if h.spender == nil {
		return
	}
	record := store.SpendRecord{
		Model: model, Provider: dep.ProviderName, Duration: duration.Milliseconds(), Status: 200, IsEU: dep.IsEU, AuthMode: dep.AuthMode, BillingMode: dep.BillingMode,
	}
	if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo != nil {
		record.ApplyIdentity(keyInfo)
	}
	if usage != nil {
		applyUsageTokens(&record, dep, usage)
		if h.pricing != nil {
			record.Cost = computeCostUsage(dep, h.pricing, usage)
		}
		// Fill UsageCapture for upstream middleware (HiveState). The cache
		// split travels with it: without it HiveState's own metrics stay blind
		// to exactly the prompt-cache behaviour its guard is deciding on.
		if uc := store.GetUsageCapture(r.Context()); uc != nil {
			uc.PromptTokens = usage.PromptTokens
			uc.CompletionTokens = usage.CompletionTokens
			uc.TotalTokens = usage.TotalTokens
			uc.CachedPromptTokens = usage.CachedTokens()
			uc.CacheCreationTokens = usage.CacheCreation()
			uc.Cost = record.Cost
			uc.Filled = true
		}
	}
	h.spender.Record(record)
}

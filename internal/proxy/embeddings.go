package proxy

import (
	"encoding/json"
	"fmt"
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

// EmbeddingsHandler handles POST /v1/embeddings requests.
type EmbeddingsHandler struct {
	registry *provider.Registry
	router   *router.Router
	logger   *slog.Logger
	pricing  *pricing.Calculator
	spender  *spend.BatchWriter
}

// NewEmbeddingsHandler creates a new embeddings handler.
func NewEmbeddingsHandler(registry *provider.Registry, rt *router.Router, logger *slog.Logger, pc *pricing.Calculator, spender *spend.BatchWriter) *EmbeddingsHandler {
	return &EmbeddingsHandler{
		registry: registry,
		router:   rt,
		logger:   logger,
		pricing:  pc,
		spender:  spender,
	}
}

// EmbeddingRequest represents an OpenAI embeddings request.
type EmbeddingRequest struct {
	Model          string      `json:"model"`
	Input          interface{} `json:"input"` // string or []string
	EncodingFormat string      `json:"encoding_format,omitempty"`
	Dimensions     *int        `json:"dimensions,omitempty"`
	User           string      `json:"user,omitempty"`
}

func (h *EmbeddingsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if req.Input == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
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

	// Route to provider
	dep, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	start := time.Now()

	// Forward as OpenAI-compatible embedding request to the provider
	embReq := &provider.EmbeddingRequest{
		Model:          dep.ProviderModel,
		Input:          req.Input,
		EncodingFormat: req.EncodingFormat,
		Dimensions:     req.Dimensions,
		User:           req.User,
	}

	embedder, ok := dep.Provider.(provider.Embedder)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q does not support embeddings", req.Model))
		return
	}

	resp, err := embedder.Embed(r.Context(), embReq)
	if err != nil {
		h.logger.Error("embeddings upstream error", "error", err)
		if ue, ok := err.(*provider.UpstreamError); ok {
			status := ue.StatusCode
			if status < 400 || status >= 600 {
				status = http.StatusBadGateway
			}
			writeError(w, status, "upstream_error", ue.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	// Log spend
	if h.spender != nil {
		record := store.SpendRecord{
			Model:        req.Model,
			Provider:     dep.ProviderName,
			Duration:     time.Since(start).Milliseconds(),
			Status:       200,
			IsEU:         dep.IsEU,
			AuthMode:     dep.AuthMode,
			BillingMode:  dep.BillingMode,
			PromptTokens: resp.Usage.PromptTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
		if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo != nil {
			record.ApplyIdentity(keyInfo)
		}
		if h.pricing != nil {
			record.Cost = computeCost(dep, h.pricing, resp.Usage.PromptTokens, 0)
		}
		h.spender.Record(record)
	}

	// Return response with user-facing model name
	resp.Model = req.Model
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

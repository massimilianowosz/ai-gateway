package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// ModerationsHandler handles POST /v1/moderations requests.
type ModerationsHandler struct {
	registry *provider.Registry
	logger   *slog.Logger
}

// NewModerationsHandler creates a new moderations handler.
func NewModerationsHandler(registry *provider.Registry, logger *slog.Logger) *ModerationsHandler {
	return &ModerationsHandler{registry: registry, logger: logger}
}

// ModerationRequest represents a moderation request.
type ModerationRequest struct {
	Model string      `json:"model,omitempty"`
	Input interface{} `json:"input"` // string or []string
}

func (h *ModerationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req ModerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Input == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	// Default model
	if req.Model == "" {
		req.Model = "text-moderation-latest"
	}

	// Enforce model access control — deny_models/deny_providers/residency,
	// not just the allow-list the previous hand-rolled check here missed
	// entirely (GW-02).
	if !checkModelAccess(w, r, h.registry, req.Model) {
		return
	}

	// A key that cannot pay — never funded, or funded and spent — may only
	// reach a model the gateway does not pay for. Checked here, where the model
	// is known: authentication cannot see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(req.Model)); status != auth.BudgetOK {
		writeBudgetError(w, status, req.Model)
		return
	}

	dep, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	moderator, ok := dep.Provider.(provider.Moderator)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q does not support moderations", req.Model))
		return
	}

	resp, err := moderator.Moderate(r.Context(), &provider.ModerationRequest{
		Model: dep.ProviderModel,
		Input: req.Input,
	})
	if err != nil {
		h.logger.Error("moderations upstream error", "error", err)
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

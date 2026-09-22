package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ModelsHandler handles GET /v1/models requests.
type ModelsHandler struct {
	registry *provider.Registry
	store    store.Store
}

// NewModelsHandler creates a new models handler.
func NewModelsHandler(registry *provider.Registry, db store.Store) *ModelsHandler {
	return &ModelsHandler{registry: registry, store: db}
}

func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var models []string

	if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo != nil && keyInfo.KeyHash == "master" {
		// Master key: check if ?team_id= is set to view as that team
		if teamID := r.URL.Query().Get("team_id"); teamID != "" && h.store != nil {
			if team, err := h.store.GetTeam(r.Context(), teamID); err == nil && team != nil {
				allowed := []string(team.AllowedModels)
				// The tenant admin UI has to render the models the whitelist
				// turns off, or a model could be disabled and never switched
				// back on. Restricted models stay gated by granted_models.
				if r.URL.Query().Get("include_disabled") == "true" {
					allowed = nil
				}
				models = h.registry.ListVisibleModels(allowed, team.GrantedModels)
			} else {
				models = h.registry.ListModels()
			}
		} else {
			models = h.registry.ListModels()
		}
	} else {
		// Determine visible models based on tenant's allowed/granted models
		var allowedModels, grantedModels []string
		if team := auth.TeamFromContext(r.Context()); team != nil {
			allowedModels = team.AllowedModels
			grantedModels = team.GrantedModels
		}
		models = h.registry.ListVisibleModels(allowedModels, grantedModels)
	}

	// If anthropic-version header is present, respond in Anthropic format
	if r.Header.Get("anthropic-version") != "" {
		h.serveAnthropic(w, models)
		return
	}

	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	data := make([]modelObject, len(models))
	now := time.Now().Unix()
	for i, m := range models {
		data[i] = modelObject{
			ID:      m,
			Object:  "model",
			Created: now,
			OwnedBy: "ubiquum-ai-gateway",
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

// ModelAliasesHandler serves the public alias → configured model mapping.
type ModelAliasesHandler struct {
	registry *provider.Registry
}

// NewModelAliasesHandler creates a handler for GET /v1/model/aliases.
func NewModelAliasesHandler(registry *provider.Registry) *ModelAliasesHandler {
	return &ModelAliasesHandler{registry: registry}
}

// ServeHTTP exposes the mapping to the operator only. The portal needs it to
// tell whether a stored whitelist entry and a published model id are the same
// model: whitelists written before an alias existed hold the configured name,
// so comparing them by string alone reports every one of them as absent.
func (h *ModelAliasesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if keyInfo := auth.KeyInfoFromContext(r.Context()); keyInfo == nil || keyInfo.KeyHash != "master" {
		writeError(w, http.StatusForbidden, "access_denied", "master key required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"aliases": h.registry.Aliases()})
}

func (h *ModelsHandler) serveAnthropic(w http.ResponseWriter, models []string) {
	type anthropicModel struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	}

	data := make([]anthropicModel, len(models))
	for i, m := range models {
		data[i] = anthropicModel{
			Type:        "model",
			ID:          m,
			DisplayName: m,
			CreatedAt:   "2024-01-01T00:00:00Z",
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data":     data,
		"has_more": false,
		"first_id": func() string {
			if len(data) > 0 {
				return data[0].ID
			}
			return ""
		}(),
		"last_id": func() string {
			if len(data) > 0 {
				return data[len(data)-1].ID
			}
			return ""
		}(),
	})
}

// ModelDetailHandler handles GET /v1/models/{model} requests.
type ModelDetailHandler struct {
	registry *provider.Registry
}

// NewModelDetailHandler creates a handler for model detail lookups.
func NewModelDetailHandler(registry *provider.Registry) *ModelDetailHandler {
	return &ModelDetailHandler{registry: registry}
}

func (h *ModelDetailHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("model")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model id is required")
		return
	}

	_, err := h.registry.GetDeployment(modelID)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q does not exist", modelID))
		return
	}
	// A restricted model the caller cannot use must not be confirmed to exist:
	// the listing already hides it, and answering here would give the same
	// information away one request later.
	if !auth.IsModelAllowed(r.Context(), modelID, h.registry.IsRestricted(modelID)) {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q does not exist", modelID))
		return
	}
	if !auth.IsProviderAllowed(r.Context(), h.registry.ProvidersFor(modelID)) {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q does not exist", modelID))
		return
	}
	if allEU, hasDeployments := h.registry.AllDeploymentsEU(modelID); !auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments) {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q does not exist", modelID))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       modelID,
		"object":   "model",
		"created":  time.Now().Unix(),
		"owned_by": "ubiquum-ai-gateway",
	})
}

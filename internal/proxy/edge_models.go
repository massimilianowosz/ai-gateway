package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/openai"
)

// EdgeModelsHandler handles runtime registration of edge models.
// POST /model/new  — register an edge-served model
// POST /model/delete — remove an edge-served model
type EdgeModelsHandler struct {
	registry  *provider.Registry
	masterKey string
}

func NewEdgeModelsHandler(registry *provider.Registry, masterKey string) *EdgeModelsHandler {
	return &EdgeModelsHandler{registry: registry, masterKey: masterKey}
}

type addModelRequest struct {
	ModelName     string                 `json:"model_name"`
	LiteLLMParams map[string]interface{} `json:"litellm_params"`
}

type deleteModelRequest struct {
	// Accept either "id" (LiteLLM compat) or "model_name"
	ID        string `json:"id"`
	ModelName string `json:"model_name"`
}

func (h *EdgeModelsHandler) ServeAdd(w http.ResponseWriter, r *http.Request) {
	if !h.isMasterKey(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "master key required")
		return
	}

	var req addModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if req.ModelName == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "model_name is required")
		return
	}

	// Extract api_base and api_key from litellm_params (LiteLLM-compatible format).
	params := req.LiteLLMParams
	apiBase, _ := params["api_base"].(string)
	apiKey, _ := params["api_key"].(string)
	if apiBase == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "litellm_params.api_base is required")
		return
	}
	if apiKey == "" {
		apiKey = "unused"
	}

	// Derive provider_model: strip "edge/" prefix from model_name, or use explicit field.
	providerModel := strings.TrimPrefix(req.ModelName, "edge/")

	// Edge model names are global (edge/qwen) while the runtimes behind them
	// belong to one tenant each, so the backend cannot resolve a request
	// without knowing who made it.
	p := openai.NewCompatible(apiBase, apiKey, openai.WithTenantForwarding())
	dep := &provider.Deployment{
		ID:            fmt.Sprintf("edge-%s", req.ModelName),
		ModelName:     req.ModelName,
		ProviderName:  "edge",
		ProviderModel: providerModel,
		Provider:      p,
	}
	h.registry.AddDeployment(req.ModelName, dep)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"model_name": req.ModelName,
		"status":     "registered",
	})
}

func (h *EdgeModelsHandler) ServeDelete(w http.ResponseWriter, r *http.Request) {
	if !h.isMasterKey(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "master key required")
		return
	}

	var req deleteModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	name := req.ModelName
	if name == "" {
		name = req.ID
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "model_name or id is required")
		return
	}

	h.registry.RemoveDeployments(name)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"model_name": name,
		"status":     "deleted",
	})
}

func (h *EdgeModelsHandler) isMasterKey(r *http.Request) bool {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return h.masterKey != "" && auth == h.masterKey
}

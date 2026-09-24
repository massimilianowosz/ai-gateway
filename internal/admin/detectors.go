package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

type detectorSettingsStore interface {
	ListDetectorSettings(ctx context.Context) ([]store.DetectorSetting, error)
	SetDetectorEnabled(ctx context.Context, detector string, enabled bool) error
}

type detectorState struct {
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
	Default bool   `json:"default"`
}

// GetDetectors lists the standard PII types with whether traffic analysis
// reports each one.
func (h *Handler) GetDetectors(w http.ResponseWriter, r *http.Request) {
	overrides := map[string]bool{}
	if ss, ok := h.store.(detectorSettingsStore); ok {
		rows, err := ss.ListDetectorSettings(r.Context())
		if err != nil {
			h.logger.Error("admin: list detector settings failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load detector settings"})
			return
		}
		for _, row := range rows {
			overrides[row.Type] = row.Enabled
		}
	}
	types := guardrail.PIITypes()
	out := make([]detectorState, 0, len(types))
	for _, t := range types {
		def := guardrail.PIIReportedByDefault(t)
		on, set := overrides[t]
		if !set {
			on = def
		}
		out = append(out, detectorState{Type: t, Kind: "pii", Enabled: on, Default: def})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type detectorUpdateRequest struct {
	Type    string `json:"type"`
	Enabled *bool  `json:"enabled"`
}

func (h *Handler) UpdateDetector(w http.ResponseWriter, r *http.Request) {
	ss, ok := h.store.(detectorSettingsStore)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "detector settings storage unavailable"})
		return
	}
	var req detectorUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type and enabled are required"})
		return
	}
	req.Type = strings.ToUpper(strings.TrimSpace(req.Type))
	if !slices.Contains(guardrail.PIITypes(), req.Type) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown detector type"})
		return
	}
	if err := ss.SetDetectorEnabled(r.Context(), req.Type, *req.Enabled); err != nil {
		h.logger.Error("admin: update detector setting failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update detector"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": req.Type, "enabled": *req.Enabled})
}

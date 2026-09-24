package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// watchlistStore is asserted rather than added to store.Store: the watchlist
// is an appliance feature, and a gateway pointed at a portal-owned database
// that has no such table should keep working without it.
type watchlistStore interface {
	ListWatchlistTerms(ctx context.Context) ([]store.WatchlistTerm, error)
	CreateWatchlistTerm(ctx context.Context, term *store.WatchlistTerm) error
	SetWatchlistTermEnabled(ctx context.Context, id uint, enabled bool) error
	DeleteWatchlistTerm(ctx context.Context, id uint) error
}

func (h *Handler) watchlist() (watchlistStore, bool) {
	ws, ok := h.store.(watchlistStore)
	return ws, ok
}

type watchlistTermRequest struct {
	Label   string `json:"label"`
	Pattern string `json:"pattern"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func (h *Handler) GetWatchlist(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.watchlist()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	terms, err := ws.ListWatchlistTerms(r.Context())
	if err != nil {
		h.logger.Error("admin: list watchlist failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load watchlist"})
		return
	}
	if terms == nil {
		terms = []store.WatchlistTerm{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": terms})
}

func (h *Handler) CreateWatchlistTerm(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.watchlist()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "watchlist storage unavailable"})
		return
	}
	var req watchlistTermRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	req.Pattern = strings.TrimSpace(req.Pattern)
	if req.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "label is required"})
		return
	}
	// A pattern that cannot compile would be stored and never match, and the
	// operator would believe they were covered.
	if !guardrail.ValidWatchPattern(req.Pattern) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "pattern is empty or unusable"})
		return
	}
	// A bare "*" matches every token in every prompt, which is not a watchlist
	// but an outage of the findings panel.
	if strings.Trim(req.Pattern, "*") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "pattern must contain something to match"})
		return
	}

	term := store.WatchlistTerm{Label: req.Label, Pattern: req.Pattern, Enabled: true}
	if req.Enabled != nil {
		term.Enabled = *req.Enabled
	}
	if err := ws.CreateWatchlistTerm(r.Context(), &term); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "that pattern is already on the watchlist"})
			return
		}
		h.logger.Error("admin: create watchlist term failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save term"})
		return
	}
	writeJSON(w, http.StatusOK, term)
}

// UpdateWatchlistTerm toggles a term. Only the enabled flag is mutable: an
// edited pattern is a different rule, and keeping the old one's history under
// the new text would misdescribe every finding already recorded.
func (h *Handler) UpdateWatchlistTerm(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.watchlist()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "watchlist storage unavailable"})
		return
	}
	id, err := watchlistID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req watchlistTermRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enabled is required"})
		return
	}
	if err := ws.SetWatchlistTermEnabled(r.Context(), id, *req.Enabled); err != nil {
		h.logger.Error("admin: update watchlist term failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update term"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": *req.Enabled})
}

func (h *Handler) DeleteWatchlistTerm(w http.ResponseWriter, r *http.Request) {
	ws, ok := h.watchlist()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "watchlist storage unavailable"})
		return
	}
	id, err := watchlistID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if err := ws.DeleteWatchlistTerm(r.Context(), id); err != nil {
		h.logger.Error("admin: delete watchlist term failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to delete term"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func watchlistID(r *http.Request) (uint, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("id"))
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(n), nil
}

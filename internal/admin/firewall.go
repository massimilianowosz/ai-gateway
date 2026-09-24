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

// firewallStore is asserted rather than added to store.Store: the firewall is
// an appliance feature, and a gateway pointed at a portal-owned database that
// has no such table should keep working without it — the same reasoning as
// watchlistStore.
type firewallStore interface {
	ListFirewallRules(ctx context.Context) ([]store.FirewallRule, error)
	CreateFirewallRule(ctx context.Context, rule *store.FirewallRule) error
	SetFirewallRuleEnabled(ctx context.Context, id uint, enabled bool) error
	DeleteFirewallRule(ctx context.Context, id uint) error
}

func (h *Handler) firewall() (firewallStore, bool) {
	fs, ok := h.store.(firewallStore)
	return fs, ok
}

type firewallRuleRequest struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Pattern string `json:"pattern"`
	Enabled *bool  `json:"enabled,omitempty"`
}

var validFirewallKinds = map[string]bool{
	store.FirewallKindMCPServer: true,
	store.FirewallKindTool:      true,
	store.FirewallKindFileRead:  true,
	store.FirewallKindFileWrite: true,
}

func (h *Handler) GetFirewallRules(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.firewall()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	rules, err := fs.ListFirewallRules(r.Context())
	if err != nil {
		h.logger.Error("admin: list firewall rules failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load firewall rules"})
		return
	}
	if rules == nil {
		rules = []store.FirewallRule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rules})
}

func (h *Handler) CreateFirewallRule(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.firewall()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "firewall storage unavailable"})
		return
	}
	var req firewallRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.Label = strings.TrimSpace(req.Label)
	req.Pattern = strings.TrimSpace(req.Pattern)
	if !validFirewallKinds[req.Kind] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind must be mcp_server, tool, file_read or file_write"})
		return
	}
	if req.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "label is required"})
		return
	}
	// A pattern that cannot compile would be stored and never match, and the
	// operator would believe the call was covered.
	if !guardrail.ValidWatchPattern(req.Pattern) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "pattern is empty or unusable"})
		return
	}
	if strings.Trim(req.Pattern, "*") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "pattern must contain something to match"})
		return
	}

	rule := store.FirewallRule{Kind: req.Kind, Label: req.Label, Pattern: req.Pattern, Enabled: true}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if err := fs.CreateFirewallRule(r.Context(), &rule); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "that pattern is already a rule of this kind"})
			return
		}
		h.logger.Error("admin: create firewall rule failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save rule"})
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// UpdateFirewallRule toggles a rule. Only the enabled flag is mutable, the
// same reasoning as UpdateWatchlistTerm: an edited pattern is a different
// rule, and keeping the old one's history under the new text would
// misdescribe every finding already recorded against it.
func (h *Handler) UpdateFirewallRule(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.firewall()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "firewall storage unavailable"})
		return
	}
	id, err := firewallRuleID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	var req firewallRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enabled is required"})
		return
	}
	if err := fs.SetFirewallRuleEnabled(r.Context(), id, *req.Enabled); err != nil {
		h.logger.Error("admin: update firewall rule failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update rule"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": *req.Enabled})
}

func (h *Handler) DeleteFirewallRule(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.firewall()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "firewall storage unavailable"})
		return
	}
	id, err := firewallRuleID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}
	if err := fs.DeleteFirewallRule(r.Context(), id); err != nil {
		h.logger.Error("admin: delete firewall rule failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to delete rule"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func firewallRuleID(r *http.Request) (uint, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("id"))
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(n), nil
}

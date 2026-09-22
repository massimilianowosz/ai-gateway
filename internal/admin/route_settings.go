package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// routeSettingsRequest is the request body for updating per-team route config.
type routeSettingsRequest struct {
	TeamID                  string             `json:"team_id"`
	KeyID                   string             `json:"key_id,omitempty"` // optional: per-key override
	Enabled                 *bool              `json:"enabled,omitempty"`
	Levels                  []store.RouteLevel `json:"levels,omitempty"`
	ThinkingBudgets         map[string]int     `json:"thinking_budgets,omitempty"`
	FeedbackEnabled         *bool              `json:"feedback_enabled,omitempty"`    // nil=inherit, true/false=override
	ResetFeedback           bool               `json:"reset_feedback,omitempty"`      // true=clear feedback_enabled to nil (inherit)
	ResetEnabled            bool               `json:"reset_enabled,omitempty"`       // true=clear the hiveroute enabled override to nil (inherit)
	CacheEnabled            *bool              `json:"cache_enabled,omitempty"`       // nil=inherit, true/false=override
	ResetCacheEnabled       bool               `json:"reset_cache_enabled,omitempty"` // true=clear cache_enabled to nil (inherit)
	CompressionEnabled      *bool              `json:"compression_enabled,omitempty"`
	ResetCompressionEnabled bool               `json:"reset_compression_enabled,omitempty"`
	// Cache threshold overrides. A response_cache policy always sets or
	// clears all three together, so one reset flag covers the group rather
	// than one per field.
	CacheDirectThreshold *float64 `json:"cache_direct_threshold,omitempty"`
	CacheReuseThreshold  *float64 `json:"cache_reuse_threshold,omitempty"`
	CacheTweakThreshold  *float64 `json:"cache_tweak_threshold,omitempty"`
	ResetCacheThresholds bool     `json:"reset_cache_thresholds,omitempty"`
	// Compression overrides — same group-reset reasoning as the cache
	// thresholds above: a context_compression policy sets or clears all of
	// these together.
	CompressionThreshold      *int  `json:"compression_threshold,omitempty"`
	CompressionTokenBudget    *int  `json:"compression_token_budget,omitempty"`
	CompressionStepWindow     *int  `json:"compression_step_window,omitempty"`
	CompressionAppendOnly     *bool `json:"compression_append_only_state,omitempty"`
	ResetCompressionOverrides bool  `json:"reset_compression_overrides,omitempty"`
	// ResetAll deletes the entire per-key row, back to inheriting every
	// team default. Explicit on purpose: policy_gateway_sync's every push
	// legitimately sends enabled=nil, levels=[] whenever no model_routing
	// policy applies (its own "reset just this one field" signal, not "wipe
	// the whole row") — inferring a full reset from those same values, as
	// this used to, deleted a key's cache/compression overrides underneath
	// any policy sync that ran while no routing policy happened to be
	// enforced alongside it.
	ResetAll bool `json:"reset_all,omitempty"`
	// RequiredGuardrails/GuardrailNonDerogable set or clear together — a
	// guardrail_set policy's own rule always carries both — so one reset
	// flag covers the pair rather than one per field.
	RequiredGuardrails     []string `json:"required_guardrails,omitempty"`
	GuardrailNonDerogable  *bool    `json:"guardrail_non_derogable,omitempty"`
	ResetGuardrailOverride bool     `json:"reset_guardrail_override,omitempty"`
	// LogRetentionDays bounds how long this key's spend/audit rows are kept
	// before the retention janitor purges them. nil=keep forever (today's
	// actual behavior).
	LogRetentionDays      *int `json:"log_retention_days,omitempty"`
	ResetLogRetentionDays bool `json:"reset_log_retention_days,omitempty"`
}

// GetRouteSettings returns the HiveRoute config for a team or specific key.
func (h *Handler) GetRouteSettings(w http.ResponseWriter, r *http.Request) {
	teamID := r.URL.Query().Get("team_id")
	keyID := r.URL.Query().Get("key_id")

	if teamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "team_id is required"})
		return
	}

	// Collect global defaults from config
	var defaultLevels []config.HiveRouteLevel
	var globalEnabled bool
	var globalFeedbackEnabled bool
	if h.cfg != nil {
		defaultLevels = h.cfg.HiveState.HiveRoute.Levels
		globalEnabled = h.cfg.HiveState.HiveRoute.Enabled
		globalFeedbackEnabled = h.cfg.Feedback.Enabled
	}

	settings, err := h.store.GetTenantSettings(r.Context(), teamID)
	if err != nil {
		h.logger.Error("admin: get route settings failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to get settings"})
		return
	}

	// If key_id is specified, return per-key settings
	if keyID != "" {
		keySettings, err := h.store.GetKeyRouteSettings(r.Context(), keyID)
		if err != nil {
			h.logger.Error("admin: get key route settings failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to get key settings"})
			return
		}

		// Determine effective team-level enabled state for context
		teamEnabled := (*bool)(nil)
		var teamLevels []store.RouteLevel
		var teamFeedbackEnabled *bool
		teamCacheEnabled := true
		teamCompressionEnabled := true
		if settings != nil {
			teamEnabled = settings.HiveRouteEnabled
			teamLevels = settings.HiveRouteLevels
			teamFeedbackEnabled = settings.FeedbackEnabled
			teamCacheEnabled = settings.CacheEnabled
			teamCompressionEnabled = settings.CompressionEnabled
		}

		if keySettings == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"team_id":                       teamID,
				"key_id":                        keyID,
				"enabled":                       nil,
				"team_enabled":                  teamEnabled,
				"global_enabled":                globalEnabled,
				"levels":                        []store.RouteLevel{},
				"team_levels":                   teamLevels,
				"default_levels":                defaultLevels,
				"feedback_enabled":              nil,
				"team_feedback_enabled":         teamFeedbackEnabled,
				"global_feedback_enabled":       globalFeedbackEnabled,
				"cache_enabled":                 nil,
				"team_cache_enabled":            teamCacheEnabled,
				"compression_enabled":           nil,
				"team_compression_enabled":      teamCompressionEnabled,
				"cache_direct_threshold":        nil,
				"cache_reuse_threshold":         nil,
				"cache_tweak_threshold":         nil,
				"compression_threshold":         nil,
				"compression_token_budget":      nil,
				"compression_step_window":       nil,
				"compression_append_only_state": nil,
				"guardrail_override":            nil,
				"log_retention_days":            nil,
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"team_id":                       teamID,
			"key_id":                        keySettings.KeyID,
			"enabled":                       keySettings.Enabled,
			"team_enabled":                  teamEnabled,
			"global_enabled":                globalEnabled,
			"levels":                        keySettings.Levels,
			"team_levels":                   teamLevels,
			"thinking_budgets":              keySettings.ThinkingBudgets,
			"default_levels":                defaultLevels,
			"feedback_enabled":              keySettings.FeedbackEnabled,
			"team_feedback_enabled":         teamFeedbackEnabled,
			"global_feedback_enabled":       globalFeedbackEnabled,
			"cache_enabled":                 keySettings.CacheEnabled,
			"team_cache_enabled":            teamCacheEnabled,
			"compression_enabled":           keySettings.CompressionEnabled,
			"team_compression_enabled":      teamCompressionEnabled,
			"cache_direct_threshold":        keySettings.CacheDirectThreshold,
			"cache_reuse_threshold":         keySettings.CacheReuseThreshold,
			"cache_tweak_threshold":         keySettings.CacheTweakThreshold,
			"compression_threshold":         keySettings.CompressionThreshold,
			"compression_token_budget":      keySettings.CompressionTokenBudget,
			"compression_step_window":       keySettings.CompressionStepWindow,
			"compression_append_only_state": keySettings.CompressionAppendOnlyState,
			"guardrail_override":            keySettings.GuardrailOverride,
			"log_retention_days":            keySettings.LogRetentionDays,
		})
		return
	}

	// Team-level settings (original behavior)
	if settings == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"team_id":                 teamID,
			"enabled":                 nil,
			"global_enabled":          globalEnabled,
			"levels":                  []store.RouteLevel{},
			"default_levels":          defaultLevels,
			"feedback_enabled":        nil,
			"global_feedback_enabled": globalFeedbackEnabled,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"team_id":                 settings.TeamID,
		"enabled":                 settings.HiveRouteEnabled,
		"global_enabled":          globalEnabled,
		"levels":                  settings.HiveRouteLevels,
		"thinking_budgets":        settings.HiveRouteThinkingBudgets,
		"default_levels":          defaultLevels,
		"feedback_enabled":        settings.FeedbackEnabled,
		"global_feedback_enabled": globalFeedbackEnabled,
	})
}

// UpdateRouteSettings updates the HiveRoute config for a team or specific key.
func (h *Handler) UpdateRouteSettings(w http.ResponseWriter, r *http.Request) {
	var req routeSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.TeamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "team_id is required"})
		return
	}

	// Per-key settings
	if req.KeyID != "" {
		if governedKey, resolveErr := h.resolveKeyIdentifier(r.Context(), req.KeyID); resolveErr == nil {
			if !h.allowGovernanceBreakGlass(w, r, governedKey, "route_settings") {
				return
			}
		} else if !errors.Is(resolveErr, errKeyNotFound) {
			h.writeKeyResolutionError(w, "authorize route settings update", resolveErr)
			return
		}
		keySettings, err := h.store.GetKeyRouteSettings(r.Context(), req.KeyID)
		if err != nil {
			h.logger.Error("admin: get key route settings failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load key settings"})
			return
		}
		if keySettings == nil {
			keySettings = &store.KeyRouteSettings{
				KeyID:  req.KeyID,
				TeamID: req.TeamID,
			}
		}
		// KeyID is whatever the caller supplied, but the request path reads
		// this table by key hash. A row written under any other identifier is
		// stored and echoed back correctly and then never applies.
		if keySettings.TeamID == "" {
			keySettings.TeamID = req.TeamID
		}

		// Handle reset: an explicit signal deletes key settings entirely,
		// back to inheriting every team default.
		if req.ResetAll {
			if err := h.store.DeleteKeyRouteSettings(r.Context(), req.KeyID); err != nil {
				h.logger.Error("admin: delete key route settings failed", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to delete key settings"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"key_id":              req.KeyID,
				"team_id":             req.TeamID,
				"enabled":             nil,
				"levels":              []store.RouteLevel{},
				"feedback_enabled":    nil,
				"cache_enabled":       nil,
				"compression_enabled": nil,
			})
			return
		}

		if req.Enabled != nil {
			keySettings.Enabled = req.Enabled
		}
		if req.Levels != nil {
			keySettings.Levels = req.Levels
		}
		if req.ThinkingBudgets != nil {
			keySettings.ThinkingBudgets = req.ThinkingBudgets
		}
		if req.ResetFeedback {
			keySettings.FeedbackEnabled = nil
		} else if req.FeedbackEnabled != nil {
			keySettings.FeedbackEnabled = req.FeedbackEnabled
		}
		if req.ResetCacheEnabled {
			keySettings.CacheEnabled = nil
		} else if req.CacheEnabled != nil {
			keySettings.CacheEnabled = req.CacheEnabled
		}
		if req.ResetCompressionEnabled {
			keySettings.CompressionEnabled = nil
		} else if req.CompressionEnabled != nil {
			keySettings.CompressionEnabled = req.CompressionEnabled
		}
		if req.ResetCacheThresholds {
			keySettings.CacheDirectThreshold = nil
			keySettings.CacheReuseThreshold = nil
			keySettings.CacheTweakThreshold = nil
		} else {
			if req.CacheDirectThreshold != nil {
				keySettings.CacheDirectThreshold = req.CacheDirectThreshold
			}
			if req.CacheReuseThreshold != nil {
				keySettings.CacheReuseThreshold = req.CacheReuseThreshold
			}
			if req.CacheTweakThreshold != nil {
				keySettings.CacheTweakThreshold = req.CacheTweakThreshold
			}
		}
		if req.ResetCompressionOverrides {
			keySettings.CompressionThreshold = nil
			keySettings.CompressionTokenBudget = nil
			keySettings.CompressionStepWindow = nil
			keySettings.CompressionAppendOnlyState = nil
		} else {
			if req.CompressionThreshold != nil {
				keySettings.CompressionThreshold = req.CompressionThreshold
			}
			if req.CompressionTokenBudget != nil {
				keySettings.CompressionTokenBudget = req.CompressionTokenBudget
			}
			if req.CompressionStepWindow != nil {
				keySettings.CompressionStepWindow = req.CompressionStepWindow
			}
			if req.CompressionAppendOnly != nil {
				keySettings.CompressionAppendOnlyState = req.CompressionAppendOnly
			}
		}
		if req.ResetGuardrailOverride {
			keySettings.GuardrailOverride = nil
		} else if req.RequiredGuardrails != nil || req.GuardrailNonDerogable != nil {
			if keySettings.GuardrailOverride == nil {
				keySettings.GuardrailOverride = &store.GuardrailOverride{}
			}
			if req.RequiredGuardrails != nil {
				keySettings.GuardrailOverride.RequiredGuardrails = req.RequiredGuardrails
			}
			if req.GuardrailNonDerogable != nil {
				keySettings.GuardrailOverride.NonDerogable = *req.GuardrailNonDerogable
			}
		}
		if req.ResetLogRetentionDays {
			keySettings.LogRetentionDays = nil
		} else if req.LogRetentionDays != nil {
			keySettings.LogRetentionDays = req.LogRetentionDays
		}

		if err := h.store.UpsertKeyRouteSettings(r.Context(), keySettings); err != nil {
			h.logger.Error("admin: update key route settings failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save key settings"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"key_id":                        keySettings.KeyID,
			"team_id":                       keySettings.TeamID,
			"enabled":                       keySettings.Enabled,
			"levels":                        keySettings.Levels,
			"thinking_budgets":              keySettings.ThinkingBudgets,
			"feedback_enabled":              keySettings.FeedbackEnabled,
			"cache_enabled":                 keySettings.CacheEnabled,
			"compression_enabled":           keySettings.CompressionEnabled,
			"cache_direct_threshold":        keySettings.CacheDirectThreshold,
			"cache_reuse_threshold":         keySettings.CacheReuseThreshold,
			"cache_tweak_threshold":         keySettings.CacheTweakThreshold,
			"compression_threshold":         keySettings.CompressionThreshold,
			"compression_token_budget":      keySettings.CompressionTokenBudget,
			"compression_step_window":       keySettings.CompressionStepWindow,
			"compression_append_only_state": keySettings.CompressionAppendOnlyState,
			"guardrail_override":            keySettings.GuardrailOverride,
			"log_retention_days":            keySettings.LogRetentionDays,
		})
		return
	}

	// Team-level settings (original behavior)
	settings, err := h.store.GetTenantSettings(r.Context(), req.TeamID)
	if err != nil {
		h.logger.Error("admin: get tenant settings failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load settings"})
		return
	}
	if settings == nil {
		settings = &store.TenantSettings{
			TeamID:             req.TeamID,
			CacheEnabled:       true,
			CompressionEnabled: true,
		}
	}

	// Apply updates
	if req.Levels != nil {
		settings.HiveRouteLevels = req.Levels
	}
	if req.ThinkingBudgets != nil {
		settings.HiveRouteThinkingBudgets = req.ThinkingBudgets
	}
	// An explicit reset clears the enabled override back to inherit. Without it
	// a team set to false stayed false through a "reset" that reported success:
	// the key branch achieves this by deleting its row, and the team branch has
	// no row to delete.
	if req.ResetEnabled {
		settings.HiveRouteEnabled = nil
	} else if req.Enabled != nil {
		settings.HiveRouteEnabled = req.Enabled
	}

	// reset_feedback clears the override back to inherit, the same way it does
	// for a key. Without this branch a team that had been set to false could
	// never return to inheriting: the request was accepted and the stale value
	// echoed back.
	if req.ResetFeedback {
		settings.FeedbackEnabled = nil
	} else if req.FeedbackEnabled != nil {
		settings.FeedbackEnabled = req.FeedbackEnabled
	}

	if err := h.store.UpsertTenantSettings(r.Context(), settings); err != nil {
		h.logger.Error("admin: update route settings failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to save settings"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"team_id":          settings.TeamID,
		"enabled":          settings.HiveRouteEnabled,
		"levels":           settings.HiveRouteLevels,
		"thinking_budgets": settings.HiveRouteThinkingBudgets,
		"feedback_enabled": settings.FeedbackEnabled,
	})
}

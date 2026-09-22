package hivestate

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// resolveRouteConfig merges global config with per-team overrides from tenant settings,
// then applies per-key overrides if available.
// Priority: per-key > per-team > global config.
func resolveRouteConfig(global config.HiveRouteConfig, ts *store.TenantSettings, ks *store.KeyRouteSettings) config.HiveRouteConfig {
	if ts == nil && ks == nil {
		return global
	}
	cfg := global

	// Apply team-level overrides
	if ts != nil {
		if ts.HiveRouteEnabled != nil {
			cfg.Enabled = *ts.HiveRouteEnabled
		}
		if len(ts.HiveRouteLevels) > 0 {
			cfg.Levels = make([]config.HiveRouteLevel, len(ts.HiveRouteLevels))
			for i, l := range ts.HiveRouteLevels {
				cfg.Levels[i] = config.HiveRouteLevel{
					Name:        l.Name,
					Model:       l.Model,
					Description: l.Description,
				}
			}
		}
		if len(ts.HiveRouteThinkingBudgets) > 0 {
			cfg.ThinkingBudgets = ts.HiveRouteThinkingBudgets
		}
	}

	// Apply per-key overrides (highest priority)
	if ks != nil {
		if ks.Enabled != nil {
			cfg.Enabled = *ks.Enabled
		}
		if len(ks.Levels) > 0 {
			cfg.Levels = make([]config.HiveRouteLevel, len(ks.Levels))
			for i, l := range ks.Levels {
				cfg.Levels[i] = config.HiveRouteLevel{
					Name:        l.Name,
					Model:       l.Model,
					Description: l.Description,
				}
			}
		}
		if len(ks.ThinkingBudgets) > 0 {
			cfg.ThinkingBudgets = ks.ThinkingBudgets
		}
	}

	return cfg
}

// resolveHiveRouteModel finds the target model for a given difficulty level.
// Returns empty string if no matching level is found.
func resolveHiveRouteModel(cfg config.HiveRouteConfig, difficulty string) string {
	difficulty = strings.ToLower(strings.TrimSpace(difficulty))
	for _, level := range cfg.Levels {
		if strings.ToLower(level.Name) == difficulty {
			return level.Model
		}
	}
	return ""
}

// overrideModelInBody replaces the "model" field in a JSON request body.
func overrideModelInBody(body []byte, model string) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body // fail-safe: return unchanged
	}
	modelJSON, _ := json.Marshal(model)
	raw["model"] = modelJSON
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// injectReasoningEffort injects the reasoning effort / thinking configuration into the request body.
// For Anthropic: sets thinking.type = "enabled" with budget_tokens from config.
// For OpenAI-compatible: sets reasoning_effort field directly.
// knownReasoningEfforts is the vocabulary the wire accepts. The value reaching
// here is free text an extraction model produced, so anything outside this set
// is dropped rather than forwarded into a field the provider validates.
var knownReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true}

func injectReasoningEffort(body []byte, effort string, cfg config.HiveRouteConfig, isAnthropic bool) []byte {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if !knownReasoningEfforts[effort] {
		return body
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	if isAnthropic {
		// Anthropic: set thinking config with budget_tokens.
		//
		// Two constraints the API enforces and this used to ignore, turning a
		// request that would have succeeded into a hard 400: budget_tokens must
		// be below max_tokens, and temperature must be 1 while thinking is on.
		budget := resolveThinkingBudget(cfg, effort)
		maxTokens := 0
		if rawMax, ok := raw["max_tokens"]; ok {
			_ = json.Unmarshal(rawMax, &maxTokens)
		}
		if maxTokens > 0 && budget >= maxTokens {
			// Leave headroom for the answer itself.
			budget = maxTokens / 2
		}
		if budget >= minThinkingBudget {
			thinking := map[string]interface{}{
				"type":          "enabled",
				"budget_tokens": budget,
			}
			thinkingJSON, _ := json.Marshal(thinking)
			raw["thinking"] = thinkingJSON
			one, _ := json.Marshal(1)
			raw["temperature"] = one
		}
	} else {
		// OpenAI-compatible: set reasoning_effort field
		effortJSON, _ := json.Marshal(effort)
		raw["reasoning_effort"] = effortJSON
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// minThinkingBudget is Anthropic's floor for extended thinking; below it the
// request is better sent without the field at all.
const minThinkingBudget = 1024

// resolveThinkingBudget maps a reasoning effort level to a budget_tokens value.
// Uses configurable mapping with sensible defaults.
func resolveThinkingBudget(cfg config.HiveRouteConfig, effort string) int {
	if cfg.ThinkingBudgets != nil {
		if budget, ok := cfg.ThinkingBudgets[effort]; ok {
			return budget
		}
	}
	// Defaults
	switch effort {
	case "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 16384
	default:
		return 0
	}
}

// hiveRoute is the outcome of a routing decision.
type hiveRoute struct {
	Level     string
	Model     string
	Effort    string
	SavedCost float64
}

// applyHiveRoute picks a model for the extracted difficulty and rewrites the
// body's model field, returning the new body and what was decided. The body
// comes back unchanged, with a zero hiveRoute, when routing is disabled, no
// difficulty was extracted, or the target model is unreachable.
//
// This runs on the pass-through path as well as the rewrite path. The
// prefix-cache guard declines to *rewrite the body*; which model should serve
// the request is a separate question, and coupling the two silently disabled
// HiveRoute for every guard-protected request.
func applyHiveRoute(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	result *Result,
	cfg config.HiveRouteConfig,
	requestModel string,
	isAnthropic, isResponses bool,
	registry *provider.Registry,
	pc *pricing.Calculator,
	logger *slog.Logger,
) ([]byte, hiveRoute) {
	var route hiveRoute
	if !cfg.Enabled || r.Header.Get("x-ubiquum-route") == "false" {
		return body, route
	}
	if result.State == nil || result.State.Difficulty == "" {
		return body, route
	}
	rm := resolveHiveRouteModel(cfg, result.State.Difficulty)
	if rm == "" {
		return body, route
	}

	// Guard: restricted models (Claude Pro/Max, ChatGPT Codex, ...) are
	// OAuth pass-through and only work if the caller's request carries a
	// scoped upstream token for that exact provider. Routing "complex"
	// from a claude-sonnet-5 (anthropic) call into codex-gpt-5.6-sol
	// (chatgpt_codex) silently fails downstream (no matching token, no
	// api_key configured) — skip the route instead of rewriting into a
	// model the caller cannot actually reach.
	if registry != nil && registry.IsRestricted(rm) {
		dep, depErr := registry.GetDeployment(rm)
		if depErr != nil {
			logger.Warn("hiveroute: routed model not registered, skipping route", "model", rm, "error", depErr)
			return body, route
		}
		if auth.UpstreamTokenForProvider(r.Context(), dep.Provider.Name()) == "" {
			logger.Warn("hiveroute: no upstream OAuth token for routed provider, skipping route",
				"model", rm, "provider", dep.Provider.Name(), "difficulty", result.State.Difficulty)
			return body, route
		}
	}

	// The caller's own whitelist applies to the routed model too. This block
	// checked restriction and OAuth reachability but not access, so a request
	// against a model the caller was allowed to use could be rewritten into one
	// they were not — and the handler then answered 403 for a request that was
	// perfectly valid as sent.
	if !auth.IsModelAllowed(r.Context(), rm, registry != nil && registry.IsRestricted(rm)) {
		logger.Warn("hiveroute: caller may not use the routed model, skipping route",
			"model", rm, "difficulty", result.State.Difficulty)
		return body, route
	}

	route.Level = result.State.Difficulty
	route.Model = rm
	route.Effort = result.State.ReasoningEffort
	body = overrideModelInBody(body, rm)
	w.Header().Set("x-hiveroute-level", route.Level)
	w.Header().Set("x-hiveroute-model", route.Model)
	// Reasoning-effort injection is not yet wired through the Responses
	// API request struct downstream, so skip it for /v1/responses to
	// avoid setting a field that would silently be ignored.
	if route.Effort != "" && !isResponses {
		body = injectReasoningEffort(body, route.Effort, cfg, isAnthropic)
		w.Header().Set("x-hiveroute-effort", route.Effort)
	}
	// Estimate cost savings: delta between original model and routed model.
	if pc != nil {
		// Use result tokens as proxy for expected input+output of the actual call
		originalCost := pc.Cost(requestModel, result.ResultTokens, result.ResultTokens)
		routedCost := pc.Cost(rm, result.ResultTokens, result.ResultTokens)
		route.SavedCost = originalCost - routedCost
	}
	w.Header().Set("x-hiveroute-savings", fmt.Sprintf("%.6f", route.SavedCost))
	logger.Info("hiveroute: model routed",
		"original_model", requestModel,
		"difficulty", route.Level,
		"target_model", route.Model,
		"reasoning_effort", route.Effort,
		"estimated_savings", fmt.Sprintf("%.6f", route.SavedCost),
	)
	return body, route
}

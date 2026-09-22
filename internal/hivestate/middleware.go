package hivestate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// Middleware returns an HTTP middleware that applies HiveState to chat completions.
// On cache MISS (when this middleware runs), it extracts conversational state and
// rewrites the request body to contain only system + state + last user message.
// OptimizationRecorder receives the aggregate counters behind the
// prefix-cache guard and CCR. Narrow on purpose: the package records what it
// decides without depending on the metrics implementation, and a nil recorder
// disables collection.
type OptimizationRecorder interface {
	RecordGuardDecision(reason, api string)
	RecordCCRInjection(files, tokens int)
	RecordCCRBudgetSkip()
}

// apiLabel maps a surface to a low-cardinality metric label.
func apiLabel(api APIFlavor) string {
	switch api {
	case APIAnthropic:
		return "anthropic"
	case APIResponses:
		return "responses"
	default:
		return "openai"
	}
}

func Middleware(hs *HiveState, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, registry *provider.Registry, rec OptimizationRecorder, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only process chat completions, Anthropic messages, and Responses API requests
			isAnthropic := r.URL.Path == "/v1/messages" && r.Method == "POST"
			isOpenAI := r.URL.Path == "/v1/chat/completions" && r.Method == "POST"
			isResponses := r.URL.Path == "/v1/responses" && r.Method == "POST"
			if !isAnthropic && !isOpenAI && !isResponses {
				next.ServeHTTP(w, r)
				return
			}

			// Check opt-out header
			if r.Header.Get("x-ubiquum-state") == "false" {
				next.ServeHTTP(w, r)
				return
			}

			// Check per-team compression toggle from tenant_settings
			var teamSettings *store.TenantSettings
			var keyRouteSettings *store.KeyRouteSettings
			if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
				if ki.TeamID != "" {
					if settings, err := db.GetTenantSettings(r.Context(), ki.TeamID); err == nil && settings != nil {
						teamSettings = settings
					}
				}
				// Per-key route settings, keyed by hash — the same key the
				// feedback middleware and the admin API use for this table.
				// Nesting this under a team meant a key without one could never
				// carry an override, even though one can be stored for it.
				if ki.KeyHash != "" {
					if ks, captured := auth.KeyRouteSettingsFromContext(r.Context()); captured {
						keyRouteSettings = ks
					} else if ks, err := db.GetKeyRouteSettings(r.Context(), ki.KeyHash); err == nil && ks != nil {
						keyRouteSettings = ks
					}
				}
				// A per-key override wins over the team-wide default either
				// way; with neither set, compression defaults on.
				compressionEnabled := teamSettings == nil || teamSettings.CompressionEnabled
				if keyRouteSettings != nil && keyRouteSettings.CompressionEnabled != nil {
					compressionEnabled = *keyRouteSettings.CompressionEnabled
				}
				if !compressionEnabled {
					// Logged because a disabled toggle is indistinguishable
					// from a broken pipeline when it is silent.
					logger.Info("hivestate: disabled",
						"team_id", ki.TeamID, "key_hash", ki.KeyHash, "path", r.URL.Path)
					next.ServeHTTP(w, r)
					return
				}
			}
			// := (not =) shadows the shared engine with a request-scoped
			// local for the rest of this handler invocation — every hs.cfg
			// read below, the prefix-cache guard included, must agree with
			// what Process() itself uses. Reassigning the outer hs instead
			// would race every other in-flight request against this one's
			// overrides, since the same *HiveState is shared across calls;
			// WithOverrides itself returns h unchanged when there is nothing
			// to override, so this is always safe to call.
			var compressionOverrides CompressionOverrides
			if keyRouteSettings != nil {
				compressionOverrides = CompressionOverrides{
					Threshold:       keyRouteSettings.CompressionThreshold,
					TokenBudget:     keyRouteSettings.CompressionTokenBudget,
					StepWindow:      keyRouteSettings.CompressionStepWindow,
					AppendOnlyState: keyRouteSettings.CompressionAppendOnlyState,
				}
			}
			hs := hs.WithOverrides(compressionOverrides)

			// Read body
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			// Parse messages based on format
			var messages []Message
			var requestModel string
			if isAnthropic {
				messages = parseAnthropicMessages(bodyBytes)
				var anthReq struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(bodyBytes, &anthReq)
				requestModel = anthReq.Model
			} else if isResponses {
				messages = parseResponsesMessages(bodyBytes)
				var respReq struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(bodyBytes, &respReq)
				requestModel = respReq.Model
			} else {
				var req struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal(bodyBytes, &req); err == nil {
					requestModel = req.Model
				}
				messages = parseOpenAIMessages(bodyBytes)
			}

			if len(messages) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Scope identifies this conversation for both CCR (below) and the
			// prefix-cache guard's observed-cache lookup: the guard needs to know,
			// before it decides anything, what the provider actually cached last
			// turn for this same conversation.
			var scope Scope
			if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
				scope.TeamID = ki.TeamID
				scope.KeyHash = ki.KeyHash
			}
			scope.SessionID = DeriveSessionID(r.Header.Get(SessionHeader), messages)

			// guardSkip is non-nil when the prefix-cache guard decided against
			// rewriting; it stands in for a Process() result.
			var guardSkip *Result

			// Prefix-cache guard: HiveState rewrites the head of the
			// conversation, which invalidates the provider's prompt cache from
			// that position onward. When the client is already getting cached
			// reads, forwarding unmodified is usually cheaper than a rewrite
			// that shrinks the token count — see prefix_cache.go for the
			// break-even. Run this before Process() so a skip also avoids the
			// cost of a state extraction.
			api := APIOpenAI
			if isAnthropic {
				api = APIAnthropic
			} else if isResponses {
				api = APIResponses
			}
			window := hs.cfg.StepWindow
			if hs.cfg.PrefixCacheGuard.GuardEnabled() {
				g := hs.cfg.PrefixCacheGuard
				originalTokens := hs.counter.CountMessages(messages)
				// Process passes a conversation below the threshold through
				// untouched, so there is no rewrite for the guard to weigh and
				// no cache decision to report. Testing that first keeps the
				// probe — two unmarshal and two marshal passes over the whole
				// body — off the hot path for traffic HiveState would never
				// compress anyway.
				if originalTokens >= hs.cfg.Threshold {
					observedCachedTokens, _ := globalObservedCache.get(scope.key())
					prefix := DetectPrefixState(bodyBytes, api, originalTokens, g.MinCacheableTokens, g.AssumeImplicitCache, observedCachedTokens)
					predicted, reusable, wouldRewrite := PredictRewrite(
						bodyBytes, api, window, originalTokens, g.StateSummaryChars,
					)
					ratios := cacheRatiosFor(pc, registry, requestModel, api, g.AnthropicCacheReadRatio, g.OpenAICacheReadRatio)
					if !hs.cfg.AppendOnlyState {
						// Only the append-only log produces a state region that
						// survives to the next turn; a snapshot is rewritten
						// from scratch and caches nothing.
						reusable = 0
					}
					decision := EvaluateGuard(GuardInput{
						Prefix:               prefix,
						OriginalTokens:       originalTokens,
						PredictedTokens:      predicted,
						ReusableAfterRewrite: reusable,
						CacheReadRatio:       ratios.Read,
						CacheWriteRatio:      ratios.Write,
						Margin:               g.Margin,
						Horizon:              g.AmortizeTurns(),
					})

					if rec != nil {
						rec.RecordGuardDecision(decision.Reason, apiLabel(api))
					}
					w.Header().Set("x-hivestate-prefix-cache", decision.Reason)
					if decision.FrozenTokens > 0 {
						w.Header().Set("x-hivestate-cached-prefix-tokens", fmt.Sprintf("%d", decision.FrozenTokens))
					}
					if decision.BreakEvenTurns > 0 {
						w.Header().Set("x-hivestate-break-even-turns", fmt.Sprintf("%.1f", decision.BreakEvenTurns))
					}

					// A guard skip synthesizes a NO_OP result rather than
					// returning here. Falling through keeps the shared header and
					// metric path as the single place observability is emitted:
					// an early return would make skipped requests invisible, which
					// is precisely the traffic the guard exists to explain.
					switch {
					case !wouldRewrite:
						// The rewriter would leave the body unchanged, so there is
						// nothing to gain and an extraction call to avoid.
						//
						// This is logged because it is otherwise indistinguishable
						// from HiveState never running at all: no state line, no
						// guard line, nothing. On agentic Responses traffic it is
						// the common outcome, since the rewriter splits on user
						// turns and a tool-driven session has very few.
						logger.Info("hivestate: skipped, rewrite would change nothing",
							"api", apiLabel(api),
							"original_tokens", originalTokens,
							"parsed_messages", len(messages),
							"window", window,
							"turns", countRewriteTurns(bodyBytes, api),
						)
						guardSkip = &Result{
							Mode:           ModeNoOp,
							OriginalTokens: originalTokens,
							ResultTokens:   originalTokens,
							FallbackReason: "rewrite_would_be_noop",
						}
					case decision.Skip:
						logger.Info("hivestate: skipped to preserve provider prefix cache",
							"reason", decision.Reason,
							"prefix_detection", prefix.Reason,
							"explicit_markers", prefix.Explicit,
							"cached_prefix_tokens", decision.FrozenTokens,
							"original_tokens", originalTokens,
							"predicted_tokens", predicted,
							"reusable_tokens", reusable,
							"cache_read_ratio", ratios.Read,
							"ratios_from_catalog", ratios.FromCatalog,
							"passthrough_cost", decision.PassthroughCost,
							"rewrite_cost", decision.RewriteCost,
							"break_even_turns", decision.BreakEvenTurns,
							"assumed_turns", g.AmortizeTurns(),
						)
						guardSkip = &Result{
							Mode:           ModeNoOp,
							OriginalTokens: originalTokens,
							ResultTokens:   originalTokens,
							FallbackReason: decision.Reason,
						}
					}
				}
			}

			// Run HiveState (pass level overrides: per-key > per-team)
			start := time.Now()
			var teamLevelOverrides []config.HiveRouteLevel
			if keyRouteSettings != nil && len(keyRouteSettings.Levels) > 0 {
				teamLevelOverrides = make([]config.HiveRouteLevel, len(keyRouteSettings.Levels))
				for i, l := range keyRouteSettings.Levels {
					teamLevelOverrides[i] = config.HiveRouteLevel{Name: l.Name, Model: l.Model, Description: l.Description}
				}
			} else if teamSettings != nil && len(teamSettings.HiveRouteLevels) > 0 {
				teamLevelOverrides = make([]config.HiveRouteLevel, len(teamSettings.HiveRouteLevels))
				for i, l := range teamSettings.HiveRouteLevels {
					teamLevelOverrides[i] = config.HiveRouteLevel{Name: l.Name, Model: l.Model, Description: l.Description}
				}
			}
			// HiveRoute needs the extracted difficulty, and the guard skips
			// Process() to save an extraction call. Those are separate
			// features: the guard protects the provider's prefix cache from a
			// body rewrite, while HiveRoute only chooses a model. So when
			// routing is active for this request, extract anyway and keep the
			// body unrewritten — otherwise a guard skip would silently disable
			// routing for what is, with the guard on by default, most traffic.
			routeCfg := resolveRouteConfig(hs.cfg.HiveRoute, teamSettings, keyRouteSettings)
			routeWanted := routeCfg.Enabled && r.Header.Get("x-ubiquum-route") != "false"

			result := guardSkip
			if result == nil {
				result = hs.Process(r.Context(), messages, scope, teamLevelOverrides)
			} else if routeWanted {
				extracted := hs.Process(r.Context(), messages, scope, teamLevelOverrides)
				result.State = extracted.State
				result.PromptTokens = extracted.PromptTokens
				result.CompletionTokens = extracted.CompletionTokens
				result.CacheHit = extracted.CacheHit
			}
			latencyMs := time.Since(start).Milliseconds()

			if rec != nil {
				if len(result.CCRMessages) > 0 {
					// The framing message is not a recovered file; the rest are.
					rec.RecordCCRInjection(len(result.CCRMessages)-1, hs.counter.CountMessages(result.CCRMessages))
				}
				if result.CCRBudgetSkipped {
					rec.RecordCCRBudgetSkip()
				}
			}

			// Set observability headers
			w.Header().Set("x-hivestate-mode", string(result.Mode))
			w.Header().Set("x-hivestate-original-tokens", fmt.Sprintf("%d", result.OriginalTokens))
			w.Header().Set("x-hivestate-tokens", fmt.Sprintf("%d", result.ResultTokens))
			if result.OriginalTokens > 0 {
				ratio := 1.0 - float64(result.ResultTokens)/float64(result.OriginalTokens)
				w.Header().Set("x-hivestate-ratio", fmt.Sprintf("%.2f", ratio))
			}
			w.Header().Set("x-hivestate-latency-ms", fmt.Sprintf("%d", latencyMs))
			w.Header().Set("x-hivestate-extraction-prompt-tokens", fmt.Sprintf("%d", result.PromptTokens))
			w.Header().Set("x-hivestate-extraction-completion-tokens", fmt.Sprintf("%d", result.CompletionTokens))
			if result.State != nil && result.State.Intent != "" {
				w.Header().Set("x-hivestate-intent", result.State.Intent)
			}
			if result.State != nil && result.State.ConversationStatus != "" {
				w.Header().Set("x-hivestate-status", result.State.ConversationStatus)
			}
			if result.State != nil && result.State.ActiveConstraints != nil {
				if cBytes, err := json.Marshal(result.State.ActiveConstraints); err == nil {
					w.Header().Set("x-hivestate-constraints", string(cBytes))
				}
			}
			if result.FallbackReason != "" {
				w.Header().Set("x-hivestate-fallback", result.FallbackReason)
			}

			// Log extraction spend attributed to the caller's key. This sits
			// ahead of the NO_OP branch because a guard skip that extracted
			// purely for routing still spends those tokens; the guard clause
			// keeps it a no-op when no extraction ran.
			if result.PromptTokens > 0 || result.CompletionTokens > 0 {
				// Resolved on this goroutine, before the closure below ever
				// runs: both branches below reassign `r` (`r = r.WithContext
				// (...)`, NO_OP and STATE alike) after this point, and a
				// goroutine closing over the `r` variable itself — instead of
				// a value read from it up front — races that reassignment.
				// Confirmed with `go test -race`: this was VAL-02's flagged
				// data race between this goroutine's read of r.Context() and
				// that write. The two other goroutines in this function are
				// spawned after their branch's own reassignment and after
				// next.ServeHTTP has already returned, with no further write
				// to `r` afterwards — they read the closed-over `r` safely
				// and are unchanged.
				keyInfo := auth.KeyInfoFromContext(r.Context())
				go func() {
					record := store.SpendRecord{
						Model:            hs.cfg.Model,
						Provider:         "@hivestate",
						PromptTokens:     result.PromptTokens,
						CompletionTokens: result.CompletionTokens,
						TotalTokens:      result.PromptTokens + result.CompletionTokens,
						Duration:         latencyMs,
						Status:           200,
						CreatedAt:        time.Now(),
					}
					if pc != nil {
						record.Cost = pc.Cost(hs.cfg.Model, result.PromptTokens, result.CompletionTokens)
					}
					if keyInfo != nil {
						record.ApplyIdentity(keyInfo)
					}
					spender.Record(record)
				}()
			}

			// If NO_OP, pass the original body through — routed, if HiveRoute
			// has something to say about it.
			if result.Mode == ModeNoOp {
				// Every outcome has to say why. Without this the package can
				// decide to do nothing on every request and look identical to
				// not being installed at all.
				logger.Info("hivestate: not applied",
					"api", apiLabel(api),
					"reason", result.FallbackReason,
					"original_tokens", result.OriginalTokens,
					"parsed_messages", len(messages),
					"body_bytes", len(bodyBytes),
				)
				noopBody, route := applyHiveRoute(w, r, bodyBytes, result, routeCfg,
					requestModel, isAnthropic, isResponses, registry, pc, logger)
				if route.Model != "" {
					r.Body = io.NopCloser(bytes.NewReader(noopBody))
					r.ContentLength = int64(len(noopBody))
				}

				// Inject UsageCapture even on a guard skip: this is exactly the
				// traffic the guard is reasoning about, so its real
				// cached_prompt_tokens is what the NEXT request's guard
				// decision needs to ground its guess in.
				uc := &store.UsageCapture{}
				r = r.WithContext(store.WithUsageCapture(r.Context(), uc))

				// Serve before logging, so a routed call that failed downstream
				// does not get recorded as a saving — the same rule the rewrite
				// path applies.
				sw := &hiveStateStatusWriter{ResponseWriter: w, status: http.StatusOK}
				next.ServeHTTP(sw, r)
				if route.Model != "" && sw.status >= 400 {
					logger.Warn("hiveroute: routed call failed downstream, discarding savings estimate",
						"route_model", route.Model, "status", sw.status)
					route.SavedCost = 0
				}
				if uc.Filled {
					globalObservedCache.record(scope.key(), uc.CachedPromptTokens)
				}

				go func() {
					intent := ""
					if result.State != nil {
						intent = result.State.Intent
					}
					keyPrefix := ""
					if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
						keyPrefix = ki.KeyPrefix
					}
					_ = db.LogHiveStateMetric(context.Background(), store.HiveStateMetric{
						RequestID:      r.Header.Get("x-request-id"),
						Model:          hs.cfg.Model,
						RequestModel:   requestModel,
						KeyPrefix:      keyPrefix,
						Mode:           string(result.Mode),
						Intent:         intent,
						OriginalTokens: result.OriginalTokens,
						ResultTokens:   result.ResultTokens,
						LatencyMs:      latencyMs,
						FallbackReason: result.FallbackReason,
						RouteLevel:     route.Level,
						RouteModel:     route.Model,
						RouteEffort:    route.Effort,
						RouteSavedCost: route.SavedCost,
						CreatedAt:      time.Now(),
					})
				}()
				return
			}

			// STATE mode: rewrite request body preserving recent messages raw
			// (tool_use, tool_result, function calls stay intact in the recent window)
			// Same `window` the guard probed with, so the prediction and the
			// real rewrite can never disagree about the preserved window.
			var newBody []byte
			if isAnthropic {
				newBody, err = rewriteAnthropicBodyWithWindow(bodyBytes, result, window)
			} else if isResponses {
				newBody, err = rewriteResponsesBodyWithWindow(bodyBytes, result, window)
			} else {
				newBody, err = rewriteOpenAIBodyWithWindow(bodyBytes, result, window)
			}
			if err != nil {
				logger.Warn("hivestate: body rewrite failed, passing through", "error", err)
				next.ServeHTTP(w, r)
				return
			}

			// HiveRoute: automatic model routing based on extracted difficulty
			newBody, route := applyHiveRoute(w, r, newBody, result, routeCfg,
				requestModel, isAnthropic, isResponses, registry, pc, logger)
			routeSavedCost := route.SavedCost

			r.Body = io.NopCloser(bytes.NewReader(newBody))
			r.ContentLength = int64(len(newBody))

			// Inject UsageCapture in context so downstream proxy fills real token counts
			uc := &store.UsageCapture{}
			r = r.WithContext(store.WithUsageCapture(r.Context(), uc))

			logger.Info("hivestate: state applied",
				"mode", string(result.Mode),
				"original_tokens", result.OriginalTokens,
				"result_tokens", result.ResultTokens,
				"latency_ms", latencyMs,
				"cache_hit", result.CacheHit,
				"out_bytes", len(newBody),
				"prefix", outgoingPrefixHashes(newBody),
				"in_head", describeLeadingItems(bodyBytes, 10),
				"out_head", describeLeadingItems(newBody, 10),
				"user_turns_in", countBodyUserTurns(bodyBytes, api),
				"user_turns_out", countBodyUserTurns(newBody, api),
			)

			sw := &hiveStateStatusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			if uc.Filled {
				globalObservedCache.record(scope.key(), uc.CachedPromptTokens)
			}

			// Routing savings are only real if the routed call actually succeeded.
			// If the downstream request errored (e.g. the routed-to model rejected
			// the request), zero the estimate instead of recording a "saving" for
			// a call that was never billed. This is purely a metrics/reporting
			// adjustment — it does not touch spend recording, which happens
			// independently in the proxy layer regardless of this middleware.
			if route.Model != "" && sw.status >= 400 {
				logger.Warn("hiveroute: routed call failed downstream, discarding savings estimate",
					"route_model", route.Model, "status", sw.status)
				routeSavedCost = 0
			}

			// Log metric to DB (async) — use real tokens from provider response if available
			go func() {
				intent := ""
				if result.State != nil {
					intent = result.State.Intent
				}

				resultTokens := result.ResultTokens
				savedCost := 0.0
				if uc.Filled && uc.PromptTokens > 0 {
					resultTokens = uc.PromptTokens
					// Compression alone, priced at the model the caller asked
					// for. Comparing against uc.Cost instead mixed two effects:
					// that figure is billed at the *routed* model, so it already
					// contained the routing delta and the column named for
					// compression reported both.
					if pc != nil {
						originalCost := pc.Cost(requestModel, result.OriginalTokens, uc.CompletionTokens)
						compressedCost := pc.Cost(requestModel, uc.PromptTokens, uc.CompletionTokens)
						savedCost = originalCost - compressedCost
					}
					logger.Info("hivestate: real usage captured",
						"estimated_tokens", result.ResultTokens,
						"real_prompt_tokens", uc.PromptTokens,
						"real_completion_tokens", uc.CompletionTokens,
						"real_cost", uc.Cost,
						"saved_cost", savedCost,
						"request_model", requestModel,
					)
				} else {
					logger.Info("hivestate: usage capture missed",
						"filled", uc.Filled,
						"prompt_tokens", uc.PromptTokens,
					)
				}

				// Both sides of the ratio come from the same counter. Dividing
				// the provider's prompt tokens by the gateway's own estimate of
				// the original mixed two tokenizers, and could report a negative
				// saving on a request that had in fact shrunk.
				ratio := 0.0
				if result.OriginalTokens > 0 {
					ratio = 1.0 - float64(result.ResultTokens)/float64(result.OriginalTokens)
				}
				keyPrefix := ""
				if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
					keyPrefix = ki.KeyPrefix
				}
				_ = db.LogHiveStateMetric(context.Background(), store.HiveStateMetric{
					RequestID:            r.Header.Get("x-request-id"),
					Model:                hs.cfg.Model,
					RequestModel:         requestModel,
					KeyPrefix:            keyPrefix,
					Mode:                 string(result.Mode),
					Intent:               intent,
					OriginalTokens:       result.OriginalTokens,
					ResultTokens:         resultTokens,
					Ratio:                ratio,
					LatencyMs:            latencyMs,
					FallbackReason:       result.FallbackReason,
					RouteLevel:           route.Level,
					RouteModel:           route.Model,
					RouteEffort:          route.Effort,
					RouteSavedCost:       routeSavedCost,
					CompressionSavedCost: savedCost,
					CreatedAt:            time.Now(),
				})
			}()
		})
	}
}

// hiveStateStatusWriter wraps http.ResponseWriter to capture the response
// status code so the middleware can tell whether a routed call actually
// succeeded. Implements http.Flusher to support streaming responses.
type hiveStateStatusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *hiveStateStatusWriter) WriteHeader(code int) {
	sw.status = code
	sw.wroteHeader = true
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *hiveStateStatusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.status = http.StatusOK
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

func (sw *hiveStateStatusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rewriteOpenAIBodyWithWindow rewrites an OpenAI request body, preserving
// recent messages as raw JSON (including tool_calls and tool responses).
// Only older history is replaced with the state summary.
// Uses turn-based splitting to keep tool call chains intact.
func rewriteOpenAIBodyWithWindow(original []byte, result *Result, recentWindow int) ([]byte, error) {
	if recentWindow <= 0 {
		recentWindow = 4 // default: keep last 4 user turns
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(original, &body); err != nil {
		return nil, err
	}

	// Parse original messages as raw JSON array
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(body["messages"], &rawMessages); err != nil {
		return nil, err
	}

	// Find system messages at the head
	systemEnd := 0
	for i := range rawMessages {
		var m struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(rawMessages[i], &m); err == nil {
			if m.Role == "system" || m.Role == "developer" {
				systemEnd = i + 1
			} else {
				break
			}
		}
	}

	// Body is everything after system messages
	bodyMsgs := rawMessages[systemEnd:]
	if len(bodyMsgs) == 0 {
		return original, nil
	}

	// The last message is preserved as-is (last user message)
	lastMsg := rawMessages[len(rawMessages)-1]
	bodyMsgs = bodyMsgs[:len(bodyMsgs)-1] // exclude last

	// Find user turn boundaries (user messages that are NOT tool responses)
	turnStarts := []int{}
	for i, raw := range bodyMsgs {
		if isOpenAIUserTurn(raw) {
			turnStarts = append(turnStarts, i)
		}
	}

	// If fewer turns than window, nothing to compress
	if len(turnStarts) <= recentWindow {
		return original, nil
	}

	// Split at the turn boundary: keep last recentWindow turns
	splitTurnIdx := len(turnStarts) - recentWindow
	splitPoint := turnStarts[splitTurnIdx]
	recentRaw := bodyMsgs[splitPoint:]

	// Build state summary message
	stateMsg := map[string]string{
		"role":    "user",
		"content": "Conversation state (summary of older context):\n" + result.StateJSON,
	}
	stateMsgJSON, err := json.Marshal(stateMsg)
	if err != nil {
		return nil, err
	}

	// Rebuild: [system...] + [state] + [recent raw...] + [last], with CCR
	// spliced as late as ccrInsertIndex allows.
	//
	// That position is past anything a provider prompt cache covers, so
	// recovered context never invalidates a cached prefix. Putting it next to
	// the state summary would have — it sits at the head.
	ccrRaw, err := marshalCCRMessages(result.CCRMessages)
	if err != nil {
		return nil, err
	}
	tail := make([]json.RawMessage, 0, len(recentRaw)+1)
	tail = append(tail, recentRaw...)
	tail = append(tail, lastMsg)
	at := ccrInsertIndex(tail, isOpenAIToolResponse)

	registryJSON, err := registryMessageJSON(result)
	if err != nil {
		return nil, err
	}

	newMessages := make([]json.RawMessage, 0, systemEnd+2+len(tail)+len(ccrRaw))
	newMessages = append(newMessages, rawMessages[:systemEnd]...)
	newMessages = append(newMessages, stateMsgJSON)
	if registryJSON != nil {
		newMessages = append(newMessages, registryJSON)
	}
	newMessages = append(newMessages, tail[:at]...)
	newMessages = append(newMessages, ccrRaw...)
	newMessages = append(newMessages, tail[at:]...)

	msgsJSON, err := json.Marshal(newMessages)
	if err != nil {
		return nil, err
	}
	body["messages"] = msgsJSON

	return json.Marshal(body)
}

// isOpenAIUserTurn checks if a raw OpenAI message is a "real" user turn
// (not a tool response). In OpenAI format, tool responses have role="tool".
func isOpenAIUserTurn(raw json.RawMessage) bool {
	var m struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Role == "user"
}

// registryMessageJSON renders the Code Registry in the "messages" wire shape,
// or nil when there is nothing to send.
func registryMessageJSON(result *Result) (json.RawMessage, error) {
	if result.RegistryMessage.Content == "" {
		return nil, nil
	}
	return json.Marshal(map[string]string{
		"role":    "user",
		"content": result.RegistryMessage.Content,
	})
}

// registryItemJSON is registryMessageJSON in the Responses input-item shape.
func registryItemJSON(result *Result) (json.RawMessage, error) {
	if result.RegistryMessage.Content == "" {
		return nil, nil
	}
	return json.Marshal(map[string]interface{}{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": result.RegistryMessage.Content},
		},
	})
}

// ccrInsertIndex returns the index in tail where recovered-context messages may
// be spliced.
//
// The position we want is immediately before the final message: that is past
// anything a provider prompt cache covers, so recovered context never
// invalidates a cached prefix. In an agentic loop the final message is a tool
// response, and separating one from the tool call it answers makes the provider
// reject the entire request — Anthropic and OpenAI both return 400 — so the
// index walks back over the trailing tool exchange to the last position that
// keeps every call adjacent to its response. Landing after a *completed* pair
// is fine, so this costs at most one assistant step of cache reach.
func ccrInsertIndex(tail []json.RawMessage, isToolResponse func(json.RawMessage) bool) int {
	if len(tail) == 0 {
		return 0
	}
	k := len(tail) - 1
	for k > 0 && isToolResponse(tail[k]) {
		k--
	}
	return k
}

// isOpenAIToolResponse reports whether a message answers a tool call and so
// must stay adjacent to the assistant message that made it.
func isOpenAIToolResponse(raw json.RawMessage) bool {
	var m struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Role == "tool"
}

// isAnthropicToolResponse reports whether a user message carries tool_result
// blocks. Anthropic delivers tool results inside user messages rather than as a
// role of their own, so role alone cannot distinguish one from a real turn.
func isAnthropicToolResponse(raw json.RawMessage) bool {
	var m struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	// A string content field fails this unmarshal, which is the right answer:
	// it cannot carry a tool_result.
	if err := json.Unmarshal(raw, &m); err != nil || m.Role != "user" {
		return false
	}
	for _, b := range m.Content {
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &t); err == nil && t.Type == "tool_result" {
			return true
		}
	}
	return false
}

// isResponsesToolOutput reports whether an input item carries the output of a
// call item. The rewriter only models function calls today; computer_call_output
// is listed so a computer-use transcript cannot reintroduce the same split. A
// LocalShellCall's result is not a distinct wire type — it comes back as an
// ordinary function_call_output, so there is no separate
// "local_shell_call_output" to match.
func isResponsesToolOutput(raw json.RawMessage) bool {
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	switch m.Type {
	case "function_call_output", "computer_call_output":
		return true
	}
	return false
}

// rewriteAnthropicBodyWithWindow rewrites an Anthropic request body, preserving
// recent messages as raw JSON (including tool_use and tool_result blocks).
// Only older history is replaced with the state summary.
// Split by assistant steps — keeps the last stepWindow steps intact.
func rewriteAnthropicBodyWithWindow(original []byte, result *Result, stepWindow int) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(original, &body); err != nil {
		return nil, err
	}

	// Parse original messages as raw JSON array
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(body["messages"], &rawMessages); err != nil {
		return nil, err
	}

	if len(rawMessages) == 0 {
		return original, nil
	}

	// The last message is preserved as-is
	lastMsg := rawMessages[len(rawMessages)-1]
	bodyMsgs := rawMessages[:len(rawMessages)-1]

	if len(bodyMsgs) == 0 {
		return original, nil
	}

	// Find assistant step boundaries
	if stepWindow <= 0 {
		stepWindow = 4
	}
	stepStarts := []int{}
	for i, raw := range bodyMsgs {
		if isAnthropicAssistant(raw) {
			stepStarts = append(stepStarts, i)
		}
	}
	if len(stepStarts) <= stepWindow {
		return original, nil // not enough steps to compress
	}

	splitStepIdx := len(stepStarts) - stepWindow
	splitPoint := stepStarts[splitStepIdx]
	recentRaw := bodyMsgs[splitPoint:]

	// Build state summary message (Anthropic format: role + content string)
	stateMsgJSON, err := anthropicStateMessage(result)
	if err != nil {
		return nil, err
	}

	// Ensure alternation: if recent starts with user, state(user) is fine → user, assistant...
	// But if recent starts with assistant, we need: state(user), assistant... which is valid.
	// Rebuild: [state] + [recent raw...] + [last], with CCR spliced as late as
	// ccrInsertIndex allows — past the reach of any provider prompt cache, so
	// recovered context never busts a cached prefix.
	ccrRaw, err := marshalCCRMessages(result.CCRMessages)
	if err != nil {
		return nil, err
	}
	tail := make([]json.RawMessage, 0, len(recentRaw)+1)
	tail = append(tail, recentRaw...)
	tail = append(tail, lastMsg)
	at := ccrInsertIndex(tail, isAnthropicToolResponse)

	registryJSON, err := registryMessageJSON(result)
	if err != nil {
		return nil, err
	}

	newMessages := make([]json.RawMessage, 0, 2+len(tail)+len(ccrRaw))
	newMessages = append(newMessages, stateMsgJSON)
	if registryJSON != nil {
		newMessages = append(newMessages, registryJSON)
	}
	newMessages = append(newMessages, tail[:at]...)
	newMessages = append(newMessages, ccrRaw...)
	newMessages = append(newMessages, tail[at:]...)

	msgsJSON, err := json.Marshal(newMessages)
	if err != nil {
		return nil, err
	}
	body["messages"] = msgsJSON

	return json.Marshal(body)
}

// isAnthropicAssistant checks if a raw Anthropic message has role=assistant.
func isAnthropicAssistant(raw json.RawMessage) bool {
	var m struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return m.Role == "assistant"
}

// parseAnthropicMessages extracts simplified text messages from an Anthropic request body.
// Used only for state extraction input (the extractor only needs text to understand context).
func parseAnthropicMessages(body []byte) []Message {
	var req struct {
		Messages []struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
		System interface{} `json:"system,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var messages []Message

	// Add system message if present
	if req.System != nil {
		sysText := extractTextFromContent(req.System)
		if sysText != "" {
			messages = append(messages, Message{Role: "system", Content: sysText})
		}
	}

	for _, m := range req.Messages {
		text := extractTextFromContent(m.Content)
		if text == "" {
			continue
		}
		msg := Message{Role: m.Role, Content: text}
		// Mark user messages that are tool responses (contain tool_result blocks)
		// so SplitMessages can distinguish them from real human turns.
		if m.Role == "user" {
			if toolID := extractToolCallID(m.Content); toolID != "" {
				msg.ToolCallID = toolID
			}
		}
		messages = append(messages, msg)
	}

	return messages
}

// extractToolCallID returns a non-empty tool_use_id if the content is a tool_result
// (i.e., user message containing only tool_result blocks, no plain text from the human).
func extractToolCallID(content interface{}) string {
	blocks, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var toolID string
	for _, block := range blocks {
		bm, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			// Has a real text block → this is a human message, not a pure tool response
			return ""
		case "tool_result":
			if id, ok := bm["tool_use_id"].(string); ok && id != "" {
				toolID = id
			}
		}
	}
	return toolID
}

// extractTextFromContent extracts plain text from Anthropic content (string or []content blocks).
// Handles text blocks, tool_use (name + input summary), and tool_result (output).
func extractTextFromContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if blocks, ok := content.([]interface{}); ok {
		var parts []string
		for _, block := range blocks {
			bm, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			switch bm["type"] {
			case "text":
				if text, ok := bm["text"].(string); ok {
					parts = append(parts, text)
				}
			case "tool_use":
				// Extract tool call: name and input summary for the extractor
				name, _ := bm["name"].(string)
				if name != "" {
					summary := "[TOOL_CALL " + name
					if input, ok := bm["input"].(map[string]interface{}); ok {
						if cmd, ok := input["command"].(string); ok {
							summary += "] $ " + cmd
						} else if content, ok := input["content"].(string); ok {
							summary += " file=" + fmt.Sprintf("%v", input["file_path"]) + "] wrote " + content
						} else if path, ok := input["file_path"].(string); ok {
							summary += " file=" + path + "]"
						} else {
							summary += "]"
						}
					} else {
						summary += "]"
					}
					parts = append(parts, summary)
				}
			case "tool_result":
				// Extract tool result content, stripping ANSI codes from terminal output
				if resultContent, ok := bm["content"].(string); ok && resultContent != "" {
					clean := stripANSI(resultContent)
					isErr := false
					if errFlag, ok := bm["is_error"].(bool); ok {
						isErr = errFlag
					}
					label := "[TOOL_OUTPUT]"
					if isErr {
						label = "[TOOL_ERROR]"
					}
					// Untruncated: this view is what the threshold and every
					// cache-cost comparison measure, and truncating it had the
					// guard reasoning about a quarter of the real request.
					parts = append(parts, label+" "+clean)
				} else if resultContent, ok := bm["content"].([]interface{}); ok {
					for _, rc := range resultContent {
						if rcm, ok := rc.(map[string]interface{}); ok {
							if rcm["type"] == "text" {
								if text, ok := rcm["text"].(string); ok {
									clean := stripANSI(text)
									parts = append(parts, "[TOOL_OUTPUT] "+clean)
								}
							}
						}
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// stripANSI removes ANSI escape sequences from terminal output.
func stripANSI(s string) string {
	var out []byte
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' {
			// Skip ESC sequence
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && ((s[i] < 'A' || s[i] > 'Z') && (s[i] < 'a' || s[i] > 'z') && s[i] != '?') {
					i++
				}
				if i < len(s) {
					i++ // skip final char
				}
			}
		} else {
			out = append(out, s[i])
			i++
		}
	}
	return string(out)
}

// parseResponsesMessages extracts simplified text messages from an OpenAI
// Responses API request body (used by Codex CLI/Desktop). Used only for
// state extraction input (the extractor only needs text to understand context).
func parseResponsesMessages(body []byte) []Message {
	var req struct {
		Instructions string          `json:"instructions,omitempty"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}

	var messages []Message
	if strings.TrimSpace(req.Instructions) != "" {
		messages = append(messages, Message{Role: "system", Content: req.Instructions})
	}

	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Action    json.RawMessage `json:"action"`
		Tools     json.RawMessage `json:"tools"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return messages
	}

	for _, item := range items {
		switch item.Type {
		// Codex ships its tool schema as an input item, not in the tools field.
		// It is tens of kilobytes and never changes, so leaving it unparsed hid
		// the single most cacheable part of the request from the guard.
		case "additional_tools":
			if text := rawToText(item.Tools); text != "" {
				messages = append(messages, Message{Role: "developer", Content: "[TOOLS] " + text})
			}
		case "message":
			text := extractTextFromResponsesContent(item.Content)
			if text == "" {
				continue
			}
			messages = append(messages, Message{Role: item.Role, Content: text})
		case "function_call":
			summary := "[TOOL_CALL " + item.Name + "] " + item.Arguments
			messages = append(messages, Message{Role: "assistant", Content: summary})
		// Codex sends custom_tool_call/local_shell_call, not function_call, and
		// carries the arguments in "input" or "action" instead of "arguments".
		// While these were unhandled the parser saw roughly an eighth of the
		// conversation, so the token count driving every threshold and cost
		// comparison was measured against the wrong body.
		case "custom_tool_call", "local_shell_call", "computer_call":
			args := item.Arguments
			if args == "" {
				args = rawToText(item.Input)
			}
			if args == "" {
				args = rawToText(item.Action)
			}
			name := item.Name
			if name == "" {
				name = item.Type
			}
			messages = append(messages, Message{
				Role:    "assistant",
				Content: "[TOOL_CALL " + name + "] " + args,
			})
		case "function_call_output", "custom_tool_call_output", "computer_call_output":
			outText := extractTextFromResponsesContent(item.Output)
			if outText == "" {
				outText = rawToText(item.Output)
			}
			if outText == "" {
				continue
			}
			// Untruncated on purpose: this view is what every token count,
			// threshold and cache-cost comparison is measured against, and a
			// truncated one had the guard reasoning about a third of the real
			// request. Extraction shrinks these separately.
			messages = append(messages, Message{
				Role:       "tool",
				Content:    "[TOOL_OUTPUT] " + stripANSI(outText),
				ToolCallID: item.CallID,
			})
		}
	}

	return messages
}

// rawToText renders a JSON value as text, unquoting it when it is a string.
func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// extractTextFromResponsesContent extracts plain text from a Responses API
// content field, which may be a plain string or an array of content parts
// (e.g. [{"type":"input_text","text":"..."}]).
func extractTextFromResponsesContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// rewriteResponsesBodyWithWindow rewrites an OpenAI Responses API request
// body, preserving recent input items raw (including function_call and
// function_call_output items). Only older history is replaced with the
// state summary. The top-level "instructions" field (system prompt) is left
// untouched since it isn't part of the input array.
// countRewriteTurns reports how many split points the body rewriter can see.
// It is the quantity compared against the step window, so when a rewrite comes
// back as a no-op this is the number that explains why.
func countRewriteTurns(body []byte, api APIFlavor) int {
	if api != APIResponses {
		return -1
	}
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		return 0
	}
	n := 0
	for _, raw := range req.Input[:len(req.Input)-1] {
		if isResponsesStepStart(raw) {
			n++
		}
	}
	return n
}

func rewriteResponsesBodyWithWindow(original []byte, result *Result, recentWindow int) ([]byte, error) {
	if recentWindow <= 0 {
		recentWindow = 4 // default: keep last 4 user turns
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(original, &body); err != nil {
		return nil, err
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(body["input"], &rawItems); err != nil {
		return nil, err
	}
	if len(rawItems) == 0 {
		return original, nil
	}

	// The last item is preserved as-is (last user message)
	lastItem := rawItems[len(rawItems)-1]
	bodyItems := rawItems[:len(rawItems)-1]
	if len(bodyItems) == 0 {
		return original, nil
	}

	// The preamble carries the tool schema and system prompt; it is forwarded
	// verbatim and is not eligible for summarisation.
	preamble := responsesPreambleEnd(bodyItems)
	conversation := bodyItems[preamble:]

	// Find the cut in agent steps, aligned so no kept output loses its call.
	splitPoint := responsesSplitPoint(conversation, recentWindow)
	if splitPoint <= 0 {
		return original, nil
	}
	recentRaw := conversation[splitPoint:]

	// Build state summary input items, one per frozen block.
	stateItems, err := responsesStateItems(result)
	if err != nil {
		return nil, err
	}

	// Rebuild: [state] + [recent raw...] + [last], with CCR spliced as late as
	// ccrInsertIndex allows — past the reach of any provider prompt cache, so
	// recovered context never busts a cached prefix.
	ccrRaw, err := marshalCCRResponseItems(result.CCRMessages)
	if err != nil {
		return nil, err
	}
	tail := make([]json.RawMessage, 0, len(recentRaw)+1)
	tail = append(tail, recentRaw...)
	tail = append(tail, lastItem)
	at := ccrInsertIndex(tail, isResponsesToolOutput)

	registryItem, err := registryItemJSON(result)
	if err != nil {
		return nil, err
	}

	newItems := make([]json.RawMessage, 0, preamble+1+len(stateItems)+len(tail)+len(ccrRaw))
	newItems = append(newItems, bodyItems[:preamble]...)
	newItems = append(newItems, stateItems...)
	if registryItem != nil {
		newItems = append(newItems, registryItem)
	}
	newItems = append(newItems, tail[:at]...)
	newItems = append(newItems, ccrRaw...)
	newItems = append(newItems, tail[at:]...)

	itemsJSON, err := json.Marshal(newItems)
	if err != nil {
		return nil, err
	}
	body["input"] = itemsJSON

	return json.Marshal(body)
}

// marshalCCRMessages renders recovered context as wire messages.
//
// Everything is emitted with role "user": recovered content is input to the
// model, and attributing it to the assistant would let stale history be read
// back as something the model itself said.
func marshalCCRMessages(msgs []Message) ([]json.RawMessage, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		raw, err := json.Marshal(map[string]string{
			"role":    "user",
			"content": m.Content,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// marshalCCRResponseItems renders recovered context as Responses API input
// items. Same placement and role rationale as marshalCCRMessages; only the
// wire shape differs.
func marshalCCRResponseItems(msgs []Message) ([]json.RawMessage, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		raw, err := json.Marshal(map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": m.Content},
			},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

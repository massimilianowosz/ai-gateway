package cache

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
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// cacheSettingsFor resolves whether semantic caching applies to this request,
// and any per-key threshold overrides to apply if so: a per-key override
// (from a response_cache policy bound to one agent environment) wins over
// the team-wide tenant_settings default either way; with neither set,
// caching defaults on and thresholds fall back to the engine's own config.
//
// Only the three per-lookup thresholds are overridable this way. The cache's
// backend, embedding model and TTL/max-entries janitor sweep are chosen once
// when the engine is constructed and have no per-key (or even per-team) hook
// to narrow — a response_cache policy touching only those fields is not
// enforceable per agent yet.
func cacheSettingsFor(ctx context.Context, db store.Store, ki *store.APIKey) (bool, ThresholdOverrides) {
	if ki.KeyHash != "" {
		keySettings, captured := auth.KeyRouteSettingsFromContext(ctx)
		var err error
		if !captured {
			keySettings, err = db.GetKeyRouteSettings(ctx, ki.KeyHash)
		}
		if err == nil && keySettings != nil {
			overrides := ThresholdOverrides{
				DirectThreshold: keySettings.CacheDirectThreshold,
				ReuseThreshold:  keySettings.CacheReuseThreshold,
				TweakThreshold:  keySettings.CacheTweakThreshold,
			}
			if keySettings.CacheEnabled != nil {
				return *keySettings.CacheEnabled, overrides
			}
			if ki.TeamID != "" {
				if settings, err := db.GetTenantSettings(ctx, ki.TeamID); err == nil && settings != nil {
					return settings.CacheEnabled, overrides
				}
			}
			return true, overrides
		}
	}
	if ki.TeamID != "" {
		if settings, err := db.GetTenantSettings(ctx, ki.TeamID); err == nil && settings != nil {
			return settings.CacheEnabled, ThresholdOverrides{}
		}
	}
	return true, ThresholdOverrides{}
}

// Middleware returns an HTTP middleware that intercepts chat completions
// and serves cached responses when similarity is above threshold.
//
// registry is used only to re-run the same model/provider authorization a
// cache miss would hit downstream in the proxy handler (auth.IsModelAllowed/
// IsProviderAllowed) — GW-02: a semantic-cache entry is looked up by team,
// not by key, so a response another key on the same team caused to be
// cached is otherwise served to any key whose prompt is similar enough,
// with no per-key model/provider check in between. A key denied a model
// must not be able to read that model's answers back out of a teammate's
// cache entry. nil is accepted (existing tests construct this middleware
// without a registry) and simply skips the check, matching this package's
// other optional dependencies.
func Middleware(c *Cache, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, registry *provider.Registry, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only cache chat completions and Anthropic messages
			isAnthropic := r.URL.Path == "/v1/messages" && r.Method == "POST"
			isOpenAI := r.URL.Path == "/v1/chat/completions" && r.Method == "POST"
			if !isAnthropic && !isOpenAI {
				next.ServeHTTP(w, r)
				return
			}

			// Check x-ubiquum-cache header: "false" skips lookup but still stores the response
			cacheHeader := r.Header.Get("x-ubiquum-cache")
			skipLookup := cacheHeader == "false"

			// A per-key override (set by a response_cache policy bound to one
			// agent environment) wins over the team-wide default either way,
			// the same precedence HiveRoute and feedback already use.
			var thresholdOverrides ThresholdOverrides
			if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
				enabled, overrides := cacheSettingsFor(r.Context(), db, ki)
				thresholdOverrides = overrides
				if !enabled {
					next.ServeHTTP(w, r)
					return
				}
			}

			// Read body (we need it for both cache lookup and forwarding)
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			// Parse messages based on API format
			var model string
			var messages []Message
			var stream bool
			var hasTools bool

			if isAnthropic {
				model, messages, stream = parseAnthropicBody(bodyBytes)
				// Check for tools in the request
				var raw struct {
					Tools json.RawMessage `json:"tools"`
				}
				if json.Unmarshal(bodyBytes, &raw) == nil && len(raw.Tools) > 2 {
					hasTools = true
				}
			} else {
				var req struct {
					Model    string          `json:"model"`
					Messages []Message       `json:"messages"`
					Stream   bool            `json:"stream"`
					Tools    json.RawMessage `json:"tools"`
				}
				if err := json.Unmarshal(bodyBytes, &req); err == nil {
					model, messages, stream = req.Model, req.Messages, req.Stream
					if len(req.Tools) > 2 {
						hasTools = true
					}
				}
			}

			if len(messages) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Skip cache for agentic requests (with tools) — each response is context-dependent
			if hasTools {
				next.ServeHTTP(w, r)
				return
			}

			// A key denied this model, or every provider that serves it, must
			// never read the answer back out of the cache either — the lookup
			// below is scoped by team, not by key, so a teammate's own
			// permitted traffic could otherwise leak a cached response this
			// key could never have obtained directly. Falls through to next
			// rather than answering the denial here itself, so the caller
			// gets the exact same status/body the proxy handler's own check
			// produces on a genuine cache miss — one behavior, not two.
			// Residency is checked the same way: the cache is not scoped by
			// data residency, so an EU-only key could otherwise read back a
			// non-EU-only teammate's cached answer for the same model
			// (CTX-03).
			residencyAllowed := true
			if registry != nil {
				allEU, hasDeployments := registry.AllDeploymentsEU(model)
				residencyAllowed = auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments)
			}
			if registry != nil && (!auth.IsModelAllowed(r.Context(), model, registry.IsRestricted(model)) ||
				!auth.IsProviderAllowed(r.Context(), registry.ProvidersFor(model)) ||
				!residencyAllowed) {
				next.ServeHTTP(w, r)
				return
			}

			// Scope model by format to avoid cross-format cache collisions
			cacheModel := model
			if isAnthropic {
				cacheModel = "@anthropic/" + model
			}

			// Get team context
			teamID := ""
			keyPrefix := ""
			if ki := auth.KeyInfoFromContext(r.Context()); ki != nil {
				teamID = ki.TeamID
				keyPrefix = ki.KeyPrefix
			}

			// Read optional client metadata (for benchmarking/tracing)
			cacheMeta := r.Header.Get("x-ubiquum-cache-meta")

			// Cache lookup (skip if x-ubiquum-cache: false)
			if !skipLookup {
				start := time.Now()
				result, err := c.Lookup(r.Context(), teamID, cacheModel, messages, thresholdOverrides)
				lookupMs := time.Since(start).Milliseconds()

				// Track embedding cost attributed to the caller's key (@hivecache)
				if err == nil && result != nil && result.EmbedTokens > 0 {
					go func() {
						keyInfo := auth.KeyInfoFromContext(r.Context())
						record := store.SpendRecord{
							Model:        c.EmbeddingModel(),
							Provider:     "@hivecache",
							PromptTokens: result.EmbedTokens,
							TotalTokens:  result.EmbedTokens,
							Duration:     lookupMs,
							Status:       200,
							CreatedAt:    time.Now(),
						}
						if pc != nil {
							record.Cost = pc.Cost(c.EmbeddingModel(), result.EmbedTokens, 0)
						}
						if keyInfo != nil {
							record.ApplyIdentity(keyInfo)
						}
						spender.Record(record)
					}()
				}

				if err != nil {
					logger.Warn("cache lookup error", "error", err, "duration_ms", lookupMs)
				} else if result.Hit && (result.Band == BandDirect || result.Band == BandReuse) {
					// Cache HIT — serve directly
					w.Header().Set("x-hivecache-status", string(result.Band))
					w.Header().Set("x-hivecache-score", formatFloat(result.Score))
					w.Header().Set("x-hivecache-tokens-saved", formatInt(result.TokensSaved))
					if result.Entry.Meta != "" {
						w.Header().Set("x-hivecache-matched-meta", result.Entry.Meta)
					}
					if stream {
						// Replay cached response as SSE
						if isAnthropic {
							serveAnthropicSSEFromJSON(w, result.Entry.Response, model)
						} else {
							serveSSE(w, result.Entry.Response)
						}
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(result.Entry.Response))
					}

					// Log metric async
					go func() {
						_ = db.LogCacheMetric(context.Background(), store.CacheMetric{
							TeamID:      teamID,
							KeyPrefix:   keyPrefix,
							Model:       model,
							Band:        string(result.Band),
							Score:       result.Score,
							TokensSaved: result.TokensSaved,
							LatencyMs:   lookupMs,
							CreatedAt:   time.Now(),
						})
					}()

					logger.Info("cache hit",
						"band", string(result.Band),
						"score", result.Score,
						"tokens_saved", result.TokensSaved,
						"model", model,
						"duration_ms", lookupMs,
					)
					return
				} else if result != nil && result.Band == BandTweak {
					// TWEAK band — cost-benefit analysis using real pricing
					// Compare: cost of calling original model vs cost of tweak call
					tweakModel := c.TweakModel()
					tweakInputTokens := 150 + result.TokensSaved // system prompt + cached content + new query
					tweakOutputTokens := result.TokensSaved / 2  // output ≈ completion portion

					originalPrice, hasOrigPrice := pc.GetPrice(model)
					tweakPrice, hasTweakPrice := pc.GetPrice(tweakModel)

					var worthIt bool
					if hasOrigPrice && hasTweakPrice {
						if originalPrice.InputCostPerToken == 0 && originalPrice.OutputCostPerToken == 0 {
							// Free/local models → only tweak if enough tokens to justify the extra call
							worthIt = result.TokensSaved > 100
						} else {
							// Real cost comparison
							savingsCost := float64(result.TokensSaved/2)*originalPrice.InputCostPerToken +
								float64(result.TokensSaved/2)*originalPrice.OutputCostPerToken
							tweakCost := float64(tweakInputTokens)*tweakPrice.InputCostPerToken +
								float64(tweakOutputTokens)*tweakPrice.OutputCostPerToken
							worthIt = savingsCost > tweakCost
							logger.Debug("cache tweak cost analysis",
								"savings_cost", savingsCost,
								"tweak_cost", tweakCost,
								"ratio", savingsCost/max(tweakCost, 1e-12),
							)
						}
					} else {
						// No pricing info → only tweak if enough tokens to justify the call
						worthIt = result.TokensSaved > 100
					}

					if !worthIt {
						logger.Info("cache tweak skipped (not worth it)",
							"savings_tokens", result.TokensSaved,
							"model", model,
							"tweak_model", tweakModel,
							"score", result.Score,
						)
						w.Header().Set("x-hivecache-status", "MISS")
						w.Header().Set("x-hivecache-score", formatFloat(result.Score))
						go func() {
							_ = db.LogCacheMetric(context.Background(), store.CacheMetric{
								TeamID:    teamID,
								KeyPrefix: keyPrefix,
								Model:     model,
								Band:      "TWEAK_SKIP",
								Score:     result.Score,
								LatencyMs: lookupMs,
								CreatedAt: time.Now(),
							})
						}()
					} else {
						w.Header().Set("x-hivecache-status", "TWEAK")
						w.Header().Set("x-hivecache-score", formatFloat(result.Score))

						newQuery := ExtractQuery(messages)
						tweaked, err := c.Tweak(r.Context(), newQuery, result.Entry)
						if err != nil {
							// Tweak failed — fall through to upstream
							logger.Warn("cache tweak failed, forwarding upstream", "error", err, "score", result.Score)
							go func() {
								_ = db.LogCacheMetric(context.Background(), store.CacheMetric{
									TeamID:    teamID,
									KeyPrefix: keyPrefix,
									Model:     model,
									Band:      "TWEAK_FAIL",
									Score:     result.Score,
									LatencyMs: lookupMs,
									CreatedAt: time.Now(),
								})
							}()
						} else {
							// Serve the tweaked response
							tweakMs := time.Since(start).Milliseconds()
							w.Header().Set("Content-Type", "application/json")
							w.Header().Set("x-hivecache-tokens-saved", formatInt(result.TokensSaved))
							if result.Entry.Meta != "" {
								w.Header().Set("x-hivecache-matched-meta", result.Entry.Meta)
							}

							if stream {
								if isAnthropic {
									serveAnthropicSSEFromJSON(w, tweaked, model)
								} else {
									serveSSE(w, tweaked)
								}
							} else {
								w.Header().Set("Content-Type", "application/json")
								_, _ = w.Write([]byte(tweaked))
							}

							// Store tweaked response so next identical query hits DIRECT
							tweakedTokens := extractTokenCount(tweaked)
							go func() {
								if err := c.Store(context.Background(), teamID, cacheModel, messages, tweaked, tweakedTokens, cacheMeta); err != nil {
									logger.Warn("cache store tweaked response error", "error", err)
								} else {
									logger.Info("cache stored tweaked response", "model", model, "tokens", tweakedTokens)
								}
							}()

							go func() {
								_ = db.LogCacheMetric(context.Background(), store.CacheMetric{
									TeamID:      teamID,
									KeyPrefix:   keyPrefix,
									Model:       model,
									Band:        "TWEAK",
									Score:       result.Score,
									TokensSaved: result.TokensSaved,
									LatencyMs:   tweakMs,
									CreatedAt:   time.Now(),
								})
							}()

							// Log tweak spend attributed to caller's key
							go func() {
								keyInfo := auth.KeyInfoFromContext(r.Context())
								record := store.SpendRecord{
									Model:            c.TweakModel(),
									Provider:         "@hivecache",
									PromptTokens:     tweakInputTokens,
									CompletionTokens: tweakOutputTokens,
									TotalTokens:      tweakInputTokens + tweakOutputTokens,
									Duration:         tweakMs - lookupMs,
									Status:           200,
									CreatedAt:        time.Now(),
								}
								if pc != nil {
									record.Cost = pc.Cost(c.TweakModel(), tweakInputTokens, tweakOutputTokens)
								}
								if keyInfo != nil {
									record.ApplyIdentity(keyInfo)
								}
								spender.Record(record)
							}()

							logger.Info("cache tweak hit",
								"score", result.Score,
								"tokens_saved", result.TokensSaved,
								"model", model,
								"tweak_ms", tweakMs-lookupMs,
								"total_ms", tweakMs,
							)
							return
						}
					}
				} else if result != nil {
					// MISS — set header for observability
					w.Header().Set("x-hivecache-status", "MISS")
					if result.Score > 0 {
						w.Header().Set("x-hivecache-score", formatFloat(result.Score))
					}

					// Log miss metric async
					go func() {
						_ = db.LogCacheMetric(context.Background(), store.CacheMetric{
							TeamID:    teamID,
							KeyPrefix: keyPrefix,
							Model:     model,
							Band:      "MISS",
							Score:     result.Score,
							LatencyMs: lookupMs,
							CreatedAt: time.Now(),
						})
					}()
					logger.Info("cache miss", "model", model, "score", result.Score, "duration_ms", lookupMs)
				}
			}

			// Forward to upstream, capture response
			cw := &captureWriter{ResponseWriter: w, buf: &bytes.Buffer{}, statusCode: 200}
			next.ServeHTTP(cw, r)

			// Store response in cache (async, only on success)
			if cw.statusCode >= 200 && cw.statusCode < 300 && cw.buf.Len() > 0 {
				response := cw.buf.String()
				var totalTokens int

				if stream {
					// For streaming responses, reconstruct a JSON response from SSE events
					if isAnthropic {
						if jsonResp, tokens := reconstructAnthropicJSON(response, model); jsonResp != "" {
							response = jsonResp
							totalTokens = tokens
						} else {
							// Could not reconstruct; skip storing
							return
						}
					} else {
						// OpenAI streaming: skip storing (format not easily reconstructable)
						return
					}
				} else {
					if isAnthropic {
						totalTokens = extractAnthropicTokenCount(response)
					} else {
						totalTokens = extractTokenCount(response)
					}
				}

				go func() {
					if err := c.Store(context.Background(), teamID, cacheModel, messages, response, totalTokens, cacheMeta); err != nil {
						logger.Warn("cache store error", "error", err)
					} else {
						logger.Info("cache stored", "model", model, "tokens", totalTokens)
					}
				}()
			}
		})
	}
}

// captureWriter wraps http.ResponseWriter to capture the response body.
// It also implements http.Flusher to support streaming responses.
type captureWriter struct {
	http.ResponseWriter
	buf        *bytes.Buffer
	statusCode int
}

func (cw *captureWriter) WriteHeader(code int) {
	cw.statusCode = code
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *captureWriter) Write(b []byte) (int, error) {
	cw.buf.Write(b)
	return cw.ResponseWriter.Write(b)
}

func (cw *captureWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// serveSSE replays a cached JSON response as an SSE stream.
func serveSSE(w http.ResponseWriter, response string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	// Send the full response as a single SSE data event
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write([]byte(response))
	_, _ = w.Write([]byte("\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func extractTokenCount(response string) int {
	var resp struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(response), &resp); err == nil {
		return resp.Usage.TotalTokens
	}
	return 0
}

func formatFloat(f float64) string {
	return fmt.Sprintf("%.4f", f)
}

func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}

// --- Anthropic format helpers ---

// parseAnthropicBody extracts model, messages, and stream from an Anthropic /v1/messages request body.
func parseAnthropicBody(body []byte) (model string, messages []Message, stream bool) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
		System interface{} `json:"system,omitempty"`
		Stream bool        `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", nil, false
	}

	// Add system message if present
	if req.System != nil {
		sysText := extractAnthropicText(req.System)
		if sysText != "" {
			messages = append(messages, Message{Role: "system", Content: sysText})
		}
	}

	// Convert Anthropic messages (content can be string or []blocks)
	for _, m := range req.Messages {
		text := extractAnthropicText(m.Content)
		if text != "" {
			messages = append(messages, Message{Role: m.Role, Content: text})
		}
	}

	return req.Model, messages, req.Stream
}

// extractAnthropicText extracts plain text from Anthropic content (string or []content blocks).
func extractAnthropicText(content interface{}) string {
	if content == nil {
		return ""
	}

	// Simple string case
	if s, ok := content.(string); ok {
		return s
	}

	// Array of content blocks
	if blocks, ok := content.([]interface{}); ok {
		var parts []string
		for _, block := range blocks {
			if bm, ok := block.(map[string]interface{}); ok {
				if bm["type"] == "text" {
					if text, ok := bm["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// extractAnthropicTokenCount extracts token count from an Anthropic JSON response.
func extractAnthropicTokenCount(response string) int {
	var resp struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(response), &resp); err == nil {
		return resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	return 0
}

// serveAnthropicSSEFromJSON converts a stored Anthropic JSON response into SSE events for streaming clients.
func serveAnthropicSSEFromJSON(w http.ResponseWriter, response string, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	// Parse the stored JSON response
	var resp struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(response), &resp); err != nil {
		// Fallback: send as raw data
		_, _ = w.Write([]byte("data: " + response + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	msgID := resp.ID
	if msgID == "" {
		msgID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}

	writeEvent := func(event string, data interface{}) {
		encoded, _ := json.Marshal(data)
		_, _ = w.Write([]byte("event: " + event + "\ndata: "))
		_, _ = w.Write(encoded)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// message_start
	writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []interface{}{},
			"model":   model,
			"usage":   map[string]int{"input_tokens": resp.Usage.InputTokens, "output_tokens": 0},
		},
	})

	// For each content block
	for i, block := range resp.Content {
		if block.Type != "text" {
			continue
		}

		// content_block_start
		writeEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         i,
			"content_block": map[string]string{"type": "text", "text": ""},
		})

		// content_block_delta (send full text as single delta)
		writeEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": i,
			"delta": map[string]string{"type": "text_delta", "text": block.Text},
		})

		// content_block_stop
		writeEvent("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": i,
		})
	}

	// message_delta
	stopReason := resp.StopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	writeEvent("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason},
		"usage": map[string]int{"output_tokens": resp.Usage.OutputTokens},
	})

	// message_stop
	writeEvent("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// reconstructAnthropicJSON reconstructs a non-streaming Anthropic JSON response from captured SSE events.
func reconstructAnthropicJSON(sseData string, model string) (string, int) {
	var textContent strings.Builder
	var inputTokens, outputTokens int
	var stopReason string
	var msgID string

	// Parse SSE events line by line
	lines := strings.Split(sseData, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "message_start":
			if msg, ok := event["message"].(map[string]interface{}); ok {
				if id, ok := msg["id"].(string); ok {
					msgID = id
				}
				if usage, ok := msg["usage"].(map[string]interface{}); ok {
					if v, ok := usage["input_tokens"].(float64); ok {
						inputTokens = int(v)
					}
				}
			}
		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]interface{}); ok {
				if text, ok := delta["text"].(string); ok {
					textContent.WriteString(text)
				}
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]interface{}); ok {
				if sr, ok := delta["stop_reason"].(string); ok {
					stopReason = sr
				}
			}
			if usage, ok := event["usage"].(map[string]interface{}); ok {
				if v, ok := usage["output_tokens"].(float64); ok {
					outputTokens = int(v)
				}
			}
		}
	}

	text := textContent.String()
	if text == "" {
		return "", 0
	}

	if msgID == "" {
		msgID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}

	// Build non-streaming Anthropic response JSON
	resp := map[string]interface{}{
		"id":   msgID,
		"type": "message",
		"role": "assistant",
		"content": []map[string]string{
			{"type": "text", "text": text},
		},
		"model":       model,
		"stop_reason": stopReason,
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		return "", 0
	}
	return string(encoded), inputTokens + outputTokens
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/admin"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/cache"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/livezone"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/proxy"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/webhook"
)

// RegisterRoutes sets up all HTTP routes on the server.
func RegisterRoutes(s *Server, registry *provider.Registry, rt *router.Router, authMiddleware *auth.Middleware, db store.Store, pc *pricing.Calculator, spender *spend.BatchWriter, webhooks *webhook.Dispatcher, semanticCache *cache.Cache, metrics *middleware.Metrics, extraMiddlewares ...middleware.Middleware) {
	mux := s.Mux()

	// Concurrency limiter (backpressure for high throughput)
	maxConcurrent := int64(s.cfg.Server.MaxConcurrent)
	if maxConcurrent <= 0 {
		maxConcurrent = 10000
	}
	sem := middleware.NewSemaphore(maxConcurrent)

	// Per-key rate limiter (token bucket, RPM)
	rateLimiter := middleware.NewRateLimiter()

	// Prometheus metrics collector (created by the caller so middlewares built
	// before route registration — HiveState — can share it).
	if metrics == nil {
		metrics = middleware.NewMetrics()
	}

	// Pre/post request hooks
	hooks := middleware.NewHooks(s.cfg.Hooks.PreRequest, s.cfg.Hooks.PostRequest, s.logger)

	// Scoped upstream-token forwarding (used by local shims such as ubiquum-cli).
	// This must run after Ubiquum auth and before hooks/request logging so the
	// secret forwarding header is stripped before generic middleware sees it.
	upstreamTokenForwarding := auth.NewUpstreamTokenForwardingMiddleware(auth.UpstreamTokenForwardingOptions{
		Enabled:          s.cfg.Server.UpstreamTokenForwarding.Enabled,
		Header:           s.cfg.Server.UpstreamTokenForwarding.Header,
		ProviderHeader:   s.cfg.Server.UpstreamTokenForwarding.ProviderHeader,
		AccountIDHeader:  s.cfg.Server.UpstreamTokenForwarding.AccountIDHeader,
		AllowedProviders: s.cfg.Server.UpstreamTokenForwarding.AllowedProviders,
	})
	// A subscriber may use their provider's own model name: the scoped token
	// says which provider is answering, so that name is unambiguous and must
	// win over the public alias, which points at a metered deployment.
	modelAliases := middleware.ModelAliases(registry.CanonicalModel,
		func(ctx context.Context, model string) string {
			if upstream := auth.ScopedUpstreamProvider(ctx); upstream != "" {
				if name, ok := registry.UpstreamAlias(upstream, model); ok {
					return name
				}
			}
			return registry.CanonicalModel(model)
		})

	// LLM Guard content guardrail — inline engine or legacy HTTP
	var guardEngine *guardrail.Engine
	if s.cfg.Guardrail.HasInlineScanners() {
		var scanners []guardrail.Scanner
		if s.cfg.Guardrail.PII.Enabled {
			scanners = append(scanners, guardrail.NewPIIScanner())
		}
		if s.cfg.Guardrail.Secrets.Enabled {
			scanners = append(scanners, guardrail.NewSecretsScanner())
		}
		if s.cfg.Guardrail.Injection.Enabled {
			scanners = append(scanners, guardrail.NewInjectionScanner())
		}
		if s.cfg.Guardrail.Moderation.Enabled && s.cfg.Guardrail.Moderation.Model != "" {
			scanners = append(scanners, guardrail.NewModerationScanner(registry, s.cfg.Guardrail.Moderation.Model, s.logger))
		}
		if len(scanners) > 0 {
			guardEngine = guardrail.NewEngine(scanners, db, pc, spender, s.cfg.Guardrail.FailOpen, s.logger)
		}
	}
	guard := middleware.NewGuardrail(s.cfg.Guardrail, guardEngine, db, s.logger)

	// Feedback collection middleware
	feedback := middleware.NewFeedback(s.cfg.Feedback, db, s.logger)

	// Health + readiness probes (no auth, no semaphore)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /ready", handleReady(registry, db))
	mux.HandleFunc("GET /metrics", metrics.Handler())

	// OpenAI-compatible API (requires auth)
	completionMiddlewares := []middleware.Middleware{
		middleware.Recovery(s.logger),
		middleware.RequestID,
		middleware.Logging(s.logger),
		metrics.Middleware,
		middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB) << 20),
		authMiddleware.Authenticate,
		upstreamTokenForwarding,
		guard.SensitiveDataMiddleware,
		modelAliases,
	}
	// The optional middlewares — workflow, cache, hivestate — go after the
	// rate limiter and the concurrency semaphore, not before. Each of them can
	// answer a request on its own or make an outbound call of its own (the
	// workflow callout allows 30s), and ahead of those two that work was
	// neither counted nor bounded: a burst piled up unbounded in-flight calls,
	// and a workflow block/respond never touched the caller's rate limit.
	// Their order relative to each other is preserved, so workflow still runs
	// before cache and hivestate see the prompt, and every guardrail runs
	// before any of them.
	completionMiddlewares = append(completionMiddlewares,
		rateLimiter.Middleware,
		sem.Middleware,
		authMiddleware.RecheckGovernance,
		// Feedback sits under the limits, not ahead of them: it can answer a
		// request on its own and holds per-key state, so it is work that has to
		// be counted and bounded like any other.
		feedback.Middleware,
		// Injection, toxicity and abuse belong here for the same reason
		// sensitive-data runs earlier: a cached answer is returned without ever
		// reaching what sits behind the cache, so a jailbreak answered once was
		// served from cache on every later send with the scanner never
		// consulted. Under the limits rather than above them, because the
		// moderation scanner calls a model and that call has to be bounded.
		guard.Middleware,
	)
	completionMiddlewares = append(completionMiddlewares, extraMiddlewares...)
	completionMiddlewares = append(completionMiddlewares,
		hooks.PreRequest,
		hooks.PostRequest,
		authMiddleware.RecheckGovernance,
	)
	mux.Handle("POST /v1/chat/completions",
		middleware.Chain(
			proxy.NewCompletionHandler(registry, rt, s.logger, db, pc, spender, metrics),
			completionMiddlewares...,
		),
	)

	// OpenAI-compatible Files API. Upload bodies are streamed directly to the
	// selected provider; Ubiquum stores only tenant-scoped id metadata.
	filesHandler := proxy.NewFilesHandler(registry, db, s.logger)
	filesMiddlewares := []middleware.Middleware{
		middleware.Recovery(s.logger),
		middleware.RequestID,
		middleware.Logging(s.logger),
		metrics.Middleware,
		middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB) << 20),
		authMiddleware.Authenticate,
		upstreamTokenForwarding,
		modelAliases,
		rateLimiter.Middleware,
		sem.Middleware,
		authMiddleware.RecheckGovernance,
	}
	for _, route := range []struct {
		method  string
		path    string
		handler http.Handler
	}{
		{http.MethodPost, "/v1/files", http.HandlerFunc(filesHandler.ServeCollection)},
		{http.MethodGet, "/v1/files", http.HandlerFunc(filesHandler.ServeCollection)},
		{http.MethodGet, "/v1/files/{file_id}", http.HandlerFunc(filesHandler.ServeObject)},
		{http.MethodDelete, "/v1/files/{file_id}", http.HandlerFunc(filesHandler.ServeObject)},
		{http.MethodGet, "/v1/files/{file_id}/content", http.HandlerFunc(filesHandler.ServeContent)},
	} {
		mux.Handle(route.method+" "+route.path, middleware.Chain(route.handler, filesMiddlewares...))
	}

	mux.Handle("GET /v1/models",
		middleware.Chain(
			proxy.NewModelsHandler(registry, db),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			authMiddleware.Authenticate,
			upstreamTokenForwarding,
			modelAliases,
		),
	)

	// GET /v1/models/{model} — single model detail
	mux.Handle("GET /v1/models/{model}",
		middleware.Chain(
			proxy.NewModelDetailHandler(registry),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			authMiddleware.Authenticate,
			upstreamTokenForwarding,
			modelAliases,
		),
	)

	// GET /v1/model/aliases — operator-only alias map, used by the portal to
	// match stored whitelists against published model ids.
	mux.Handle("GET /v1/model/aliases",
		middleware.Chain(
			proxy.NewModelAliasesHandler(registry),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			authMiddleware.Authenticate,
		),
	)

	// POST /v1/messages — Anthropic Messages API (Claude Code, Anthropic SDK)
	anthropicMiddlewares := []middleware.Middleware{
		middleware.Recovery(s.logger),
		middleware.RequestID,
		middleware.Logging(s.logger),
		metrics.Middleware,
		middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB) << 20),
		authMiddleware.Authenticate,
		upstreamTokenForwarding,
		guard.SensitiveDataMiddleware,
		modelAliases,
	}
	// After the rate limiter and semaphore; see the completions chain above.
	anthropicMiddlewares = append(anthropicMiddlewares,
		rateLimiter.Middleware,
		sem.Middleware,
		authMiddleware.RecheckGovernance,
		// Feedback sits under the limits, not ahead of them: it can answer a
		// request on its own and holds per-key state, so it is work that has to
		// be counted and bounded like any other.
		feedback.Middleware,
		// Injection, toxicity and abuse belong here for the same reason
		// sensitive-data runs earlier: a cached answer is returned without ever
		// reaching what sits behind the cache, so a jailbreak answered once was
		// served from cache on every later send with the scanner never
		// consulted. Under the limits rather than above them, because the
		// moderation scanner calls a model and that call has to be bounded.
		guard.Middleware,
	)
	anthropicMiddlewares = append(anthropicMiddlewares, extraMiddlewares...)
	anthropicMiddlewares = append(anthropicMiddlewares,
		hooks.PreRequest,
		hooks.PostRequest,
		authMiddleware.RecheckGovernance,
	)
	mux.Handle("POST /v1/messages",
		middleware.Chain(
			proxy.NewAnthropicHandler(registry, rt, s.logger, db, pc, spender, metrics),
			anthropicMiddlewares...,
		),
	)

	// POST /v1/responses — OpenAI Responses API (Codex CLI/Desktop, Responses SDK)
	responsesMiddlewares := []middleware.Middleware{
		middleware.Recovery(s.logger),
		middleware.RequestID,
		middleware.Logging(s.logger),
		metrics.Middleware,
		middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB) << 20),
		authMiddleware.Authenticate,
		upstreamTokenForwarding,
		guard.SensitiveDataMiddleware,
		modelAliases,
	}
	// After the rate limiter and semaphore; see the completions chain above.
	responsesMiddlewares = append(responsesMiddlewares,
		rateLimiter.Middleware,
		sem.Middleware,
		authMiddleware.RecheckGovernance,
		// Feedback sits under the limits, not ahead of them: it can answer a
		// request on its own and holds per-key state, so it is work that has to
		// be counted and bounded like any other.
		feedback.Middleware,
		// Injection, toxicity and abuse belong here for the same reason
		// sensitive-data runs earlier: a cached answer is returned without ever
		// reaching what sits behind the cache, so a jailbreak answered once was
		// served from cache on every later send with the scanner never
		// consulted. Under the limits rather than above them, because the
		// moderation scanner calls a model and that call has to be bounded.
		guard.Middleware,
	)
	responsesMiddlewares = append(responsesMiddlewares, extraMiddlewares...)
	responsesMiddlewares = append(responsesMiddlewares,
		hooks.PreRequest,
		hooks.PostRequest,
		authMiddleware.RecheckGovernance,
	)
	responsesHandler := proxy.NewResponsesHandler(registry, rt, s.logger, db, pc, spender, metrics)
	responsesHandler.SetResponseTTL(s.cfg.Responses.TTL)
	responsesHandler.SetBackgroundTimeout(s.cfg.Responses.BackgroundTimeout)
	mux.Handle("POST /v1/responses",
		middleware.Chain(
			responsesHandler,
			responsesMiddlewares...,
		),
	)

	// Stateful half of the Responses API, plus the conversations it reads from.
	// These read and mutate stored state rather than calling a model, so they
	// skip the model-facing middlewares: guardrail, cache and the hooks.
	//
	// They do not skip the resource limits. Several of these routes accept a
	// body and read it whole, so without MaxBody any authenticated key can
	// stream an unbounded request into memory; the rate limiter and the
	// concurrency semaphore likewise bound writes to shared state, which is
	// what these endpoints do.
	responsesStateMiddlewares := []middleware.Middleware{
		middleware.Recovery(s.logger),
		middleware.RequestID,
		middleware.Logging(s.logger),
		middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB) << 20),
		authMiddleware.Authenticate,
		upstreamTokenForwarding,
		rateLimiter.Middleware,
		sem.Middleware,
	}
	conversationsHandler := proxy.NewConversationsHandler(s.logger, db)
	conversationsHandler.SetConversationTTL(s.cfg.Responses.TTL)

	for _, route := range []struct {
		method  string
		path    string
		handler http.Handler
	}{
		{http.MethodGet, "/v1/responses/{response_id}", http.HandlerFunc(responsesHandler.ServeObject)},
		{http.MethodDelete, "/v1/responses/{response_id}", http.HandlerFunc(responsesHandler.ServeObject)},
		{http.MethodPost, "/v1/responses/{response_id}/cancel", http.HandlerFunc(responsesHandler.ServeCancel)},
		{http.MethodGet, "/v1/responses/{response_id}/input_items", http.HandlerFunc(responsesHandler.ServeInputItems)},

		{http.MethodPost, "/v1/conversations", http.HandlerFunc(conversationsHandler.ServeCollection)},
		{http.MethodGet, "/v1/conversations/{conversation_id}", http.HandlerFunc(conversationsHandler.ServeObject)},
		{http.MethodPost, "/v1/conversations/{conversation_id}", http.HandlerFunc(conversationsHandler.ServeObject)},
		{http.MethodDelete, "/v1/conversations/{conversation_id}", http.HandlerFunc(conversationsHandler.ServeObject)},
		{http.MethodGet, "/v1/conversations/{conversation_id}/items", http.HandlerFunc(conversationsHandler.ServeItems)},
		{http.MethodPost, "/v1/conversations/{conversation_id}/items", http.HandlerFunc(conversationsHandler.ServeItems)},
		{http.MethodGet, "/v1/conversations/{conversation_id}/items/{item_id}", http.HandlerFunc(conversationsHandler.ServeItem)},
		{http.MethodDelete, "/v1/conversations/{conversation_id}/items/{item_id}", http.HandlerFunc(conversationsHandler.ServeItem)},
	} {
		mux.Handle(route.method+" "+route.path, middleware.Chain(route.handler, responsesStateMiddlewares...))
	}

	// POST /v1/embeddings — OpenAI Embeddings API
	mux.Handle("POST /v1/embeddings",
		middleware.Chain(
			proxy.NewEmbeddingsHandler(registry, rt, s.logger, pc, spender),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			metrics.Middleware,
			middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB)<<20),
			authMiddleware.Authenticate,
			upstreamTokenForwarding,
			modelAliases,
			rateLimiter.Middleware,
			sem.Middleware,
			authMiddleware.RecheckGovernance,
		),
	)

	// POST /v1/completions — Legacy text completions
	mux.Handle("POST /v1/completions",
		middleware.Chain(
			proxy.NewCompletionsHandler(registry, rt, s.logger, pc, spender, metrics),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			metrics.Middleware,
			middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB)<<20),
			authMiddleware.Authenticate,
			upstreamTokenForwarding,
			guard.SensitiveDataMiddleware,
			modelAliases,
			rateLimiter.Middleware,
			sem.Middleware,
			authMiddleware.RecheckGovernance,
			guard.Middleware,
			hooks.PreRequest,
			hooks.PostRequest,
			authMiddleware.RecheckGovernance,
		),
	)

	// POST /v1/moderations — Content moderation
	mux.Handle("POST /v1/moderations",
		middleware.Chain(
			proxy.NewModerationsHandler(registry, s.logger),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			// Every route that reads a body needs a ceiling on it. This one had
			// none: an authenticated caller could stream an unbounded request
			// into memory.
			middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB)<<20),
			authMiddleware.Authenticate,
			modelAliases,
			rateLimiter.Middleware,
			sem.Middleware,
			authMiddleware.RecheckGovernance,
		),
	)

	// POST /analyze/batch/beta/litellm_basic_guardrail_api — backward-compatible
	// guardrail endpoint for backend workflow nodes (replaces external llm-guard-service)
	if guardEngine != nil {
		mux.Handle("POST /analyze/batch/beta/litellm_basic_guardrail_api",
			middleware.Chain(
				guardrail.NewAnalyzeHandler(guardEngine, s.logger),
				middleware.Recovery(s.logger),
				middleware.RequestID,
				middleware.Logging(s.logger),
				middleware.MaxBody(int64(s.cfg.Server.MaxRequestSizeMB)<<20),
			),
		)
	}

	// POST /v1/images/generations — Image generation (DALL-E, etc.)
	mux.Handle("POST /v1/images/generations",
		middleware.Chain(
			proxy.NewImageGenerationsHandler(registry, s.logger),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			middleware.MaxBody(10<<20),
			authMiddleware.Authenticate,
			modelAliases,
			rateLimiter.Middleware,
			sem.Middleware,
			authMiddleware.RecheckGovernance,
		),
	)

	// POST /v1/audio/speech — Text-to-speech
	mux.Handle("POST /v1/audio/speech",
		middleware.Chain(
			proxy.NewAudioSpeechHandler(registry, s.logger),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			middleware.MaxBody(1<<20),
			authMiddleware.Authenticate,
			modelAliases,
			rateLimiter.Middleware,
			authMiddleware.RecheckGovernance,
		),
	)

	// POST /v1/audio/transcriptions — Speech-to-text (Whisper)
	mux.Handle("POST /v1/audio/transcriptions",
		middleware.Chain(
			proxy.NewAudioTranscriptionHandler(registry, s.logger),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			middleware.MaxBody(25<<20),
			authMiddleware.Authenticate,
			modelAliases,
			rateLimiter.Middleware,
			authMiddleware.RecheckGovernance,
		),
	)

	// Admin API (requires master key)
	adminHandler := admin.NewHandler(db, s.cfg.Server.MasterKey, s.logger, webhooks)
	adminHandler.SetConfig(s.cfg)
	if s.Console != nil {
		adminHandler.SetAdminSessionValidator(s.Console.ValidateAdminSession)
		s.Console.Register(mux, adminHandler.RequireMasterKey)
	}

	// Edge model runtime registration (master key required)
	edgeModelsHandler := proxy.NewEdgeModelsHandler(registry, s.cfg.Server.MasterKey)
	mux.HandleFunc("POST /model/new", edgeModelsHandler.ServeAdd)
	mux.HandleFunc("POST /model/delete", edgeModelsHandler.ServeDelete)

	// GET /v1/hive/retrieve/{id} — content a lossy live-compression transform
	// removed. The prompt carries the pointer, so an agent fetches it with the
	// shell it already has and no tool has to be injected into the request.
	if vault := livezone.DefaultVault(); vault != nil {
		mux.Handle("GET "+livezone.RetrievePath+"{id}",
			middleware.Chain(
				livezone.RetrieveHandler(s.logger),
				middleware.Recovery(s.logger),
				middleware.RequestID,
				middleware.Logging(s.logger),
				authMiddleware.Authenticate,
			),
		)
	}

	// GET /v1/key/info — dual-purpose: with master key looks up any key, with regular key returns self info
	mux.Handle("GET /v1/key/info",
		middleware.Chain(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Check if caller has master key
				key := r.Header.Get("Authorization")
				if len(key) > 7 && key[:7] == "Bearer " {
					key = key[7:]
				}
				isMaster := len(s.cfg.Server.MasterKey) > 0 && key == s.cfg.Server.MasterKey

				if isMaster {
					keyParam := r.URL.Query().Get("key")
					if keyParam == "" {
						writeJSON(w, http.StatusOK, map[string]any{
							"key_prefix": "master",
							"name":       "master",
							"active":     true,
						})
						return
					}
					// Look up key: try as raw key (hash it), or as hash directly
					keyHash := auth.HashKey(keyParam)
					apiKey, _ := db.GetKeyByHash(r.Context(), keyHash)
					if apiKey == nil {
						// Maybe it's already a hash — try directly
						apiKey, _ = db.GetKeyByHash(r.Context(), keyParam)
					}
					if apiKey != nil {
						writeJSON(w, http.StatusOK, apiKey)
						return
					}
					// Try by prefix/name/id
					keys, _ := db.ListKeys(r.Context(), store.KeyFilter{})
					for _, k := range keys {
						if k.ID == keyParam || k.KeyPrefix == keyParam || k.Name == keyParam {
							writeJSON(w, http.StatusOK, &k)
							return
						}
					}
					// Not found — return null info (like LiteLLM does)
					writeJSON(w, http.StatusOK, map[string]any{"info": nil})
					return
				}
				// Regular key: return info about self from context
				keyInfo := auth.KeyInfoFromContext(r.Context())
				if keyInfo == nil {
					writeJSON(w, http.StatusOK, map[string]any{"info": nil})
					return
				}
				writeJSON(w, http.StatusOK, keyInfo)
			}),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			authMiddleware.Authenticate,
		),
	)

	adminRoutes := []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"POST", "/v1/key/generate", adminHandler.GenerateKey},
		{"POST", "/v1/key/update", adminHandler.UpdateKey},
		{"POST", "/v1/governance/snapshot/apply", adminHandler.ApplyGovernanceSnapshot},
		{"POST", "/v1/key/delete", adminHandler.DeleteKey},
		{"GET", "/v1/key/list", adminHandler.ListKeys},
		{"GET", "/v1/spend/logs", adminHandler.GetSpendLogs},
		{"GET", "/v1/spend/records", adminHandler.GetSpendRecords},
		// Teams
		{"POST", "/v1/team/create", adminHandler.CreateTeam},
		{"GET", "/v1/team/info", adminHandler.GetTeam},
		{"GET", "/v1/team/list", adminHandler.ListTeams},
		{"POST", "/v1/team/update", adminHandler.UpdateTeam},
		{"POST", "/v1/team/delete", adminHandler.DeleteTeam},
		// Route settings (per-team HiveRoute config)
		{"GET", "/v1/route/settings", adminHandler.GetRouteSettings},
		{"POST", "/v1/route/settings", adminHandler.UpdateRouteSettings},
		// Feedback
		{"GET", "/v1/feedback/list", adminHandler.ListFeedback},
		// Users
		{"POST", "/v1/user/create", adminHandler.CreateUser},
		{"GET", "/v1/user/info", adminHandler.GetUser},
		{"GET", "/v1/user/list", adminHandler.ListUsers},
		{"POST", "/v1/user/update", adminHandler.UpdateUser},
		{"POST", "/v1/user/delete", adminHandler.DeleteUser},
	}
	for _, route := range adminRoutes {
		mux.Handle(route.method+" "+route.path,
			middleware.Chain(
				route.handler,
				middleware.Recovery(s.logger),
				middleware.RequestID,
				middleware.Logging(s.logger),
				middleware.MaxBody(1<<20), // 1MB limit for admin bodies
				adminHandler.RequireMasterKey,
			),
		)
	}

	s.logger.Info("routes registered")

	// Cache admin endpoints (only if cache is enabled)
	if semanticCache != nil {
		cacheFlush := func(w http.ResponseWriter, r *http.Request) {
			if err := semanticCache.Flush(r.Context()); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "flushed"})
		}
		cacheMetrics := func(w http.ResponseWriter, r *http.Request) {
			limit := 100
			if l := r.URL.Query().Get("limit"); l != "" {
				if n, err := strconv.Atoi(l); err == nil && n > 0 {
					limit = n
				}
			}
			metrics, err := db.ListCacheMetrics(r.Context(), limit)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"metrics": metrics, "count": len(metrics)})
		}
		cacheStats := func(w http.ResponseWriter, r *http.Request) {
			count, err := semanticCache.Count(context.Background())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"entries": count})
		}

		cacheAdminRoutes := []struct {
			method, path string
			handler      http.HandlerFunc
		}{
			{"POST", "/v1/cache/flush", cacheFlush},
			{"GET", "/v1/cache/metrics", cacheMetrics},
			{"GET", "/v1/cache/stats", cacheStats},
		}
		for _, route := range cacheAdminRoutes {
			mux.Handle(route.method+" "+route.path,
				middleware.Chain(
					route.handler,
					middleware.Recovery(s.logger),
					middleware.RequestID,
					middleware.Logging(s.logger),
					adminHandler.RequireMasterKey,
				),
			)
		}
	}

	// HiveState metrics endpoint
	stateMetricsHandler := func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		metrics, err := db.ListHiveStateMetrics(r.Context(), limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"metrics": metrics, "count": len(metrics)})
	}
	mux.Handle("GET /v1/state/metrics",
		middleware.Chain(
			http.HandlerFunc(stateMetricsHandler),
			middleware.Recovery(s.logger),
			middleware.RequestID,
			middleware.Logging(s.logger),
			adminHandler.RequireMasterKey,
		),
	)

	// Swagger UI (opt-in via config)
	if s.cfg.Server.Swagger && len(s.OpenAPISpec) > 0 {
		registerSwagger(mux, s.OpenAPISpec)
		s.logger.Info("swagger UI enabled at /docs")
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type governanceReadinessChecker interface {
	GovernanceReadiness(context.Context) error
}

func handleReady(registry *provider.Registry, readinessSource any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		models := registry.ListModels()
		if len(models) == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "not_ready",
				"reason": "no models registered",
			})
			return
		}
		if checker, ok := readinessSource.(governanceReadinessChecker); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := checker.GovernanceReadiness(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "not_ready", "reason": "governance store unavailable",
				})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ready",
			"models": len(models),
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/api"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/agenttoken"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/cache"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/cli"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/console"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/livezone"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/logbuf"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/middleware"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/router"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/server"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/spend"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/steering"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/webhook"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/workflow"
)

var version = "dev"

const banner = `
    ██╗   ██╗██████╗ ██╗ ██████╗ ██╗   ██╗██╗   ██╗███╗   ███╗
    ██║   ██║██╔══██╗██║██╔═══██╗██║   ██║██║   ██║████╗ ████║
    ██║   ██║██████╔╝██║██║   ██║██║   ██║██║   ██║██╔████╔██║
    ██║   ██║██╔══██╗██║██║▄▄ ██║██║   ██║██║   ██║██║╚██╔╝██║
    ╚██████╔╝██████╔╝██║╚██████╔╝╚██████╔╝╚██████╔╝██║ ╚═╝ ██║
     ╚═════╝ ╚═════╝ ╚═╝ ╚══▀▀═╝  ╚═════╝  ╚═════╝ ╚═╝     ╚═╝

                 Secure AI Egress Gateway`

func main() {
	tuneRuntime()

	args := os.Args[1:]

	// No args or first arg is a flag → open interactive shell
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		shellArgs := append([]string{"shell"}, args...)
		if err := cli.Run(context.Background(), shellArgs, version, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// "serve" → start the gateway server (foreground)
	if args[0] == "serve" {
		if err := runServer(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Other CLI commands
	if isCLICommand(args[0]) {
		if err := cli.Run(context.Background(), args, version, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Unknown arg → show usage
	fmt.Fprintln(os.Stderr, "unknown command:", args[0])
	_ = cli.Run(context.Background(), []string{"help"}, version, os.Stdin, os.Stdout)
	os.Exit(1)
}

func tuneRuntime() {
	// Runtime tuning for high-throughput proxy workload
	// GOGC=200: reduce GC frequency (proxy has short-lived allocs, GC pressure is the enemy)
	// If GOMEMLIMIT is set via env (recommended in containers), Go will respect it.
	debug.SetGCPercent(200)
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func isCLICommand(arg string) bool {
	switch arg {
	case "serve", "shell", "init", "start", "stop", "restart", "status", "model", "models", "provider", "providers", "use", "key", "keys", "tenant", "tenants", "spend", "test", "config", "cache", "state", "route", "logs", "help", "version", "-h", "--help":
		return true
	default:
		return false
	}
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", cli.ConfigFilePath(), "path to configuration file")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Printf("ubiquum-ai-gateway %s\n", version)
		return nil
	}

	// Set up logging to both stdout and ~/.ubiquum/gateway.log
	logDir := filepath.Join(cli.UbiquumHome(), "")
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(cli.UbiquumHome(), "gateway.log")
	logRing := logbuf.New(2000)
	logWriter := io.MultiWriter(os.Stdout, logRing)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		logWriter = io.MultiWriter(os.Stdout, f, logRing)
		defer f.Close()
	}

	logger := slog.New(slog.NewJSONHandler(logWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return err
	}

	factory := &providerFactory{}
	var credentialVault *console.Vault
	if !cfg.Console.Disabled {
		credentialVault, err = console.OpenVault(cfg.Console.VaultPath, cfg.Console.VaultKeyFile)
		if err != nil {
			return fmt.Errorf("initialize encrypted credential vault: %w", err)
		}
	}

	// Create provider registry.
	registry, err := provider.NewRegistryWithAliases(cfg.Models, cfg.ModelAliases, factory)
	if err != nil {
		logger.Error("failed to create provider registry", "error", err)
		return err
	}
	if credentialVault != nil {
		for _, connection := range credentialVault.Connections() {
			if err := console.InstallConnection(registry, factory, connection); err != nil {
				return fmt.Errorf("restore BYOK connection %q: %w", connection.Name, err)
			}
		}
	}

	// Initialize database
	db, err := store.Open(cfg.Database)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		return err
	}
	defer db.Close()

	if err := db.Migrate(context.Background()); err != nil {
		logger.Error("failed to run migrations", "error", err)
		return err
	}
	dbDriver := cfg.Database.Driver
	if dbDriver == "" {
		dbDriver = "sqlite"
	}
	logger.Info("database ready", "driver", dbDriver)

	if stop := startResponseJanitor(db, cfg.Responses, logger); stop != nil {
		defer stop()
	}

	if stop := startSpendRetentionJanitor(db, cfg.Responses.CleanupInterval, logger); stop != nil {
		defer stop()
	}

	// Build per-model pricing overrides from config
	priceOverrides := make(map[string]pricing.ModelPrice)
	// aliasedPrices collects the overrides that want to be reachable under a
	// shared provider_model, so conflicts can be spotted before any of them is
	// registered.
	aliasedPrices := make(map[string][]pricing.ModelPrice)
	for _, m := range cfg.Models {
		var override pricing.ModelPrice
		switch {
		case m.InputCostPerMillion > 0 || m.OutputCostPerMillion > 0:
			// Per-million takes priority (human-friendly format)
			override = pricing.ModelPrice{
				InputCostPerToken:  m.InputCostPerMillion / 1_000_000,
				OutputCostPerToken: m.OutputCostPerMillion / 1_000_000,
			}
		case m.InputCostPerToken > 0 || m.OutputCostPerToken > 0:
			// Legacy per-token fields (backward compat)
			override = pricing.ModelPrice{
				InputCostPerToken:  m.InputCostPerToken,
				OutputCostPerToken: m.OutputCostPerToken,
			}
		default:
			continue
		}
		priceOverrides[m.Name] = override
		// Spend lookups go through ProviderModel, so the override has to be
		// reachable under that name too — but only when it is unambiguous.
		// Aliasing it unconditionally applied one alias's negotiated price to
		// every model sharing the same upstream id, and which one won depended
		// on config order.
		if m.ProviderModel != "" && m.ProviderModel != m.Name {
			aliasedPrices[m.ProviderModel] = append(aliasedPrices[m.ProviderModel], override)
		}
	}

	for providerModel, prices := range aliasedPrices {
		if _, explicit := priceOverrides[providerModel]; explicit {
			// A model configured under its own upstream id speaks for itself.
			continue
		}
		ambiguous := false
		for _, p := range prices[1:] {
			if p != prices[0] {
				ambiguous = true
				break
			}
		}
		if ambiguous {
			logger.Warn("provider_model has conflicting price overrides; falling back to the catalog price",
				"provider_model", providerModel, "distinct_entries", len(prices))
			continue
		}
		priceOverrides[providerModel] = prices[0]
	}

	// Load model pricing (embedded defaults + optional remote refresh + config overrides)
	pricingURL := pricing.DefaultPricingURL
	if cfg.Pricing.RemoteURL != "" {
		pricingURL = cfg.Pricing.RemoteURL
	}
	if cfg.Pricing.DisableRemoteRefresh {
		pricingURL = ""
	}
	pc := pricing.NewCalculator(logger, pricingURL, priceOverrides)

	// Register each gateway model name with the price resolved from its
	// provider_model. The proxy prices by provider_model, but internal services
	// (HiveState, HiveCache, guardrail) price by the gateway model name — without
	// this, their spend is logged as $0 even though real tokens are billed.
	for _, m := range cfg.Models {
		if _, ok := pc.GetPrice(m.Name); ok {
			continue
		}
		if m.ProviderModel == "" {
			continue
		}
		if p, ok := pc.GetPrice(m.ProviderModel); ok {
			pc.SetPrice(m.Name, p)
		}
	}

	// Print startup banner
	printBanner(cfg, registry)

	// Create router with retry and circuit breaker
	rt := router.New(registry, cfg.Router, logger)

	// Create auth middleware
	var authMw *auth.Middleware
	if cfg.Server.PassThrough {
		authMw = auth.NewPassThroughMiddleware()
		logger.Info("auth mode: pass-through (client tokens forwarded to upstream)")
	} else {
		authMw = auth.NewMiddleware(cfg.Server.MasterKey, db)
		// Lets the funding gate recognise a key scoped entirely to OAuth
		// pass-through models, which the gateway does not pay for.
		authMw.SetGatewayBilledFunc(registry.IsGatewayBilled)

		// Short-lived agent tokens, verified against the control plane's public
		// keys. Nil without a configured endpoint, which leaves the gateway
		// accepting virtual keys only.
		if verifier := agenttoken.New(cfg.Server.AgentTokenJWKSURL, logger); verifier != nil {
			authMw.SetAgentTokenVerifier(verifier)
			logger.Info("agent tokens enabled", "jwks", cfg.Server.AgentTokenJWKSURL)
		}
	}

	// Create webhook dispatcher
	var webhookConfigs []webhook.Config
	for _, wh := range cfg.Webhooks {
		webhookConfigs = append(webhookConfigs, webhook.Config{
			URL:    wh.URL,
			Secret: wh.Secret,
			Events: wh.Events,
		})
	}
	webhooks := webhook.NewDispatcher(webhookConfigs, logger)
	defer webhooks.Close()

	// Create spend batch writer
	batchInterval := cfg.Database.BatchWriteInterval
	spender := spend.NewBatchWriter(db, logger, batchInterval, webhooks)
	defer spender.Close()

	// Create and configure server
	srv := server.New(cfg, logger)
	srv.OpenAPISpec = api.OpenAPISpec
	if credentialVault != nil {
		manager, err := console.NewManager(cfg.Console, cfg.Server.MasterKey, credentialVault, registry, factory, logger)
		if err != nil {
			return fmt.Errorf("initialize appliance console: %w", err)
		}
		srv.Console = manager
	}

	// Initialize semantic cache (if enabled)
	var extraMiddlewares []middleware.Middleware
	var semanticCache *cache.Cache

	// x-ubiquum-workflow (if enabled) — must run before cache/hivestate so those
	// see the workflow-transformed prompt, not the original one.
	if cfg.Workflow.Enabled {
		// workflow.New returns nil without a URL, and the middleware then
		// degrades to a passthrough. Saying "enabled" for that is worse than
		// useless: the operator asked for workflows and silently has none.
		if strings.TrimSpace(cfg.Workflow.URL) == "" {
			return fmt.Errorf("workflow.enabled is set but workflow.url is empty")
		}
		wf := workflow.New(cfg.Workflow, db, logger)
		wf.SetResponseTTL(cfg.Responses.TTL)
		extraMiddlewares = append(extraMiddlewares, workflow.Middleware(wf))
		logger.Info("workflow (x-ubiquum-workflow) enabled",
			"url", cfg.Workflow.URL,
			"fail_open", cfg.Workflow.ShouldFailOpen(),
		)
	}

	if cfg.Cache.Enabled {
		cacheStore, err := initCacheStore(cfg.Cache, cfg.Database, logger)
		if err != nil {
			logger.Error("failed to initialize cache store", "error", err)
			return err
		}
		if cacheStore != nil {
			defer cacheStore.Close()
		}

		embedFn, err := cache.NewEmbedFunc(registry, cfg.Cache.EmbeddingModel)
		if err != nil {
			logger.Error("failed to initialize cache embedding function", "error", err)
			return err
		}

		var tweakFn cache.TweakFunc
		if cfg.Cache.TweakModel != "" {
			tweakFn, err = cache.NewTweakFunc(registry, cfg.Cache.TweakModel)
			if err != nil {
				logger.Warn("failed to initialize cache tweak function", "error", err)
			} else {
				logger.Info("cache tweak enabled", "tweak_model", cfg.Cache.TweakModel)
			}
		}

		semanticCache = cache.New(cfg.Cache, cacheStore, embedFn, logger, tweakFn)
		extraMiddlewares = append(extraMiddlewares, cache.Middleware(semanticCache, db, pc, spender, registry, logger))
		logger.Info("semantic cache enabled", "backend", cfg.Cache.Backend, "embedding_model", cfg.Cache.EmbeddingModel)
	}

	// Initialize request logging (if enabled) — captures full request bodies for analysis
	if cfg.RequestLog.Enabled {
		dir := cfg.RequestLog.Dir
		if dir == "" {
			dir = "logs"
		}
		extraMiddlewares = append(extraMiddlewares, middleware.RequestLog(middleware.RequestLogConfig{Dir: dir}, logger))
		logger.Info("request logging enabled", "dir", dir)
	}

	// Shared metrics collector: HiveState is constructed before routes are
	// registered, so both need the same instance.
	metrics := middleware.NewMetrics()

	// Steering works on the answer rather than the prompt, so it is
	// independent of the two compressors and runs before them: they only ever
	// read the body it has already settled.
	if cfg.Steering.Enabled {
		extraMiddlewares = append(extraMiddlewares,
			steering.Middleware(cfg.Steering, metrics, logger))
		logger.Info("steering enabled", "verbosity_note", cfg.Steering.VerbosityNote != "")
	}

	// Live compression runs ahead of HiveState: history compression then sees
	// an already-slimmer delta, and the two never touch the same messages.
	if cfg.LiveCompression.Enabled {
		extraMiddlewares = append(extraMiddlewares,
			livezone.Middleware(cfg.LiveCompression, metrics, db, logger))
		logger.Info("live compression enabled",
			"allow_lossy", cfg.LiveCompression.AllowLossy,
			"canary_percent", cfg.LiveCompression.CanaryPercent,
			"live_turns", cfg.LiveCompression.LiveTurns,
			"dedupe_repeats", cfg.LiveCompression.DedupeRepeats)
	}

	// Initialize HiveState (if enabled)
	if cfg.HiveState.Enabled {
		hs, err := hivestate.New(cfg.HiveState, registry, logger)
		if err != nil {
			logger.Error("failed to initialize hivestate", "error", err)
			return err
		}
		extraMiddlewares = append(extraMiddlewares, hivestate.Middleware(hs, db, pc, spender, registry, metrics, logger))
		logger.Info("hivestate enabled", "model", cfg.HiveState.Model, "threshold", cfg.HiveState.Threshold)
	}

	server.RegisterRoutes(srv, registry, rt, authMw, db, pc, spender, webhooks, semanticCache, metrics, extraMiddlewares...)

	// Admin-only logs endpoint (streams from in-memory ring buffer)
	srv.Mux().HandleFunc("GET /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		// Require master key
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key != cfg.Server.MasterKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, line := range logRing.Lines(200) {
			fmt.Fprintln(w, line)
		}
	})

	// Run server (blocks until shutdown signal)
	if err := srv.Run(context.Background()); err != nil {
		logger.Error("server error", "error", err)
		return err
	}
	return nil
}

// startResponseJanitor keeps the /v1/responses table honest on a timer: it
// purges rows past their retention, and closes out background turns abandoned
// by a worker that died. The gateway keeps a full copy of every stored turn so
// the stateful endpoints work on providers with no server-side state, so
// without this the table grows for as long as the gateway runs. Returns nil
// when the configured store cannot hold responses at all.
func startResponseJanitor(db store.Store, cfg config.ResponsesConfig, logger *slog.Logger) func() {
	rs, ok := db.(store.ResponseStore)
	if !ok {
		return nil
	}

	purge := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		removed, err := rs.PurgeExpiredResponses(ctx)
		if err != nil {
			logger.Warn("expired response purge failed", "error", err)
		} else if removed > 0 {
			logger.Info("purged expired responses", "count", removed)
		}

		if cs, ok := db.(store.ConversationStore); ok {
			if dropped, err := cs.PurgeExpiredConversations(ctx); err != nil {
				logger.Warn("expired conversation purge failed", "error", err)
			} else if dropped > 0 {
				logger.Info("purged expired conversations", "count", dropped)
			}
		}

		// A background turn whose worker died stays queued forever, and a
		// client polling it waits for an answer nobody is producing.
		abandoned, err := rs.FailStaleResponses(ctx, time.Now().Add(-cfg.BackgroundTimeout))
		if err != nil {
			logger.Warn("stale response sweep failed", "error", err)
		} else if abandoned > 0 {
			logger.Info("failed abandoned background responses", "count", abandoned)
		}
	}

	done := make(chan struct{})
	go func() {
		// Run once at startup: this is also where turns abandoned by the
		// process that just died get closed out.
		purge()
		ticker := time.NewTicker(cfg.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				purge()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// startSpendRetentionJanitor purges gw_spend_records rows past a key's
// request_logging retention override on a timer. Shares the Responses
// cleanup cadence rather than adding its own config knob: retention is
// day-granularity, so hourly precision on when a day's rows actually leave
// is more than enough.
func startSpendRetentionJanitor(db store.Store, interval time.Duration, logger *slog.Logger) func() {
	if interval <= 0 {
		interval = time.Hour
	}

	purge := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		removed, err := db.PurgeExpiredSpendRecords(ctx)
		if err != nil {
			logger.Warn("spend retention purge failed", "error", err)
		} else if removed > 0 {
			logger.Info("purged expired spend records", "count", removed)
		}
	}

	done := make(chan struct{})
	go func() {
		purge()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				purge()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func printBanner(cfg *config.Config, registry *provider.Registry) {
	fmt.Println(banner)
	fmt.Println()
	fmt.Printf("  Version:    %s\n", version)
	fmt.Printf("  Port:       :%d\n", cfg.Server.Port)
	fmt.Printf("  Strategy:   %s\n", cfg.Router.Strategy)
	fmt.Printf("  Retries:    %d\n", cfg.Router.Retries)
	if cfg.Router.CircuitBreaker.Threshold > 0 {
		fmt.Printf("  Circuit:    threshold=%d recovery=%s\n",
			cfg.Router.CircuitBreaker.Threshold, cfg.Router.CircuitBreaker.Recovery)
	}
	fmt.Println()

	fmt.Println("  ┌─ Registered Models ─────────────────────────────────────┐")
	models := registry.ListModels()
	for _, name := range models {
		deployments, _ := registry.GetDeployments(name)
		providerName := ""
		if len(deployments) > 0 {
			providerName = deployments[0].Provider.Name()
		}
		icon := providerIcon(providerName)
		fmt.Printf("  │  %s %-30s  [%d deployment(s)]  │\n", icon, name, len(deployments))
	}
	fmt.Println("  └────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Printf("  Total: %d model(s), %d deployment(s)\n", len(models), len(cfg.Models))
	fmt.Println()
	fmt.Println(strings.Repeat("─", 64))
	fmt.Println()
}

func providerIcon(name string) string {
	switch name {
	case ProviderAzureOpenAI:
		return "🔷"
	case ProviderVertex:
		return "🔶"
	case ProviderBedrock:
		return "🟠"
	case ProviderOpenAI, ProviderOpenAICompatible:
		return "🟢"
	case ProviderGitHubCopilot:
		return "🟣"
	case ProviderAnthropic:
		return "🟤"
	case ProviderGoogleAI:
		return "🔵"
	default:
		return "⚪"
	}
}

func initCacheStore(cfg config.CacheConfig, dbCfg config.DatabaseConfig, logger *slog.Logger) (cache.VectorStore, error) {
	cfg.ApplyDefaults()
	switch cfg.Backend {
	case "memory":
		return cache.NewMemoryStore(cfg.FilePath)
	case "redis":
		return cache.NewRedisStore(cfg.RedisURL, "hivecache:")
	case "qdrant":
		return cache.NewQdrantStore(cfg.QdrantURL, "hivecache", 1536)
	case "pgvector":
		if dbCfg.Driver != "postgres" {
			return nil, fmt.Errorf("pgvector backend requires database.driver=postgres")
		}
		return cache.NewPgvectorStore(dbCfg.URL, 1536)
	default:
		return nil, fmt.Errorf("unsupported cache backend: %q", cfg.Backend)
	}
}

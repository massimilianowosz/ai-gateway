package config

import "time"

// Config is the top-level gateway configuration.
type Config struct {
	Server          ServerConfig              `yaml:"server"`
	Console         ConsoleConfig             `yaml:"console"`
	Database        DatabaseConfig            `yaml:"database"`
	Providers       map[string]ProviderConfig `yaml:"providers"`
	Models          []ModelConfig             `yaml:"models"`
	ModelAliases    map[string]string         `yaml:"model_aliases,omitempty"`
	Router          RouterConfig              `yaml:"router"`
	Cache           CacheConfig               `yaml:"cache"`
	HiveState       HiveStateConfig           `yaml:"hivestate"`
	LiveCompression LiveCompressionConfig     `yaml:"live_compression"`
	Steering        SteeringConfig            `yaml:"steering"`
	RequestLog      RequestLogConfig          `yaml:"request_log"`
	Webhooks        []WebhookConfig           `yaml:"webhooks"`
	Hooks           HooksConfig               `yaml:"hooks"`
	Pricing         PricingConfig             `yaml:"pricing"`
	Guardrail       GuardrailConfig           `yaml:"guardrail"`
	Feedback        FeedbackConfig            `yaml:"feedback"`
	Workflow        WorkflowConfig            `yaml:"workflow"`
	Responses       ResponsesConfig           `yaml:"responses"`
}

// ConsoleConfig controls the local appliance management plane. The console is
// served by the gateway itself, so an appliance needs no second web service.
// Provider secrets are kept in an encrypted vault whose key lives in a
// separate file; deployments backed by a TPM may mount that file at runtime.
type ConsoleConfig struct {
	Disabled      bool          `yaml:"disabled"`
	SessionTTL    time.Duration `yaml:"session_ttl"`
	SecureCookies bool          `yaml:"secure_cookies"`
	// AdminPasswordSHA256 is the lowercase hex SHA-256 of the console password.
	// When omitted, the master key is accepted as a one-time bootstrap login.
	AdminPasswordSHA256 string `yaml:"admin_password_sha256"`
	VaultPath           string `yaml:"vault_path"`
	VaultKeyFile        string `yaml:"vault_key_file"`
}

// ResponsesConfig controls retention of stored /v1/responses turns. The
// gateway keeps a full copy of every non-store:false turn so the stateful half
// of the Responses API (retrieve, previous_response_id, background polling)
// works on providers that have no server-side state; without an expiry that
// table would grow without bound.
type ResponsesConfig struct {
	TTL             time.Duration `yaml:"ttl"`              // how long a stored response stays retrievable (default 720h, i.e. 30 days, matching OpenAI retention)
	CleanupInterval time.Duration `yaml:"cleanup_interval"` // how often expired rows are purged (default 1h)
	// BackgroundTimeout is how long a background turn may stay queued or
	// in_progress before the gateway declares it dead. It exists because a
	// worker killed mid-turn leaves its row running forever, and a client
	// polling that row would wait for an answer nobody is producing. Keep it
	// above the slowest turn you expect: with several replicas, any turn older
	// than this is failed regardless of which one owns it (default 1h).
	BackgroundTimeout time.Duration `yaml:"background_timeout"`
}

// ApplyDefaults fills zero-value responses fields with sane defaults.
func (c *ResponsesConfig) ApplyDefaults() {
	// Non-positive, not just zero: a negative interval reaches time.NewTicker,
	// which panics, so a typo in the config took the gateway down at startup
	// rather than being corrected here like every other unset value.
	if c.TTL <= 0 {
		c.TTL = 30 * 24 * time.Hour
	}
	if c.CleanupInterval <= 0 {
		c.CleanupInterval = time.Hour
	}
	if c.BackgroundTimeout <= 0 {
		c.BackgroundTimeout = time.Hour
	}
}

// WorkflowConfig configures the x-ubiquum-workflow integration, which calls out
// to the backend's /workflows/execute endpoint to transform, block, or
// short-circuit a request before it reaches the LLM.
type WorkflowConfig struct {
	Enabled bool          `yaml:"enabled"`
	URL     string        `yaml:"url"`     // backend base URL, e.g. http://backend:8000
	Timeout time.Duration `yaml:"timeout"` // per-call timeout (default 30s)
	// FailOpen passes the request through unmodified when the backend call
	// fails. A pointer so an omitted key is distinguishable from an explicit
	// false: as a plain bool it zero-valued to fail-*closed*, which turned any
	// backend blip into a 502 on every request for operators who set
	// workflow.enabled and nothing else. Read it through ShouldFailOpen.
	FailOpen *bool `yaml:"fail_open"` // default true
}

// ShouldFailOpen reports whether a failed backend call lets the request
// through. Unset means true.
func (c *WorkflowConfig) ShouldFailOpen() bool {
	return c.FailOpen == nil || *c.FailOpen
}

// ApplyDefaults fills zero-value workflow fields with sane defaults.
func (c *WorkflowConfig) ApplyDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
}

// GuardrailConfig configures inline content guardrails.
// When enabled, every chat/message request is scanned before being proxied.
type GuardrailConfig struct {
	Enabled    bool          `yaml:"enabled"`
	URL        string        `yaml:"url"`        // deprecated: legacy LLM Guard URL (ignored when inline scanners are configured)
	Timeout    time.Duration `yaml:"timeout"`    // per-call timeout for ML-based scanners (default 5s)
	FailOpen   bool          `yaml:"fail_open"`  // if true, allow request when a scanner fails (default true)
	Guardrails []string      `yaml:"guardrails"` // e.g. ["prompt-injection","toxicity","sensitive-data"]

	// Inline scanner configs (when set, the external LLM Guard service is not used)
	PII        PIIScannerConfig        `yaml:"pii"`
	Secrets    SecretsScannerConfig    `yaml:"secrets"`
	Injection  InjectionScannerConfig  `yaml:"injection"`
	Moderation ModerationScannerConfig `yaml:"moderation"`
}

// PIIScannerConfig configures PII detection via regex patterns.
type PIIScannerConfig struct {
	Enabled bool `yaml:"enabled"`
}

// SecretsScannerConfig configures secret/credential detection via regex patterns.
type SecretsScannerConfig struct {
	Enabled bool `yaml:"enabled"`
}

// InjectionScannerConfig configures prompt injection detection via regex heuristics.
type InjectionScannerConfig struct {
	Enabled bool `yaml:"enabled"`
}

// ModerationScannerConfig configures ML-based content moderation via a fast LLM.
// Calls are attributed to the customer key as @hiveguard spend.
type ModerationScannerConfig struct {
	Enabled bool   `yaml:"enabled"`
	Model   string `yaml:"model"` // gateway model name, e.g. "azure-gpt-5-nano"
}

// HasInlineScanners returns true if any inline scanner is enabled.
func (c *GuardrailConfig) HasInlineScanners() bool {
	return c.PII.Enabled || c.Secrets.Enabled || c.Injection.Enabled || c.Moderation.Enabled
}

// RequestLogConfig enables full request body logging for analysis.
type RequestLogConfig struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"` // directory to write log files (default: "logs")
}

// ServerConfig defines HTTP server settings.
type ServerConfig struct {
	Port                    int                           `yaml:"port"`
	MasterKey               string                        `yaml:"master_key"`
	MaxRequestSizeMB        int                           `yaml:"max_request_size_mb"`
	MaxConcurrent           int                           `yaml:"max_concurrent"` // Max in-flight requests (0 = 10000)
	Swagger                 bool                          `yaml:"swagger"`        // Enable Swagger UI at /docs
	PassThrough             bool                          `yaml:"pass_through"`   // Whole-process auth mode with NO governance at all (see NewPassThroughMiddleware): every request is forwarded upstream using the client's own bearer token, with no virtual key, tenant, budget, or policy involved. Mutually exclusive with normal virtual-key/agent-token auth for the entire gateway instance -- never set true in the SaaS or self-hosted multi-tenant deployment configs. Exists only for standalone local proxies (e.g. ubiquum-cli's own OAuth-passthrough wrap) where the caller is meant to spend their own personal provider subscription with zero Ubiquum account involved. A governed agent that wants to use its own upstream credentials authenticates normally with a virtual key and forwards a scoped token instead (UpstreamTokenForwardingConfig below), which stays fully governed.
	UpstreamTokenForwarding UpstreamTokenForwardingConfig `yaml:"upstream_token_forwarding"`
	// AgentTokenJWKSURL is where the control plane publishes the public keys
	// that verify agent tokens. Empty means this gateway accepts virtual keys
	// only, which is what it did before agent tokens existed.
	AgentTokenJWKSURL string `yaml:"agent_token_jwks_url"`
}

// UpstreamTokenForwardingConfig controls scoped forwarding of client-supplied
// upstream bearer tokens. It is intended for local shims such as ubiquum-cli
// that authenticate to Ubiquum with a virtual key while forwarding a separate
// provider token for a specific upstream.
type UpstreamTokenForwardingConfig struct {
	Enabled          bool     `yaml:"enabled"`
	Header           string   `yaml:"header"`
	AccountIDHeader  string   `yaml:"account_id_header"`
	AllowedProviders []string `yaml:"allowed_providers"`
}

// DatabaseConfig defines database connection settings.
type DatabaseConfig struct {
	Driver             string        `yaml:"driver"` // "sqlite" (default) or "postgres"
	URL                string        `yaml:"url"`    // file path for sqlite, connection string for postgres
	PoolSize           int           `yaml:"pool_size"`
	BatchWriteInterval time.Duration `yaml:"batch_write_interval"`
}

// ProviderConfig defines reusable provider credentials and endpoint settings.
type ProviderConfig struct {
	Type         string `yaml:"type"`
	APIBase      string `yaml:"api_base"`
	APIKey       string `yaml:"api_key"`
	APISecret    string `yaml:"api_secret"`
	APIVersion   string `yaml:"api_version"`
	Project      string `yaml:"project"`
	Location     string `yaml:"location"`
	Region       string `yaml:"region"`
	SessionToken string `yaml:"session_token"`
}

// ModelConfig defines a model deployment.
type ModelConfig struct {
	Name                 string   `yaml:"name"`
	Provider             string   `yaml:"provider"`
	ProviderProfile      string   `yaml:"-"` // resolved: original provider profile name
	ProviderModel        string   `yaml:"provider_model"`
	APIBase              string   `yaml:"api_base"`
	APIKey               string   `yaml:"api_key"`
	APISecret            string   `yaml:"api_secret"` // AWS secret access key
	APIVersion           string   `yaml:"api_version"`
	Project              string   `yaml:"project"`       // GCP project ID
	Location             string   `yaml:"location"`      // GCP location (e.g., europe-west1)
	Region               string   `yaml:"region"`        // AWS region
	SessionToken         string   `yaml:"session_token"` // AWS session token (optional)
	DropParams           []string `yaml:"drop_params"`
	IsEU                 bool     `yaml:"is_eu,omitempty"`                   // Whether this model runs in EU region
	Restricted           bool     `yaml:"restricted,omitempty"`              // Hidden from /v1/models unless tenant has it in allowed_models
	InputCostPerMillion  float64  `yaml:"input_cost_per_million,omitempty"`  // Cost per 1M input tokens (e.g. 5.0 = $5/1M). Takes priority over input_cost_per_token.
	OutputCostPerMillion float64  `yaml:"output_cost_per_million,omitempty"` // Cost per 1M output tokens. Takes priority over output_cost_per_token.
	InputCostPerToken    float64  `yaml:"input_cost_per_token,omitempty"`    // Legacy: cost per single input token. Use input_cost_per_million instead.
	OutputCostPerToken   float64  `yaml:"output_cost_per_token,omitempty"`   // Legacy: cost per single output token.
	// AuthMode and BillingMode are independent axes: how the gateway authenticates
	// upstream (api_key vs oauth_passthrough) never determines how the tenant is
	// billed for the call (metered vs flat), and vice versa.
	AuthMode    string `yaml:"auth_mode,omitempty"`    // "api_key" (default) | "oauth_passthrough"
	BillingMode string `yaml:"billing_mode,omitempty"` // "metered" (default) | "flat"
	// NativeResponses overrides whether /v1/responses is forwarded to this
	// deployment verbatim instead of being translated to chat completions.
	// nil keeps the provider default: on for openai and azure_openai, off for
	// openai_compatible and azure_openai_compat. Set it when the endpoint
	// disagrees with that default, e.g. an OpenAI-compatible proxy that does
	// implement /responses, or an api_base for provider: openai that does not.
	NativeResponses *bool `yaml:"native_responses,omitempty"`
}

const (
	AuthModeAPIKey           = "api_key"
	AuthModeOAuthPassthrough = "oauth_passthrough"

	BillingModeMetered = "metered"
	BillingModeFlat    = "flat"
)

// RouterConfig defines routing behavior.
type RouterConfig struct {
	Strategy       string         `yaml:"strategy"`
	Retries        int            `yaml:"retries"`
	RetryDelay     time.Duration  `yaml:"retry_delay"`
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
}

// CircuitBreaker defines circuit breaker settings.
type CircuitBreaker struct {
	Threshold int           `yaml:"threshold"`
	Recovery  time.Duration `yaml:"recovery"`
}

// WebhookConfig defines a webhook endpoint subscription.
type WebhookConfig struct {
	URL    string   `yaml:"url"`
	Secret string   `yaml:"secret"`
	Events []string `yaml:"events"`
}

// PricingConfig controls pricing catalog refresh behavior.
type PricingConfig struct {
	RemoteURL            string `yaml:"remote_url"`
	DisableRemoteRefresh bool   `yaml:"disable_remote_refresh"`
}

// HooksConfig defines pre/post request hook callouts.
type HooksConfig struct {
	PreRequest  *HookEndpoint `yaml:"pre_request"`
	PostRequest *HookEndpoint `yaml:"post_request"`
}

// HookEndpoint defines a single hook HTTP callout.
type HookEndpoint struct {
	URL     string            `yaml:"url"`
	Timeout time.Duration     `yaml:"timeout"`
	Headers map[string]string `yaml:"headers"`
}

// CacheConfig defines semantic cache settings.
type CacheConfig struct {
	Enabled         bool          `yaml:"enabled"`
	Backend         string        `yaml:"backend"`          // "memory" (default), "redis", "qdrant", "pgvector"
	EmbeddingModel  string        `yaml:"embedding_model"`  // model name from models:[] that supports embeddings
	TweakModel      string        `yaml:"tweak_model"`      // model for TWEAK band (optional, must be in models:[])
	DirectThreshold float64       `yaml:"direct_threshold"` // score >= this → DIRECT (default 0.95)
	ReuseThreshold  float64       `yaml:"reuse_threshold"`  // score >= this → REUSE (default 0.90)
	TweakThreshold  float64       `yaml:"tweak_threshold"`  // score >= this → TWEAK (default 0.85)
	AlphaWeight     float64       `yaml:"alpha"`            // query embedding weight (default 0.7)
	BetaWeight      float64       `yaml:"beta"`             // context embedding weight (default 0.3)
	TTL             time.Duration `yaml:"ttl"`              // entry time-to-live (default 24h)
	MaxEntries      int           `yaml:"max_entries"`      // max cache entries (default 50000)
	// Backend-specific settings
	RedisURL  string `yaml:"redis_url"`  // Redis connection URL (for redis backend)
	QdrantURL string `yaml:"qdrant_url"` // Qdrant HTTP URL (for qdrant backend)
	FilePath  string `yaml:"file_path"`  // persistence file path (for memory backend)
}

// CacheDefaults fills zero-value fields with sane defaults.
func (c *CacheConfig) ApplyDefaults() {
	if c.Backend == "" {
		c.Backend = "memory"
	}
	if c.DirectThreshold == 0 {
		c.DirectThreshold = 0.95
	}
	if c.ReuseThreshold == 0 {
		c.ReuseThreshold = 0.90
	}
	if c.TweakThreshold == 0 {
		c.TweakThreshold = 0.85
	}
	if c.AlphaWeight == 0 {
		c.AlphaWeight = 0.7
	}
	if c.BetaWeight == 0 {
		c.BetaWeight = 0.3
	}
	if c.TTL == 0 {
		c.TTL = 24 * time.Hour
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = 50000
	}
}

// HiveStateConfig defines the intent-aware conversational state engine settings.
type HiveStateConfig struct {
	Enabled      bool            `yaml:"enabled"`
	Model        string          `yaml:"model"`          // model for state extraction (from models:[])
	Threshold    int             `yaml:"threshold"`      // token count below this → pass through (default 4000)
	MaxLatencyMs int             `yaml:"max_latency_ms"` // timeout for state extraction in ms (default 3000)
	StepWindow   int             `yaml:"step_window"`    // number of recent assistant steps to preserve uncompressed (default 4)
	TokenBudget  int             `yaml:"token_budget"`   // max tokens for result (Recent+State); if exceeded, reduce step_window dynamically (0=unlimited)
	RecentWindow int             `yaml:"recent_window"`  // deprecated: kept for backward compat, ignored
	HiveRoute    HiveRouteConfig `yaml:"hiveroute"`      // automatic model routing based on task difficulty

	// AppendOnlyState builds the state as a growing log of frozen blocks
	// instead of re-extracting a snapshot every turn. The snapshot rewrites
	// the head of the prompt and so invalidates the provider prefix cache on
	// every request; a log only ever appends, which the cache rewards.
	AppendOnlyState bool `yaml:"append_only_state"`

	// PrefixCacheGuard gates HiveState on provider prompt-cache economics.
	PrefixCacheGuard PrefixCacheGuardConfig `yaml:"prefix_cache_guard"`
}

// HiveRouteConfig defines automatic model routing based on task difficulty classification.
type HiveRouteConfig struct {
	Enabled         bool             `yaml:"enabled"`
	Levels          []HiveRouteLevel `yaml:"levels"`           // difficulty levels with model mappings
	ThinkingBudgets map[string]int   `yaml:"thinking_budgets"` // reasoning_effort → budget_tokens mapping (for legacy Anthropic models)
}

// HiveRouteLevel defines a difficulty level and its target model + effort.
type HiveRouteLevel struct {
	Name        string `yaml:"name" json:"name"`               // e.g. "trivial", "standard", "complex"
	Model       string `yaml:"model" json:"model"`             // gateway model name to route to
	Description string `yaml:"description" json:"description"` // human description (injected into extraction prompt)
}

// PrefixCacheGuardConfig controls whether HiveState is allowed to rewrite the
// head of a conversation that the provider is already serving from its prompt
// cache.
//
// Provider caches are positional: rewriting at the head re-bills every
// surviving token at full input price. On Anthropic (cache reads at 0.1x) that
// makes a head rewrite more expensive than passthrough unless it shrinks the
// prompt below ~10% of its original size. The guard runs that comparison per
// request and skips HiveState when passthrough is cheaper.
type PrefixCacheGuardConfig struct {
	// Enabled turns the guard on. Default true.
	Enabled *bool `yaml:"enabled"`
	// AnthropicCacheReadRatio is the cache-read price multiplier for
	// /v1/messages traffic (default 0.1).
	AnthropicCacheReadRatio float64 `yaml:"anthropic_cache_read_ratio"`
	// OpenAICacheReadRatio is the cache-read price multiplier for
	// /v1/chat/completions traffic (default 0.5).
	OpenAICacheReadRatio float64 `yaml:"openai_cache_read_ratio"`
	// AssumeImplicitCache extends the guard to providers that expose no cache
	// markers (OpenAI, Responses), where a warm cache can only be guessed at
	// from the shape of the conversation. Off by default: a wrong guess
	// silently disables HiveState on traffic that had no cache to protect.
	// Anthropic is unaffected — its cache_control markers are real evidence
	// and are always honoured.
	AssumeImplicitCache bool `yaml:"assume_implicit_cache"`
	// MinCacheableTokens is the prompt size below which no implicit provider
	// cache is assumed (default 1024, OpenAI's documented minimum). Only
	// consulted when AssumeImplicitCache is true.
	MinCacheableTokens int `yaml:"min_cacheable_tokens"`
	// StateSummaryChars is the assumed serialized size of the injected state
	// summary, used to probe the rewrite before extraction (default 1200).
	StateSummaryChars int `yaml:"state_summary_chars"`
	// Margin is the fractional advantage a rewrite must show over passthrough
	// before it may bust the cache (default 0.10).
	Margin float64 `yaml:"margin"`
	// AmortizeOverTurns is how many turns a rewrite is judged over. A rewrite
	// always loses on the turn it happens — it discards a warm cache to build
	// a smaller one — so at 1 it can never be chosen however much it shrinks
	// the prompt. Only meaningful with append_only_state, which is what makes
	// the rewritten prompt cacheable in the first place (default 1).
	//
	// This is an assumption about how much longer the session runs, not a
	// measurement — the same kind of guess Headroom's net-cost formula makes
	// with its default expected_reads=10. The guard reports the break-even
	// turn count it actually computed (x-hivestate-break-even-turns,
	// "break_even_turns" in the skip log) alongside the decision, so this
	// number can be checked against reality instead of trusted blindly.
	AmortizeOverTurns int `yaml:"amortize_over_turns"`
}

// AmortizeTurns returns the horizon the guard judges a rewrite over.
func (c *PrefixCacheGuardConfig) AmortizeTurns() int {
	if c.AmortizeOverTurns < 1 {
		return 1
	}
	return c.AmortizeOverTurns
}

// GuardEnabled reports whether the prefix-cache guard is active. It defaults
// to true so existing deployments gain the protection without a config change.
func (c *PrefixCacheGuardConfig) GuardEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// ApplyDefaults fills zero-value HiveState fields with sane defaults.
func (c *HiveStateConfig) ApplyDefaults() {
	if c.Threshold == 0 {
		c.Threshold = 4000
	}
	if c.MaxLatencyMs == 0 {
		c.MaxLatencyMs = 3000
	}
	if c.StepWindow == 0 {
		c.StepWindow = 4
	}
	// TokenBudget=0 means unlimited (no cap)
	c.PrefixCacheGuard.ApplyDefaults()
}

// ApplyDefaults fills zero-value prefix-cache guard fields.
func (c *PrefixCacheGuardConfig) ApplyDefaults() {
	if c.AnthropicCacheReadRatio == 0 {
		c.AnthropicCacheReadRatio = 0.1
	}
	if c.OpenAICacheReadRatio == 0 {
		c.OpenAICacheReadRatio = 0.5
	}
	if c.MinCacheableTokens == 0 {
		c.MinCacheableTokens = 1024
	}
	if c.StateSummaryChars == 0 {
		c.StateSummaryChars = 1200
	}
	if c.Margin == 0 {
		c.Margin = 0.10
	}
}

// MaxLatency returns the max latency as a time.Duration.
func (c *HiveStateConfig) MaxLatency() time.Duration {
	return time.Duration(c.MaxLatencyMs) * time.Millisecond
}

// SteeringConfig controls what the model is asked to produce, as opposed to
// what it is asked to read.
//
// Output tokens cost several times what input tokens cost, so trimming the
// answer is worth more per byte than trimming the prompt. Off by default: it
// changes what the model writes, not just how the request is encoded.
//
// Reasoning effort is not here on purpose — HiveRoute owns that decision, and
// two middlewares writing the same field would eventually disagree.
type SteeringConfig struct {
	Enabled bool `yaml:"enabled"`
	// VerbosityNote is appended to the end of the system prompt. Empty
	// disables the lever. It must be a constant: a note that varies between
	// turns moves every token after it and costs a prompt-cache miss each
	// time.
	VerbosityNote string `yaml:"verbosity_note"`
}

// ApplyDefaults fills zero-value steering fields.
func (c *SteeringConfig) ApplyDefaults() {
	if c.VerbosityNote == "" {
		c.VerbosityNote = defaultVerbosityNote
	}
}

// defaultVerbosityNote tells the model to stop paying for text that repeats
// what the caller already has. It names the specific habits that cost the most
// in an agentic loop rather than asking for brevity in the abstract, which
// models comply with only briefly.
const defaultVerbosityNote = "Output style: be concise. Do not restate the " +
	"contents of files, tool results, or instructions you were just given. " +
	"Skip preamble, summaries of what you are about to do, and recaps of what " +
	"you just did. Answer directly."

// LiveCompressionConfig controls compression of the newest tool output
// entering a prompt.
//
// This is the tail of the prompt, past the reach of any provider prompt cache,
// so shrinking it cannot invalidate a cached prefix the way rewriting history
// does. Lossless compaction is safe enough to enable broadly; discarding
// content is a policy decision and stays opt-in.
type LiveCompressionConfig struct {
	// Enabled turns the middleware on. Default false: a new transform on the
	// request path earns its way in per deployment.
	Enabled bool `yaml:"enabled"`
	// AllowLossy permits transforms that discard content — collapsing repeated
	// log lines, truncating long JSON arrays. Without it only encoding
	// overhead is removed.
	AllowLossy bool `yaml:"allow_lossy"`
	// CanaryPercent limits the change to a share of requests, chosen by a hash
	// of the API key so a given caller sees consistent behaviour rather than
	// flickering between compressed and not. 0 with Enabled means everyone.
	CanaryPercent int `yaml:"canary_percent"`
	// ProtectedTools extends the built-in never-compress list. Entries may end
	// in "*" to cover a whole MCP server.
	ProtectedTools []string `yaml:"protected_tools"`
	// MinBytes is the block size below which nothing is attempted.
	MinBytes int `yaml:"min_bytes"`
	// MaxBytes caps the block a transform will read, so a pathological payload
	// cannot stall the request path.
	MaxBytes int `yaml:"max_bytes"`
	// MinGainPercent is the saving a transform must show to be applied.
	MinGainPercent int `yaml:"min_gain_percent"`
	// LiveTurns is how many trailing user turns count as live. 1 is the
	// newest turn only.
	LiveTurns int `yaml:"live_turns"`
	// DedupeRepeats replaces a tool result that repeats one already in the
	// request with a reference to the first copy, which is left untouched.
	// Separate from AllowLossy: nothing leaves the conversation, it is only
	// stated once.
	DedupeRepeats bool `yaml:"dedupe_repeats"`
	// DeltaRepeats extends DedupeRepeats to results that are a small edit of
	// an earlier one — the same file re-read after a change — replacing the
	// later copy with a statement of what differs. This is what a long coding
	// session spends most of its prompt on.
	DeltaRepeats bool `yaml:"delta_repeats"`
	// PruneReasoning drops the reasoning items of already-answered turns from
	// a /v1/responses body. Codex sends back every step's reasoning for the
	// whole session; only the chain since the last user message is still live.
	PruneReasoning bool `yaml:"prune_reasoning"`
	// RetrieveTTLMinutes keeps the originals of content a lossy transform
	// removed, so a caller can fetch what was taken out. 0 disables retrieval,
	// which makes every lossy transform final.
	RetrieveTTLMinutes int `yaml:"retrieve_ttl_minutes"`
	// RetrieveMaxEntries and RetrieveMaxMB bound that store. A store that
	// cannot forget is a leak, not a cache.
	RetrieveMaxEntries int `yaml:"retrieve_max_entries"`
	RetrieveMaxMB      int `yaml:"retrieve_max_mb"`
	// JSONMinItems, JSONKeepHead and JSONKeepTail bound array truncation.
	JSONMinItems int `yaml:"json_min_items"`
	JSONKeepHead int `yaml:"json_keep_head"`
	JSONKeepTail int `yaml:"json_keep_tail"`
	// DiffContext is how many unchanged lines to keep around each change.
	DiffContext int `yaml:"diff_context"`
	// CSVMinRows, CSVKeepHead and CSVKeepTail bound tabular truncation.
	CSVMinRows  int `yaml:"csv_min_rows"`
	CSVKeepHead int `yaml:"csv_keep_head"`
	CSVKeepTail int `yaml:"csv_keep_tail"`
}

// ApplyDefaults fills zero-value live-compression fields.
func (c *LiveCompressionConfig) ApplyDefaults() {
	if c.MinBytes == 0 {
		c.MinBytes = 512
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 4 << 20
	}
	if c.MinGainPercent == 0 {
		c.MinGainPercent = 5
	}
	if c.LiveTurns == 0 {
		c.LiveTurns = 1
	}
	// Retrieval is only worth defaulting on when something can be lost. The
	// TTL matches how long an agent might plausibly come back for a tool
	// result it saw shortened earlier in the same task.
	if c.AllowLossy && c.RetrieveTTLMinutes == 0 {
		c.RetrieveTTLMinutes = 60
	}
	if c.RetrieveMaxEntries == 0 {
		c.RetrieveMaxEntries = 512
	}
	if c.RetrieveMaxMB == 0 {
		c.RetrieveMaxMB = 128
	}
	if c.JSONMinItems == 0 {
		c.JSONMinItems = 20
	}
	if c.JSONKeepHead == 0 {
		c.JSONKeepHead = 5
	}
	if c.JSONKeepTail == 0 {
		c.JSONKeepTail = 3
	}
	if c.DiffContext == 0 {
		c.DiffContext = 3
	}
	if c.CSVMinRows == 0 {
		c.CSVMinRows = 30
	}
	if c.CSVKeepHead == 0 {
		c.CSVKeepHead = 10
	}
	if c.CSVKeepTail == 0 {
		c.CSVKeepTail = 5
	}
}

// FeedbackConfig defines in-session feedback collection settings.
type FeedbackConfig struct {
	Enabled            bool     `yaml:"enabled"`
	MinIntervalMinutes int      `yaml:"min_interval_minutes"` // min minutes between two prompts (default 20)
	MaxPerDay          int      `yaml:"max_per_day"`          // max prompts per key per day (default 3)
	SavingsMilestones  []int    `yaml:"savings_milestones"`   // dollar thresholds (e.g. [5, 10, 20, 50])
	LongSessionAfter   int      `yaml:"long_session_after"`   // trigger general after N requests (default 50)
	Teams              []string `yaml:"teams"`                // if set, only these team IDs get feedback (empty=all)
}

// ApplyFeedbackDefaults fills zero-value feedback fields with sane defaults.
func (c *FeedbackConfig) ApplyDefaults() {
	if c.MinIntervalMinutes == 0 {
		c.MinIntervalMinutes = 20
	}
	if c.MaxPerDay == 0 {
		c.MaxPerDay = 3
	}
	if len(c.SavingsMilestones) == 0 {
		c.SavingsMilestones = []int{5, 10, 20, 50}
	}
	if c.LongSessionAfter == 0 {
		c.LongSessionAfter = 50
	}
}

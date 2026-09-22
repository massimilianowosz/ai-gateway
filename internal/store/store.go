package store

import (
	"context"
	"errors"
	"time"
)

var ErrKeyNotFound = errors.New("store: key not found")
var ErrStaleGovernanceRevision = errors.New("store: stale governance revision")
var ErrGovernanceRevisionConflict = errors.New("store: governance revision digest conflict")

// Store defines the persistence interface for the gateway.
type Store interface {
	// Keys
	CreateKey(ctx context.Context, key *APIKey) error
	GetKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	ListKeys(ctx context.Context, filter KeyFilter) ([]APIKey, error)
	UpdateKey(ctx context.Context, hash string, params UpdateKeyParams) error
	DeleteKey(ctx context.Context, hash string) error

	// Teams
	CreateTeam(ctx context.Context, team *Team) error
	GetTeam(ctx context.Context, id string) (*Team, error)
	ListTeams(ctx context.Context, filter TeamFilter) ([]Team, error)
	UpdateTeam(ctx context.Context, id string, params UpdateTeamParams) error
	DeleteTeam(ctx context.Context, id string) error

	// Users
	CreateUser(ctx context.Context, user *User) error
	GetUser(ctx context.Context, id string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	ListUsers(ctx context.Context, filter UserFilter) ([]User, error)
	UpdateUser(ctx context.Context, id string, params UpdateUserParams) error
	DeleteUser(ctx context.Context, id string) error

	// Spend
	LogSpend(ctx context.Context, record SpendRecord) error
	GetSpend(ctx context.Context, filter SpendFilter) ([]SpendRecord, error)
	GetSpendSummary(ctx context.Context, filter SpendFilter) ([]SpendSummary, error)
	GetTotalSpend(ctx context.Context, keyHash string) (float64, error)
	// PurgeExpiredSpendRecords deletes gw_spend_records rows for keys that
	// carry a request_logging retention override (KeyRouteSettings.
	// LogRetentionDays), past that many days old. A key with no override is
	// never touched — nil means keep forever, not delete immediately. Every
	// row is folded into SpendDailySummary before it is deleted, in the same
	// transaction, so the total it contributed survives even though the row
	// itself does not.
	PurgeExpiredSpendRecords(ctx context.Context) (int64, error)

	// Cache metrics
	LogCacheMetric(ctx context.Context, metric CacheMetric) error
	ListCacheMetrics(ctx context.Context, limit int) ([]CacheMetric, error)
	DeleteCacheMetrics(ctx context.Context) error

	// HiveState metrics
	LogHiveStateMetric(ctx context.Context, metric HiveStateMetric) error
	ListHiveStateMetrics(ctx context.Context, limit int) ([]HiveStateMetric, error)

	// Tenant settings
	LogKeyEvent(ctx context.Context, event KeyEvent) error
	GetTenantSettings(ctx context.Context, teamID string) (*TenantSettings, error)
	UpsertTenantSettings(ctx context.Context, settings *TenantSettings) error

	// Key route settings
	GetKeyRouteSettings(ctx context.Context, keyID string) (*KeyRouteSettings, error)
	UpsertKeyRouteSettings(ctx context.Context, settings *KeyRouteSettings) error
	DeleteKeyRouteSettings(ctx context.Context, keyID string) error

	// Feedback
	StoreFeedback(ctx context.Context, record *FeedbackRecord) error
	ListFeedback(ctx context.Context, teamID string, limit int) ([]FeedbackRecord, error)
	GetFeedbackFiredToday(ctx context.Context, keyHash string) (triggers []string, count int, err error)
	SetKeyFeedbackEnabled(ctx context.Context, keyID string, enabled bool) error
	GetFeedbackSessionSignals(ctx context.Context, keyPrefix string) (*FeedbackSessionSignals, error)

	// Migrations
	Migrate(ctx context.Context) error

	// Lifecycle
	Close() error
}

// APIKey represents a virtual API key (stored in api_keys_local).
type APIKey struct {
	ID                    string     `json:"id" gorm:"primaryKey;column:id"`
	KeyHash               string     `json:"gateway_key_id" gorm:"uniqueIndex;column:gateway_key_id;not null"`
	KeyPrefix             string     `json:"key_prefix" gorm:"column:key_prefix"`
	Name                  string     `json:"name" gorm:"column:key_name"`
	TeamID                string     `json:"team_id" gorm:"index;column:team_id"`
	Budget                float64    `json:"budget" gorm:"column:budget;default:0"`
	WalletAllocatedBudget float64    `json:"wallet_allocated_budget" gorm:"column:wallet_allocated_budget;default:0"`
	Spend                 float64    `json:"spend" gorm:"column:spend;default:0"`
	RateLimit             int        `json:"rate_limit" gorm:"column:rate_limit;default:0"`
	Models                StringList `json:"models" gorm:"column:models;type:text"`
	// DeniedModels is evaluated after canonical model alias resolution and
	// takes precedence over every allow/grant list. Nil and [] both mean that
	// no model is denied; an empty deny-list is never a deny-all sentinel.
	DeniedModels StringList `json:"denied_models" gorm:"column:denied_models;type:text"`
	// AllowedProviders mirrors Models' NULL/[]/list semantics one level up:
	// NULL means every provider a requested model can reach is fine, [] denies
	// every provider outright, and a list restricts to those named. Written
	// only by policy_gateway_sync's provider_allowlist projection.
	AllowedProviders StringList `json:"allowed_providers" gorm:"column:allowed_providers;type:text"`
	// DeniedProviders takes precedence over AllowedProviders. Routing filters
	// denied deployments before selection/retry so an allowed alternative may
	// still be used without ever falling back to a denied provider.
	DeniedProviders StringList `json:"denied_providers" gorm:"column:denied_providers;type:text"`
	// RequireEUResidency, unlike AllowedProviders, needs no NULL-vs-empty
	// tri-state — it is a plain always-meaningful flag, so it is written
	// through the normal update_key HTTP path rather than a direct ORM write.
	// Enforced as ALL of a model's deployments being EU (Deployment.IsEU), not
	// just one: a compliance claim must not be satisfiable by a model that can
	// still fail over to a non-EU deployment.
	RequireEUResidency bool       `json:"require_eu_residency" gorm:"column:require_eu_residency;default:false"`
	Active             bool       `json:"active" gorm:"index;column:active;default:true"`
	ExpiresAt          *time.Time `json:"expires_at" gorm:"column:expires_at"`
	// Who the credential belongs to. Written by the portal, read here: a key
	// whose agent is not active is refused, which is what makes suspending an
	// agent a control rather than a note.
	AgentID         string    `json:"agent_id" gorm:"column:agent_id"`
	CreatedByUserID string    `json:"created_by_user_id" gorm:"column:created_by_user_id"`
	CreatedAt       time.Time `json:"created_at" gorm:"column:created_at"`
	UpdatedAt       time.Time `json:"updated_at" gorm:"column:updated_at"`
	// ContextInstructions is the assembled, published-only text of every
	// context pack Ubiquum's portal resolved as applicable to this key's
	// agent/environment. The portal recomputes and pushes it on every
	// publish/binding change; the gateway only ever reads it — never derives
	// or edits it — so a key with no packs bound simply carries "".
	ContextInstructions string `json:"context_instructions,omitempty" gorm:"column:context_instructions;type:text"`
	// ContextInstructionsDigest is the sha256 of the pack ids/versions that
	// produced ContextInstructions, for audit trails on the portal side. The
	// gateway does not interpret it.
	ContextInstructionsDigest string `json:"context_instructions_digest,omitempty" gorm:"column:context_instructions_digest"`
	// GovernanceBlocked is a scope-level authorization state, distinct from
	// Active (a manual admin kill switch, unrelated reason) and from any
	// single enforcement field (RateLimit/Budget/RequireEUResidency/Models).
	// Ubiquum's policy engine sets it the moment ANY policy kind's
	// enforcing composition is invalid for this key's agent/environment —
	// including kinds with no per-field deny projection of their own, such
	// as model_routing or guardrail_set — so a conflict there still stops
	// traffic instead of silently falling back to the tenant default.
	// Checked in Authenticate, before CheckBudget's flat/BYOK economic
	// exemption: identity and authorization must never be skippable by that
	// exemption. Stays set until a valid revision is projected. See
	// policy_gateway_sync.scope_authorization_for on the Ubiquum side.
	GovernanceBlocked  bool `json:"governance_blocked" gorm:"column:governance_blocked;default:false"`
	GovernanceRequired bool `json:"governance_required" gorm:"column:governance_required;default:false"`
	// GovernanceBlockReason names which kind(s) made the composition
	// invalid, for the error surfaced to the caller and for audit. Empty
	// when GovernanceBlocked is false.
	GovernanceBlockReason        string     `json:"governance_block_reason,omitempty" gorm:"column:governance_block_reason;type:text"`
	GovernanceRevision           int64      `json:"governance_revision" gorm:"column:governance_revision;default:0"`
	GovernanceDigest             string     `json:"governance_digest,omitempty" gorm:"column:governance_digest;type:text"`
	GovernanceSchemaVersion      int        `json:"governance_schema_version" gorm:"column:governance_schema_version;default:0"`
	GovernanceCompilerVersion    int        `json:"governance_compiler_version" gorm:"column:governance_compiler_version;default:0"`
	GovernanceAgentEnvironmentID string     `json:"governance_agent_environment_id,omitempty" gorm:"column:governance_agent_environment_id;type:text"`
	GovernanceSnapshot           string     `json:"-" gorm:"column:governance_snapshot;type:text"`
	GovernanceAppliedAt          *time.Time `json:"governance_applied_at,omitempty" gorm:"column:governance_applied_at"`
	AutoRechargeEnabled          bool       `json:"auto_recharge_enabled" gorm:"column:auto_recharge_enabled;default:false"`
	RechargeThreshold            float64    `json:"recharge_threshold" gorm:"column:recharge_threshold;default:10"`
	RechargeAmount               float64    `json:"recharge_amount" gorm:"column:recharge_amount;default:50"`
}

// GovernanceApplyParams is a complete, validated replacement derived from one
// immutable control-plane snapshot. The store advances the runtime values and
// their revision pointer in one transaction.
type GovernanceApplyParams struct {
	KeyHash, AgentID, AgentEnvironmentID, Digest, CanonicalSnapshot string
	Revision                                                        int64
	SchemaVersion, CompilerVersion                                  int
	Models, DeniedModels, AllowedProviders, DeniedProviders         StringList
	RateLimit                                                       int
	RequireEUResidency, GovernanceBlocked                           bool
	GovernanceBlockReason                                           string
	ContextInstructions, ContextInstructionsDigest                  string
	Budget                                                          *float64
	WalletAllocatedBudget                                           *float64
	AutoRechargeEnabled                                             bool
	RechargeThreshold, RechargeAmount                               float64
	RouteSettings                                                   GovernanceRouteSettings
}

type GovernanceRouteSettings struct {
	Enabled                                                             *bool
	Levels                                                              []RouteLevel
	ThinkingBudgets                                                     map[string]int
	FeedbackEnabled, CacheEnabled, CompressionEnabled                   *bool
	CacheDirectThreshold, CacheReuseThreshold, CacheTweakThreshold      *float64
	CompressionThreshold, CompressionTokenBudget, CompressionStepWindow *int
	CompressionAppendOnlyState                                          *bool
	GuardrailOverride                                                   *GuardrailOverride
	LogRetentionDays                                                    *int
}

type GovernanceApplyResult struct {
	Applied  bool   `json:"applied"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

// CreateKeyParams holds parameters for creating a new key.
type CreateKeyParams struct {
	Name               string     `json:"name"`
	TeamID             string     `json:"team_id"`
	Budget             float64    `json:"budget"`
	RateLimit          int        `json:"rate_limit"`
	Models             []string   `json:"models"`
	ExpiresAt          *time.Time `json:"expires_at"`
	GovernanceRequired bool       `json:"governance_required"`
}

// UpdateKeyParams holds parameters for updating a key.
type UpdateKeyParams struct {
	Name             *string    `json:"name,omitempty"`
	Budget           *float64   `json:"budget,omitempty"`
	RateLimit        *int       `json:"rate_limit,omitempty"`
	Models           []string   `json:"models,omitempty"`
	DeniedModels     []string   `json:"denied_models,omitempty"`
	AllowedProviders []string   `json:"allowed_providers,omitempty"`
	DeniedProviders  []string   `json:"denied_providers,omitempty"`
	Active           *bool      `json:"active,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	// ContextInstructions/ContextInstructionsDigest: see APIKey. A non-nil
	// pointer here — including one pointing at "" — replaces the stored
	// value, so the portal can clear an emptied bundle by pushing "".
	ContextInstructions       *string `json:"context_instructions,omitempty"`
	ContextInstructionsDigest *string `json:"context_instructions_digest,omitempty"`
	RequireEUResidency        *bool   `json:"require_eu_residency,omitempty"`
	GovernanceBlocked         *bool   `json:"governance_blocked,omitempty"`
	// GovernanceBlockReason: a non-nil pointer, including one pointing at
	// "", always overwrites — the same convention ContextInstructions uses
	// — so unblocking a scope can clear a stale reason instead of leaving
	// it stuck from the last denial.
	GovernanceBlockReason *string `json:"governance_block_reason,omitempty"`
}

// KeyFilter for listing keys.
type KeyFilter struct {
	TeamID string
	Active *bool
	Limit  int
	Offset int
}

// ApplyIdentity copies onto a spend row everything known about who made the
// call. Every handler that logs spend used to do this by hand, which is how a
// row ended up naming the key and nothing else: adding a field meant finding
// thirteen places, and missing one produced traffic attributable to nobody.
func (r *SpendRecord) ApplyIdentity(key *APIKey) {
	if key == nil {
		return
	}
	r.KeyHash = key.KeyHash
	r.KeyPrefix = key.KeyPrefix
	r.TeamID = key.TeamID
	r.AgentID = key.AgentID
	r.UserID = key.CreatedByUserID
}

// SpendRecord represents a single API call cost entry.
type SpendRecord struct {
	ID               string `json:"id" gorm:"primaryKey"`
	KeyHash          string `json:"-" gorm:"index;not null"`
	KeyPrefix        string `json:"key_prefix"`
	TeamID           string `json:"team_id" gorm:"index"`
	Model            string `json:"model" gorm:"index"`
	Provider         string `json:"provider"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	// CachedPromptTokens and CacheCreationTokens are subsets of PromptTokens,
	// billed at the provider's cache-read and cache-write tariffs. They are
	// not additional tokens — summing them into the total would double count.
	CachedPromptTokens  int `json:"cached_prompt_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
	// ReasoningTokens is the subset of CompletionTokens the model spent
	// thinking. Billed at the output rate and, on a reasoning model, usually
	// most of the bill — summing it into the total would double count.
	ReasoningTokens int `json:"reasoning_tokens"`
	// CostEstimated is true when the provider never reported a cache
	// breakdown, so Cost assumes nothing was cached. The charge is then an
	// upper bound rather than a measurement. Flat-billed rows are not
	// estimates: they are exactly zero by policy.
	CostEstimated bool    `json:"cost_estimated"`
	Cost          float64 `json:"cost"`
	Duration      int64   `json:"duration_ms"`
	Status        int     `json:"status"`
	IsEU          bool    `json:"is_eu"`
	AuthMode      string  `json:"auth_mode,omitempty"`
	BillingMode   string  `json:"billing_mode,omitempty"`
	// Copied from the key so a request can be attributed after the fact. A
	// spend row named only the key, and a key named nobody, so neither the
	// person nor the agent behind a call could be recovered from it.
	AgentID   string    `json:"agent_id,omitempty" gorm:"column:agent_id;index"`
	UserID    string    `json:"user_id,omitempty" gorm:"column:user_id;index"`
	CreatedAt time.Time `json:"created_at" gorm:"index"`
}

// SpendSummary is an aggregated spend view per model.
type SpendSummary struct {
	Model     string  `json:"model"`
	Provider  string  `json:"provider"`
	KeyPrefix string  `json:"key_prefix"`
	Requests  int     `json:"requests"`
	Tokens    int     `json:"tokens"`
	Cost      float64 `json:"cost"`
	AvgMs     float64 `json:"avg_ms"`
}

// SpendDailySummary is a persisted per-day rollup that PurgeExpiredSpendRecords
// writes for the exact rows it is about to delete, in the same transaction as
// the delete that earns it. Retention narrows row-level detail down to a day
// once it expires; it must never make the total disappear, so GetSpendSummary
// reads this table too for any date range a purge has already swept.
type SpendDailySummary struct {
	ID          string    `json:"id" gorm:"primaryKey"`
	Day         time.Time `json:"day" gorm:"column:day;uniqueIndex:idx_spend_daily_summary,priority:1"`
	KeyHash     string    `json:"-" gorm:"column:key_hash;not null;uniqueIndex:idx_spend_daily_summary,priority:2"`
	KeyPrefix   string    `json:"key_prefix" gorm:"column:key_prefix"`
	TeamID      string    `json:"team_id" gorm:"index;column:team_id"`
	Model       string    `json:"model" gorm:"column:model;uniqueIndex:idx_spend_daily_summary,priority:3"`
	Provider    string    `json:"provider" gorm:"column:provider;uniqueIndex:idx_spend_daily_summary,priority:4"`
	Requests    int       `json:"requests" gorm:"column:requests"`
	TotalTokens int       `json:"total_tokens" gorm:"column:total_tokens"`
	TotalCost   float64   `json:"total_cost" gorm:"column:total_cost"`
	// TotalDurationMs, not an average: an average cannot be merged across
	// several purge runs without storing the count it was taken over, which
	// Requests already is.
	TotalDurationMs int64 `json:"total_duration_ms" gorm:"column:total_duration_ms"`
}

func (SpendDailySummary) TableName() string { return "gw_spend_daily_summaries" }

// SpendFilter for querying spend records.
type SpendFilter struct {
	KeyHash   string
	TeamID    string
	Model     string
	StartDate *time.Time
	EndDate   *time.Time
	Limit     int
	Offset    int
}

// Table name overrides — APIKey and Team now use portal tables directly.
func (APIKey) TableName() string      { return "api_keys_local" }
func (SpendRecord) TableName() string { return "gw_spend_records" }

package store

import (
	"context"
	"encoding/json"
	"time"
)

// Team represents an organizational group (stored in tenants table).
type Team struct {
	InternalID         string     `json:"-" gorm:"primaryKey;column:id"`
	ID                 string     `json:"id" gorm:"uniqueIndex;column:gateway_team_id;not null"`
	Name               string     `json:"name" gorm:"column:name;not null"`
	Budget             float64    `json:"budget" gorm:"column:budget;default:0"`
	Spend              float64    `json:"spend" gorm:"column:spend;default:0"`
	AllowedModels      StringList `json:"allowed_models" gorm:"column:allowed_models;type:text"`
	GrantedModels      StringList `json:"granted_models" gorm:"column:granted_models;type:text"`
	SubscriptionTier   string     `json:"subscription_tier" gorm:"column:subscription_tier;default:free"`
	SubscriptionStatus string     `json:"subscription_status" gorm:"column:subscription_status;default:active"`
	CreatedAt          time.Time  `json:"created_at" gorm:"column:created_at"`
	UpdatedAt          time.Time  `json:"updated_at" gorm:"column:updated_at"`
}

// User represents a member of a team.
type User struct {
	ID        string    `json:"id" gorm:"primaryKey"`
	Email     string    `json:"email" gorm:"uniqueIndex;not null"`
	Name      string    `json:"name"`
	TeamID    string    `json:"team_id" gorm:"index;not null"`
	Role      string    `json:"role" gorm:"default:member"` // "admin" or "member"
	Active    bool      `json:"active" gorm:"index;default:true"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateTeamParams holds parameters for creating a team.
type CreateTeamParams struct {
	Name   string  `json:"name"`
	Budget float64 `json:"budget"`
}

// UpdateTeamParams holds parameters for updating a team.
type UpdateTeamParams struct {
	Name          *string  `json:"name,omitempty"`
	Budget        *float64 `json:"budget,omitempty"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	GrantedModels []string `json:"granted_models,omitempty"`
}

// TeamFilter for listing teams.
type TeamFilter struct {
	Limit  int
	Offset int
}

// CreateUserParams holds parameters for creating a user.
type CreateUserParams struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	TeamID string `json:"team_id"`
	Role   string `json:"role"`
}

// UpdateUserParams holds parameters for updating a user.
type UpdateUserParams struct {
	Name   *string `json:"name,omitempty"`
	TeamID *string `json:"team_id,omitempty"`
	Role   *string `json:"role,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

// UserFilter for listing users.
type UserFilter struct {
	TeamID string
	Active *bool
	Limit  int
	Offset int
}

// CacheMetric records a single cache evaluation (hit or miss).
type CacheMetric struct {
	ID          uint   `gorm:"primaryKey;autoIncrement"`
	TeamID      string `gorm:"index"`
	KeyPrefix   string `gorm:"index"`
	Model       string `gorm:"index"`
	Band        string // DIRECT, REUSE, TWEAK, MISS
	Score       float64
	TokensSaved int
	LatencyMs   int64
	CreatedAt   time.Time `gorm:"index"`
}

// HiveStateMetric records a single HiveState processing event.
type HiveStateMetric struct {
	ID             uint   `gorm:"primaryKey;autoIncrement"`
	RequestID      string `gorm:"index"`
	Model          string `gorm:"index"`
	RequestModel   string `gorm:"index"`
	KeyPrefix      string `gorm:"index"`
	Mode           string // "state" or "none"
	Intent         string
	OriginalTokens int
	ResultTokens   int
	Ratio          float64
	LatencyMs      int64
	FallbackReason string
	// HiveRoute fields
	RouteLevel     string  // difficulty level chosen (e.g. "trivial", "standard", "complex")
	RouteModel     string  // target model routed to
	RouteEffort    string  // reasoning effort decided (low/medium/high)
	RouteSavedCost float64 // cost delta: (default model cost - routed model cost) per 1K tokens estimate
	// CompressionSavedCost is what the history rewrite saved, computed from the
	// provider's real usage. It has a column of its own because RouteSavedCost
	// is a pure routing figure that the CLI and the savings aggregation both
	// read as such; folding the two together made neither answerable, and
	// dropping this one left it in a log line and nowhere else.
	CompressionSavedCost float64
	CreatedAt            time.Time `gorm:"index"`
}

func (HiveStateMetric) TableName() string { return "gw_state_metrics" }

// TenantSettings stores per-team configuration (guardrails, cache, compression, etc.).
// The model whitelist is not here: tenants.allowed_models is the single source
// of truth, and a second copy in this table was written by the portal but never
// read by access control.
type TenantSettings struct {
	TeamID                   string          `json:"team_id" gorm:"primaryKey;column:team_id"`
	GuardrailsConfig         map[string]bool `json:"guardrails_config" gorm:"column:guardrails_config;serializer:json"`
	DefaultWorkflowID        *string         `json:"-" gorm:"column:default_workflow_id"`
	CacheEnabled             bool            `json:"cache_enabled" gorm:"column:cache_enabled;default:true"`
	CompressionEnabled       bool            `json:"compression_enabled" gorm:"column:compression_enabled;default:true"`
	HiveRouteEnabled         *bool           `json:"hiveroute_enabled,omitempty" gorm:"column:hiveroute_enabled"`                                   // nil=inherit global, true/false=override
	HiveRouteLevels          []RouteLevel    `json:"hiveroute_levels,omitempty" gorm:"column:hiveroute_levels;serializer:json"`                     // per-team difficulty→model mappings
	HiveRouteThinkingBudgets map[string]int  `json:"hiveroute_thinking_budgets,omitempty" gorm:"column:hiveroute_thinking_budgets;serializer:json"` // per-team effort→budget overrides
	FeedbackEnabled          *bool           `json:"feedback_enabled,omitempty" gorm:"column:feedback_enabled"`                                     // nil=inherit global, true/false=override
}

// RouteLevel defines a difficulty level and its target model (stored in tenant_settings JSON).
type RouteLevel struct {
	Name        string `json:"name"`
	Model       string `json:"model"`
	Description string `json:"description,omitempty"`
}

// KeyEvent records the lifecycle of an API key: created, changed, revoked.
//
// A key is the only thing the admin API hands out that lets someone spend
// money, and until this existed the act of creating or deleting one left no
// trace anywhere. That is not a gap you notice until you need it: six keys
// once spent under a customer's tenant and were deleted, and answering "who
// made these, and when" meant inferring it from the shape of the traffic
// they left behind. A revoked key's row is gone by definition, so the event
// carries what identified it rather than pointing at it.
type KeyEvent struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Event     string `gorm:"index;column:event;not null"` // created, updated, deleted
	KeyHash   string `gorm:"index;column:key_hash"`
	KeyPrefix string `gorm:"column:key_prefix"`
	KeyName   string `gorm:"column:key_name"`
	TeamID    string `gorm:"index;column:team_id"`
	// Actor is who asked. The admin API authenticates with the master key, so
	// without something more specific every action reads as "the operator";
	// callers that know the human — the portal does — send it in a header.
	Actor     string    `gorm:"column:actor"`
	RemoteIP  string    `gorm:"column:remote_ip"`
	Budget    float64   `gorm:"column:budget"`
	Details   string    `gorm:"column:details"`
	CreatedAt time.Time `gorm:"index;column:created_at"`
}

func (KeyEvent) TableName() string { return "gw_key_events" }

// SecurityEvent records one thing a guardrail found: a PII entity it redacted,
// a secret or an injection it refused. The dashboard's Security Defense card
// counts these rows.
//
// The table is the portal's, and the portal has always owned its schema. The
// gateway only writes to it — and until now it did not even do that: the last
// row was written in May 2026 by the LLM Guard service this gateway replaced,
// so the card read zero for every tenant while the guardrails were doing the
// work.
//
// It deliberately does not store the value that was found. Recording the
// customer's PII in order to report that we prevented it from leaving would
// be the same leak, written to a table with a longer retention.
// Agent is the portal's registry of the non-human principals a tenant runs.
// The gateway only reads it, and only to answer one question on the hot path:
// may this key still be used. Schema is owned by the portal's migrations.
type Agent struct {
	ID       string `json:"id" gorm:"primaryKey;column:id"`
	TenantID string `json:"tenant_id" gorm:"column:tenant_id"`
	Name     string `json:"name" gorm:"column:name"`
	Status   string `json:"status" gorm:"column:status"`
}

func (Agent) TableName() string { return "agents" }

type SecurityEvent struct {
	ID        uint      `gorm:"primaryKey;autoIncrement;column:id"`
	TenantID  string    `gorm:"index;column:tenant_id"`  // the team id, which is what the portal filters on
	EventType string    `gorm:"index;column:event_type"` // pii_detected, secret_detected, security_violation
	Scanner   string    `gorm:"column:scanner_name"`
	Finding   string    `gorm:"column:finding_type"` // EMAIL_ADDRESS, AWS_KEY, jailbreak, ...
	Masked    string    `gorm:"column:masked_value"` // always empty; the column is the portal's
	KeyHash   string    `gorm:"index;column:key_hash"`
	Details   string    `gorm:"column:description"`
	Timestamp time.Time `gorm:"index;column:timestamp"`
}

func (SecurityEvent) TableName() string { return "security_events" }

func (TenantSettings) TableName() string { return "tenant_settings" }

// KeyRouteSettings stores per-key HiveRoute configuration (overrides team settings).
type KeyRouteSettings struct {
	KeyID              string         `json:"key_id" gorm:"primaryKey;column:key_id"`
	TeamID             string         `json:"team_id" gorm:"index;column:team_id;not null"`
	GovernanceRevision int64          `json:"governance_revision" gorm:"column:governance_revision;default:0"`
	Enabled            *bool          `json:"enabled,omitempty" gorm:"column:enabled"`
	Levels             []RouteLevel   `json:"levels,omitempty" gorm:"column:levels;serializer:json"`
	ThinkingBudgets    map[string]int `json:"thinking_budgets,omitempty" gorm:"column:thinking_budgets;serializer:json"`
	FeedbackEnabled    *bool          `json:"feedback_enabled,omitempty" gorm:"column:feedback_enabled"`       // nil=inherit team, false=disabled for this key
	CacheEnabled       *bool          `json:"cache_enabled,omitempty" gorm:"column:cache_enabled"`             // nil=inherit team, true/false=override for this key
	CompressionEnabled *bool          `json:"compression_enabled,omitempty" gorm:"column:compression_enabled"` // nil=inherit team, true/false=override for this key
	// Cache threshold overrides for this key. Only the three per-lookup
	// comparisons: the cache's backend, embedding model and TTL/max-entries
	// janitor sweep are chosen once when the engine is constructed and have
	// no per-key hook.
	CacheDirectThreshold *float64 `json:"cache_direct_threshold,omitempty" gorm:"column:cache_direct_threshold"`
	CacheReuseThreshold  *float64 `json:"cache_reuse_threshold,omitempty" gorm:"column:cache_reuse_threshold"`
	CacheTweakThreshold  *float64 `json:"cache_tweak_threshold,omitempty" gorm:"column:cache_tweak_threshold"`
	// Compression overrides for this key — the values HiveState.Process
	// actually reads per call. live_compression is a separate, global-only
	// subsystem (internal/livezone) with no per-key override path at all and
	// is not covered here.
	CompressionThreshold       *int  `json:"compression_threshold,omitempty" gorm:"column:compression_threshold"`
	CompressionTokenBudget     *int  `json:"compression_token_budget,omitempty" gorm:"column:compression_token_budget"`
	CompressionStepWindow      *int  `json:"compression_step_window,omitempty" gorm:"column:compression_step_window"`
	CompressionAppendOnlyState *bool `json:"compression_append_only_state,omitempty" gorm:"column:compression_append_only_state"`
	// GuardrailOverride lets a guardrail_set policy bound to one agent
	// environment require additional guardrails beyond the tenant/global
	// default for this key alone — always additive, never a removal, the
	// same "policies can only narrow" rule every other projected kind
	// follows. scanners (per-scanner toggles) and fail_mode have no per-key
	// — or even per-tenant, for fail_mode — enforcement hook in the gateway
	// at all and are not covered here.
	GuardrailOverride *GuardrailOverride `json:"guardrail_override,omitempty" gorm:"column:guardrail_override;serializer:json"`
	// LogRetentionDays bounds how long this key's gw_spend_records rows are
	// kept before PurgeExpiredSpendRecords deletes them. nil means no bound —
	// today's actual behavior, since nothing purges spend rows otherwise.
	// enabled from a request_logging policy has no hook here: a spend row is
	// written for every request regardless, because spend_budget's ceiling
	// (already enforced for the same key) reads GetTotalSpend over these same
	// rows — turning that write off per policy would silently break an
	// already-enforced, more safety-critical check for the same scope.
	LogRetentionDays *int `json:"log_retention_days,omitempty" gorm:"column:log_retention_days"`
}

// GuardrailOverride is the per-key portion of KeyRouteSettings that the
// guardrail middleware reads.
type GuardrailOverride struct {
	RequiredGuardrails []string `json:"required_guardrails,omitempty"`
	// NonDerogable means the request-level x-ubiquum-guard/x-guardrails header
	// cannot remove these guardrails, including via "none". The header can
	// still narrow or change every guardrail this key's policy did not
	// require.
	NonDerogable bool `json:"non_derogable,omitempty"`
}

func (KeyRouteSettings) TableName() string { return "key_route_settings" }

// FeedbackRecord stores user feedback collected during sessions.
type FeedbackRecord struct {
	ID                  string          `json:"id" gorm:"primaryKey;column:id"`
	KeyHash             string          `json:"-" gorm:"index;column:key_hash;not null"`
	KeyPrefix           string          `json:"key_prefix" gorm:"column:key_prefix"`
	TeamID              string          `json:"team_id" gorm:"index;column:team_id"`
	TriggerType         string          `json:"trigger_type" gorm:"column:trigger_type;not null"` // hivestate_compression, hiveroute_downgrade, complex_task, long_session, savings_milestone
	TriggerContext      json.RawMessage `json:"trigger_context" gorm:"column:trigger_context;type:jsonb"`
	QuestionShown       string          `json:"question_shown" gorm:"column:question_shown;type:text"`
	OptionsShown        string          `json:"options_shown" gorm:"column:options_shown;type:text"` // JSON of options presented to the user
	Answer              string          `json:"answer" gorm:"column:answer"`
	AnswerNormalized    string          `json:"answer_normalized" gorm:"column:answer_normalized"` // positive, neutral, negative, critical, opt_out
	SessionRequestCount int             `json:"session_request_count" gorm:"column:session_request_count"`
	CreatedAt           time.Time       `json:"created_at" gorm:"column:created_at;index"`
}

func (FeedbackRecord) TableName() string { return "gw_feedback_records" }

// FeedbackSessionSignals holds aggregated session state from gw_state_metrics
// for hydrating feedback sessions across pods.
type FeedbackSessionSignals struct {
	RequestCount int
	Ratios       []float64
	RouteLevels  []string
	TotalSavings float64
}

// ProviderFile stores only the tenant-scoped mapping between a Ubiquum file
// id and the upstream provider file id. File contents are streamed directly to
// the provider and are never persisted by Ubiquum.
type ProviderFile struct {
	ID           string     `json:"id" gorm:"primaryKey;column:id"`
	OwnerID      string     `json:"-" gorm:"index;column:owner_id;not null"`
	Model        string     `json:"-" gorm:"index;column:model;not null"`
	DeploymentID string     `json:"-" gorm:"index;column:deployment_id;not null"`
	UpstreamID   string     `json:"-" gorm:"column:upstream_id;not null"`
	Filename     string     `json:"filename" gorm:"column:filename;not null"`
	Purpose      string     `json:"purpose" gorm:"index;column:purpose;not null"`
	MimeType     string     `json:"-" gorm:"column:mime_type"`
	Bytes        int64      `json:"bytes" gorm:"column:bytes;not null"`
	CreatedAt    time.Time  `json:"created_at" gorm:"index;column:created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty" gorm:"index;column:expires_at"`
}

func (ProviderFile) TableName() string { return "gw_provider_files" }

// ProviderFileFilter controls tenant-scoped Files API listings.
type ProviderFileFilter struct {
	Purpose string
	After   string
	Order   string
	Limit   int
}

// StoredResponse persists one /v1/responses turn so the stateful half of the
// Responses API (previous_response_id, retrieve, cancel, input_items,
// background polling) works on every provider, including those reached through
// the Chat Completions translation which have no server-side state of their own.
//
// Payload holds the full response object as returned to the client and
// InputItems the request input; both are stored as JSON text rather than
// normalised columns, because the gateway only ever replays them verbatim.
type StoredResponse struct {
	ID                 string `json:"id" gorm:"primaryKey;column:id"`
	OwnerID            string `json:"-" gorm:"index;column:owner_id;not null"`
	Model              string `json:"-" gorm:"index;column:model;not null"`
	DeploymentID       string `json:"-" gorm:"column:deployment_id"`
	UpstreamID         string `json:"-" gorm:"column:upstream_id"`
	Status             string `json:"-" gorm:"index;column:status;not null"`
	PreviousResponseID string `json:"-" gorm:"index;column:previous_response_id"`
	ConversationID     string `json:"-" gorm:"index;column:conversation_id"`
	Payload            string `json:"-" gorm:"column:payload;type:text"`
	InputItems         string `json:"-" gorm:"column:input_items;type:text"`
	// Events holds the SSE events of a streamed background turn as a JSON
	// array, so a client that disconnects can resume from a sequence number
	// instead of losing the turn. Empty for non-streamed turns.
	Events string `json:"-" gorm:"column:events;type:text"`
	// CancelRequested is set by a cancel that reached a replica other than the
	// one running the turn; the worker checks it as it streams.
	CancelRequested bool       `json:"-" gorm:"column:cancel_requested"`
	CreatedAt       time.Time  `json:"-" gorm:"index;column:created_at"`
	ExpiresAt       *time.Time `json:"-" gorm:"index;column:expires_at"`
}

func (StoredResponse) TableName() string { return "gw_responses" }

// ResponseStore is the optional persistence capability used by the stateful
// Responses endpoints. Like FileStore it is kept out of Store so lightweight
// test stores stay valid.
type ResponseStore interface {
	CreateResponse(ctx context.Context, response *StoredResponse) error
	GetResponse(ctx context.Context, id, ownerID string) (*StoredResponse, error)
	// UpdateResponse overwrites a stored response, reporting whether the write
	// landed — a terminal row is never overwritten.
	UpdateResponse(ctx context.Context, response *StoredResponse) (bool, error)
	// UpdateResponseStatus moves a non-terminal turn to a new status, writing
	// only the status and payload columns. Reports whether the write landed:
	// a turn that reached a terminal state first keeps it.
	UpdateResponseStatus(ctx context.Context, id, ownerID, status, payload string) (bool, error)
	DeleteResponse(ctx context.Context, id, ownerID string) error
	// PurgeExpiredResponses drops every row past its expiry and reports how
	// many went, so the caller can log a meaningful line.
	PurgeExpiredResponses(ctx context.Context) (int64, error)
	// RequestResponseCancel marks a turn for cancellation across replicas.
	RequestResponseCancel(ctx context.Context, id, ownerID string) error
	// ResponseCancelRequested reports whether such a mark is pending.
	ResponseCancelRequested(ctx context.Context, id string) (bool, error)
	// FailStaleResponses closes out background turns abandoned by a dead worker.
	FailStaleResponses(ctx context.Context, cutoff time.Time) (int64, error)
}

// StoredConversation is a gateway-owned conversation: a durable, ordered list
// of Responses items that turns append to and read back from. The gateway mints
// and owns these, so they work identically on providers that have their own
// conversation store and on those that have none.
type StoredConversation struct {
	ID        string     `json:"id" gorm:"primaryKey;column:id"`
	OwnerID   string     `json:"-" gorm:"index;column:owner_id;not null"`
	Metadata  string     `json:"-" gorm:"column:metadata;type:text"`
	CreatedAt time.Time  `json:"-" gorm:"index;column:created_at"`
	ExpiresAt *time.Time `json:"-" gorm:"index;column:expires_at"`
}

func (StoredConversation) TableName() string { return "gw_conversations" }

// StoredConversationItem is one item of a conversation. Position, not
// CreatedAt, defines the order: two items appended in the same turn can share a
// timestamp, and the order they were sent in is the one that must be replayed.
type StoredConversationItem struct {
	ID             string    `json:"id" gorm:"primaryKey;column:id"`
	ConversationID string    `json:"-" gorm:"index;column:conversation_id;not null"`
	OwnerID        string    `json:"-" gorm:"index;column:owner_id;not null"`
	Position       int64     `json:"-" gorm:"index;column:position"`
	Payload        string    `json:"-" gorm:"column:payload;type:text"`
	CreatedAt      time.Time `json:"-" gorm:"column:created_at"`
}

func (StoredConversationItem) TableName() string { return "gw_conversation_items" }

// ConversationStore is the optional persistence capability behind
// /v1/conversations, kept separate from Store for the same reason as
// ResponseStore and FileStore.
type ConversationStore interface {
	CreateConversation(ctx context.Context, conversation *StoredConversation) error
	GetConversation(ctx context.Context, id, ownerID string) (*StoredConversation, error)
	UpdateConversationMetadata(ctx context.Context, id, ownerID, metadata string) error
	DeleteConversation(ctx context.Context, id, ownerID string) error
	// AppendConversationItems adds items to the end of a conversation and
	// returns them with the ids and positions they were given.
	AppendConversationItems(ctx context.Context, conversationID, ownerID string, payloads []string) ([]StoredConversationItem, error)
	ListConversationItems(ctx context.Context, conversationID, ownerID string) ([]StoredConversationItem, error)
	GetConversationItem(ctx context.Context, conversationID, ownerID, itemID string) (*StoredConversationItem, error)
	DeleteConversationItem(ctx context.Context, conversationID, ownerID, itemID string) error
	// PurgeExpiredConversations drops conversations past their retention along
	// with the items belonging to them.
	PurgeExpiredConversations(ctx context.Context) (int64, error)
}

// FileStore is the optional persistence capability used by the Files API.
// It is intentionally separate from Store so existing lightweight test stores
// remain valid.
type FileStore interface {
	CreateProviderFile(ctx context.Context, file *ProviderFile) error
	GetProviderFile(ctx context.Context, id, ownerID string) (*ProviderFile, error)
	ListProviderFiles(ctx context.Context, ownerID string, filter ProviderFileFilter) ([]ProviderFile, error)
	DeleteProviderFile(ctx context.Context, id, ownerID string) error
}

// Table name overrides.
func (Team) TableName() string        { return "tenants" }
func (User) TableName() string        { return "gw_users" }
func (CacheMetric) TableName() string { return "gw_cache_metrics" }

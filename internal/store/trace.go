package store

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TraceEvent is one captured model call, written by the hivetrace ingestion
// worker. Tools, Files and Secrets are stored as JSON text rather than
// normalised child tables for the same reason StoredResponse.Payload is: the
// gateway only ever reads them back whole, and a per-request fan-out of three
// extra inserts would cost more than the capture itself.
//
// RequestBody and ResponseBody are empty unless hivetrace.capture_bodies is on,
// and carry redacted text unless the deployment explicitly opted out.
type TraceEvent struct {
	ID        string `json:"id" gorm:"primaryKey;column:id"`
	RequestID string `json:"request_id" gorm:"index;column:request_id"`
	SessionID string `json:"session_id" gorm:"index:idx_trace_session,priority:1;column:session_id;not null"`

	TeamID    string `json:"team_id" gorm:"index:idx_trace_team,priority:1;column:team_id"`
	KeyHash   string `json:"-" gorm:"index:idx_trace_key,priority:1;column:key_hash"`
	KeyPrefix string `json:"key_prefix" gorm:"column:key_prefix"`
	UserID    string `json:"user_id" gorm:"index;column:user_id"`
	AgentID   string `json:"agent_id" gorm:"index;column:agent_id"`

	API           string `json:"api" gorm:"column:api"`
	Path          string `json:"path" gorm:"column:path"`
	Model         string `json:"model" gorm:"index;column:model"`
	Provider      string `json:"provider" gorm:"column:provider"`
	ClientProduct string `json:"client_product" gorm:"column:client_product"`
	ClientVersion string `json:"client_version" gorm:"column:client_version"`
	Agent         string `json:"agent" gorm:"index;column:agent"`
	AgentVia      string `json:"agent_via" gorm:"column:agent_via"`
	ClientIP      string `json:"client_ip" gorm:"column:client_ip"`
	ClientHost    string `json:"client_host" gorm:"column:client_host"`
	BillingMode   string `json:"billing_mode" gorm:"column:billing_mode"`

	Status    int  `json:"status" gorm:"column:status"`
	Streaming bool `json:"streaming" gorm:"column:streaming"`

	StartedAt  time.Time `json:"started_at" gorm:"column:started_at"`
	DurationMs int64     `json:"duration_ms" gorm:"column:duration_ms"`
	TTFBMs     int64     `json:"ttfb_ms" gorm:"column:ttfb_ms"`

	PromptTokens       int     `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens" gorm:"column:completion_tokens"`
	TotalTokens        int     `json:"total_tokens" gorm:"column:total_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens" gorm:"column:cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens" gorm:"column:reasoning_tokens"`
	Cost               float64 `json:"cost" gorm:"column:cost"`

	RequestBody  string     `json:"request_body,omitempty" gorm:"column:request_body;type:text"`
	ResponseBody string     `json:"response_body,omitempty" gorm:"column:response_body;type:text"`
	Truncated    bool       `json:"truncated" gorm:"column:truncated"`
	Redactions   StringList `json:"redactions,omitempty" gorm:"column:redactions;type:text"`

	Tools    string `json:"tools,omitempty" gorm:"column:tools;type:text"`
	Files    string `json:"files,omitempty" gorm:"column:files;type:text"`
	Findings string `json:"findings,omitempty" gorm:"column:findings;type:text"`
	// FindingCount is denormalised so "show me every turn where a credential or
	// personal datum went through" is an indexed lookup instead of a scan over
	// JSON text.
	FindingCount int `json:"finding_count" gorm:"index;column:finding_count"`

	ErrorMessage  string    `json:"error_message,omitempty" gorm:"column:error_message"`
	TaskHint      string    `json:"task_hint,omitempty" gorm:"column:task_hint"`
	TaskHintScore float64   `json:"task_hint_score,omitempty" gorm:"column:task_hint_score"`
	CreatedAt     time.Time `json:"created_at" gorm:"index;index:idx_trace_session,priority:2;index:idx_trace_team,priority:2;index:idx_trace_key,priority:2;column:created_at"`
}

func (TraceEvent) TableName() string { return "gw_trace_events" }

// TraceSession is the aggregate the hivetrace analyzer recomputes from a
// session's events. It is derived state: dropping the table costs nothing but
// the time to rebuild it from gw_trace_events.
type TraceSession struct {
	SessionID string `json:"session_id" gorm:"primaryKey;column:session_id"`

	TeamID    string `json:"team_id" gorm:"index;column:team_id"`
	KeyHash   string `json:"-" gorm:"index;column:key_hash"`
	KeyPrefix string `json:"key_prefix" gorm:"column:key_prefix"`
	UserID    string `json:"user_id" gorm:"index;column:user_id"`
	AgentID   string `json:"agent_id" gorm:"index;column:agent_id"`

	ClientProduct string `json:"client_product" gorm:"column:client_product"`
	Agent         string `json:"agent" gorm:"index;column:agent"`
	AgentVia      string `json:"agent_via" gorm:"column:agent_via"`
	ClientIP      string `json:"client_ip" gorm:"column:client_ip"`
	ClientHost    string `json:"client_host" gorm:"column:client_host"`
	BillingMode   string `json:"billing_mode" gorm:"column:billing_mode"`

	StartedAt time.Time `json:"started_at" gorm:"index;column:started_at"`
	EndedAt   time.Time `json:"ended_at" gorm:"index;column:ended_at"`

	Requests int `json:"requests" gorm:"column:requests"`
	Errors   int `json:"errors" gorm:"column:errors"`

	Models    StringList `json:"models,omitempty" gorm:"column:models;type:text"`
	Providers StringList `json:"providers,omitempty" gorm:"column:providers;type:text"`

	PromptTokens       int     `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens" gorm:"column:completion_tokens"`
	TotalTokens        int     `json:"total_tokens" gorm:"column:total_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens" gorm:"column:cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens" gorm:"column:reasoning_tokens"`
	Cost               float64 `json:"cost" gorm:"column:cost"`

	LatencyP50Ms int64 `json:"latency_p50_ms" gorm:"column:latency_p50_ms"`
	LatencyP95Ms int64 `json:"latency_p95_ms" gorm:"column:latency_p95_ms"`

	ToolCalls  int    `json:"tool_calls" gorm:"column:tool_calls"`
	Tools      string `json:"tools,omitempty" gorm:"column:tools;type:text"`
	MCPServers string `json:"mcp_servers,omitempty" gorm:"column:mcp_servers;type:text"`

	FilesRead    StringList `json:"files_read,omitempty" gorm:"column:files_read;type:text"`
	FilesWritten StringList `json:"files_written,omitempty" gorm:"column:files_written;type:text"`

	Findings     string `json:"findings,omitempty" gorm:"column:findings;type:text"`
	FindingCount int    `json:"finding_count" gorm:"index;column:finding_count"`

	TaskType   string `json:"task_type" gorm:"index;column:task_type"`
	TaskVia    string `json:"task_via" gorm:"column:task_via"`
	Difficulty int    `json:"difficulty" gorm:"column:difficulty"`

	PrimaryModel string `json:"primary_model" gorm:"column:primary_model"`
	ModelTier    string `json:"model_tier" gorm:"column:model_tier"`
	ModelFit     string `json:"model_fit" gorm:"index;column:model_fit"`

	UpdatedAt time.Time `json:"updated_at" gorm:"index;column:updated_at"`
}

func (TraceSession) TableName() string { return "gw_trace_sessions" }

// TraceFilter narrows a trace query. Zero values mean "no constraint".
type TraceFilter struct {
	SessionID   string
	TeamID      string
	KeyHash     string
	UserID      string
	AgentID     string
	Model       string
	Since       time.Time
	Until       time.Time
	HasFindings bool
	Limit       int
	Offset      int
}

// TraceStore is the optional persistence capability behind AI traffic
// observability. Like ResponseStore and FileStore it is kept out of Store so
// lightweight test stores stay valid.
type TraceStore interface {
	InsertTraceEvents(ctx context.Context, events []TraceEvent) error
	ListTraceEvents(ctx context.Context, filter TraceFilter) ([]TraceEvent, error)
	// ListTraceSessionIDsSince returns the sessions touched since a cutoff, so
	// the analyzer recomputes only what changed.
	ListTraceSessionIDsSince(ctx context.Context, since time.Time, limit int) ([]string, error)
	UpsertTraceSession(ctx context.Context, session *TraceSession) error
	GetTraceSession(ctx context.Context, sessionID string) (*TraceSession, error)
	ListTraceSessions(ctx context.Context, filter TraceFilter) ([]TraceSession, error)
	// PurgeTraceData drops events older than the cutoff and the sessions left
	// with none, reporting how many events went.
	PurgeTraceData(ctx context.Context, before time.Time) (int64, error)
}

func (s *GormStore) InsertTraceEvents(ctx context.Context, events []TraceEvent) error {
	if len(events) == 0 {
		return nil
	}
	// A replayed or duplicated capture must not abort the whole batch: the
	// worker retries nothing, so a conflict here would silently lose the other
	// records that shared the flush.
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		CreateInBatches(events, 200).Error
}

func (s *GormStore) ListTraceEvents(ctx context.Context, filter TraceFilter) ([]TraceEvent, error) {
	q := applyTraceFilter(s.db.WithContext(ctx).Model(&TraceEvent{}), filter, "created_at", "model")
	var out []TraceEvent
	if err := q.Order("created_at ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *GormStore) ListTraceSessionIDsSince(ctx context.Context, since time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	var ids []string
	// UTC, not the caller's zone: timestamps are written UTC, and SQLite
	// compares them as text — a local-time bound silently matches nothing.
	err := s.db.WithContext(ctx).Model(&TraceEvent{}).
		Where("created_at >= ?", since.UTC()).
		Distinct().
		Pluck("session_id", &ids).Error
	if err != nil {
		return nil, err
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (s *GormStore) UpsertTraceSession(ctx context.Context, session *TraceSession) error {
	if session == nil || session.SessionID == "" {
		return nil
	}
	session.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "session_id"}}, UpdateAll: true}).
		Create(session).Error
}

func (s *GormStore) GetTraceSession(ctx context.Context, sessionID string) (*TraceSession, error) {
	var out TraceSession
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).First(&out).Error; err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *GormStore) ListTraceSessions(ctx context.Context, filter TraceFilter) ([]TraceSession, error) {
	// No model column here: a session spans models, and gw_trace_sessions keeps
	// them as a list. Filtering by model is an event-level question.
	q := applyTraceFilter(s.db.WithContext(ctx).Model(&TraceSession{}), filter, "started_at", "")
	var out []TraceSession
	if err := q.Order("started_at DESC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *GormStore) PurgeTraceData(ctx context.Context, before time.Time) (int64, error) {
	cutoff := before.UTC()
	res := s.db.WithContext(ctx).Where("created_at < ?", cutoff).Delete(&TraceEvent{})
	if res.Error != nil {
		return 0, res.Error
	}
	// A summary whose events are all gone describes nothing retrievable, so it
	// goes with them rather than accumulating forever.
	if err := s.db.WithContext(ctx).
		Where("ended_at < ?", cutoff).
		Delete(&TraceSession{}).Error; err != nil {
		return res.RowsAffected, err
	}
	return res.RowsAffected, nil
}

func applyTraceFilter(q *gorm.DB, filter TraceFilter, timeColumn, modelColumn string) *gorm.DB {
	if filter.SessionID != "" {
		q = q.Where("session_id = ?", filter.SessionID)
	}
	if filter.TeamID != "" {
		q = q.Where("team_id = ?", filter.TeamID)
	}
	if filter.KeyHash != "" {
		q = q.Where("key_hash = ?", filter.KeyHash)
	}
	if filter.UserID != "" {
		q = q.Where("user_id = ?", filter.UserID)
	}
	if filter.AgentID != "" {
		q = q.Where("agent_id = ?", filter.AgentID)
	}
	if filter.Model != "" && modelColumn != "" {
		q = q.Where(modelColumn+" = ?", filter.Model)
	}
	if !filter.Since.IsZero() {
		q = q.Where(timeColumn+" >= ?", filter.Since.UTC())
	}
	if !filter.Until.IsZero() {
		q = q.Where(timeColumn+" <= ?", filter.Until.UTC())
	}
	if filter.HasFindings {
		q = q.Where("finding_count > 0")
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	return q
}

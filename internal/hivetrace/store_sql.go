package hivetrace

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ErrStoreUnsupported reports that the configured database cannot back trace
// storage. It is returned at startup rather than per request.
var ErrStoreUnsupported = errors.New("hivetrace: database does not implement store.TraceStore")

// sqlStore keeps traces in the gateway's own database.
//
// It deliberately reuses the existing *gorm.DB rather than opening a second
// connection: on SQLite the pool is pinned to one connection because a second
// writer cannot upgrade its lock, and a private connection here would
// reintroduce exactly that deadlock.
type sqlStore struct {
	db store.TraceStore
}

// NewSQLStore adapts an existing gateway store to the TrafficStore interface.
func NewSQLStore(db store.Store) (TrafficStore, error) {
	ts, ok := db.(store.TraceStore)
	if !ok {
		return nil, ErrStoreUnsupported
	}
	return &sqlStore{db: ts}, nil
}

func (s *sqlStore) InsertEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	rows := make([]store.TraceEvent, 0, len(events))
	for i := range events {
		row, err := toRow(&events[i])
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	return s.db.InsertTraceEvents(ctx, rows)
}

func (s *sqlStore) ListEvents(ctx context.Context, filter Filter) ([]Event, error) {
	rows, err := s.db.ListTraceEvents(ctx, toStoreFilter(filter))
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for i := range rows {
		out = append(out, fromRow(&rows[i]))
	}
	return out, nil
}

func (s *sqlStore) SessionIDsSince(ctx context.Context, since time.Time, limit int) ([]string, error) {
	return s.db.ListTraceSessionIDsSince(ctx, since, limit)
}

func (s *sqlStore) UpsertSession(ctx context.Context, summary *SessionSummary) error {
	if summary == nil {
		return nil
	}
	row, err := toSessionRow(summary)
	if err != nil {
		return err
	}
	return s.db.UpsertTraceSession(ctx, &row)
}

func (s *sqlStore) GetSession(ctx context.Context, sessionID string) (*SessionSummary, error) {
	row, err := s.db.GetTraceSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := fromSessionRow(row)
	return &out, nil
}

func (s *sqlStore) ListSessions(ctx context.Context, filter Filter) ([]SessionSummary, error) {
	rows, err := s.db.ListTraceSessions(ctx, toStoreFilter(filter))
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(rows))
	for i := range rows {
		out = append(out, fromSessionRow(&rows[i]))
	}
	return out, nil
}

func (s *sqlStore) Purge(ctx context.Context, before time.Time) (int64, error) {
	return s.db.PurgeTraceData(ctx, before)
}

// Close is a no-op: the connection belongs to the gateway store, which closes
// it on shutdown.
func (s *sqlStore) Close() error { return nil }

func toStoreFilter(f Filter) store.TraceFilter {
	return store.TraceFilter{
		SessionID:   f.SessionID,
		TeamID:      f.TeamID,
		KeyHash:     f.KeyHash,
		UserID:      f.UserID,
		AgentID:     f.AgentID,
		Model:       f.Model,
		Since:       f.Since,
		Until:       f.Until,
		HasFindings: f.HasFindings,
		Limit:       f.Limit,
		Offset:      f.Offset,
	}
}

func toRow(e *Event) (store.TraceEvent, error) {
	tools, err := marshalList(e.Tools)
	if err != nil {
		return store.TraceEvent{}, err
	}
	files, err := marshalList(e.Files)
	if err != nil {
		return store.TraceEvent{}, err
	}
	findings, err := marshalList(e.Findings)
	if err != nil {
		return store.TraceEvent{}, err
	}
	findingCount := 0
	for _, f := range e.Findings {
		findingCount += f.Occurrences
	}
	return store.TraceEvent{
		ID:                 e.ID,
		RequestID:          e.RequestID,
		SessionID:          e.SessionID,
		TeamID:             e.TeamID,
		KeyHash:            e.KeyHash,
		KeyPrefix:          e.KeyPrefix,
		UserID:             e.UserID,
		AgentID:            e.AgentID,
		API:                e.API,
		Path:               e.Path,
		Model:              e.Model,
		Provider:           e.Provider,
		ClientProduct:      e.ClientProduct,
		ClientVersion:      e.ClientVersion,
		Agent:              e.Agent,
		AgentVia:           e.AgentVia,
		ClientIP:           e.ClientIP,
		ClientHost:         e.ClientHost,
		BillingMode:        e.BillingMode,
		Status:             e.Status,
		Streaming:          e.Streaming,
		StartedAt:          e.StartedAt,
		DurationMs:         e.DurationMs,
		TTFBMs:             e.TTFBMs,
		PromptTokens:       e.PromptTokens,
		CompletionTokens:   e.CompletionTokens,
		TotalTokens:        e.TotalTokens,
		CachedPromptTokens: e.CachedPromptTokens,
		ReasoningTokens:    e.ReasoningTokens,
		Cost:               e.Cost,
		RequestBody:        e.RequestBody,
		ResponseBody:       e.ResponseBody,
		Truncated:          e.Truncated,
		Redactions:         store.StringList(e.Redactions),
		Tools:              tools,
		Files:              files,
		Findings:           findings,
		FindingCount:       findingCount,
		ErrorMessage:       e.ErrorMessage,
		TaskHint:           e.TaskHint,
		TaskHintScore:      e.TaskHintScore,
		CreatedAt:          e.CreatedAt,
	}, nil
}

func fromRow(r *store.TraceEvent) Event {
	e := Event{
		ID:                 r.ID,
		RequestID:          r.RequestID,
		SessionID:          r.SessionID,
		TeamID:             r.TeamID,
		KeyHash:            r.KeyHash,
		KeyPrefix:          r.KeyPrefix,
		UserID:             r.UserID,
		AgentID:            r.AgentID,
		API:                r.API,
		Path:               r.Path,
		Model:              r.Model,
		Provider:           r.Provider,
		ClientProduct:      r.ClientProduct,
		ClientVersion:      r.ClientVersion,
		Agent:              r.Agent,
		AgentVia:           r.AgentVia,
		ClientIP:           r.ClientIP,
		ClientHost:         r.ClientHost,
		BillingMode:        r.BillingMode,
		Status:             r.Status,
		Streaming:          r.Streaming,
		StartedAt:          r.StartedAt,
		DurationMs:         r.DurationMs,
		TTFBMs:             r.TTFBMs,
		PromptTokens:       r.PromptTokens,
		CompletionTokens:   r.CompletionTokens,
		TotalTokens:        r.TotalTokens,
		CachedPromptTokens: r.CachedPromptTokens,
		ReasoningTokens:    r.ReasoningTokens,
		Cost:               r.Cost,
		RequestBody:        r.RequestBody,
		ResponseBody:       r.ResponseBody,
		Truncated:          r.Truncated,
		Redactions:         []string(r.Redactions),
		ErrorMessage:       r.ErrorMessage,
		TaskHint:           r.TaskHint,
		TaskHintScore:      r.TaskHintScore,
		CreatedAt:          r.CreatedAt,
	}
	// A row whose JSON no longer parses is still worth showing for its
	// metadata; the extracted detail is the part that is lost.
	_ = json.Unmarshal([]byte(emptyToNull(r.Tools)), &e.Tools)
	_ = json.Unmarshal([]byte(emptyToNull(r.Files)), &e.Files)
	_ = json.Unmarshal([]byte(emptyToNull(r.Findings)), &e.Findings)
	return e
}

func toSessionRow(s *SessionSummary) (store.TraceSession, error) {
	tools, err := marshalList(s.Tools)
	if err != nil {
		return store.TraceSession{}, err
	}
	servers, err := marshalList(s.MCPServers)
	if err != nil {
		return store.TraceSession{}, err
	}
	findings, err := marshalList(s.Findings)
	if err != nil {
		return store.TraceSession{}, err
	}
	findingCount := 0
	for _, x := range s.Findings {
		findingCount += x.Occurrences
	}
	return store.TraceSession{
		SessionID:          s.SessionID,
		TeamID:             s.TeamID,
		KeyHash:            s.KeyHash,
		KeyPrefix:          s.KeyPrefix,
		UserID:             s.UserID,
		AgentID:            s.AgentID,
		ClientProduct:      s.ClientProduct,
		Agent:              s.Agent,
		AgentVia:           s.AgentVia,
		ClientIP:           s.ClientIP,
		ClientHost:         s.ClientHost,
		BillingMode:        s.BillingMode,
		StartedAt:          s.StartedAt,
		EndedAt:            s.EndedAt,
		Requests:           s.Requests,
		Errors:             s.Errors,
		Models:             store.StringList(s.Models),
		Providers:          store.StringList(s.Providers),
		PromptTokens:       s.PromptTokens,
		CompletionTokens:   s.CompletionTokens,
		TotalTokens:        s.TotalTokens,
		CachedPromptTokens: s.CachedPromptTokens,
		ReasoningTokens:    s.ReasoningTokens,
		Cost:               s.Cost,
		LatencyP50Ms:       s.LatencyP50Ms,
		LatencyP95Ms:       s.LatencyP95Ms,
		ToolCalls:          s.ToolCalls,
		Tools:              tools,
		MCPServers:         servers,
		FilesRead:          store.StringList(s.FilesRead),
		FilesWritten:       store.StringList(s.FilesWritten),
		Findings:           findings,
		FindingCount:       findingCount,
		TaskType:           s.TaskType,
		TaskVia:            s.TaskVia,
		Difficulty:         s.Difficulty,
		PrimaryModel:       s.PrimaryModel,
		ModelTier:          s.ModelTier,
		ModelFit:           s.ModelFit,
	}, nil
}

func fromSessionRow(r *store.TraceSession) SessionSummary {
	s := SessionSummary{
		SessionID:          r.SessionID,
		TeamID:             r.TeamID,
		KeyHash:            r.KeyHash,
		KeyPrefix:          r.KeyPrefix,
		UserID:             r.UserID,
		AgentID:            r.AgentID,
		ClientProduct:      r.ClientProduct,
		Agent:              r.Agent,
		AgentVia:           r.AgentVia,
		ClientIP:           r.ClientIP,
		ClientHost:         r.ClientHost,
		BillingMode:        r.BillingMode,
		StartedAt:          r.StartedAt,
		EndedAt:            r.EndedAt,
		Requests:           r.Requests,
		Errors:             r.Errors,
		Models:             []string(r.Models),
		Providers:          []string(r.Providers),
		PromptTokens:       r.PromptTokens,
		CompletionTokens:   r.CompletionTokens,
		TotalTokens:        r.TotalTokens,
		CachedPromptTokens: r.CachedPromptTokens,
		ReasoningTokens:    r.ReasoningTokens,
		Cost:               r.Cost,
		LatencyP50Ms:       r.LatencyP50Ms,
		LatencyP95Ms:       r.LatencyP95Ms,
		ToolCalls:          r.ToolCalls,
		FilesRead:          []string(r.FilesRead),
		FilesWritten:       []string(r.FilesWritten),
		TaskType:           r.TaskType,
		TaskVia:            r.TaskVia,
		Difficulty:         r.Difficulty,
		PrimaryModel:       r.PrimaryModel,
		ModelTier:          r.ModelTier,
		ModelFit:           r.ModelFit,
		UpdatedAt:          r.UpdatedAt,
	}
	_ = json.Unmarshal([]byte(emptyToNull(r.Tools)), &s.Tools)
	_ = json.Unmarshal([]byte(emptyToNull(r.MCPServers)), &s.MCPServers)
	_ = json.Unmarshal([]byte(emptyToNull(r.Findings)), &s.Findings)
	return s
}

// marshalList returns "" for an empty slice so the column stays NULL-ish
// instead of holding "null" or "[]" on every row that extracted nothing.
func marshalList[T any](items []T) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func emptyToNull(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

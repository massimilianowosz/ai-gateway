package hivetrace

import (
	"encoding/json"
	"time"
)

// clickHouseTimeLayout is what ClickHouse emits and accepts for DateTime64(3)
// in JSON: a space-separated, timezone-less UTC timestamp, not RFC 3339.
const clickHouseTimeLayout = "2006-01-02 15:04:05.000"

// chTime adapts time.Time to that layout on both sides of the wire.
type chTime time.Time

func (t chTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(clickHouseTimeLayout))
}

func (t *chTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*t = chTime(time.Time{})
		return nil
	}
	for _, layout := range []string{clickHouseTimeLayout, "2006-01-02 15:04:05", time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, s); err == nil {
			*t = chTime(parsed)
			return nil
		}
	}
	*t = chTime(time.Time{})
	return nil
}

func (t chTime) time() time.Time { return time.Time(t) }

// chBool maps Go's bool to ClickHouse's UInt8.
type chBool bool

func (b chBool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte("1"), nil
	}
	return []byte("0"), nil
}

func (b *chBool) UnmarshalJSON(raw []byte) error {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		*b = n != 0
		return nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*b = chBool(v)
	return nil
}

type clickHouseEvent struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	TeamID    string `json:"team_id"`
	KeyHash   string `json:"key_hash"`
	KeyPrefix string `json:"key_prefix"`
	UserID    string `json:"user_id"`
	AgentID   string `json:"agent_id"`

	API           string `json:"api"`
	Path          string `json:"path"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	ClientProduct string `json:"client_product"`
	ClientVersion string `json:"client_version"`
	Agent         string `json:"agent"`
	AgentVia      string `json:"agent_via"`
	ClientIP      string `json:"client_ip"`
	ClientHost    string `json:"client_host"`
	BillingMode   string `json:"billing_mode"`

	Status    int    `json:"status"`
	Streaming chBool `json:"streaming"`

	StartedAt  chTime `json:"started_at"`
	DurationMs int64  `json:"duration_ms"`
	TTFBMs     int64  `json:"ttfb_ms"`

	PromptTokens       int     `json:"prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	Cost               float64 `json:"cost"`

	RequestBody  string   `json:"request_body"`
	ResponseBody string   `json:"response_body"`
	Truncated    chBool   `json:"truncated"`
	Redactions   []string `json:"redactions"`

	Tools        string `json:"tools"`
	Files        string `json:"files"`
	Findings     string `json:"findings"`
	FindingCount int    `json:"finding_count"`

	ErrorMessage  string  `json:"error_message"`
	TaskHint      string  `json:"task_hint"`
	TaskHintScore float64 `json:"task_hint_score"`
	CreatedAt     chTime  `json:"created_at"`
}

func toClickHouseEvent(e *Event) (clickHouseEvent, error) {
	tools, err := marshalList(e.Tools)
	if err != nil {
		return clickHouseEvent{}, err
	}
	files, err := marshalList(e.Files)
	if err != nil {
		return clickHouseEvent{}, err
	}
	findings, err := marshalList(e.Findings)
	if err != nil {
		return clickHouseEvent{}, err
	}
	findingCount := 0
	for _, f := range e.Findings {
		findingCount += f.Occurrences
	}
	redactions := e.Redactions
	if redactions == nil {
		// ClickHouse rejects null for Array(String); an absent list is empty.
		redactions = []string{}
	}
	return clickHouseEvent{
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
		Streaming:          chBool(e.Streaming),
		StartedAt:          chTime(e.StartedAt),
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
		Truncated:          chBool(e.Truncated),
		Redactions:         redactions,
		Tools:              tools,
		Files:              files,
		Findings:           findings,
		FindingCount:       findingCount,
		ErrorMessage:       e.ErrorMessage,
		TaskHint:           e.TaskHint,
		TaskHintScore:      e.TaskHintScore,
		CreatedAt:          chTime(e.CreatedAt),
	}, nil
}

func (r clickHouseEvent) toEvent() Event {
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
		Streaming:          bool(r.Streaming),
		StartedAt:          r.StartedAt.time(),
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
		Truncated:          bool(r.Truncated),
		Redactions:         r.Redactions,
		ErrorMessage:       r.ErrorMessage,
		TaskHint:           r.TaskHint,
		TaskHintScore:      r.TaskHintScore,
		CreatedAt:          r.CreatedAt.time(),
	}
	_ = json.Unmarshal([]byte(emptyToNull(r.Tools)), &e.Tools)
	_ = json.Unmarshal([]byte(emptyToNull(r.Files)), &e.Files)
	_ = json.Unmarshal([]byte(emptyToNull(r.Findings)), &e.Findings)
	return e
}

type clickHouseSession struct {
	SessionID     string `json:"session_id"`
	TeamID        string `json:"team_id"`
	KeyHash       string `json:"key_hash"`
	KeyPrefix     string `json:"key_prefix"`
	UserID        string `json:"user_id"`
	AgentID       string `json:"agent_id"`
	ClientProduct string `json:"client_product"`
	Agent         string `json:"agent"`
	AgentVia      string `json:"agent_via"`
	ClientIP      string `json:"client_ip"`
	ClientHost    string `json:"client_host"`
	BillingMode   string `json:"billing_mode"`

	StartedAt chTime `json:"started_at"`
	EndedAt   chTime `json:"ended_at"`

	Requests int `json:"requests"`
	Errors   int `json:"errors"`

	Models    []string `json:"models"`
	Providers []string `json:"providers"`

	PromptTokens       int     `json:"prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	Cost               float64 `json:"cost"`

	LatencyP50Ms int64 `json:"latency_p50_ms"`
	LatencyP95Ms int64 `json:"latency_p95_ms"`

	ToolCalls  int    `json:"tool_calls"`
	Tools      string `json:"tools"`
	MCPServers string `json:"mcp_servers"`

	FilesRead    []string `json:"files_read"`
	FilesWritten []string `json:"files_written"`

	Findings     string `json:"findings"`
	FindingCount int    `json:"finding_count"`

	TaskType   string `json:"task_type"`
	TaskVia    string `json:"task_via"`
	Difficulty int    `json:"difficulty"`

	PrimaryModel string `json:"primary_model"`
	ModelTier    string `json:"model_tier"`
	ModelFit     string `json:"model_fit"`

	UpdatedAt chTime `json:"updated_at"`
}

func toClickHouseSession(s *SessionSummary) (clickHouseSession, error) {
	tools, err := marshalList(s.Tools)
	if err != nil {
		return clickHouseSession{}, err
	}
	servers, err := marshalList(s.MCPServers)
	if err != nil {
		return clickHouseSession{}, err
	}
	findings, err := marshalList(s.Findings)
	if err != nil {
		return clickHouseSession{}, err
	}
	findingCount := 0
	for _, f := range s.Findings {
		findingCount += f.Occurrences
	}
	return clickHouseSession{
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
		StartedAt:          chTime(s.StartedAt),
		EndedAt:            chTime(s.EndedAt),
		Requests:           s.Requests,
		Errors:             s.Errors,
		Models:             emptySlice(s.Models),
		Providers:          emptySlice(s.Providers),
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
		FilesRead:          emptySlice(s.FilesRead),
		FilesWritten:       emptySlice(s.FilesWritten),
		Findings:           findings,
		FindingCount:       findingCount,
		TaskType:           s.TaskType,
		TaskVia:            s.TaskVia,
		Difficulty:         s.Difficulty,
		PrimaryModel:       s.PrimaryModel,
		ModelTier:          s.ModelTier,
		ModelFit:           s.ModelFit,
		UpdatedAt:          chTime(s.UpdatedAt),
	}, nil
}

func (r clickHouseSession) toSummary() SessionSummary {
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
		StartedAt:          r.StartedAt.time(),
		EndedAt:            r.EndedAt.time(),
		Requests:           r.Requests,
		Errors:             r.Errors,
		Models:             r.Models,
		Providers:          r.Providers,
		PromptTokens:       r.PromptTokens,
		CompletionTokens:   r.CompletionTokens,
		TotalTokens:        r.TotalTokens,
		CachedPromptTokens: r.CachedPromptTokens,
		ReasoningTokens:    r.ReasoningTokens,
		Cost:               r.Cost,
		LatencyP50Ms:       r.LatencyP50Ms,
		LatencyP95Ms:       r.LatencyP95Ms,
		ToolCalls:          r.ToolCalls,
		FilesRead:          r.FilesRead,
		FilesWritten:       r.FilesWritten,
		TaskType:           r.TaskType,
		TaskVia:            r.TaskVia,
		Difficulty:         r.Difficulty,
		PrimaryModel:       r.PrimaryModel,
		ModelTier:          r.ModelTier,
		ModelFit:           r.ModelFit,
		UpdatedAt:          r.UpdatedAt.time(),
	}
	_ = json.Unmarshal([]byte(emptyToNull(r.Tools)), &s.Tools)
	_ = json.Unmarshal([]byte(emptyToNull(r.MCPServers)), &s.MCPServers)
	_ = json.Unmarshal([]byte(emptyToNull(r.Findings)), &s.Findings)
	return s
}

func emptySlice(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

// Package hivetrace captures AI traffic as it passes through the gateway and
// turns it into a per-session account of what an agent actually did.
//
// The gateway already knows what every request cost and how many tokens it
// burned, but a spend row says nothing about the work: which tools ran, which
// MCP servers were reached, which files were read or written, and whether a
// model handed a credential back to the caller. That information only exists
// in the request and response bodies, which nothing persisted until now.
//
// Capture is strictly passthrough. The response is written and flushed to the
// client exactly as before; bytes are copied aside into a bounded buffer and
// everything expensive — redaction, parsing, extraction, persistence — happens
// on a background worker. A capture failure never fails a request.
package hivetrace

import "time"

// API surfaces a request arrived on.
const (
	APIChatCompletions = "chat_completions"
	APIMessages        = "messages"
	APIResponses       = "responses"
)

// Tool sources.
const (
	ToolSourceNative = "native"
	ToolSourceMCP    = "mcp"
)

// File operations inferred from a tool call.
const (
	FileOpRead   = "read"
	FileOpWrite  = "write"
	FileOpSearch = "search"
)

// Kinds of sensitive-data finding.
const (
	KindSecret = "secret"
	KindPII    = "pii"
	// KindWatchlist is a term the operator declared, so its Type is their
	// label rather than a detector id.
	KindWatchlist = "watchlist"
	// KindFirewall is an MCP server, tool or file a declared rule denies. Like
	// KindWatchlist its Type is the rule's label, not a detector id.
	KindFirewall = "firewall"
)

// Origins a finding can have.
//
// Both matter, and they mean different things. A request finding is data the
// caller sent to the model — a credential pasted into a prompt, a customer
// record in a tool result the agent fed back. A response finding is data the
// model handed out. The first is an exposure the organisation caused, the
// second one it received; an operator needs to tell them apart.
const (
	OriginRequest  = "request"
	OriginResponse = "response"
)

// Event is one captured model call.
type Event struct {
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`

	TeamID    string `json:"team_id,omitempty"`
	KeyHash   string `json:"key_hash,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`

	API           string `json:"api"`
	Path          string `json:"path"`
	Model         string `json:"model,omitempty"`
	Provider      string `json:"provider,omitempty"`
	ClientProduct string `json:"client_product,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	// Agent is the agent product behind the call, or the HTTP library when the
	// caller is somebody's own script. AgentVia records which evidence named
	// it, so a console can distinguish a stated identity from an inferred one.
	Agent    string `json:"agent,omitempty"`
	AgentVia string `json:"agent_via,omitempty"`
	// ClientIP is the address the call arrived from. A connection carries an
	// address and no hostname, so this is the strongest network identity the
	// appliance can state as fact; anything friendlier is a lookup, and a
	// lookup can be wrong.
	ClientIP string `json:"client_ip,omitempty"`
	// ClientHost is the reverse lookup of ClientIP when the network answers
	// one. A PTR record is controlled by whoever owns the reverse zone, so
	// it is a label for reading, never the identity itself.
	ClientHost string `json:"client_host,omitempty"`
	// BillingMode distinguishes a call that cost nothing from one the caller's
	// own subscription paid for, which a zero Cost cannot express on its own.
	BillingMode string `json:"billing_mode,omitempty"`

	Status    int  `json:"status"`
	Streaming bool `json:"streaming"`

	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	// TTFBMs is the time to the first response byte. On a streaming turn this
	// is the number a user perceives as latency; DurationMs is dominated by
	// how long the answer is.
	TTFBMs int64 `json:"ttfb_ms"`

	PromptTokens       int     `json:"prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	Cost               float64 `json:"cost"`

	// RequestBody and ResponseBody are populated only when capture_bodies is
	// on, and carry redacted text unless the deployment opted out.
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
	// Truncated reports that a body exceeded max_body_bytes. The client always
	// received the whole stream; only the retained copy is short.
	Truncated bool `json:"truncated,omitempty"`
	// Redactions lists the entity kinds replaced in the stored bodies.
	Redactions []string `json:"redactions,omitempty"`

	Tools    []ToolInvocation `json:"tools,omitempty"`
	Files    []FileAccess     `json:"files,omitempty"`
	Findings []Finding        `json:"findings,omitempty"`

	ErrorMessage string `json:"error_message,omitempty"`
	// TaskHint is the classifier's label for the session's opening request,
	// set on the turn that asked; TaskHintScore is its probability.
	TaskHint      string    `json:"task_hint,omitempty"`
	TaskHintScore float64   `json:"task_hint_score,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// ToolInvocation is a single tool call the model asked for.
type ToolInvocation struct {
	// Name is the tool as the model named it, verbatim.
	Name string `json:"name"`
	// Tool is the bare tool name with any MCP wrapper removed, so the same
	// capability reached through different bridges aggregates together.
	Tool string `json:"tool"`
	// Server is the MCP server the tool belongs to, empty for native tools.
	Server string `json:"server,omitempty"`
	Source string `json:"source"`
	CallID string `json:"call_id,omitempty"`
	// ArgumentsBytes sizes the call without retaining its arguments, which
	// routinely contain file contents and credentials.
	ArgumentsBytes int `json:"arguments_bytes"`
}

// FileAccess is a path a tool call touched.
type FileAccess struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Tool      string `json:"tool"`
}

// Finding records that a sensitive-data detector matched. It deliberately
// carries no value and no surrounding text: the trace store must not become
// the place an attacker goes to harvest what it detected. This mirrors what
// store.SecurityEvent already does for request-side guardrail hits.
type Finding struct {
	// Kind is secret or pii.
	Kind string `json:"kind"`
	// Type is the detector id — GITHUB_TOKEN, EMAIL_ADDRESS, CODICE_FISCALE …
	Type        string `json:"type"`
	Origin      string `json:"origin"`
	Occurrences int    `json:"occurrences"`
	// Samples holds matched values, and only when hivetrace.finding_samples
	// is on. See that setting for what keeping them means.
	Samples []string `json:"samples,omitempty"`
}

// SessionSummary is the aggregate the analyzer derives from a session's events.
type SessionSummary struct {
	SessionID string `json:"session_id"`
	TeamID    string `json:"team_id,omitempty"`
	KeyHash   string `json:"key_hash,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`

	ClientProduct string `json:"client_product,omitempty"`
	Agent         string `json:"agent,omitempty"`
	AgentVia      string `json:"agent_via,omitempty"`
	BillingMode   string `json:"billing_mode,omitempty"`
	// ClientIP is the address the session's turns arrived from. A session that
	// moved between addresses keeps the most recent one rather than claiming a
	// single origin it did not have.
	ClientIP   string `json:"client_ip,omitempty"`
	ClientHost string `json:"client_host,omitempty"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	Requests int `json:"requests"`
	Errors   int `json:"errors"`

	Models    []string `json:"models,omitempty"`
	Providers []string `json:"providers,omitempty"`

	PromptTokens       int     `json:"prompt_tokens"`
	CompletionTokens   int     `json:"completion_tokens"`
	TotalTokens        int     `json:"total_tokens"`
	CachedPromptTokens int     `json:"cached_prompt_tokens"`
	ReasoningTokens    int     `json:"reasoning_tokens"`
	Cost               float64 `json:"cost"`

	LatencyP50Ms int64 `json:"latency_p50_ms"`
	LatencyP95Ms int64 `json:"latency_p95_ms"`

	ToolCalls  int           `json:"tool_calls"`
	Tools      []ToolUsage   `json:"tools,omitempty"`
	MCPServers []ServerUsage `json:"mcp_servers,omitempty"`

	FilesRead    []string `json:"files_read,omitempty"`
	FilesWritten []string `json:"files_written,omitempty"`

	Findings []FindingUsage `json:"findings,omitempty"`

	// TaskType is empty when the recorded evidence does not settle it.
	TaskType   string `json:"task_type,omitempty"`
	TaskVia    string `json:"task_via,omitempty"`
	Difficulty int    `json:"difficulty,omitempty"`

	// PrimaryModel served the most successful turns. ModelTier ranks it by
	// list price; ModelFit flags a tier that does not match the difficulty.
	PrimaryModel string `json:"primary_model,omitempty"`
	ModelTier    string `json:"model_tier,omitempty"`
	ModelFit     string `json:"model_fit,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// ToolUsage counts how often one tool was called in a session.
type ToolUsage struct {
	Tool   string `json:"tool"`
	Server string `json:"server,omitempty"`
	Source string `json:"source"`
	Calls  int    `json:"calls"`
}

// ServerUsage counts calls per MCP server.
type ServerUsage struct {
	Server string `json:"server"`
	Calls  int    `json:"calls"`
}

// FindingUsage counts findings of one kind and type across a session.
type FindingUsage struct {
	Kind        string `json:"kind"`
	Type        string `json:"type"`
	Origin      string `json:"origin"`
	Occurrences int    `json:"occurrences"`
	// Samples holds matched values, and only when hivetrace.finding_samples
	// is on. See that setting for what keeping them means.
	Samples []string `json:"samples,omitempty"`
}

// Filter narrows an event or session query. Zero values mean "no constraint".
type Filter struct {
	SessionID string
	TeamID    string
	KeyHash   string
	UserID    string
	AgentID   string
	Model     string
	Since     time.Time
	Until     time.Time
	// HasFindings restricts results to records where a PII or secret detector
	// matched.
	HasFindings bool
	Limit       int
	Offset      int
}

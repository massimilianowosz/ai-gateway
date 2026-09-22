package hivestate

import "time"

// Mode represents the HiveState processing mode.
type Mode string

const (
	ModeNoOp  Mode = "none"  // pass through unchanged
	ModeState Mode = "state" // LLM-based intent + state extraction
)

// State is the structured conversational state extracted by the state model.
type State struct {
	Intent             string      `json:"intent"`
	Difficulty         string      `json:"difficulty,omitempty"`       // HiveRoute: task difficulty level
	ReasoningEffort    string      `json:"reasoning_effort,omitempty"` // HiveRoute: recommended reasoning effort (low/medium/high)
	ActiveConstraints  interface{} `json:"active_constraints"`
	ConversationStatus string      `json:"conversation_status,omitempty"`
	// Legacy fields (kept for backward compat with older extractions)
	Domain           string      `json:"domain,omitempty"`
	ResolvedItems    []string    `json:"resolved_items,omitempty"`
	PendingItems     []string    `json:"pending_items,omitempty"`
	ImportantContext interface{} `json:"important_context,omitempty"`
}

// Result is the output of a HiveState processing step.
type Result struct {
	Mode           Mode
	Messages       []Message // rewritten messages to send upstream
	OriginalTokens int
	ResultTokens   int
	State          *State // non-nil if STATE mode succeeded
	StateJSON      string // raw JSON of the extracted state (for body rewriting)
	// StateParts is StateJSON split at frozen-block boundaries. Providers that
	// match their cache per content block need the pieces kept apart, or a
	// state that only ever grows still looks new on every turn.
	StateParts       []string
	FallbackReason   string // non-empty if we fell back to NO_OP
	PromptTokens     int    // tokens used by extraction call (prompt)
	CompletionTokens int    // tokens used by extraction call (completion)
	CacheHit         bool   // true if state was served from cache (no extraction call)

	// CCRMessages are original messages recovered from the compressed history
	// for this request. The HTTP rewriters splice them in as late as tool-call
	// adjacency allows — the tail of the prompt, which no provider cache
	// covers — so retrieval never invalidates a cached prefix.
	CCRMessages []Message

	// CCRBudgetSkipped is true when a working set was found but re-injecting it
	// would have eaten the compression saving. Process has no recorder of its
	// own, so it reports the decision here and the middleware counts it.
	CCRBudgetSkipped bool

	// RegistryMessage is the Code Registry: the identifiers extracted from the
	// history this rewrite is about to compress away.
	//
	// It needs a carrier of its own for the same reason CCRMessages does. The
	// rewriters rebuild the request body from the fields of this struct, not
	// from Messages, so a message appended only there never reaches the
	// provider — it just inflated ResultTokens and suppressed compressions that
	// were in fact worthwhile. It rides immediately after the state summary:
	// both are derived from the same history and change together, so it adds no
	// cache churn the summary does not already cause.
	RegistryMessage Message
}

// Metric records a HiveState processing event for observability.
type Metric struct {
	TeamID         string
	Model          string
	Mode           string
	OriginalTokens int
	ResultTokens   int
	ReductionRatio float64
	LatencyMs      int64
	Intent         string
	FallbackReason string
	CreatedAt      time.Time
}

// Message represents a chat message for HiveState processing.
type Message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

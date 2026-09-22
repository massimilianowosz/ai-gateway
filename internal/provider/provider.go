package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Provider handles communication with a specific LLM API backend.
type Provider interface {
	// Complete sends a chat completion request and returns the full response body.
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

	// Stream sends a chat completion request and returns a reader for SSE chunks.
	Stream(ctx context.Context, req *CompletionRequest) (StreamReader, error)

	// Name returns the provider identifier (e.g., "azure_openai", "vertex").
	Name() string
}

// StreamReader reads SSE chunks from a streaming completion response.
type StreamReader interface {
	// Next returns the next SSE data chunk as raw bytes.
	// Returns io.EOF when the stream is complete (after [DONE] sentinel).
	Next() ([]byte, error)

	// Close releases the underlying HTTP response resources.
	Close() error

	// Headers returns the response headers from the upstream provider.
	// Available immediately after Stream() returns.
	Headers() http.Header
}

// CompletionRequest represents an OpenAI-compatible chat completion request.
type CompletionRequest struct {
	Model               string                 `json:"model"`
	Messages            []Message              `json:"messages"`
	Stream              bool                   `json:"stream,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
	MaxTokens           *int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                   `json:"max_completion_tokens,omitempty"`
	Stop                interface{}            `json:"stop,omitempty"`
	PresencePenalty     *float64               `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64               `json:"frequency_penalty,omitempty"`
	N                   *int                   `json:"n,omitempty"`
	User                string                 `json:"user,omitempty"`
	Tools               []Tool                 `json:"tools,omitempty"`
	ToolChoice          interface{}            `json:"tool_choice,omitempty"`
	ResponseFormat      interface{}            `json:"response_format,omitempty"`
	StreamOptions       *StreamOptions         `json:"stream_options,omitempty"`
	Extra               map[string]interface{} `json:"-"` // provider-specific pass-through
	PinnedDeploymentID  string                 `json:"-"` // provider file references cannot fail over across accounts
}

var completionRequestKnownFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"stream":                {},
	"temperature":           {},
	"top_p":                 {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"stop":                  {},
	"presence_penalty":      {},
	"frequency_penalty":     {},
	"n":                     {},
	"user":                  {},
	"tools":                 {},
	"tool_choice":           {},
	"response_format":       {},
	"stream_options":        {},
}

// UnmarshalJSON preserves OpenAI-compatible fields this gateway does not model
// explicitly, so providers can pass them through unless drop_params removes them.
func (r *CompletionRequest) UnmarshalJSON(data []byte) error {
	type alias CompletionRequest
	var known alias
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	*r = CompletionRequest(known)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	for field := range completionRequestKnownFields {
		delete(raw, field)
	}
	if len(raw) == 0 {
		r.Extra = nil
		return nil
	}

	r.Extra = make(map[string]interface{}, len(raw))
	for field, value := range raw {
		var decoded interface{}
		if err := json.Unmarshal(value, &decoded); err != nil {
			return err
		}
		r.Extra[field] = decoded
	}
	return nil
}

// MarshalJSON reattaches preserved pass-through fields to outgoing
// OpenAI-compatible requests.
func (r CompletionRequest) MarshalJSON() ([]byte, error) {
	type alias CompletionRequest
	known := alias(r)
	known.Extra = nil

	data, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = make(map[string]json.RawMessage)
	}

	for field, value := range r.Extra {
		if _, reserved := completionRequestKnownFields[field]; reserved {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out[field] = encoded
	}

	return json.Marshal(out)
}

// Message represents a chat message.
type Message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"` // string or []ContentPart
	Name       string      `json:"name,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	// CacheControl is a prompt-cache breakpoint the caller placed at the end of
	// this message. It is never serialized: providers that support explicit
	// caching re-attach it in their own wire shape, and the ones that do not
	// must not receive a field they would reject. Dropping it silently costs
	// about ten times the input price on every cached prefix.
	CacheControl interface{} `json:"-"`
	// ReasoningContent and Refusal are response-only fields emitted by
	// providers that expose chain-of-thought summaries or content refusals.
	// The Responses API surfaces them as dedicated output items, so they must
	// survive the trip through the internal chat representation.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Refusal          string `json:"refusal,omitempty"`
}

// ContentPart represents a multimodal content part.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL holds image reference data.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// Tool represents a function tool definition.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
	// CacheControl marks the end of the cacheable tool prefix; see
	// Message.CacheControl for why it stays out of the wire format.
	CacheControl interface{} `json:"-"`
}

// PartCacheControl returns the prompt-cache breakpoint carried on an OpenAI
// content part. Clients on /v1/chat/completions place it there, inside the
// part, which is why it survives as a raw map rather than a modelled field.
func PartCacheControl(part interface{}) interface{} {
	m, ok := part.(map[string]interface{})
	if !ok {
		return nil
	}
	if cc, ok := m["cache_control"]; ok {
		return cc
	}
	return nil
}

// Function defines a callable function.
type Function struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// ToolCall represents a tool call in an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function invocation in a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// StreamOptions controls streaming behavior.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// CompletionResponse represents an OpenAI-compatible chat completion response.
type CompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`

	// Headers from the upstream response (not serialized to JSON).
	Headers http.Header `json:"-"`
}

// Choice is a completion choice.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
	// Logprobs is the provider's token-probability block, kept raw because the
	// gateway only ever relays it: the Responses API carries the same entries
	// on its output_text content part.
	Logprobs *ChoiceLogprobs `json:"logprobs,omitempty"`
}

// ChoiceLogprobs mirrors the Chat Completions logprobs object. Only Content is
// modelled: the refusal entries have no place to go in a Responses output.
type ChoiceLogprobs struct {
	Content []json.RawMessage `json:"content,omitempty"`
}

// Usage contains token usage information.
//
// Providers disagree on whether prompt-cache tokens are counted inside the
// prompt total. OpenAI reports prompt_tokens inclusive of cached_tokens;
// Anthropic reports input_tokens *excluding* both cache_read_input_tokens and
// cache_creation_input_tokens. Provider adapters normalize to the inclusive
// form so one internal model serves every surface:
//
//	PromptTokens = uncached + CachedPromptTokens + CacheCreationTokens
//
// Billing must therefore subtract the cache categories out of PromptTokens
// rather than adding them on top; see pricing.Calculator.CostUsage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Detail breakdowns are optional upstream. They unmarshal automatically
	// from any OpenAI-compatible provider and feed the Responses API's
	// input_tokens_details / output_tokens_details.
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`

	// The fields below are internal accounting state for providers that
	// report cache usage *outside* prompt_tokens_details — the whole
	// Anthropic family. They are never serialized: the response body a caller
	// receives keeps the provider's own shape. Read them through CachedTokens
	// and CacheCreation rather than directly, so both reporting styles resolve
	// the same way.
	cachedPromptTokens  int
	cacheCreationTokens int
	cacheReported       bool
}

// UnmarshalJSON also accepts the Anthropic family's sibling cache counters, so
// a stream chunk carrying them between an adapter and the handler that bills
// the turn arrives with its breakdown intact — unexported state cannot survive
// the round trip on its own.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type alias Usage
	var raw struct {
		alias
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*u = Usage(raw.alias)
	if raw.CacheReadInputTokens != nil || raw.CacheCreationInputTokens != nil {
		var read, created int
		if raw.CacheReadInputTokens != nil {
			read = *raw.CacheReadInputTokens
		}
		if raw.CacheCreationInputTokens != nil {
			created = *raw.CacheCreationInputTokens
		}
		u.SetCacheUsage(read, created)
	}
	return nil
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// SetCacheUsage records a provider-reported prompt-cache breakdown. Used by
// adapters whose wire format carries cache counters outside
// prompt_tokens_details (Anthropic, Vertex, Bedrock).
func (u *Usage) SetCacheUsage(cached, created int) {
	u.cachedPromptTokens = cached
	u.cacheCreationTokens = created
	u.cacheReported = true
}

// CachedTokens returns prompt tokens the provider served from its cache,
// resolving either reporting style.
func (u Usage) CachedTokens() int {
	if u.cacheReported {
		return u.cachedPromptTokens
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

// CacheCreation returns prompt tokens the provider wrote into its cache. Only
// the Anthropic family reports this; OpenAI has no equivalent.
func (u Usage) CacheCreation() int {
	if u.cacheReported {
		return u.cacheCreationTokens
	}
	return 0
}

// CacheReported is true when the provider actually reported a cache
// breakdown. False means the zeros are an absence of data, not an observation
// of zero — spend records mark such rows estimated rather than silently
// billing everything at full input price.
func (u Usage) CacheReported() bool {
	return u.cacheReported || u.PromptTokensDetails != nil
}

// WireMap renders the usage in the shape UnmarshalJSON round-trips, including
// the cache counters no OpenAI-standard field can carry. Stream adapters use it
// to hand a breakdown to the relay that bills the turn.
func (u Usage) WireMap() map[string]int {
	m := map[string]int{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.cacheReported {
		m["cache_read_input_tokens"] = u.cachedPromptTokens
		m["cache_creation_input_tokens"] = u.cacheCreationTokens
	}
	return m
}

// UncachedPromptTokens returns the prompt tokens billed at full input price.
func (u Usage) UncachedPromptTokens() int {
	n := u.PromptTokens - u.CachedTokens() - u.CacheCreation()
	if n < 0 {
		// A provider that double-counts, or a partial report, must never
		// produce a negative charge.
		return 0
	}
	return n
}

// Deployment represents a specific model deployment on a provider.
type Deployment struct {
	ID            string // unique identifier
	ModelName     string // user-facing name (e.g., "gpt-4o")
	ProviderName  string // provider profile name (e.g., "openai", "ollama")
	ProviderModel string // provider-specific name (e.g., "gpt-4o-2024-11-20")
	Provider      Provider
	DropParams    []string // parameters to strip before sending
	IsEU          bool     // whether this model runs in EU region
	AuthMode      string   // "api_key" (default) | "oauth_passthrough" — how the gateway authenticates upstream
	BillingMode   string   // "metered" (default) | "flat" — how the tenant is charged; independent of AuthMode
}

// UpstreamError represents an error from an upstream provider.
type UpstreamError struct {
	StatusCode int
	Message    string
	Type       string
	Code       string
}

func (e *UpstreamError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("upstream error (HTTP %d, %s): %s", e.StatusCode, e.Type, e.Message)
	}
	return fmt.Sprintf("upstream error (HTTP %d): %s", e.StatusCode, e.Message)
}

// eofReader is a no-op implementation of StreamReader for testing.
type eofReader struct{}

func (e *eofReader) Next() ([]byte, error) { return nil, io.EOF }
func (e *eofReader) Close() error          { return nil }
func (e *eofReader) Headers() http.Header  { return http.Header{} }

// --- Embeddings ---

// Embedder is an optional interface that providers can implement to support embeddings.
type Embedder interface {
	Embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error)
}

// EmbeddingRequest represents an embeddings API request.
type EmbeddingRequest struct {
	Model          string      `json:"model"`
	Input          interface{} `json:"input"` // string or []string
	EncodingFormat string      `json:"encoding_format,omitempty"`
	Dimensions     *int        `json:"dimensions,omitempty"`
	User           string      `json:"user,omitempty"`
}

// EmbeddingResponse represents an embeddings API response.
type EmbeddingResponse struct {
	Object string            `json:"object"`
	Data   []EmbeddingObject `json:"data"`
	Model  string            `json:"model"`
	Usage  EmbeddingUsage    `json:"usage"`
}

// EmbeddingObject is a single embedding vector.
type EmbeddingObject struct {
	Object    string      `json:"object"`
	Index     int         `json:"index"`
	Embedding interface{} `json:"embedding"` // []float64 or base64 string
}

// EmbeddingUsage is token usage for embeddings (input only).
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// --- Moderations ---

// Moderator is an optional interface for content moderation.
type Moderator interface {
	Moderate(ctx context.Context, req *ModerationRequest) (*ModerationResponse, error)
}

// ModerationRequest represents a moderation request.
type ModerationRequest struct {
	Model string      `json:"model"`
	Input interface{} `json:"input"` // string or []string
}

// ModerationResponse represents a moderation response.
type ModerationResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Results []ModerationResult `json:"results"`
}

// ModerationResult is a single result from the moderation API.
type ModerationResult struct {
	Flagged        bool               `json:"flagged"`
	Categories     map[string]bool    `json:"categories"`
	CategoryScores map[string]float64 `json:"category_scores"`
}

// --- Image Generation ---

// ImageGenerator is an optional interface for image generation.
type ImageGenerator interface {
	// GenerateImage generates an image from a JSON request body.
	// Returns the raw JSON response.
	GenerateImage(ctx context.Context, requestBody []byte) ([]byte, error)
}

// --- Audio ---

// TextToSpeech is an optional interface for text-to-speech.
type TextToSpeech interface {
	// Speak generates audio from a JSON request body.
	// Returns audio bytes and content type (e.g., "audio/mpeg").
	Speak(ctx context.Context, requestBody []byte) (audio []byte, contentType string, err error)
}

// Transcriber is an optional interface for speech-to-text.
type Transcriber interface {
	// Transcribe converts audio to text.
	Transcribe(ctx context.Context, req *TranscriptionRequest) ([]byte, error)
}

// TranscriptionRequest represents an audio transcription request.
type TranscriptionRequest struct {
	Model    string
	File     []byte
	Filename string
	Language string
	Format   string // response_format
}

// --- Files API ---

// FileRequest describes an OpenAI-compatible Files API request that must be
// forwarded to the provider without buffering the uploaded payload.
type FileRequest struct {
	Method        string
	Path          string
	RawQuery      string
	ContentType   string
	ContentLength int64
	Body          io.Reader
}

// FileAPI is implemented by providers that expose the OpenAI-compatible Files
// API. The caller owns the returned response body and must close it.
type FileAPI interface {
	DoFileRequest(ctx context.Context, req FileRequest) (*http.Response, error)
}

// ResponsesAPI is implemented by providers that expose a native OpenAI
// Responses endpoint. The caller owns the returned response body.
type ResponsesAPI interface {
	DoResponsesRequest(ctx context.Context, body io.Reader, contentLength int64) (*http.Response, error)
}

// ResponsesResourceAPI is implemented by providers that also expose the
// stateful half of the Responses API: retrieve, delete, cancel and input-item
// listing. It is separate from ResponsesAPI because a provider can accept
// Responses requests without storing them (the ChatGPT Codex backend requires
// store:false), in which case the gateway serves those operations from its own
// records instead.
//
// path is appended to the provider's /responses endpoint, e.g.
// "/resp_123" or "/resp_123/cancel".
type ResponsesResourceAPI interface {
	DoResponsesResourceRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)
}

// NativeResponsesCapability lets a type that implements ResponsesAPI report
// whether the endpoint behind a given deployment really exposes /responses.
// One client type serves both api.openai.com and arbitrary OpenAI-*compatible*
// backends (Groq, Together, vLLM, Ollama, Mistral on Azure AI Foundry, ...),
// and most of those speak /chat/completions only. Without this gate the
// gateway would hand them a Responses request verbatim and return their 404 to
// the client instead of falling back to the Chat Completions translation.
//
// A provider that does not implement this interface is assumed to be native.
type NativeResponsesCapability interface {
	SupportsNativeResponses() bool
}

// --- Generic Passthrough ---

// RawForwarder is an optional interface for generic request forwarding.
type RawForwarder interface {
	// Forward sends a raw request to the specified endpoint and returns the response.
	Forward(ctx context.Context, endpoint string, contentType string, body io.Reader) (respBody []byte, respContentType string, err error)
}

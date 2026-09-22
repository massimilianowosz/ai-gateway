package hivestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// ExtractionResult holds both the parsed state and raw JSON string.
type ExtractionResult struct {
	State *State
	JSON  string
	// Parts is JSON split at frozen-block boundaries, set only by the
	// append-only path. Empty means the state is one indivisible piece.
	Parts            []string
	PromptTokens     int
	CompletionTokens int
}

// StateExtractor calls a cheap LLM to extract conversational state.
type StateExtractor struct {
	provider        provider.Provider
	providerModel   string
	timeout         time.Duration
	hiveRouteLevels []config.HiveRouteLevel // HiveRoute difficulty levels (injected into prompt)
}

// NewStateExtractor creates a StateExtractor using a model from the registry.
func NewStateExtractor(registry *provider.Registry, modelName string, timeout time.Duration, hiveRouteLevels []config.HiveRouteLevel) (*StateExtractor, error) {
	deps, err := registry.GetDeployments(modelName)
	if err != nil || len(deps) == 0 {
		return nil, fmt.Errorf("hivestate: model %q not found in registry", modelName)
	}

	dep := deps[0]
	return &StateExtractor{
		provider:        dep.Provider,
		providerModel:   dep.ProviderModel,
		timeout:         timeout,
		hiveRouteLevels: hiveRouteLevels,
	}, nil
}

// Extract sends the conversation history to the extraction model and returns structured state.
func (e *StateExtractor) Extract(ctx context.Context, history []Message, lastUser Message) (*ExtractionResult, error) {
	return e.ExtractWithBudget(ctx, history, lastUser, 0, nil)
}

// ExtractWithBudget sends the conversation history to the extraction model with a token budget.
// If maxOutputTokens > 0, it constrains the output and adds conciseness instructions.
// If levelOverrides is non-nil and non-empty, those levels are used instead of the default ones.
func (e *StateExtractor) ExtractWithBudget(ctx context.Context, history []Message, lastUser Message, maxOutputTokens int, levelOverrides []config.HiveRouteLevel) (*ExtractionResult, error) {
	levels := e.levelsFor(levelOverrides)
	return e.extract(ctx, stateExtractionSystemPrompt, buildExtractionPrompt(history, lastUser, levels), maxOutputTokens)
}

// ExtractDelta extracts only what newMessages add to a state that already exists.
//
// The difference from ExtractWithBudget is the contract, not the plumbing: the
// model is shown the state so far as read-only context and asked for the new
// span alone. That is what lets the caller append the answer as a frozen block
// instead of replacing everything that came before it.
func (e *StateExtractor) ExtractDelta(ctx context.Context, priorState string, newMessages []Message, lastUser Message, maxOutputTokens int, levelOverrides []config.HiveRouteLevel) (*ExtractionResult, error) {
	levels := e.levelsFor(levelOverrides)
	systemPrompt := stateExtractionSystemPrompt + deltaExtractionPromptSuffix
	return e.extract(ctx, systemPrompt, buildDeltaExtractionPrompt(priorState, newMessages, lastUser, levels), maxOutputTokens)
}

func (e *StateExtractor) levelsFor(overrides []config.HiveRouteLevel) []config.HiveRouteLevel {
	if len(overrides) > 0 {
		return overrides
	}
	return e.hiveRouteLevels
}

func (e *StateExtractor) extract(ctx context.Context, systemPrompt, userPrompt string, maxOutputTokens int) (*ExtractionResult, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	maxTok := 2048

	if maxOutputTokens > 0 && maxOutputTokens < maxTok {
		// Set max_tokens with a floor of 512 to ensure LLM can produce valid JSON.
		// Below 512 BPE tokens, structured JSON gets truncated mid-stream.
		maxTok = maxOutputTokens
		if maxTok < 512 {
			maxTok = 512
		}
		systemPrompt += fmt.Sprintf("\n\nIMPORTANT: Keep output under %d tokens. Be extremely concise:\n"+
			"- actions_taken: only last 3-4 actions, one-line results\n"+
			"- Omit files_modified if not a coding task\n"+
			"- Omit errors_encountered if none\n"+
			"- identifiers: only actively referenced IDs\n"+
			"- values: only values needed for the NEXT action", maxOutputTokens)
	}

	req := &provider.CompletionRequest{
		Model: e.providerModel,
		Messages: []provider.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens:      &maxTok,
		ResponseFormat: stateResponseFormat,
	}

	const maxRetries = 3
	var resp *provider.CompletionResponse
	var err error

	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err = e.provider.Complete(ctx, req)
		if err == nil {
			break
		}
		var ue *provider.UpstreamError
		if errors.As(err, &ue) && ue.StatusCode == 429 && attempt < maxRetries-1 {
			wait := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("extraction call: %w (after retry wait cancelled)", err)
			case <-time.After(wait):
				continue
			}
		}
		return nil, fmt.Errorf("extraction call: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("extraction call: %w", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return nil, fmt.Errorf("extraction: empty response")
	}

	raw := extractContent(resp.Choices[0].Message.Content)

	// Try to find valid JSON object in the output
	raw = extractJSON(raw)

	// Post-truncate if output exceeds budget
	if maxOutputTokens > 0 {
		raw = truncateStateJSON(raw, maxOutputTokens)
	}

	// Parse state JSON
	var state State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("extraction: invalid JSON: %w\nraw: %s", err, raw)
	}

	// Validate minimum fields
	if state.Intent == "" {
		return nil, fmt.Errorf("extraction: missing intent field")
	}

	var promptTok, completionTok int
	if resp.Usage != nil {
		promptTok = resp.Usage.PromptTokens
		completionTok = resp.Usage.CompletionTokens
	}

	return &ExtractionResult{
		State:            &state,
		JSON:             raw,
		PromptTokens:     promptTok,
		CompletionTokens: completionTok,
	}, nil
}

// extractContent pulls JSON from a model response, stripping markdown fences if present.
func extractContent(content interface{}) string {
	s, ok := content.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	// Strip markdown code fences
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

// extractJSON finds the first valid JSON object in a string.
// Models sometimes emit preamble text or trailing commentary around the JSON.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	// Find matching closing brace
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	// No matching brace found, return from start
	return s[start:]
}

// stateResponseFormat forces the model to output valid JSON.
// OpenAI and Ollama (v0.5+) support {"type": "json_object"}.
// The exact schema is enforced by the system prompt + post-validation.
var stateResponseFormat = map[string]interface{}{
	"type": "json_object",
}

// truncateStateJSON progressively removes fields from state JSON to fit within budget.
// Budget is in chars/4 tokens. Returns the (possibly trimmed) JSON string.
func truncateStateJSON(raw string, budgetTokens int) string {
	budgetChars := budgetTokens * 4
	if len(raw) <= budgetChars {
		return raw
	}

	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw // can't parse, return as-is
	}

	// Progressive truncation steps (least to most important)
	steps := []func(map[string]interface{}){
		// Step 1: remove low-value optional fields
		func(m map[string]interface{}) {
			delete(m, "errors_encountered")
			delete(m, "files_modified")
			delete(m, "domain")
		},
		// Step 2: trim list fields
		func(m map[string]interface{}) {
			trimSlice(m, "resolved_items", 2)
			trimSlice(m, "pending_items", 3)
			trimSlice(m, "actions_taken", 3)
		},
		// Step 3: trim active_constraints sub-maps
		func(m map[string]interface{}) {
			if ac, ok := m["active_constraints"].(map[string]interface{}); ok {
				trimMap(ac, "identifiers", 3)
				trimMap(ac, "values", 3)
			}
		},
		// Step 4: more aggressive trimming
		func(m map[string]interface{}) {
			trimSlice(m, "actions_taken", 2)
			trimSlice(m, "resolved_items", 1)
			delete(m, "important_context")
			if ac, ok := m["active_constraints"].(map[string]interface{}); ok {
				trimMap(ac, "identifiers", 2)
				trimMap(ac, "values", 2)
			}
		},
		// Step 5: minimal state
		func(m map[string]interface{}) {
			trimSlice(m, "actions_taken", 1)
			delete(m, "resolved_items")
			delete(m, "pending_items")
			if ac, ok := m["active_constraints"].(map[string]interface{}); ok {
				trimMap(ac, "identifiers", 2)
				trimMap(ac, "values", 1)
			}
		},
	}

	for _, step := range steps {
		step(obj)
		out, err := json.Marshal(obj)
		if err != nil {
			return raw
		}
		if len(out) <= budgetChars {
			return string(out)
		}
	}

	// Final fallback: marshal whatever we have
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return string(out)
}

// trimSlice keeps only the last n elements of a slice field.
func trimSlice(m map[string]interface{}, key string, keep int) {
	v, ok := m[key]
	if !ok {
		return
	}
	arr, ok := v.([]interface{})
	if !ok || len(arr) <= keep {
		return
	}
	m[key] = arr[len(arr)-keep:]
}

// trimMap keeps only the first n entries of a nested map field.
func trimMap(m map[string]interface{}, key string, keep int) {
	v, ok := m[key]
	if !ok {
		return
	}
	sub, ok := v.(map[string]interface{})
	if !ok || len(sub) <= keep {
		return
	}
	trimmed := make(map[string]interface{}, keep)
	i := 0
	for k, val := range sub {
		if i >= keep {
			break
		}
		trimmed[k] = val
		i++
	}
	m[key] = trimmed
}

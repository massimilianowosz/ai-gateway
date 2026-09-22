package hivestate

import "strings"

// ConversationProfile describes the detected type of conversation,
// used to dynamically adjust compression parameters.
type ConversationProfile int

const (
	ProfileChat      ConversationProfile = iota // multi-turn human dialogue
	ProfileToolAgent                            // tool-augmented agent (API calls, structured data)
	ProfileCodeAgent                            // coding agent (long code, file edits)
)

// ProfileParams holds the dynamic parameters for each profile.
type ProfileParams struct {
	StepWindow     int // how many recent assistant steps to keep
	PreprocessMax  int // max content tokens for preprocessing (0 = skip preprocessing)
	PromotedBudget int // max tokens for promoted important messages
	MaxPromoted    int // max messages to promote from History
}

// DefaultProfileParams returns the tuned parameters for each profile.
func DefaultProfileParams(profile ConversationProfile) ProfileParams {
	// step_window=0 means "use config value" — don't override the configured step_window.
	// The token budget cap will dynamically reduce if needed.
	switch profile {
	case ProfileToolAgent:
		return ProfileParams{
			StepWindow:     0,
			PreprocessMax:  0,
			PromotedBudget: 1500,
			MaxPromoted:    4,
		}
	case ProfileCodeAgent:
		return ProfileParams{
			StepWindow:     0,
			PreprocessMax:  500,
			PromotedBudget: 2000,
			MaxPromoted:    5,
		}
	default: // ProfileChat
		return ProfileParams{
			StepWindow:     0,
			PreprocessMax:  800,
			PromotedBudget: 1200,
			MaxPromoted:    4,
		}
	}
}

// DetectProfile analyzes messages to determine conversation type.
// Uses heuristics on message patterns — fast, no LLM call needed.
func DetectProfile(messages []Message) ConversationProfile {
	if len(messages) == 0 {
		return ProfileChat
	}

	var (
		toolResponseCount int
		codeBlockCount    int
		jsonCount         int
		assistantCount    int
		totalMessages     int
	)

	for _, m := range messages {
		totalMessages++
		if m.Role == "assistant" {
			assistantCount++
		}

		content := m.Content

		// Detect tool responses (OpenAI format converted)
		if strings.HasPrefix(content, "[tool_response") ||
			m.ToolCallID != "" ||
			strings.Contains(content, "[tool_calls:") {
			toolResponseCount++
			continue
		}

		// Detect code patterns
		if hasCodePatterns(content) {
			codeBlockCount++
		}

		// Detect structured JSON data
		trimmed := strings.TrimSpace(content)
		if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && len(trimmed) > 100 {
			jsonCount++
		}
	}

	if totalMessages == 0 {
		return ProfileChat
	}

	// Tool agent: >30% of messages are tool-related
	toolRatio := float64(toolResponseCount) / float64(totalMessages)
	if toolRatio > 0.30 || (toolResponseCount >= 4 && jsonCount >= 2) {
		return ProfileToolAgent
	}

	// Code agent: significant code content + many assistant steps
	codeRatio := float64(codeBlockCount) / float64(totalMessages)
	if codeRatio > 0.20 || (codeBlockCount >= 3 && assistantCount >= 8) {
		return ProfileCodeAgent
	}

	return ProfileChat
}

// hasCodePatterns checks if content looks like it contains source code.
func hasCodePatterns(content string) bool {
	if len(content) < 200 {
		return false
	}

	codeIndicators := []string{
		"```", "func ", "def ", "class ",
		"import ", "package ", "from ",
		"public ", "private ", "interface ",
		"diff --git", "@@", "+++ ", "--- ",
	}

	matches := 0
	lower := strings.ToLower(content)
	for _, ind := range codeIndicators {
		if strings.Contains(lower, ind) {
			matches++
			if matches >= 2 {
				return true
			}
		}
	}
	return false
}

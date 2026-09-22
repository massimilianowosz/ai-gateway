package hivestate

import (
	"math"
	"strings"
)

// MessageScore represents the computed importance of a message.
type MessageScore struct {
	Index        int
	Score        float64
	Recency      float64
	HasError     float64
	IsDecision   float64
	TokenDensity float64
}

// ScoreMessages assigns importance scores to messages.
// Higher score = more important = should be kept in Recent zone.
// Messages with errors, decisions, or high information density score higher.
func ScoreMessages(messages []Message, counter TokenCounter) []MessageScore {
	scores := make([]MessageScore, len(messages))
	totalMsgs := float64(len(messages))

	for i, m := range messages {
		scores[i].Index = i

		// Recency: exponential decay from end (weight: 0.30)
		position := float64(i) / totalMsgs
		scores[i].Recency = position * position * 0.30

		// Error detection: messages containing error patterns (weight: 0.30)
		scores[i].HasError = detectErrorImportance(m.Content) * 0.30

		// Decision/action markers: messages with clear decisions or outcomes (weight: 0.25)
		scores[i].IsDecision = detectDecisionImportance(m.Content) * 0.25

		// Token density: unique meaningful tokens / total tokens (weight: 0.15)
		scores[i].TokenDensity = computeTokenDensity(m.Content) * 0.15

		scores[i].Score = scores[i].Recency + scores[i].HasError + scores[i].IsDecision + scores[i].TokenDensity
	}

	return scores
}

// PickImportantMessages selects messages to keep in Recent zone based on scores.
// Guarantees: always keeps last stepWindow steps, plus any high-scoring messages from History.
// Returns indices of messages that should be promoted from History to Recent.
func PickImportantMessages(historyMessages []Message, scores []MessageScore, maxPromoted int, counter TokenCounter, tokenBudget int) []int {
	if len(scores) == 0 || maxPromoted <= 0 {
		return nil
	}

	// Find messages with scores above threshold (top quartile)
	threshold := computeThreshold(scores)

	var promoted []int
	tokens := 0
	for _, s := range scores {
		if s.Score >= threshold && len(promoted) < maxPromoted {
			msgTokens := counter.CountMessages([]Message{historyMessages[s.Index]})
			if tokenBudget > 0 && tokens+msgTokens > tokenBudget {
				continue
			}
			promoted = append(promoted, s.Index)
			tokens += msgTokens
		}
	}

	return promoted
}

// computeThreshold finds the score at the 75th percentile.
func computeThreshold(scores []MessageScore) float64 {
	if len(scores) == 0 {
		return 0
	}
	// Simple approach: find mean + 0.5*stddev
	var sum float64
	for _, s := range scores {
		sum += s.Score
	}
	mean := sum / float64(len(scores))

	var variance float64
	for _, s := range scores {
		diff := s.Score - mean
		variance += diff * diff
	}
	variance /= float64(len(scores))
	stddev := math.Sqrt(variance)

	return mean + 0.5*stddev
}

// detectErrorImportance checks if a message contains error-related content.
func detectErrorImportance(content string) float64 {
	lower := strings.ToLower(content)

	// Strong error indicators
	strongIndicators := []string{
		"error:", "panic:", "fatal:", "exception:",
		"stack trace", "traceback", "segfault",
		"failed to", "cannot ", "unable to",
		"exit code", "exit status",
		"permission denied", "not found",
		"null pointer", "nil pointer",
		"undefined", "unresolved",
	}

	// Moderate indicators
	moderateIndicators := []string{
		"warning:", "warn:", "deprecated",
		"timeout", "refused", "rejected",
		"invalid", "unexpected",
	}

	score := 0.0
	for _, ind := range strongIndicators {
		if strings.Contains(lower, ind) {
			score = 1.0
			break
		}
	}
	if score == 0 {
		for _, ind := range moderateIndicators {
			if strings.Contains(lower, ind) {
				score = 0.5
				break
			}
		}
	}

	return score
}

// detectDecisionImportance checks if a message contains decisions or important outcomes.
func detectDecisionImportance(content string) float64 {
	lower := strings.ToLower(content)

	decisionIndicators := []string{
		"decided to", "chose to", "will use",
		"created ", "deleted ", "modified ",
		"installed ", "configured ",
		"fixed ", "resolved ", "implemented ",
		"test pass", "tests pass", "all tests",
		"successfully", "completed",
		"the issue was", "root cause",
		"architecture:", "design:",
	}

	for _, ind := range decisionIndicators {
		if strings.Contains(lower, ind) {
			return 1.0
		}
	}

	// Short assistant messages with tool_use are often decisions
	if len(content) < 200 && strings.Contains(lower, "let me") {
		return 0.3
	}

	return 0.0
}

// computeTokenDensity measures information density of content.
// High density = many unique words, low repetition.
func computeTokenDensity(content string) float64 {
	if len(content) == 0 {
		return 0
	}

	words := strings.Fields(content)
	if len(words) == 0 {
		return 0
	}

	// Count unique words
	seen := make(map[string]bool, len(words)/2)
	for _, w := range words {
		seen[strings.ToLower(w)] = true
	}

	density := float64(len(seen)) / float64(len(words))

	// Penalize very short messages (less informative)
	if len(words) < 10 {
		density *= 0.5
	}

	// Cap at 1.0
	if density > 1.0 {
		density = 1.0
	}

	return density
}

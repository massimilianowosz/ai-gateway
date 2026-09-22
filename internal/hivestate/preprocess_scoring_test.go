package hivestate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreProcessHistoryPreservesMetadataAndShortContent(t *testing.T) {
	input := []Message{{
		Role:       "tool",
		ToolCallID: "call-123",
		Content:    `{"status":"ok"}`,
	}}

	got := PreProcessHistory(input, 800)

	require.Len(t, got, 1)
	assert.Equal(t, input[0], got[0])
}

func TestPreprocessContentSamplesLargeJSONArray(t *testing.T) {
	items := make([]map[string]int, 8)
	for i := range items {
		items[i] = map[string]int{"id": i}
	}
	raw, err := json.Marshal(items)
	require.NoError(t, err)
	content := string(raw) + strings.Repeat(" ", 500)

	got := preprocessContent(content, 800)

	assert.Contains(t, got, "008 total items, showing 5")
	assert.Contains(t, got, `"id":0`)
	assert.Contains(t, got, `"id":7`)
	assert.NotContains(t, got, `"id":4`)
}

func TestPreprocessContentTrimsLongJSONObjectValues(t *testing.T) {
	longValue := strings.Repeat("a", 260)
	content := fmt.Sprintf(`{"id":"job-7","payload":"%s"}`, longValue) + strings.Repeat(" ", 500)

	got := preprocessContent(content, 800)

	var decoded map[string]string
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(got)), &decoded))
	assert.Equal(t, "job-7", decoded["id"])
	assert.Len(t, decoded["payload"], 153)
	assert.Contains(t, decoded["payload"], "...")
}

func TestPreprocessContentCompressesCodeAndLongText(t *testing.T) {
	code := `package sample

import "fmt"

type Service struct {
	name string
}

func (s *Service) Run() {
	if s.name != "" {
		for i := 0; i < 50; i++ {
			fmt.Println(s.name, i)
		}
	}
}
` + strings.Repeat("// padding\n", 80)

	compressedCode := preprocessContent(code, 500)
	assert.Contains(t, compressedCode, "package sample")
	assert.Contains(t, compressedCode, "func (s *Service) Run()")
	assert.Less(t, len(compressedCode), len(code))

	lines := make([]string, 50)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d %s", i, strings.Repeat("x", 20))
	}
	compressedText := preprocessContent(strings.Join(lines, "\n"), 100)
	assert.Contains(t, compressedText, "25 lines omitted")
	assert.Contains(t, compressedText, "line-00")
	assert.Contains(t, compressedText, "line-49")
	assert.NotContains(t, compressedText, "line-20")
}

func TestTruncateToolResponsesPreservesStructureAndBoundsValues(t *testing.T) {
	payload := map[string]interface{}{
		"id": "result-42",
		"nested": map[string]interface{}{
			"body": strings.Repeat("x", 300),
		},
		"items": []interface{}{
			map[string]interface{}{"value": strings.Repeat("a", 200)},
			map[string]interface{}{"value": strings.Repeat("b", 200)},
			map[string]interface{}{"value": strings.Repeat("c", 200)},
			map[string]interface{}{"value": strings.Repeat("d", 200)},
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	got := TruncateToolResponses([]Message{{
		Role:       "tool",
		ToolCallID: "call-42",
		Content:    string(raw),
	}}, 500)

	require.Len(t, got, 1)
	assert.Equal(t, "tool", got[0].Role)
	assert.Equal(t, "call-42", got[0].ToolCallID)
	assert.Contains(t, got[0].Content, `"id":"result-42"`)
	assert.NotContains(t, got[0].Content, strings.Repeat("x", 300))

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(got[0].Content), &decoded))
	assert.Len(t, decoded["items"], 3)
}

func TestTruncateToolContentFallsBackToHeadAndTail(t *testing.T) {
	content := strings.Repeat("H", 180) + strings.Repeat("T", 120)

	got := truncateToolContent(content, 90)

	assert.True(t, strings.HasPrefix(got, strings.Repeat("H", 60)))
	assert.Contains(t, got, "[...truncated...]")
	assert.True(t, strings.HasSuffix(got, strings.Repeat("T", 30)))
}

func TestScoreMessagesPrioritizesErrorsAndDecisions(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "hello hello hello hello hello"},
		{Role: "assistant", Content: "warning: request timeout while connecting"},
		{Role: "assistant", Content: "fatal: permission denied; root cause identified and fixed successfully"},
		{Role: "user", Content: "ordinary follow up with several unique meaningful words here"},
	}

	scores := ScoreMessages(messages, NewTokenCounter())

	require.Len(t, scores, len(messages))
	assert.Equal(t, 1.0, detectErrorImportance("FATAL: nil pointer"))
	assert.Equal(t, 0.5, detectErrorImportance("warning: timeout"))
	assert.Zero(t, detectErrorImportance("all good"))
	assert.Equal(t, 1.0, detectDecisionImportance("Implemented the fix successfully"))
	assert.Equal(t, 0.3, detectDecisionImportance("Let me check that"))
	assert.Zero(t, computeTokenDensity(""))
	assert.Greater(t, scores[2].Score, scores[0].Score)
	assert.Equal(t, 0.30, scores[2].HasError)
	assert.Equal(t, 0.25, scores[2].IsDecision)
}

func TestPickImportantMessagesHonorsLimitAndTokenBudget(t *testing.T) {
	history := []Message{
		{Content: strings.Repeat("a", 40)},
		{Content: strings.Repeat("b", 40)},
		{Content: strings.Repeat("c", 400)},
		{Content: strings.Repeat("d", 40)},
	}
	scores := []MessageScore{
		{Index: 0, Score: 0},
		{Index: 1, Score: 1.0},
		{Index: 2, Score: 1.0},
		{Index: 3, Score: 1.0},
	}

	got := PickImportantMessages(history, scores, 2, NewTokenCounter(), 40)

	assert.Equal(t, []int{1, 3}, got, "oversized high-score messages should be skipped without consuming the promotion limit")
	assert.Nil(t, PickImportantMessages(history, nil, 2, NewTokenCounter(), 0))
	assert.Nil(t, PickImportantMessages(history, scores, 0, NewTokenCounter(), 0))
}

func TestComputeThresholdUsesMeanAndDispersion(t *testing.T) {
	scores := []MessageScore{{Score: 0}, {Score: 0}, {Score: 1}, {Score: 1}}

	got := computeThreshold(scores)

	assert.InDelta(t, 0.75, got, 1e-9)
	assert.Zero(t, computeThreshold(nil))
}

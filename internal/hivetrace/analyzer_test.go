package hivetrace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func at(offset time.Duration) time.Time {
	return time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC).Add(offset)
}

func TestSummarize_EmptySessionYieldsNothing(t *testing.T) {
	assert.Nil(t, Summarize("s1", nil))
}

func TestSummarize_AggregatesTheSessionAccount(t *testing.T) {
	events := []Event{
		{
			ID: "e1", SessionID: "s1", TeamID: "team-a", KeyHash: "kh", KeyPrefix: "sk-ubq-1",
			UserID: "u1", AgentID: "a1", ClientProduct: "claude-code",
			Model: "claude-sonnet", Provider: "anthropic",
			Status: 200, CreatedAt: at(0), DurationMs: 100,
			PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, Cost: 0.01,
			Tools: []ToolInvocation{
				{Tool: "Read", Source: ToolSourceNative},
				{Tool: "create_issue", Server: "github", Source: ToolSourceMCP},
			},
			Files: []FileAccess{{Path: "/src/a.go", Operation: FileOpRead}},
			Findings: []Finding{
				{Kind: KindSecret, Type: "GITHUB_TOKEN", Origin: OriginRequest, Occurrences: 1},
			},
		},
		{
			ID: "e2", SessionID: "s1", Model: "claude-opus", Provider: "anthropic",
			Status: 500, CreatedAt: at(time.Minute), DurationMs: 900,
			PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55, Cost: 0.02,
			Tools: []ToolInvocation{{Tool: "Read", Source: ToolSourceNative}},
			Files: []FileAccess{{Path: "/src/b.go", Operation: FileOpWrite}},
			Findings: []Finding{
				{Kind: KindSecret, Type: "GITHUB_TOKEN", Origin: OriginRequest, Occurrences: 2},
				{Kind: KindPII, Type: "EMAIL_ADDRESS", Origin: OriginResponse, Occurrences: 1},
			},
		},
	}

	s := Summarize("s1", events)
	require.NotNil(t, s)

	assert.Equal(t, "s1", s.SessionID)
	assert.Equal(t, "team-a", s.TeamID)
	assert.Equal(t, "claude-code", s.ClientProduct)
	assert.Equal(t, 2, s.Requests)
	assert.Equal(t, 1, s.Errors)
	assert.Equal(t, at(0), s.StartedAt)
	assert.Equal(t, at(time.Minute), s.EndedAt)

	assert.Equal(t, []string{"claude-sonnet", "claude-opus"}, s.Models)
	assert.Equal(t, []string{"anthropic"}, s.Providers)

	assert.Equal(t, 150, s.PromptTokens)
	assert.Equal(t, 175, s.TotalTokens)
	assert.InDelta(t, 0.03, s.Cost, 1e-9)

	assert.Equal(t, 3, s.ToolCalls)
	require.Len(t, s.Tools, 2)
	assert.Equal(t, ToolUsage{Tool: "Read", Source: ToolSourceNative, Calls: 2}, s.Tools[0])
	assert.Equal(t, ToolUsage{Tool: "create_issue", Server: "github", Source: ToolSourceMCP, Calls: 1}, s.Tools[1])
	assert.Equal(t, []ServerUsage{{Server: "github", Calls: 1}}, s.MCPServers)

	assert.Equal(t, []string{"/src/a.go"}, s.FilesRead)
	assert.Equal(t, []string{"/src/b.go"}, s.FilesWritten)

	require.Len(t, s.Findings, 2)
	assert.Equal(t, FindingUsage{Kind: KindSecret, Type: "GITHUB_TOKEN", Origin: OriginRequest, Occurrences: 3}, s.Findings[0])
	assert.Equal(t, FindingUsage{Kind: KindPII, Type: "EMAIL_ADDRESS", Origin: OriginResponse, Occurrences: 1}, s.Findings[1])
}

// Same detector, different direction: a secret the caller sent and one the
// model returned are different exposures and must not be merged.
func TestSummarize_KeepsOriginsApart(t *testing.T) {
	s := Summarize("s1", []Event{{
		CreatedAt: at(0),
		Findings: []Finding{
			{Kind: KindSecret, Type: "AWS_ACCESS_KEY", Origin: OriginRequest, Occurrences: 1},
			{Kind: KindSecret, Type: "AWS_ACCESS_KEY", Origin: OriginResponse, Occurrences: 1},
		},
	}})

	require.Len(t, s.Findings, 2)
	assert.Equal(t, OriginRequest, s.Findings[0].Origin)
	assert.Equal(t, OriginResponse, s.Findings[1].Origin)
}

func TestSummarize_CountsErrorsFromStatusOrMessage(t *testing.T) {
	s := Summarize("s1", []Event{
		{CreatedAt: at(0), Status: 200},
		{CreatedAt: at(time.Second), Status: 429},
		{CreatedAt: at(2 * time.Second), Status: 200, ErrorMessage: "upstream reset"},
	})
	assert.Equal(t, 3, s.Requests)
	assert.Equal(t, 2, s.Errors)
}

// Nearest-rank: the reported percentile is always a latency that occurred.
func TestPercentile(t *testing.T) {
	sorted := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	assert.Equal(t, int64(50), percentile(sorted, 0.50))
	assert.Equal(t, int64(100), percentile(sorted, 0.95))
	assert.Equal(t, int64(0), percentile(nil, 0.5))
	assert.Equal(t, int64(7), percentile([]int64{7}, 0.95))
}

func TestSummarize_LatencyPercentiles(t *testing.T) {
	events := make([]Event, 0, 10)
	for i := 1; i <= 10; i++ {
		events = append(events, Event{CreatedAt: at(time.Duration(i) * time.Second), DurationMs: int64(i * 10)})
	}
	s := Summarize("s1", events)
	assert.Equal(t, int64(50), s.LatencyP50Ms)
	assert.Equal(t, int64(100), s.LatencyP95Ms)
}

func TestSampled(t *testing.T) {
	assert.True(t, sampled("anything", 1))
	assert.True(t, sampled("anything", 1.5))
	assert.False(t, sampled("anything", 0))

	// The same session always lands on the same side, so a sampled
	// conversation is captured whole rather than in fragments.
	first := sampled("session-abc", 0.5)
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, sampled("session-abc", 0.5))
	}
}

func TestDedupe(t *testing.T) {
	assert.Nil(t, dedupe(nil))
	assert.Equal(t, []string{"a", "b"}, dedupe([]string{"a", "b", "a", "b"}))
}

func TestTruncate(t *testing.T) {
	out, cut := truncate("hello", 10)
	assert.Equal(t, "hello", out)
	assert.False(t, cut)

	out, cut = truncate("hello", 3)
	assert.Equal(t, "hel", out)
	assert.True(t, cut)

	out, cut = truncate("hello", 0)
	assert.Equal(t, "hello", out)
	assert.False(t, cut)
}

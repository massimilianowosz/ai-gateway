package hivetrace

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyTask_FromWhatWasWritten(t *testing.T) {
	cases := []struct {
		name    string
		written []string
		want    string
	}{
		{"code", []string{"internal/proxy/handler.go", "internal/proxy/handler_test.go"}, TaskCoding},
		{"code beats a note", []string{"app.py", "README.md"}, TaskCoding},
		{"ops", []string{"deploy/values.yaml", "Dockerfile"}, TaskOps},
		{"ci", []string{".github/workflows/ci.json"}, TaskOps},
		{"data", []string{"out/report.csv", "notebooks/churn.ipynb"}, TaskData},
		{"writing", []string{"docs/paper.md", "notes.txt"}, TaskWriting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, via := classifyTask(&SessionSummary{FilesWritten: c.written})
			assert.Equal(t, c.want, got)
			assert.Equal(t, TaskViaFiles, via)
		})
	}
}

func TestClassifyTask_WithoutWrites(t *testing.T) {
	got, via := classifyTask(&SessionSummary{MCPServers: []ServerUsage{{Server: "sqlite", Calls: 3}}})
	assert.Equal(t, TaskData, got)
	assert.Equal(t, TaskViaTools, via)

	got, _ = classifyTask(&SessionSummary{Tools: []ToolUsage{{Tool: "WebSearch", Calls: 2}}})
	assert.Equal(t, TaskResearch, got)

	got, _ = classifyTask(&SessionSummary{FilesRead: []string{"a.go", "b.go", "c.md"}})
	assert.Equal(t, TaskResearch, got, "reading around without changing anything")
}

// No answer is better than a guessed one: the model fills these in.
func TestClassifyTask_StaysSilentWithoutEvidence(t *testing.T) {
	for _, s := range []*SessionSummary{
		{},
		{Requests: 12},
		{FilesRead: []string{"one.go"}},
		{FilesWritten: []string{"config.json", "blob.bin"}},
		{FilesRead: []string{"/Users/u/.codex/skills/pdf/SKILL.md", "/Users/u/.claude/rules/x.md", "repo/AGENTS.md"}},
	} {
		got, via := classifyTask(s)
		assert.Empty(t, got)
		assert.Empty(t, via)
	}
}

func TestMeasureDifficulty(t *testing.T) {
	assert.Equal(t, 0, measureDifficulty(&SessionSummary{Requests: 6, Errors: 6}), "nothing was served")
	assert.Equal(t, 1, measureDifficulty(&SessionSummary{Requests: 2}))
	assert.Equal(t, 2, measureDifficulty(&SessionSummary{Requests: 19, ToolCalls: 4}))
	assert.Equal(t, 3, measureDifficulty(&SessionSummary{Requests: 34, ToolCalls: 21}))
	assert.Equal(t, 5, measureDifficulty(&SessionSummary{
		Requests: 200, Errors: 20, ToolCalls: 152,
		FilesWritten: []string{"1", "2", "3", "4", "5", "6", "7", "8"},
	}))
}

// A burst of rate limits makes a session long, not hard.
func TestMeasureDifficulty_IgnoresFailedTurns(t *testing.T) {
	calm := measureDifficulty(&SessionSummary{Requests: 5})
	stormy := measureDifficulty(&SessionSummary{Requests: 45, Errors: 40})
	assert.Equal(t, calm, stormy)
}

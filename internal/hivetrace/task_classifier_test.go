package hivetrace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
)

func TestOpeningRequest_SkipsWhatTheAgentInjects(t *testing.T) {
	messages := []hivestate.Message{
		{Role: "system", Content: "You are Claude Code"},
		{Role: "user", Content: "# AGENTS.md instructions for /repo\n<INSTRUCTIONS>be terse</INSTRUCTIONS>"},
		{Role: "user", Content: "<environment_context><cwd>/repo</cwd></environment_context>"},
		{Role: "user", Content: "Warmup"},
		{Role: "user", Content: "<system-reminder>todo list empty</system-reminder>\nthe tests fail in CI with a nil pointer, why?"},
		{Role: "assistant", Content: "Looking."},
		{Role: "user", Content: "thanks"},
	}
	assert.Equal(t, "the tests fail in CI with a nil pointer, why?", openingRequest(messages))
}

func TestOpeningRequest_NothingToAsk(t *testing.T) {
	assert.Empty(t, openingRequest([]hivestate.Message{{Role: "user", Content: "<system-reminder>x</system-reminder>"}}))
	assert.Empty(t, openingRequest(nil))
}

func classifierServer(t *testing.T, choice string, p float64, status int, calls *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "Bearer k", r.Header.Get("Authorization"))
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{
			"kind": map[string]any{"choice": choice, "probabilities": map[string]float64{choice: p}},
		}})
	}))
}

func testClassifier(url string) *taskClassifier {
	cfg := config.HiveTraceConfig{TaskClassifier: config.TaskClassifierConfig{URL: url, APIKey: "k"}}
	cfg.ApplyDefaults()
	return newTaskClassifier(cfg.TaskClassifier)
}

func TestTaskClassifier_AsksOncePerSession(t *testing.T) {
	calls := 0
	srv := classifierServer(t, TaskDebugging, 0.9, http.StatusOK, &calls)
	defer srv.Close()
	c := testClassifier(srv.URL)

	label, score, ok := c.classify("s1", "why does it crash")
	assert.True(t, ok)
	assert.Equal(t, TaskDebugging, label)
	assert.InDelta(t, 0.9, score, 1e-9)
	assert.False(t, c.pending("s1"))
	assert.True(t, c.pending("s2"))
	assert.Equal(t, 1, calls)
}

// An unsure label is dropped, and the session is not asked again.
func TestTaskClassifier_DropsUnsureAnswers(t *testing.T) {
	calls := 0
	srv := classifierServer(t, TaskCoding, 0.3, http.StatusOK, &calls)
	defer srv.Close()
	c := testClassifier(srv.URL)

	_, _, ok := c.classify("s1", "hmm")
	assert.False(t, ok)
	assert.False(t, c.pending("s1"))
}

func TestTaskClassifier_PausesWhenTheServiceFails(t *testing.T) {
	calls := 0
	srv := classifierServer(t, "", 0, http.StatusBadGateway, &calls)
	defer srv.Close()
	c := testClassifier(srv.URL)

	_, _, ok := c.classify("s1", "anything")
	assert.False(t, ok)
	assert.False(t, c.pending("s2"), "paused, so the next flush does not pay a timeout")

	c.pausedUntil = time.Now().Add(-time.Second)
	assert.True(t, c.pending("s1"), "a failed call is retried once the pause is over")
}

func TestTaskClassifier_OffWithoutURL(t *testing.T) {
	var c *taskClassifier = newTaskClassifier(config.TaskClassifierConfig{})
	assert.Nil(t, c)
	assert.False(t, c.pending("s1"))
}

func TestSummarize_ModelFillsGapsAndRefinesDebugging(t *testing.T) {
	now := time.Now()
	s := Summarize("s", []Event{{CreatedAt: now, TaskHint: TaskWriting}})
	assert.Equal(t, TaskWriting, s.TaskType)
	assert.Equal(t, TaskViaModel, s.TaskVia)

	coding := []FileAccess{{Path: "main.go", Operation: FileOpWrite}}
	s = Summarize("s", []Event{{CreatedAt: now, TaskHint: TaskWriting, Files: coding}})
	assert.Equal(t, TaskCoding, s.TaskType, "the files outrank the model")

	s = Summarize("s", []Event{{CreatedAt: now, TaskHint: TaskDebugging, Files: coding}})
	assert.Equal(t, TaskDebugging, s.TaskType, "but only the model can see a fix is a fix")
}

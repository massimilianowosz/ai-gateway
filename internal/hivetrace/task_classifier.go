package hivetrace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
)

// TaskViaModel marks a task type the classifier chose from the opening request.
const TaskViaModel = "model"

var taskDescriptions = map[string]string{
	TaskCoding:    "writing or modifying application code",
	TaskDebugging: "diagnosing a failure, error or wrong behaviour",
	TaskOps:       "deploying, configuring infrastructure, CI or releases",
	TaskResearch:  "reading, comparing or understanding something without changing it",
	TaskData:      "querying, transforming or analysing data",
	TaskWriting:   "producing prose, docs or messages",
}

// taskClassifier asks SYSTEMONE what kind of task a session's opening request
// is. Each session is asked once; an unreachable service is left alone for a
// minute rather than costing every flush a timeout.
type taskClassifier struct {
	cfg    config.TaskClassifierConfig
	client *http.Client

	mu          sync.Mutex
	done        map[string]struct{}
	pausedUntil time.Time
}

const (
	classifierPause   = time.Minute
	classifierMemory  = 50000
	maxOpeningRequest = 4000
)

func newTaskClassifier(cfg config.TaskClassifierConfig) *taskClassifier {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil
	}
	return &taskClassifier{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}, done: map[string]struct{}{}}
}

// pending reports whether the session still needs a label.
func (c *taskClassifier) pending(sessionID string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.pausedUntil) {
		return false
	}
	_, seen := c.done[sessionID]
	return !seen
}

// classify returns the label when the model is confident enough. A confident
// or an unsure answer both settle the session; only a failed call is retried.
func (c *taskClassifier) classify(sessionID, prompt string) (string, float64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	label, score, err := c.ask(ctx, prompt)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.pausedUntil = time.Now().Add(classifierPause)
		return "", 0, false
	}
	if len(c.done) >= classifierMemory {
		c.done = map[string]struct{}{}
	}
	c.done[sessionID] = struct{}{}
	if taskDescriptions[label] == "" || score < c.cfg.MinConfidence {
		return "", 0, false
	}
	return label, score, true
}

func (c *taskClassifier) ask(ctx context.Context, prompt string) (string, float64, error) {
	body, err := json.Marshal(map[string]any{
		"state": prompt,
		"model": c.cfg.Model,
		"questions": map[string]any{
			"kind": map[string]any{
				"type":         "choice",
				"instructions": "What kind of task is the user asking for?",
				"criteria":     taskDescriptions,
			},
		},
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("task classifier: %s", resp.Status)
	}
	var out struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	a := out.Answers["kind"]
	return a.Choice, a.Probabilities[a.Choice], nil
}

// openingRequest is the first thing the person asked, without the context the
// agent injects around it: reminders, environment blocks, repository rules.
func openingRequest(messages []hivestate.Message) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		text := strings.TrimSpace(stripTaggedBlocks(m.Content))
		if strings.HasPrefix(text, "# AGENTS.md instructions") || strings.HasPrefix(text, "Caveat:") {
			continue
		}
		if len(strings.Fields(text)) < 2 {
			continue // "Warmup", a bare slash command
		}
		if len(text) > maxOpeningRequest {
			text = text[:maxOpeningRequest]
		}
		return text
	}
	return ""
}

// stripTaggedBlocks removes <name>…</name> blocks, which agents use for
// everything they add to a user turn on their own.
func stripTaggedBlocks(s string) string {
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			return s
		}
		end := strings.IndexByte(s[open:], '>')
		if end < 0 {
			return s
		}
		name := s[open+1 : open+end]
		if i := strings.IndexAny(name, " \t\n"); i >= 0 {
			name = name[:i]
		}
		closing := "</" + name + ">"
		if name == "" || !isTagName(name) {
			s = s[:open] + s[open+1:] // not a tag; drop the bracket and move on
			continue
		}
		stop := strings.Index(s[open:], closing)
		if stop < 0 {
			s = s[:open] + s[open+end+1:]
			continue
		}
		s = s[:open] + s[open+stop+len(closing):]
	}
}

func isTagName(name string) bool {
	for i, r := range name {
		ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

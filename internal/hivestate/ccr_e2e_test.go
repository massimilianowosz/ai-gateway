package hivestate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const recoveredFileMarker = "func ComputeTaxRate(region string) float64"

// codingSession is a realistic agentic history: the agent reads a file, a long
// discussion follows, then several short turns. HiveState compresses the early
// turns away and CCR has to bring the file back — the file is the thing the
// agent still needs to answer the final question.
func codingSession() []Message {
	discussion := strings.Repeat("Detailed discussion of the billing subsystem internals. ", 80)
	file := "package billing\n\n" + recoveredFileMarker + " {\n" +
		"\tswitch region {\n\tcase \"EU\":\n\t\treturn 0.22\n\t}\n\treturn 0\n}\n"

	msgs := []Message{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: "Explain how billing computes tax. " + discussion},
		{Role: "assistant", Content: `Read("/repo/internal/billing/tax.go")`},
		{Role: "tool", Content: file},
	}
	for i := 0; i < 2; i++ {
		msgs = append(msgs,
			Message{Role: "assistant", Content: fmt.Sprintf("Step %d done.", i)},
			Message{Role: "user", Content: fmt.Sprintf("Follow-up %d.", i)})
	}
	return append(msgs,
		Message{Role: "assistant", Content: "Added."},
		Message{Role: "user", Content: "Confirm the EU rate in /repo/internal/billing/tax.go."})
}

// seedStateCacheMirroringProcess primes the state cache for a History of any
// length.
//
// The shared seedStateCache helper caps History at 4 messages because above
// that Process reorders it by importance before hashing, which shifts the key.
// But the body rewriter only fires once there are enough turns to compress —
// and by then History is always 5 or more. The two constraints are mutually
// exclusive, so exercising the full path requires mirroring the reorder here.
// This intentionally duplicates Process's logic: if that drifts, this test
// fails loudly rather than silently testing a passthrough.
func seedStateCacheMirroringProcess(t *testing.T, hs *HiveState, messages []Message, stateJSON string, state *State) {
	t.Helper()

	profile := DetectProfile(messages)
	params := DefaultProfileParams(profile)
	stepWindow := hs.cfg.StepWindow
	if params.StepWindow > 0 && params.StepWindow < stepWindow {
		stepWindow = params.StepWindow
	}
	zones := SplitMessages(messages, stepWindow)

	historyTokens := hs.counter.CountMessages(zones.History)
	if len(zones.History) > 4 && historyTokens > 600 {
		scores := ScoreMessages(zones.History, hs.counter)
		promotedBudget := params.PromotedBudget
		if hs.cfg.TokenBudget > 0 {
			promotedBudget = hs.cfg.TokenBudget / 5
		}
		maxPromoted := params.MaxPromoted
		if historyTokens < 1500 {
			maxPromoted = 2
			promotedBudget = 500
		}
		promoted := PickImportantMessages(zones.History, scores, maxPromoted, hs.counter, promotedBudget)
		if len(promoted) > 0 {
			promSet := make(map[int]bool, len(promoted))
			for _, idx := range promoted {
				promSet[idx] = true
			}
			var regular, important []Message
			for i, m := range zones.History {
				if promSet[i] {
					important = append(important, m)
				} else {
					regular = append(regular, m)
				}
			}
			zones.History = append(regular, important...)
		}
	}
	require.NotEmpty(t, zones.History)
	hs.cache.put(historyHash(zones.History), stateJSON, state, len(zones.History))
}

// The end-to-end question: after HiveState compresses the history away, does
// the file the agent still needs actually reach the provider?
//
// Asserting that the file body appears is not enough on its own — an untouched
// passthrough body also contains it, which is how an earlier version of this
// test passed while proving nothing. So the test first proves the history
// really was compressed, then looks for the CCR framing that exists only on
// re-injection.
func TestCCR_RecoveredFileReachesTheForwardedBody(t *testing.T) {
	db := newTestStore(t) // in-memory: avoids the TempDir cleanup race the file-backed helper has

	guardOff := false
	hs := &HiveState{
		cfg: config.HiveStateConfig{
			Enabled: true, Threshold: 1, MaxLatencyMs: 3000, StepWindow: 2,
			Model:            "state-model",
			PrefixCacheGuard: config.PrefixCacheGuardConfig{Enabled: &guardOff},
		},
		extractor: &StateExtractor{},
		counter:   NewTokenCounter(),
		logger:    mwTestLogger(),
		cache:     newStateCache(128, 10*time.Minute),
		ccr:       newCCRStore(512, 32, 30*time.Minute),
	}

	msgs := codingSession()
	seedStateCacheMirroringProcess(t, hs, msgs, `{"intent":"edit tax rates"}`,
		&State{Intent: "edit tax rates"})

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(requestBodyFor("gpt-4o", msgs)))
	// CCR needs an owner: without a key in context the scope is invalid and
	// retrieval is disabled by design. Production always has one.
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{TeamID: "team-1", KeyHash: "key-hash-1"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.True(t, next.called)
	forwarded := string(next.body)

	// 1. The history really was compressed. Without this the rest proves
	//    nothing: an untouched body still carries the original file.
	require.Contains(t, forwarded, "Conversation state (summary of older context)",
		"HiveState did not rewrite the body (mode=%s fallback=%s)",
		rec.Header().Get("x-hivestate-mode"), rec.Header().Get("x-hivestate-fallback"))
	require.NotContains(t, forwarded, "Detailed discussion of the billing subsystem",
		"the old discussion should have been compressed away")

	// 2. Provenance: this framing exists only if CCR re-injected.
	require.Contains(t, forwarded, "Working file: /repo/internal/billing/tax.go",
		"CCR did not re-inject the working file")
	require.Contains(t, forwarded, recoveredFileMarker,
		"CCR framing present but the file body is missing")

	// 3. Placement: past the cacheable prefix, without displacing the live turn.
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(next.body, &parsed))
	n := len(parsed.Messages)
	at := -1
	for i, m := range parsed.Messages {
		if strings.Contains(string(m), "Working file:") {
			at = i
		}
	}
	require.Greater(t, at, 1, "recovered content sits inside the cacheable prefix")
	require.Less(t, at, n-1, "recovered content displaced the final user turn")
	require.Contains(t, string(parsed.Messages[n-1]), "Confirm the EU rate",
		"the last message must remain the live user turn")

	t.Logf("compressed %d messages to %d; recovered file at position %d of %d", len(msgs), n, at, n)
}

// The same request without an API key must compress normally but recover
// nothing: an unidentified caller has no scope, and no scope means no memory.
// This is the fail-safe direction — shared memory would be the unsafe one.
func TestCCR_WithoutIdentityCompressesButRecoversNothing(t *testing.T) {
	db := newTestStore(t)

	guardOff := false
	hs := &HiveState{
		cfg: config.HiveStateConfig{
			Enabled: true, Threshold: 1, MaxLatencyMs: 3000, StepWindow: 2,
			Model:            "state-model",
			PrefixCacheGuard: config.PrefixCacheGuardConfig{Enabled: &guardOff},
		},
		extractor: &StateExtractor{},
		counter:   NewTokenCounter(),
		logger:    mwTestLogger(),
		cache:     newStateCache(128, 10*time.Minute),
		ccr:       newCCRStore(512, 32, 30*time.Minute),
	}

	msgs := codingSession()
	seedStateCacheMirroringProcess(t, hs, msgs, `{"intent":"edit tax rates"}`,
		&State{Intent: "edit tax rates"})

	mw := Middleware(hs, db, nil, newTestSpender(t, db), nil, nil, mwTestLogger())
	next := &capturingHandler{}
	handler := mw(next.handler())

	// No auth.ContextWithKeyInfo: the scope has no owner.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(requestBodyFor("gpt-4o", msgs)))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	forwarded := string(next.body)
	require.Contains(t, forwarded, "Conversation state (summary of older context)",
		"compression itself must be unaffected by a missing scope")
	require.NotContains(t, forwarded, "Working file:",
		"an unidentified caller must not receive recovered content")
}

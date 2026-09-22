package workflow

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func chainStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(config.DatabaseConfig{Driver: "sqlite", URL: ":memory:"})
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Nothing walks previous_response_id at read time: storedResponseItems reads
// InputItems and the payload's output and stops. So a short-circuited turn has
// to file the resolved thread, exactly as the Responses path does — otherwise
// the turn after it sees this exchange and nothing before.
func TestPersistRespondTurn_StoresTheResolvedChain(t *testing.T) {
	db := chainStore(t)
	rs := db.(store.ResponseStore)
	ctx := context.Background()
	owner := "key:key-hash-1"

	// Turn 1, an ordinary stored turn.
	require.NoError(t, rs.CreateResponse(ctx, &store.StoredResponse{
		ID: "resp_1", OwnerID: owner, Model: "gpt-4o", Status: "completed",
		InputItems: `[{"id":"msg_a","type":"message","role":"user","content":[{"type":"input_text","text":"primo"}]}]`,
		Payload:    `{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"risposta uno"}]}]}`,
		CreatedAt:  time.Now(),
	}))

	wf := New(config.WorkflowConfig{Enabled: true, URL: "http://unused", Timeout: time.Second}, db, testLogger())
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(""))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{KeyHash: "key-hash-1", Active: true}))

	body := []byte(`{"model":"gpt-4o","input":"secondo","previous_response_id":"resp_1"}`)
	wf.persistRespondTurn(req, "resp_2", "gpt-4o", "risposta workflow", body)

	stored, err := rs.GetResponse(ctx, "resp_2", owner)
	require.NoError(t, err)
	require.NotNil(t, stored, "the short-circuited turn was not stored")

	assert.Contains(t, stored.InputItems, "primo", "turn 1's input was dropped from the chain")
	assert.Contains(t, stored.InputItems, "risposta uno", "turn 1's answer was dropped from the chain")
	assert.Contains(t, stored.InputItems, "secondo", "this turn's own input is missing")
	assert.Equal(t, "resp_1", stored.PreviousResponseID)
}

// A reference that does not resolve is reported, not answered as though the
// request were coherent.
func TestValidateRespondRequest_RejectsBadReferences(t *testing.T) {
	db := chainStore(t)
	wf := New(config.WorkflowConfig{Enabled: true, URL: "http://unused", Timeout: time.Second}, db, testLogger())
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(""))
	req = req.WithContext(auth.ContextWithKeyInfo(req.Context(),
		&store.APIKey{KeyHash: "key-hash-1", Active: true}))

	both, _ := json.Marshal(map[string]any{
		"conversation": "conv_x", "previous_response_id": "resp_x",
	})
	assert.Contains(t, wf.validateRespondRequest(req, both), "cannot be used together")

	missingConv, _ := json.Marshal(map[string]any{"conversation": "conv_nope"})
	assert.Contains(t, wf.validateRespondRequest(req, missingConv), "not found")

	missingPrev, _ := json.Marshal(map[string]any{"previous_response_id": "resp_nope"})
	assert.Contains(t, wf.validateRespondRequest(req, missingPrev), "not found")

	fine, _ := json.Marshal(map[string]any{"input": "ciao"})
	assert.Empty(t, wf.validateRespondRequest(req, fine))
}

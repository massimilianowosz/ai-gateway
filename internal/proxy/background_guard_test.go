package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// A worker whose deadline passed can still be holding an upstream connection.
// Without a guard its late answer overwrote the terminal state the client had
// already been shown, and a failed turn appeared to complete.
func TestStore_UpdateResponse_WontResurrectATerminalTurn(t *testing.T) {
	s := openResponsesTestStore(t)
	rs, ok := s.(store.ResponseStore)
	require.True(t, ok)
	ctx := context.Background()

	record := &store.StoredResponse{
		ID: "resp_late", OwnerID: "key:owner-1", Model: "m",
		Status: "in_progress", Payload: `{"status":"in_progress"}`,
		InputItems: "[]", CreatedAt: time.Now(),
	}
	require.NoError(t, rs.CreateResponse(ctx, record))

	// The sweep, or a cancel, closes the turn out.
	applied, err := rs.UpdateResponseStatus(ctx, record.ID, record.OwnerID, "failed",
		`{"status":"failed"}`)
	require.NoError(t, err)
	require.True(t, applied)

	// The abandoned worker answers afterwards.
	late := *record
	late.Status = "completed"
	late.Payload = `{"status":"completed"}`
	applied, err = rs.UpdateResponse(ctx, &late)
	require.NoError(t, err)
	assert.False(t, applied, "a write against a terminal row must report that it did not land")

	got, err := rs.GetResponse(ctx, record.ID, record.OwnerID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "failed", got.Status, "a late answer resurrected a closed turn")
	assert.Contains(t, got.Payload, "failed")
}

// The worker's own transition into a terminal state must still land.
func TestStore_UpdateResponse_TerminalTransitionStillLands(t *testing.T) {
	s := openResponsesTestStore(t)
	rs, _ := s.(store.ResponseStore)
	ctx := context.Background()

	record := &store.StoredResponse{
		ID: "resp_ok", OwnerID: "key:owner-1", Model: "m",
		Status: "in_progress", InputItems: "[]", CreatedAt: time.Now(),
	}
	require.NoError(t, rs.CreateResponse(ctx, record))

	record.Status = "completed"
	record.Payload = `{"status":"completed"}`
	landed, err := rs.UpdateResponse(ctx, record)
	require.NoError(t, err)
	assert.True(t, landed)

	got, err := rs.GetResponse(ctx, record.ID, record.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
}

// A background worker runs under a real deadline, so an upstream that never
// answers cannot park a goroutine and its connection indefinitely.
func TestResponsesHandler_BackgroundDeadlineIsBounded(t *testing.T) {
	h := &ResponsesHandler{}
	assert.Equal(t, defaultBackgroundTimeout, h.backgroundDeadline())

	h.SetBackgroundTimeout(90 * time.Second)
	assert.Equal(t, 90*time.Second, h.backgroundDeadline())

	h.SetBackgroundTimeout(0)
	assert.Equal(t, defaultBackgroundTimeout, h.backgroundDeadline())
}

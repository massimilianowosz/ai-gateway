package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStoredResponse(id string, expiresAt *time.Time) *StoredResponse {
	return &StoredResponse{
		ID:         id,
		OwnerID:    "key:owner-1",
		Model:      "gw-model",
		Status:     "completed",
		Payload:    `{"id":"` + id + `","object":"response","status":"completed"}`,
		InputItems: `[]`,
		CreatedAt:  time.Now(),
		ExpiresAt:  expiresAt,
	}
}

func TestStore_Response_CreateGetDelete(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_1", nil)))

	got, err := s.GetResponse(ctx, "resp_1", "key:owner-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "completed", got.Status)

	// Another tenant must not see it.
	other, err := s.GetResponse(ctx, "resp_1", "key:owner-2")
	require.NoError(t, err)
	assert.Nil(t, other)

	require.NoError(t, s.DeleteResponse(ctx, "resp_1", "key:owner-1"))
	gone, err := s.GetResponse(ctx, "resp_1", "key:owner-1")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestStore_Response_ExpiredIsNotReturned(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	past := time.Now().Add(-time.Minute)
	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_expired", &past)))

	got, err := s.GetResponse(ctx, "resp_expired", "key:owner-1")
	require.NoError(t, err)
	assert.Nil(t, got, "an expired response must read as absent even before it is purged")
}

func TestStore_Response_PurgeExpired(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_old", &past)))
	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_live", &future)))
	// Rows written before the retention setting existed carry no expiry and
	// must survive the purge.
	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_legacy", nil)))

	removed, err := s.PurgeExpiredResponses(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	live, err := s.GetResponse(ctx, "resp_live", "key:owner-1")
	require.NoError(t, err)
	assert.NotNil(t, live)

	legacy, err := s.GetResponse(ctx, "resp_legacy", "key:owner-1")
	require.NoError(t, err)
	assert.NotNil(t, legacy)
}

func TestStore_Response_CancelFlag(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_run", nil)))

	requested, err := s.ResponseCancelRequested(ctx, "resp_run")
	require.NoError(t, err)
	assert.False(t, requested)

	require.NoError(t, s.RequestResponseCancel(ctx, "resp_run", "key:owner-1"))
	requested, err = s.ResponseCancelRequested(ctx, "resp_run")
	require.NoError(t, err)
	assert.True(t, requested, "a worker on another replica polls this flag to stop")

	// A cancel from the wrong tenant must not touch the row.
	require.NoError(t, s.CreateResponse(ctx, newTestStoredResponse("resp_other", nil)))
	require.NoError(t, s.RequestResponseCancel(ctx, "resp_other", "key:someone-else"))
	requested, err = s.ResponseCancelRequested(ctx, "resp_other")
	require.NoError(t, err)
	assert.False(t, requested)
}

func TestStore_Response_FailStale(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	old := newTestStoredResponse("resp_abandoned", nil)
	old.Status = "in_progress"
	old.CreatedAt = time.Now().Add(-2 * time.Hour)
	require.NoError(t, s.CreateResponse(ctx, old))

	running := newTestStoredResponse("resp_running", nil)
	running.Status = "queued"
	require.NoError(t, s.CreateResponse(ctx, running))

	done := newTestStoredResponse("resp_done", nil)
	require.NoError(t, s.CreateResponse(ctx, done))

	failed, err := s.FailStaleResponses(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), failed)

	abandoned, err := s.GetResponse(ctx, "resp_abandoned", "key:owner-1")
	require.NoError(t, err)
	require.NotNil(t, abandoned)
	assert.Equal(t, "failed", abandoned.Status)

	// A turn started just now belongs to a live worker, possibly on another
	// replica, and must be left alone.
	live, err := s.GetResponse(ctx, "resp_running", "key:owner-1")
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, "queued", live.Status)
}

// The status column is not what a polling client reads: the handler serves the
// stored payload verbatim. Flipping only the column left the caller seeing
// "queued" forever on a turn nobody was producing.
func TestStore_Response_FailStalePatchesPayload(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	old := newTestStoredResponse("resp_stale_payload", nil)
	old.Status = "queued"
	old.Payload = `{"id":"resp_stale_payload","object":"response","status":"queued"}`
	old.CreatedAt = time.Now().Add(-2 * time.Hour)
	require.NoError(t, s.CreateResponse(ctx, old))

	_, err := s.FailStaleResponses(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	got, err := s.GetResponse(ctx, "resp_stale_payload", "key:owner-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "failed", got.Status)

	var payload struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(got.Payload), &payload))
	assert.Equal(t, "failed", payload.Status,
		"the payload a client receives must agree with the row")
	require.NotNil(t, payload.Error, "a failed turn needs an error object to be actionable")
	assert.NotEmpty(t, payload.Error.Code)
}

// A status write must touch status and payload only. Writing the whole row from
// a caller's snapshot rolls back the event log a streaming worker is appending
// to, leaving a follower with a terminal status and a truncated log — and so no
// terminal event to close its stream on.
func TestStore_Response_UpdateStatusLeavesEventsAlone(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	record := newTestStoredResponse("resp_events", nil)
	record.Status = "in_progress"
	record.Events = `[{"seq":1}]`
	require.NoError(t, s.CreateResponse(ctx, record))

	// The worker appends while a cancel is in flight elsewhere.
	live := *record
	live.Events = `[{"seq":1},{"seq":2},{"seq":3}]`
	_, err := s.UpdateResponse(ctx, &live)
	require.NoError(t, err)

	// The cancel writes from its own stale snapshot.
	applied, err := s.UpdateResponseStatus(ctx, record.ID, record.OwnerID, "cancelled",
		`{"id":"resp_events","object":"response","status":"cancelled"}`)
	require.NoError(t, err)
	assert.True(t, applied)

	got, err := s.GetResponse(ctx, record.ID, record.OwnerID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "cancelled", got.Status)
	assert.Equal(t, `[{"seq":1},{"seq":2},{"seq":3}]`, got.Events,
		"a status write rolled the event log back to a stale snapshot")
}

// A cancel that arrives after the answer landed must not overwrite it.
func TestStore_Response_UpdateStatusWontOverrideTerminal(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	record := newTestStoredResponse("resp_finished", nil) // status: completed
	require.NoError(t, s.CreateResponse(ctx, record))

	applied, err := s.UpdateResponseStatus(ctx, record.ID, record.OwnerID, "cancelled",
		`{"id":"resp_finished","object":"response","status":"cancelled"}`)
	require.NoError(t, err)
	assert.False(t, applied, "the write must report that it did not land")

	got, err := s.GetResponse(ctx, record.ID, record.OwnerID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "completed", got.Status, "a late cancel overwrote a finished turn")
}

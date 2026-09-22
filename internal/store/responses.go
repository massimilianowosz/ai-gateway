package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// CreateResponse persists one Responses API turn.
func (s *GormStore) CreateResponse(ctx context.Context, response *StoredResponse) error {
	if response == nil {
		return fmt.Errorf("store: response is required")
	}
	return s.db.WithContext(ctx).Create(response).Error
}

// GetResponse returns an unexpired response scoped to its tenant/key owner.
// A nil result means the response does not exist in that scope.
func (s *GormStore) GetResponse(ctx context.Context, id, ownerID string) (*StoredResponse, error) {
	var response StoredResponse
	err := s.db.WithContext(ctx).
		Where("id = ? AND owner_id = ? AND (expires_at IS NULL OR expires_at > ?)", id, ownerID, time.Now()).
		First(&response).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("store: get response: %w", err)
	}
	return &response, nil
}

// UpdateResponse overwrites a stored response. Background turns use it to move
// a queued placeholder to its final state.
//
// It reports whether the write landed. That matters beyond diagnostics: two
// concurrent pollers can both read a non-terminal turn and both decide it just
// finished, and only the one whose write actually applied may act on that —
// otherwise the turn is appended to its conversation twice.
func (s *GormStore) UpdateResponse(ctx context.Context, response *StoredResponse) (bool, error) {
	if response == nil {
		return false, fmt.Errorf("store: response is required")
	}
	// A terminal row is not written over. A worker whose deadline passed, or
	// whose turn was cancelled, can still be holding an upstream connection and
	// answer later; without this guard that late answer overwrote the failed or
	// cancelled state the client had already been shown, and the turn appeared
	// to complete after it had been closed out.
	//
	// The worker's own terminal write is allowed through: it is the transition
	// *into* a terminal state, and the row is still non-terminal when it lands.
	result := s.db.WithContext(ctx).
		Model(&StoredResponse{}).
		Where("id = ? AND owner_id = ? AND status IN ?",
			response.ID, response.OwnerID, nonTerminalResponseStatuses).
		Updates(map[string]interface{}{
			"status":        response.Status,
			"payload":       response.Payload,
			"input_items":   response.InputItems,
			"events":        response.Events,
			"upstream_id":   response.UpstreamID,
			"deployment_id": response.DeploymentID,
		})
	if result.Error != nil {
		return false, fmt.Errorf("store: update response: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// UpdateResponseStatus moves a turn to a new status, rewriting the stored
// payload alongside the column so a polling client is told the same thing the
// row says.
//
// It writes those two columns and nothing else. A full-row update from the
// caller's snapshot would roll the event log back to whatever that snapshot
// held, and a streaming worker appends to that log concurrently: a follower
// would then see a terminal status with a truncated log, and so no terminal
// event to close its stream on.
//
// The write applies only while the turn is still non-terminal. A cancel that
// arrives after the answer landed must not overwrite the answer.
func (s *GormStore) UpdateResponseStatus(ctx context.Context, id, ownerID, status, payload string) (bool, error) {
	fields := map[string]interface{}{"status": status}
	if payload != "" {
		fields["payload"] = payload
	}
	result := s.db.WithContext(ctx).
		Model(&StoredResponse{}).
		Where("id = ? AND owner_id = ? AND status IN ?", id, ownerID, nonTerminalResponseStatuses).
		Updates(fields)
	if result.Error != nil {
		return false, fmt.Errorf("store: update response status: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// DeleteResponse removes a tenant-scoped response.
func (s *GormStore) DeleteResponse(ctx context.Context, id, ownerID string) error {
	result := s.db.WithContext(ctx).
		Where("id = ? AND owner_id = ?", id, ownerID).
		Delete(&StoredResponse{})
	if result.Error != nil {
		return fmt.Errorf("store: delete response: %w", result.Error)
	}
	return nil
}

// PurgeExpiredResponses removes stored responses whose retention window has
// closed. Rows with no expiry are kept: they predate the retention setting.
func (s *GormStore) PurgeExpiredResponses(ctx context.Context) (int64, error) {
	result := s.db.WithContext(ctx).
		Where("expires_at IS NOT NULL AND expires_at <= ?", time.Now()).
		Delete(&StoredResponse{})
	if result.Error != nil {
		return 0, fmt.Errorf("store: purge expired responses: %w", result.Error)
	}
	return result.RowsAffected, nil
}

// RequestResponseCancel flags a running turn for cancellation. A cancel that
// lands on a replica other than the one executing the turn cannot reach its
// context directly, so it leaves this mark for the worker to notice.
func (s *GormStore) RequestResponseCancel(ctx context.Context, id, ownerID string) error {
	result := s.db.WithContext(ctx).
		Model(&StoredResponse{}).
		Where("id = ? AND owner_id = ?", id, ownerID).
		Update("cancel_requested", true)
	if result.Error != nil {
		return fmt.Errorf("store: request response cancel: %w", result.Error)
	}
	return nil
}

// FailStaleResponses marks background turns that have been running since before
// cutoff as failed. A worker that died mid-turn leaves its row queued forever
// otherwise, and a client polling it would wait for an answer nobody is
// producing. The cutoff keeps this safe with several replicas: only turns older
// than any plausible in-flight one are touched.
func (s *GormStore) FailStaleResponses(ctx context.Context, cutoff time.Time) (int64, error) {
	// The status column is not what a polling client reads: the handler serves
	// the stored payload verbatim, and the payload written at queue time says
	// "queued". Flipping only the column left the caller polling a turn nobody
	// was producing until the TTL removed the row and it started 404ing, which
	// is the exact failure this sweep exists to end. So the rows are loaded and
	// their payloads patched too.
	//
	// Only id and payload are selected: the event log of a streamed turn can be
	// large and none of it is needed here.
	var stale []struct {
		ID      string
		Payload string
	}
	if err := s.db.WithContext(ctx).
		Model(&StoredResponse{}).
		Select("id", "payload").
		Where("status IN ? AND created_at < ?", nonTerminalResponseStatuses, cutoff).
		Find(&stale).Error; err != nil {
		return 0, fmt.Errorf("store: fail stale responses: %w", err)
	}

	var failed int64
	for _, row := range stale {
		fields := map[string]interface{}{"status": "failed"}
		if payload := PatchResponsePayload(row.Payload, "failed",
			staleResponseErrorCode, staleResponseErrorMessage); payload != "" {
			fields["payload"] = payload
		}
		// Re-check the status in the WHERE clause: between the read above and
		// this write another replica may have picked the turn up, and marking a
		// live turn failed is worse than leaving a dead one queued.
		result := s.db.WithContext(ctx).
			Model(&StoredResponse{}).
			Where("id = ? AND status IN ?", row.ID, nonTerminalResponseStatuses).
			Updates(fields)
		if result.Error != nil {
			return failed, fmt.Errorf("store: fail stale responses: %w", result.Error)
		}
		failed += result.RowsAffected
	}
	return failed, nil
}

// nonTerminalResponseStatuses are the states a turn can still move out of.
// Both the stale sweep and a cancel guard their writes on it, so neither can
// overwrite a turn that already reached an answer.
var nonTerminalResponseStatuses = []string{"queued", "in_progress"}

const (
	staleResponseErrorCode    = "server_error"
	staleResponseErrorMessage = "the worker running this background turn did not finish it"
)

// PatchResponsePayload rewrites the "status" of a stored Responses payload and,
// when code is non-empty, attaches the matching "error" object.
//
// It returns "" when there is no payload or it is not a JSON object, which
// callers read as "leave the stored bytes alone". The payload is what a client
// actually receives, so any status change that must be visible to a caller has
// to go through here as well as through the row's column.
func PatchResponsePayload(payload, status, code, message string) string {
	if payload == "" {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &object) != nil {
		return ""
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return ""
	}
	object["status"] = encoded
	if code != "" {
		errObject, err := json.Marshal(map[string]string{"code": code, "message": message})
		if err != nil {
			return ""
		}
		object["error"] = errObject
	}
	out, err := json.Marshal(object)
	if err != nil {
		return ""
	}
	return string(out)
}

// ResponseCancelRequested reports whether a cancel has been requested for a
// turn. Workers poll it while streaming, so it reads the one column rather
// than the whole row with its payload and event log.
func (s *GormStore) ResponseCancelRequested(ctx context.Context, id string) (bool, error) {
	var requested bool
	err := s.db.WithContext(ctx).
		Model(&StoredResponse{}).
		Where("id = ?", id).
		Select("cancel_requested").
		Scan(&requested).Error
	if err != nil {
		return false, fmt.Errorf("store: read response cancel flag: %w", err)
	}
	return requested, nil
}

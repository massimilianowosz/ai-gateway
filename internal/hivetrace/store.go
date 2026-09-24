package hivetrace

import (
	"context"
	"time"
)

// TrafficStore persists captured events and the session summaries derived from
// them. Backends: sql (the gateway's own database) and clickhouse.
//
// Every method is called from a background worker, never from the request
// path, so implementations may block.
type TrafficStore interface {
	// InsertEvents writes a flushed batch. Re-inserting an id already stored
	// is not an error.
	InsertEvents(ctx context.Context, events []Event) error

	// ListEvents returns matching events oldest-first, so a session detail view
	// reads as a timeline.
	ListEvents(ctx context.Context, filter Filter) ([]Event, error)

	// SessionIDsSince returns the sessions with activity after a cutoff, which
	// is how the analyzer avoids recomputing quiet sessions.
	SessionIDsSince(ctx context.Context, since time.Time, limit int) ([]string, error)

	UpsertSession(ctx context.Context, summary *SessionSummary) error
	GetSession(ctx context.Context, sessionID string) (*SessionSummary, error)
	ListSessions(ctx context.Context, filter Filter) ([]SessionSummary, error)

	// Purge drops events older than the cutoff and reports how many went.
	Purge(ctx context.Context, before time.Time) (int64, error)

	Close() error
}

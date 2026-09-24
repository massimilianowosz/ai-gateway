package hivetrace

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func openTestStore(t *testing.T) (store.Store, TrafficStore) {
	t.Helper()
	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "trace.db"),
	})
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background()))
	t.Cleanup(func() { _ = db.Close() })

	ts, err := NewSQLStore(db)
	require.NoError(t, err)
	return db, ts
}

func sampleEvent(id, session string, created time.Time) Event {
	return Event{
		ID: id, RequestID: "req-" + id, SessionID: session,
		TeamID: "team-a", KeyHash: "kh-1", KeyPrefix: "sk-ubq-1",
		UserID: "u1", AgentID: "agent-1",
		API: APIMessages, Path: "/v1/messages",
		Model: "claude-sonnet", Provider: "anthropic",
		ClientProduct: "claude-code", ClientVersion: "2.1.0",
		Status: 200, Streaming: true,
		StartedAt: created, DurationMs: 420, TTFBMs: 80,
		PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120,
		CachedPromptTokens: 40, ReasoningTokens: 5, Cost: 0.0123,
		RequestBody: "read the file", ResponseBody: "here it is",
		Truncated: true, Redactions: []string{"EMAIL_ADDRESS"},
		Tools: []ToolInvocation{
			{Name: "mcp__github__create_issue", Tool: "create_issue", Server: "github", Source: ToolSourceMCP, CallID: "c1", ArgumentsBytes: 42},
		},
		Files:     []FileAccess{{Path: "/src/a.go", Operation: FileOpRead, Tool: "Read"}},
		Findings:  []Finding{{Kind: KindSecret, Type: "GITHUB_TOKEN", Origin: OriginResponse, Occurrences: 2}},
		CreatedAt: created,
	}
}

func TestSQLStore_EventRoundTrip(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Millisecond)

	require.NoError(t, ts.InsertEvents(ctx, []Event{sampleEvent("e1", "s1", created)}))

	got, err := ts.ListEvents(ctx, Filter{SessionID: "s1"})
	require.NoError(t, err)
	require.Len(t, got, 1)

	e := got[0]
	assert.Equal(t, "e1", e.ID)
	assert.Equal(t, "claude-code", e.ClientProduct)
	assert.True(t, e.Streaming)
	assert.True(t, e.Truncated)
	assert.Equal(t, []string{"EMAIL_ADDRESS"}, e.Redactions)
	assert.Equal(t, 40, e.CachedPromptTokens)
	assert.InDelta(t, 0.0123, e.Cost, 1e-9)

	require.Len(t, e.Tools, 1)
	assert.Equal(t, "github", e.Tools[0].Server)
	assert.Equal(t, ToolSourceMCP, e.Tools[0].Source)

	require.Len(t, e.Files, 1)
	assert.Equal(t, "/src/a.go", e.Files[0].Path)

	require.Len(t, e.Findings, 1)
	assert.Equal(t, KindSecret, e.Findings[0].Kind)
	assert.Equal(t, 2, e.Findings[0].Occurrences)
}

func TestSQLStore_InsertIsIdempotent(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	e := sampleEvent("e1", "s1", time.Now().UTC())

	require.NoError(t, ts.InsertEvents(ctx, []Event{e}))
	require.NoError(t, ts.InsertEvents(ctx, []Event{e}), "a replayed batch must not fail")

	got, err := ts.ListEvents(ctx, Filter{SessionID: "s1"})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestSQLStore_EmptyBatchIsANoOp(t *testing.T) {
	_, ts := openTestStore(t)
	assert.NoError(t, ts.InsertEvents(context.Background(), nil))
}

func TestSQLStore_EventsReadBackOldestFirst(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	require.NoError(t, ts.InsertEvents(ctx, []Event{
		sampleEvent("e2", "s1", base.Add(time.Minute)),
		sampleEvent("e1", "s1", base),
	}))

	got, err := ts.ListEvents(ctx, Filter{SessionID: "s1"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "e1", got[0].ID, "a session detail view reads as a timeline")
}

func TestSQLStore_FilterByFindings(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	clean := sampleEvent("clean", "s2", now)
	clean.Findings = nil

	require.NoError(t, ts.InsertEvents(ctx, []Event{sampleEvent("dirty", "s1", now), clean}))

	got, err := ts.ListEvents(ctx, Filter{HasFindings: true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "dirty", got[0].ID)
}

func TestSQLStore_FilterByIdentityAndModel(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	other := sampleEvent("other", "s2", now)
	other.TeamID = "team-b"
	other.Model = "gpt-5"

	require.NoError(t, ts.InsertEvents(ctx, []Event{sampleEvent("mine", "s1", now), other}))

	byTeam, err := ts.ListEvents(ctx, Filter{TeamID: "team-a"})
	require.NoError(t, err)
	require.Len(t, byTeam, 1)
	assert.Equal(t, "mine", byTeam[0].ID)

	byModel, err := ts.ListEvents(ctx, Filter{Model: "gpt-5"})
	require.NoError(t, err)
	require.Len(t, byModel, 1)
	assert.Equal(t, "other", byModel[0].ID)
}

func TestSQLStore_SessionSummaryRoundTripAndUpsert(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()

	summary := Summarize("s1", []Event{sampleEvent("e1", "s1", time.Now().UTC())})
	require.NoError(t, ts.UpsertSession(ctx, summary))

	got, err := ts.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Requests)
	assert.Equal(t, []string{"claude-sonnet"}, got.Models)
	require.Len(t, got.MCPServers, 1)
	assert.Equal(t, "github", got.MCPServers[0].Server)
	require.Len(t, got.Findings, 1)

	summary.Requests = 7
	require.NoError(t, ts.UpsertSession(ctx, summary))

	got, err = ts.GetSession(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, 7, got.Requests, "a recomputed summary replaces the previous one")

	all, err := ts.ListSessions(ctx, Filter{TeamID: "team-a"})
	require.NoError(t, err)
	assert.Len(t, all, 1)
}

// A session the analyzer has not reached yet is absent, not an error: the
// detail view still has events to show.
func TestSQLStore_MissingSessionIsNotAnError(t *testing.T) {
	_, ts := openTestStore(t)
	got, err := ts.GetSession(context.Background(), "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSQLStore_SessionIDsSince(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, ts.InsertEvents(ctx, []Event{
		sampleEvent("old", "s-old", now.Add(-2*time.Hour)),
		sampleEvent("new", "s-new", now),
	}))

	ids, err := ts.SessionIDsSince(ctx, now.Add(-time.Hour), 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"s-new"}, ids)
}

func TestSQLStore_PurgeDropsExpiredEventsAndSessions(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, ts.InsertEvents(ctx, []Event{
		sampleEvent("old", "s-old", now.Add(-48*time.Hour)),
		sampleEvent("fresh", "s-new", now),
	}))
	require.NoError(t, ts.UpsertSession(ctx, Summarize("s-old", []Event{sampleEvent("old", "s-old", now.Add(-48*time.Hour))})))

	removed, err := ts.Purge(ctx, now.Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	left, err := ts.ListEvents(ctx, Filter{})
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, "fresh", left[0].ID)

	gone, err := ts.GetSession(ctx, "s-old")
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestAnalyzer_BuildsSummariesFromStoredEvents(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, ts.InsertEvents(ctx, []Event{
		sampleEvent("e1", "s1", now.Add(-2*time.Minute)),
		sampleEvent("e2", "s1", now.Add(-time.Minute)),
	}))

	hub := NewHub()
	updates, unsubscribe := hub.Subscribe(4)
	defer unsubscribe()

	analyzer := NewAnalyzer(ts, time.Minute, hub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, analyzer.Run(ctx))

	got, err := ts.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 2, got.Requests)
	assert.Equal(t, 2, got.ToolCalls)

	select {
	case msg := <-updates:
		assert.Equal(t, LiveSession, msg.Type)
		require.NotNil(t, msg.Session)
		assert.Equal(t, "s1", msg.Session.SessionID)
	case <-time.After(time.Second):
		t.Fatal("expected the analyzer to publish the recomputed summary")
	}
}

// Rebuilding from events rather than accumulating means a second pass over the
// same data converges instead of doubling the totals.
func TestAnalyzer_IsIdempotent(t *testing.T) {
	_, ts := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, ts.InsertEvents(ctx, []Event{sampleEvent("e1", "s1", now)}))
	analyzer := NewAnalyzer(ts, time.Minute, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, analyzer.Run(ctx))
	require.NoError(t, analyzer.Run(ctx))

	got, err := ts.GetSession(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, 1, got.Requests)
	assert.Equal(t, 1, got.ToolCalls)
}

package spend

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"io"
	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// --- helpers ---

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mockStore for spend tests.
type mockStore struct {
	mu      sync.Mutex
	records []store.SpendRecord
	keys    map[string]*store.APIKey
	teams   map[string]*store.Team
	logErr  error
}

func newMockStore() *mockStore {
	return &mockStore{
		keys:  make(map[string]*store.APIKey),
		teams: make(map[string]*store.Team),
	}
}

func (m *mockStore) LogSpend(_ context.Context, r store.SpendRecord) error {
	if m.logErr != nil {
		return m.logErr
	}
	m.mu.Lock()
	m.records = append(m.records, r)
	m.mu.Unlock()
	return nil
}

func (m *mockStore) GetKeyByHash(_ context.Context, hash string) (*store.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[hash]
	if !ok {
		return nil, nil
	}
	return k, nil
}

func (m *mockStore) GetTeam(_ context.Context, id string) (*store.Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.teams[id]
	if !ok {
		return nil, nil
	}
	return t, nil
}

func (m *mockStore) loggedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

// satisfy full store.Store interface (unused methods)
func (m *mockStore) CreateKey(_ context.Context, _ *store.APIKey) error { return nil }
func (m *mockStore) ListKeys(_ context.Context, _ store.KeyFilter) ([]store.APIKey, error) {
	return nil, nil
}
func (m *mockStore) UpdateKey(_ context.Context, _ string, _ store.UpdateKeyParams) error {
	return nil
}
func (m *mockStore) DeleteKey(_ context.Context, _ string) error       { return nil }
func (m *mockStore) CreateTeam(_ context.Context, _ *store.Team) error { return nil }
func (m *mockStore) ListTeams(_ context.Context, _ store.TeamFilter) ([]store.Team, error) {
	return nil, nil
}
func (m *mockStore) UpdateTeam(_ context.Context, _ string, _ store.UpdateTeamParams) error {
	return nil
}
func (m *mockStore) DeleteTeam(_ context.Context, _ string) error             { return nil }
func (m *mockStore) CreateUser(_ context.Context, _ *store.User) error        { return nil }
func (m *mockStore) GetUser(_ context.Context, _ string) (*store.User, error) { return nil, nil }
func (m *mockStore) GetUserByEmail(_ context.Context, _ string) (*store.User, error) {
	return nil, nil
}
func (m *mockStore) ListUsers(_ context.Context, _ store.UserFilter) ([]store.User, error) {
	return nil, nil
}
func (m *mockStore) UpdateUser(_ context.Context, _ string, _ store.UpdateUserParams) error {
	return nil
}
func (m *mockStore) DeleteUser(_ context.Context, _ string) error { return nil }
func (m *mockStore) GetSpend(_ context.Context, _ store.SpendFilter) ([]store.SpendRecord, error) {
	return nil, nil
}
func (m *mockStore) GetTotalSpend(_ context.Context, _ string) (float64, error)  { return 0, nil }
func (m *mockStore) PurgeExpiredSpendRecords(_ context.Context) (int64, error)   { return 0, nil }
func (m *mockStore) LogCacheMetric(_ context.Context, _ store.CacheMetric) error { return nil }
func (m *mockStore) ListCacheMetrics(_ context.Context, _ int) ([]store.CacheMetric, error) {
	return nil, nil
}
func (m *mockStore) DeleteCacheMetrics(_ context.Context) error { return nil }
func (m *mockStore) LogHiveStateMetric(_ context.Context, _ store.HiveStateMetric) error {
	return nil
}
func (m *mockStore) ListHiveStateMetrics(_ context.Context, _ int) ([]store.HiveStateMetric, error) {
	return nil, nil
}
func (m *mockStore) Migrate(_ context.Context) error { return nil }
func (m *mockStore) Close() error                    { return nil }
func (m *mockStore) GetSpendSummary(_ context.Context, _ store.SpendFilter) ([]store.SpendSummary, error) {
	return nil, nil
}
func (m *mockStore) LogKeyEvent(_ context.Context, _ store.KeyEvent) error { return nil }

func (m *mockStore) GetTenantSettings(_ context.Context, _ string) (*store.TenantSettings, error) {
	return nil, nil
}
func (m *mockStore) UpsertTenantSettings(_ context.Context, _ *store.TenantSettings) error {
	return nil
}
func (m *mockStore) GetKeyRouteSettings(_ context.Context, _ string) (*store.KeyRouteSettings, error) {
	return nil, nil
}
func (m *mockStore) UpsertKeyRouteSettings(_ context.Context, _ *store.KeyRouteSettings) error {
	return nil
}
func (m *mockStore) DeleteKeyRouteSettings(_ context.Context, _ string) error { return nil }
func (m *mockStore) StoreFeedback(_ context.Context, _ *store.FeedbackRecord) error {
	return nil
}
func (m *mockStore) ListFeedback(_ context.Context, _ string, _ int) ([]store.FeedbackRecord, error) {
	return nil, nil
}
func (m *mockStore) GetFeedbackFiredToday(_ context.Context, _ string) ([]string, int, error) {
	return nil, 0, nil
}
func (m *mockStore) SetKeyFeedbackEnabled(_ context.Context, _ string, _ bool) error {
	return nil
}
func (m *mockStore) GetFeedbackSessionSignals(_ context.Context, _ string) (*store.FeedbackSessionSignals, error) {
	return nil, nil
}

// mockEmitter captures emitted events.
type mockEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

type emittedEvent struct {
	event string
	data  any
}

func (e *mockEmitter) Emit(event string, data any) {
	e.mu.Lock()
	e.events = append(e.events, emittedEvent{event, data})
	e.mu.Unlock()
}

func (e *mockEmitter) find(event string) []emittedEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []emittedEvent
	for _, ev := range e.events {
		if ev.event == event {
			out = append(out, ev)
		}
	}
	return out
}

// --- crossedBudget ---

func TestCrossedBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget float64
		spend  float64
		delta  float64
		want   bool
	}{
		{"no budget (0)", 0, 5, 1, false},
		{"spend below budget", 10, 5, 2, false},
		{"spend equals budget after delta", 10, 10, 5, true},
		{"exactly crossed threshold", 10, 10, 0.001, true},
		{"spend was already over before delta", 10, 15, 3, false},
		{"delta zero", 10, 10, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crossedBudget(tt.budget, tt.spend, tt.delta)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- BatchWriter.Record / flush ---

func TestBatchWriter_RecordAndFlush(t *testing.T) {
	ms := newMockStore()
	bw := NewBatchWriter(ms, discardLogger(), 50*time.Millisecond)
	defer bw.Close()

	bw.Record(store.SpendRecord{ID: "r1", KeyHash: "h1"})
	bw.Record(store.SpendRecord{ID: "r2", KeyHash: "h2"})

	assert.Eventually(t, func() bool {
		return ms.loggedCount() == 2
	}, 500*time.Millisecond, 10*time.Millisecond)
}

func TestBatchWriter_Close_FlushesRemainingRecords(t *testing.T) {
	ms := newMockStore()
	// Very long interval so flush never fires automatically
	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute)

	bw.Record(store.SpendRecord{ID: "r1"})
	bw.Record(store.SpendRecord{ID: "r2"})
	bw.Record(store.SpendRecord{ID: "r3"})

	require.NoError(t, bw.Close())
	assert.Equal(t, 3, ms.loggedCount())
}

func TestBatchWriter_DefaultInterval(t *testing.T) {
	ms := newMockStore()
	// interval <= 0 should default to 5s without panicking
	bw := NewBatchWriter(ms, discardLogger(), 0)
	require.NotNil(t, bw)
	require.NoError(t, bw.Close())
}

// --- RecordSync ---

func TestBatchWriter_RecordSync_ImmediateWrite(t *testing.T) {
	ms := newMockStore()
	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute)
	defer bw.Close()

	err := bw.RecordSync(context.Background(), store.SpendRecord{ID: "sync-1", KeyHash: "h1"})
	require.NoError(t, err)
	assert.Equal(t, 1, ms.loggedCount())
}

func TestBatchWriter_RecordSync_StoreError(t *testing.T) {
	ms := newMockStore()
	ms.logErr = errors.New("db down")
	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute)
	defer bw.Close()

	err := bw.RecordSync(context.Background(), store.SpendRecord{ID: "sync-fail"})
	assert.ErrorIs(t, err, ms.logErr)
}

// --- budget event emission ---

func TestBatchWriter_EmitsBudgetCrossed_Key(t *testing.T) {
	ms := newMockStore()
	em := &mockEmitter{}

	const keyHash = "hash-abc"
	ms.keys[keyHash] = &store.APIKey{
		ID:        "key-1",
		KeyHash:   keyHash,
		KeyPrefix: "sk-ab",
		Budget:    10.0,
		Spend:     10.0, // spend == budget → crossed
	}

	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute, em)
	defer bw.Close()

	err := bw.RecordSync(context.Background(), store.SpendRecord{
		ID:      "r1",
		KeyHash: keyHash,
		Cost:    1.0,
	})
	require.NoError(t, err)

	events := em.find("budget_crossed")
	require.Len(t, events, 1)
	data, ok := events[0].data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "key", data["scope"])
	assert.Equal(t, "sk-ab", data["key_prefix"])
}

func TestBatchWriter_EmitsBudgetCrossed_Team(t *testing.T) {
	ms := newMockStore()
	em := &mockEmitter{}

	ms.teams["team-1"] = &store.Team{
		ID:     "team-1",
		Budget: 50.0,
		Spend:  50.0,
	}

	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute, em)
	defer bw.Close()

	err := bw.RecordSync(context.Background(), store.SpendRecord{
		ID:     "r2",
		TeamID: "team-1",
		Cost:   5.0,
	})
	require.NoError(t, err)

	events := em.find("budget_crossed")
	require.Len(t, events, 1)
	data, ok := events[0].data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "team", data["scope"])
	assert.Equal(t, "team-1", data["team_id"])
}

func TestBatchWriter_NoBudgetEvent_WhenCostZero(t *testing.T) {
	ms := newMockStore()
	em := &mockEmitter{}

	const keyHash = "hash-zero"
	ms.keys[keyHash] = &store.APIKey{
		KeyHash: keyHash,
		Budget:  10.0,
		Spend:   10.0,
	}

	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute, em)
	defer bw.Close()

	// Cost is 0, so emitBudgetEvents should be skipped
	_ = bw.RecordSync(context.Background(), store.SpendRecord{
		ID:      "r3",
		KeyHash: keyHash,
		Cost:    0,
	})

	assert.Empty(t, em.find("budget_crossed"))
}

func TestBatchWriter_NoBudgetEvent_WhenNoBudgetSet(t *testing.T) {
	ms := newMockStore()
	em := &mockEmitter{}

	const keyHash = "hash-unlimited"
	ms.keys[keyHash] = &store.APIKey{
		KeyHash: keyHash,
		Budget:  0, // no budget allocated — auth blocks these, but spend tracking shouldn't emit budget_crossed
		Spend:   0,
	}

	bw := NewBatchWriter(ms, discardLogger(), 10*time.Minute, em)
	defer bw.Close()

	_ = bw.RecordSync(context.Background(), store.SpendRecord{
		ID:      "r4",
		KeyHash: keyHash,
		Cost:    1.0,
	})

	assert.Empty(t, em.find("budget_crossed"))
}

func TestBatchWriter_NilStore_NoError(t *testing.T) {
	// store == nil → logRecord is a no-op
	bw := NewBatchWriter(nil, discardLogger(), 10*time.Minute)
	defer bw.Close()

	bw.Record(store.SpendRecord{ID: "r1"})
	err := bw.RecordSync(context.Background(), store.SpendRecord{ID: "sync-1"})
	assert.NoError(t, err)
}

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// === Cache metrics ===

func TestStore_CacheMetric_LogAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.LogCacheMetric(ctx, CacheMetric{
		TeamID: "team-1", KeyPrefix: "sk-ab", Model: "gpt-4o",
		Band: "DIRECT", Score: 0.98, TokensSaved: 120, LatencyMs: 5,
	}))
	require.NoError(t, s.LogCacheMetric(ctx, CacheMetric{
		TeamID: "team-1", KeyPrefix: "sk-ab", Model: "gpt-4o",
		Band: "MISS", Score: 0.1, TokensSaved: 0, LatencyMs: 8,
	}))

	metrics, err := s.ListCacheMetrics(ctx, 10)
	require.NoError(t, err)
	require.Len(t, metrics, 2)
	// Most recent first.
	assert.Equal(t, "MISS", metrics[0].Band)
	assert.Equal(t, "DIRECT", metrics[1].Band)
}

func TestStore_CacheMetric_ListDefaultLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		require.NoError(t, s.LogCacheMetric(ctx, CacheMetric{Band: "DIRECT"}))
	}
	metrics, err := s.ListCacheMetrics(ctx, 0)
	require.NoError(t, err)
	assert.Len(t, metrics, 3)
}

func TestStore_CacheMetric_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.LogCacheMetric(ctx, CacheMetric{Band: "DIRECT"}))
	require.NoError(t, s.LogCacheMetric(ctx, CacheMetric{Band: "MISS"}))

	require.NoError(t, s.DeleteCacheMetrics(ctx))

	metrics, err := s.ListCacheMetrics(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, metrics)
}

// === HiveState metrics ===

func TestStore_HiveStateMetric_LogAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.LogHiveStateMetric(ctx, HiveStateMetric{
		RequestID: "req-1", Model: "gpt-4o", Mode: "state",
		OriginalTokens: 1000, ResultTokens: 200, Ratio: 0.2,
	}))
	require.NoError(t, s.LogHiveStateMetric(ctx, HiveStateMetric{
		RequestID: "req-2", Model: "gpt-4o", Mode: "none",
	}))

	metrics, err := s.ListHiveStateMetrics(ctx, 10)
	require.NoError(t, err)
	require.Len(t, metrics, 2)
	assert.Equal(t, "req-2", metrics[0].RequestID) // most recent first
}

func TestStore_HiveStateMetric_ListDefaultLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.LogHiveStateMetric(ctx, HiveStateMetric{RequestID: "only"}))

	metrics, err := s.ListHiveStateMetrics(ctx, -5)
	require.NoError(t, err)
	assert.Len(t, metrics, 1)
}

// === Tenant settings ===

func TestStore_TenantSettings_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetTenantSettings(context.Background(), "no-such-team")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_TenantSettings_UpsertAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	settings := &TenantSettings{
		TeamID:           "team-42",
		CacheEnabled:     true,
		HiveRouteEnabled: ptr(true),
	}
	require.NoError(t, s.UpsertTenantSettings(ctx, settings))

	got, err := s.GetTenantSettings(ctx, "team-42")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "team-42", got.TeamID)
	assert.True(t, got.CacheEnabled)
	require.NotNil(t, got.HiveRouteEnabled)
	assert.True(t, *got.HiveRouteEnabled)

	// Upsert again with a changed field to verify it updates in place.
	// (CacheEnabled/CompressionEnabled carry a GORM `default:true` tag, so
	// flipping a bool field to its Go zero value doesn't reliably round-trip
	// through Save() — exercise the update path via the pointer field
	// instead, which GORM writes based on nil-ness rather than zero value.)
	settings.HiveRouteEnabled = ptr(false)
	require.NoError(t, s.UpsertTenantSettings(ctx, settings))
	got, err = s.GetTenantSettings(ctx, "team-42")
	require.NoError(t, err)
	require.NotNil(t, got.HiveRouteEnabled)
	assert.False(t, *got.HiveRouteEnabled)
}

func TestStore_GetAnonymizationEntities_MapsGatewayTeamToTenant(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	team := &Team{ID: "team-anonymization", Name: "Anonymization tenant"}
	require.NoError(t, s.CreateTeam(ctx, team))

	gs := s.(*GormStore)
	require.NoError(t, gs.db.Exec(`
		CREATE TABLE anonymization_configs (
			tenant_id TEXT PRIMARY KEY,
			enabled_entities JSON NOT NULL
		)
	`).Error)
	require.NoError(t, gs.db.Exec(
		"INSERT INTO anonymization_configs (tenant_id, enabled_entities) VALUES (?, ?)",
		team.InternalID, `["EMAIL_ADDRESS","IP_ADDRESS"]`,
	).Error)

	entities, err := gs.GetAnonymizationEntities(ctx, team.ID)
	require.NoError(t, err)
	require.NotNil(t, entities)
	assert.Equal(t, []string{"EMAIL_ADDRESS", "IP_ADDRESS"}, *entities)

	missing, err := gs.GetAnonymizationEntities(ctx, "missing-team")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

// === Key route settings ===

func TestStore_KeyRouteSettings_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetKeyRouteSettings(context.Background(), "no-such-key")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_KeyRouteSettings_UpsertGetDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	settings := &KeyRouteSettings{
		KeyID:   "key-1",
		TeamID:  "team-1",
		Enabled: ptr(true),
		Levels:  []RouteLevel{{Name: "trivial", Model: "gpt-4o-mini"}},
	}
	require.NoError(t, s.UpsertKeyRouteSettings(ctx, settings))

	got, err := s.GetKeyRouteSettings(ctx, "key-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "team-1", got.TeamID)
	require.Len(t, got.Levels, 1)
	assert.Equal(t, "trivial", got.Levels[0].Name)

	require.NoError(t, s.DeleteKeyRouteSettings(ctx, "key-1"))
	got, err = s.GetKeyRouteSettings(ctx, "key-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_SetKeyFeedbackEnabled_CreatesThenUpdates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SetKeyFeedbackEnabled(ctx, "key-fb", true))
	got, err := s.GetKeyRouteSettings(ctx, "key-fb")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.FeedbackEnabled)
	assert.True(t, *got.FeedbackEnabled)

	require.NoError(t, s.SetKeyFeedbackEnabled(ctx, "key-fb", false))
	got, err = s.GetKeyRouteSettings(ctx, "key-fb")
	require.NoError(t, err)
	require.NotNil(t, got.FeedbackEnabled)
	assert.False(t, *got.FeedbackEnabled)
}

// === Feedback ===

func TestStore_Feedback_StoreAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.StoreFeedback(ctx, &FeedbackRecord{
		KeyHash: "hash-1", KeyPrefix: "sk-ab", TeamID: "team-1",
		TriggerType: "hivestate_compression", Answer: "A",
	}))
	require.NoError(t, s.StoreFeedback(ctx, &FeedbackRecord{
		KeyHash: "hash-2", KeyPrefix: "sk-cd", TeamID: "team-2",
		TriggerType: "long_session", Answer: "B",
	}))

	all, err := s.ListFeedback(ctx, "", 0)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	scoped, err := s.ListFeedback(ctx, "team-1", 0)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, "hash-1", scoped[0].KeyHash)
}

func TestStore_Feedback_GetFiredToday(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.StoreFeedback(ctx, &FeedbackRecord{
		KeyHash: "hash-today", TriggerType: "long_session", CreatedAt: time.Now(),
	}))
	require.NoError(t, s.StoreFeedback(ctx, &FeedbackRecord{
		KeyHash: "hash-today", TriggerType: "savings_milestone", CreatedAt: time.Now(),
	}))
	require.NoError(t, s.StoreFeedback(ctx, &FeedbackRecord{
		KeyHash: "hash-today", TriggerType: "old_one", CreatedAt: time.Now().AddDate(0, 0, -2),
	}))

	triggers, count, err := s.GetFeedbackFiredToday(ctx, "hash-today")
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.ElementsMatch(t, []string{"long_session", "savings_milestone"}, triggers)
}

func TestStore_GetFeedbackSessionSignals(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, s.LogHiveStateMetric(ctx, HiveStateMetric{
		KeyPrefix: "sk-sig", Mode: "state", Ratio: 0.3, RouteLevel: "standard",
		RouteSavedCost: 0.02, CreatedAt: now,
	}))
	require.NoError(t, s.LogHiveStateMetric(ctx, HiveStateMetric{
		KeyPrefix: "sk-sig", Mode: "state", Ratio: 0.5, RouteLevel: "complex",
		RouteSavedCost: 0.01, CreatedAt: now,
	}))
	// A "none" mode row should count toward RequestCount but not Ratios/RouteLevels.
	require.NoError(t, s.LogHiveStateMetric(ctx, HiveStateMetric{
		KeyPrefix: "sk-sig", Mode: "none", CreatedAt: now,
	}))

	signals, err := s.GetFeedbackSessionSignals(ctx, "sk-sig")
	require.NoError(t, err)
	require.NotNil(t, signals)
	assert.Equal(t, 3, signals.RequestCount)
	assert.Len(t, signals.Ratios, 2)
	assert.ElementsMatch(t, []string{"standard", "complex"}, signals.RouteLevels)
	assert.InDelta(t, 0.03, signals.TotalSavings, 0.0001)
}

// === Spend summary ===

func TestStore_GetSpendSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		Model: "gpt-4o", Provider: "openai", KeyPrefix: "sk-ab",
		TotalTokens: 100, Cost: 0.01, Duration: 50,
	}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		Model: "gpt-4o", Provider: "openai", KeyPrefix: "sk-ab",
		TotalTokens: 200, Cost: 0.02, Duration: 75,
	}))

	summary, err := s.GetSpendSummary(ctx, SpendFilter{})
	require.NoError(t, err)
	require.Len(t, summary, 1)
	assert.Equal(t, "gpt-4o", summary[0].Model)
	assert.Equal(t, 2, summary[0].Requests)
	assert.Equal(t, 300, summary[0].Tokens)
	assert.InDelta(t, 0.03, summary[0].Cost, 0.0001)
}

// === defaultDataDir ===

func TestDefaultDataDir_FromEnv(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", "/custom/ubiquum/home")
	assert.Equal(t, "/custom/ubiquum/home", defaultDataDir())
}

func TestDefaultDataDir_Default(t *testing.T) {
	t.Setenv("UBIQUUM_HOME", "")
	got := defaultDataDir()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".ubiquum"), got)
}

// === Usage capture context helpers ===

func TestUsageCapture_WithAndGet(t *testing.T) {
	uc := &UsageCapture{PromptTokens: 10}
	ctx := WithUsageCapture(context.Background(), uc)

	got := GetUsageCapture(ctx)
	require.NotNil(t, got)
	assert.Same(t, uc, got)
	assert.Equal(t, 10, got.PromptTokens)
}

func TestUsageCapture_GetWithoutContext(t *testing.T) {
	got := GetUsageCapture(context.Background())
	assert.Nil(t, got)
}

package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPurgeExpiredSpendRecords_KeyWithNoOverrideIsNeverTouched(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "no-override", KeyPrefix: "sk-n", Active: true}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "no-override", Model: "gpt-4o", Provider: "openai", Cost: 1,
		CreatedAt: time.Now().AddDate(0, 0, -365),
	}))

	deleted, err := s.PurgeExpiredSpendRecords(ctx)
	require.NoError(t, err)
	assert.Zero(t, deleted)

	records, err := s.GetSpend(ctx, SpendFilter{KeyHash: "no-override"})
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

func TestPurgeExpiredSpendRecords_DeletesOnlyRowsPastRetention(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "retained", KeyPrefix: "sk-r", Active: true}))
	require.NoError(t, s.UpsertKeyRouteSettings(ctx, &KeyRouteSettings{
		KeyID: "retained", TeamID: "team-1", LogRetentionDays: ptr(7),
	}))

	old := SpendRecord{KeyHash: "retained", Model: "gpt-4o", Provider: "openai", Cost: 1, CreatedAt: time.Now().AddDate(0, 0, -10)}
	recent := SpendRecord{KeyHash: "retained", Model: "gpt-4o", Provider: "openai", Cost: 2, CreatedAt: time.Now().AddDate(0, 0, -1)}
	require.NoError(t, s.LogSpend(ctx, old))
	require.NoError(t, s.LogSpend(ctx, recent))

	deleted, err := s.PurgeExpiredSpendRecords(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)

	records, err := s.GetSpend(ctx, SpendFilter{KeyHash: "retained"})
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.InDelta(t, 2.0, records[0].Cost, 1e-9)
}

func TestPurgeExpiredSpendRecords_FoldsDeletedRowsIntoDailySummary(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "folded", KeyPrefix: "sk-f", Active: true}))
	require.NoError(t, s.UpsertKeyRouteSettings(ctx, &KeyRouteSettings{
		KeyID: "folded", TeamID: "team-1", LogRetentionDays: ptr(1),
	}))

	day := time.Now().AddDate(0, 0, -30).UTC().Truncate(24 * time.Hour)
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "folded", KeyPrefix: "sk-f", TeamID: "team-1", Model: "gpt-4o", Provider: "openai",
		Cost: 1, TotalTokens: 100, Duration: 200, CreatedAt: day.Add(2 * time.Hour),
	}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "folded", KeyPrefix: "sk-f", TeamID: "team-1", Model: "gpt-4o", Provider: "openai",
		Cost: 3, TotalTokens: 300, Duration: 400, CreatedAt: day.Add(5 * time.Hour),
	}))

	deleted, err := s.PurgeExpiredSpendRecords(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)

	var summary SpendDailySummary
	require.NoError(t, s.db.Where("key_hash = ? AND model = ? AND provider = ?", "folded", "gpt-4o", "openai").First(&summary).Error)
	assert.Equal(t, 2, summary.Requests)
	assert.Equal(t, 400, summary.TotalTokens)
	assert.InDelta(t, 4.0, summary.TotalCost, 1e-9)
	assert.Equal(t, int64(600), summary.TotalDurationMs)
	assert.True(t, summary.Day.Equal(day))
}

func TestPurgeExpiredSpendRecords_AccumulatesAcrossMultipleRuns(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "multi", KeyPrefix: "sk-m", Active: true}))
	require.NoError(t, s.UpsertKeyRouteSettings(ctx, &KeyRouteSettings{
		KeyID: "multi", TeamID: "team-1", LogRetentionDays: ptr(1),
	}))

	day := time.Now().AddDate(0, 0, -10).UTC().Truncate(24 * time.Hour)
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "multi", Model: "gpt-4o", Provider: "openai", Cost: 1, CreatedAt: day.Add(time.Hour),
	}))
	_, err := s.PurgeExpiredSpendRecords(ctx)
	require.NoError(t, err)

	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "multi", Model: "gpt-4o", Provider: "openai", Cost: 2, CreatedAt: day.Add(2 * time.Hour),
	}))
	_, err = s.PurgeExpiredSpendRecords(ctx)
	require.NoError(t, err)

	var summary SpendDailySummary
	require.NoError(t, s.db.Where("key_hash = ? AND model = ? AND provider = ?", "multi", "gpt-4o", "openai").First(&summary).Error)
	assert.Equal(t, 2, summary.Requests)
	assert.InDelta(t, 3.0, summary.TotalCost, 1e-9)
}

func TestGetSpendSummary_MergesLiveAndAggregatedRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "merged", KeyPrefix: "sk-g", Active: true}))
	require.NoError(t, s.UpsertKeyRouteSettings(ctx, &KeyRouteSettings{
		KeyID: "merged", TeamID: "team-1", LogRetentionDays: ptr(1),
	}))

	old := time.Now().AddDate(0, 0, -10)
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "merged", KeyPrefix: "sk-g", TeamID: "team-1", Model: "gpt-4o", Provider: "openai",
		Cost: 1, TotalTokens: 100, Duration: 100, CreatedAt: old,
	}))
	_, err := s.PurgeExpiredSpendRecords(ctx)
	require.NoError(t, err)

	// A recent row for the same (model, provider, key_prefix) group stays in
	// gw_spend_records — the summary must add the two sources, not pick one.
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "merged", KeyPrefix: "sk-g", TeamID: "team-1", Model: "gpt-4o", Provider: "openai",
		Cost: 5, TotalTokens: 50, Duration: 50,
	}))

	results, err := s.GetSpendSummary(ctx, SpendFilter{KeyHash: "merged"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 2, results[0].Requests)
	assert.Equal(t, 150, results[0].Tokens)
	assert.InDelta(t, 6.0, results[0].Cost, 1e-9)
	assert.InDelta(t, 75.0, results[0].AvgMs, 1e-9)
}

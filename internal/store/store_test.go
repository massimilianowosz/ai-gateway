package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestStore opens a fresh in-file SQLite database, runs migrations, and
// registers cleanup. Each call gets an isolated database.
func openTestStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "test.db"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ptr[T any](v T) *T { return &v }

// === Keys ===

func TestStore_Key_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{
		KeyHash:   "hash-abc",
		KeyPrefix: "sk-ab",
		Name:      "test key",
		Active:    true,
	}
	require.NoError(t, s.CreateKey(ctx, key))
	assert.NotEmpty(t, key.ID)

	got, err := s.GetKeyByHash(ctx, "hash-abc")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "test key", got.Name)
	assert.Equal(t, "sk-ab", got.KeyPrefix)
	assert.True(t, got.Active)
}

func TestStore_Key_GetByHash_NotFound(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetKeyByHash(context.Background(), "nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// team_id, agent_id and created_by_user_id are uuid columns on the real
// Postgres table (api_keys_local), and a zero-value Go string is "" rather
// than NULL. sqlite — used by every other test here — stores that "" as text
// without complaint, so a plain create-and-check-for-an-error test would pass
// whether or not the fix is present. Only the SQL GORM actually sent proves
// it: this pins that CreateKey omits an unset column instead of writing ”.
func TestStore_Key_OmitsUnsetUUIDColumnsRatherThanSendingEmptyString(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	recorder := logger.Recorder.New()
	s.db = s.db.Session(&gorm.Session{Logger: recorder})

	err := s.CreateKey(context.Background(), &APIKey{
		KeyHash: "hash-omit", KeyPrefix: "sk-om", Active: true,
	})

	require.NoError(t, err)
	assert.NotContains(t, recorder.SQL, "agent_id", "an unset agent_id must not be sent as ''")
	assert.NotContains(t, recorder.SQL, "created_by_user_id", "an unset created_by_user_id must not be sent as ''")
	assert.NotContains(t, recorder.SQL, "team_id", "an unset team_id must not be sent as ''")
}

func TestStore_Key_SendsAgentIDWhenSet(t *testing.T) {
	s := openTestStore(t).(*GormStore)
	recorder := logger.Recorder.New()
	s.db = s.db.Session(&gorm.Session{Logger: recorder})

	err := s.CreateKey(context.Background(), &APIKey{
		KeyHash: "hash-agent", KeyPrefix: "sk-ag", Active: true,
		AgentID: "a1b2c3d4-0000-0000-0000-000000000000",
	})

	require.NoError(t, err)
	assert.Contains(t, recorder.SQL, "agent_id", "a set agent_id must still be written")
}

func TestStore_Key_UpdateFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-upd", KeyPrefix: "sk-up", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	require.NoError(t, s.UpdateKey(ctx, "hash-upd", UpdateKeyParams{
		Name:             ptr("updated name"),
		Budget:           ptr(50.0),
		RateLimit:        ptr(100),
		Models:           []string{"gpt-4o", "claude-3"},
		DeniedModels:     []string{"legacy-model"},
		AllowedProviders: []string{"openai", "azure"},
		DeniedProviders:  []string{"openai"},
		Active:           ptr(false),
	}))

	got, err := s.GetKeyByHash(ctx, "hash-upd")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "updated name", got.Name)
	assert.Equal(t, 50.0, got.Budget)
	assert.Equal(t, 100, got.RateLimit)
	assert.Equal(t, StringList{"gpt-4o", "claude-3"}, got.Models)
	assert.Equal(t, StringList{"legacy-model"}, got.DeniedModels)
	assert.Equal(t, StringList{"openai", "azure"}, got.AllowedProviders)
	assert.Equal(t, StringList{"openai"}, got.DeniedProviders)
	assert.False(t, got.Active)

	require.NoError(t, s.UpdateKey(ctx, "hash-upd", UpdateKeyParams{
		DeniedModels:    []string{},
		DeniedProviders: []string{},
	}))
	got, err = s.GetKeyByHash(ctx, "hash-upd")
	require.NoError(t, err)
	assert.Equal(t, StringList{}, got.DeniedModels)
	assert.Equal(t, StringList{}, got.DeniedProviders)
}

func TestStore_Key_UpdateExpiry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-exp", KeyPrefix: "sk-ex", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	future := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	require.NoError(t, s.UpdateKey(ctx, "hash-exp", UpdateKeyParams{
		ExpiresAt: &future,
	}))

	got, err := s.GetKeyByHash(ctx, "hash-exp")
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	assert.WithinDuration(t, future, *got.ExpiresAt, time.Second)
}

func TestStore_Key_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-del", KeyPrefix: "sk-de", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	require.NoError(t, s.DeleteKey(ctx, "hash-del"))

	got, err := s.GetKeyByHash(ctx, "hash-del")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_Key_Delete_NotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.DeleteKey(context.Background(), "nonexistent-hash")
	assert.ErrorIs(t, err, ErrKeyNotFound)
}

func TestStore_Key_ListByTeam(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i, hash := range []string{"h1", "h2", "h3"} {
		teamID := "team-a"
		if i == 2 {
			teamID = "team-b"
		}
		require.NoError(t, s.CreateKey(ctx, &APIKey{
			KeyHash:   hash,
			KeyPrefix: "sk-x",
			TeamID:    teamID,
			Active:    true,
		}))
	}

	keys, err := s.ListKeys(ctx, KeyFilter{TeamID: "team-a"})
	require.NoError(t, err)
	assert.Len(t, keys, 2)
}

func TestStore_Key_ListActiveFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "ha", KeyPrefix: "sk-a", Active: true}))
	// Create as active then deactivate — GORM ignores false zero-value on insert
	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "hb", KeyPrefix: "sk-b", Active: true}))
	require.NoError(t, s.UpdateKey(ctx, "hb", UpdateKeyParams{Active: ptr(false)}))

	active := true
	keys, err := s.ListKeys(ctx, KeyFilter{Active: &active})
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	assert.Equal(t, "ha", keys[0].KeyHash)
}

func TestStore_Key_ListPagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, s.CreateKey(ctx, &APIKey{
			KeyHash:   "hash-page-" + string(rune('a'+i)),
			KeyPrefix: "sk-p",
			Active:    true,
		}))
	}

	page1, err := s.ListKeys(ctx, KeyFilter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	assert.Len(t, page1, 2)

	page2, err := s.ListKeys(ctx, KeyFilter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	assert.Len(t, page2, 2)

	// Pages must not overlap
	assert.NotEqual(t, page1[0].KeyHash, page2[0].KeyHash)
}

// === Teams ===

func TestStore_Team_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	team := &Team{Name: "Engineering", Budget: 100.0}
	require.NoError(t, s.CreateTeam(ctx, team))
	assert.NotEmpty(t, team.ID)

	got, err := s.GetTeam(ctx, team.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "Engineering", got.Name)
	assert.Equal(t, 100.0, got.Budget)
}

func TestStore_Team_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetTeam(context.Background(), "team-missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_Team_Update(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	team := &Team{Name: "Original", Budget: 10.0}
	require.NoError(t, s.CreateTeam(ctx, team))

	require.NoError(t, s.UpdateTeam(ctx, team.ID, UpdateTeamParams{
		Name:   ptr("Renamed"),
		Budget: ptr(200.0),
	}))

	got, err := s.GetTeam(ctx, team.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.Name)
	assert.Equal(t, 200.0, got.Budget)
}

func TestStore_Team_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	team := &Team{Name: "ToDelete"}
	require.NoError(t, s.CreateTeam(ctx, team))
	require.NoError(t, s.DeleteTeam(ctx, team.ID))

	got, err := s.GetTeam(ctx, team.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_Team_ListWithPagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateTeam(ctx, &Team{Name: "TeamA"}))
	require.NoError(t, s.CreateTeam(ctx, &Team{Name: "TeamB"}))
	require.NoError(t, s.CreateTeam(ctx, &Team{Name: "TeamC"}))

	teams, err := s.ListTeams(ctx, TeamFilter{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, teams, 2)
}

// === Users ===

func TestStore_User_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	user := &User{Email: "alice@example.com", Name: "Alice", TeamID: "t1", Role: "admin", Active: true}
	require.NoError(t, s.CreateUser(ctx, user))
	assert.NotEmpty(t, user.ID)

	got, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "alice@example.com", got.Email)
	assert.Equal(t, "admin", got.Role)
}

func TestStore_User_GetByEmail(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	user := &User{Email: "bob@example.com", TeamID: "t1", Active: true}
	require.NoError(t, s.CreateUser(ctx, user))

	got, err := s.GetUserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, user.ID, got.ID)
}

func TestStore_User_GetByEmail_NotFound(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetUserByEmail(context.Background(), "nobody@example.com")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_User_Update(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	user := &User{Email: "carol@example.com", TeamID: "t1", Role: "member", Active: true}
	require.NoError(t, s.CreateUser(ctx, user))

	require.NoError(t, s.UpdateUser(ctx, user.ID, UpdateUserParams{
		Name:   ptr("Carol Updated"),
		Role:   ptr("admin"),
		Active: ptr(false),
	}))

	got, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "Carol Updated", got.Name)
	assert.Equal(t, "admin", got.Role)
	assert.False(t, got.Active)
}

func TestStore_User_ListByTeam(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &User{Email: "u1@x.com", TeamID: "team-x", Active: true}))
	require.NoError(t, s.CreateUser(ctx, &User{Email: "u2@x.com", TeamID: "team-x", Active: true}))
	require.NoError(t, s.CreateUser(ctx, &User{Email: "u3@y.com", TeamID: "team-y", Active: true}))

	users, err := s.ListUsers(ctx, UserFilter{TeamID: "team-x"})
	require.NoError(t, err)
	assert.Len(t, users, 2)
}

func TestStore_User_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	user := &User{Email: "del@example.com", TeamID: "t1", Active: true}
	require.NoError(t, s.CreateUser(ctx, user))
	require.NoError(t, s.DeleteUser(ctx, user.ID))

	got, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// === Spend ===

func TestStore_LogSpend_IncrementsKeySpend(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-spend", KeyPrefix: "sk-sp", Active: true, Budget: 10.0}
	require.NoError(t, s.CreateKey(ctx, key))

	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "hash-spend",
		Model:   "gpt-4o",
		Cost:    1.5,
	}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		KeyHash: "hash-spend",
		Model:   "gpt-4o",
		Cost:    2.0,
	}))

	got, err := s.GetKeyByHash(ctx, "hash-spend")
	require.NoError(t, err)
	assert.InDelta(t, 3.5, got.Spend, 1e-9)
}

func TestStore_LogSpend_IncrementsTeamSpend(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	team := &Team{Name: "SpendTeam", Budget: 50.0}
	require.NoError(t, s.CreateTeam(ctx, team))

	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		TeamID: team.ID,
		Model:  "claude-3",
		Cost:   5.0,
	}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		TeamID: team.ID,
		Model:  "claude-3",
		Cost:   3.0,
	}))

	got, err := s.GetTeam(ctx, team.ID)
	require.NoError(t, err)
	assert.InDelta(t, 8.0, got.Spend, 1e-9)
}

func TestStore_LogSpend_BudgetBlockedAfterAccumulation(t *testing.T) {
	// This is an integration test that validates the budget enforcement flow:
	// LogSpend increments key.Spend, and when spend >= budget, auth must block.
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{
		KeyHash:   "hash-budget",
		KeyPrefix: "sk-bg",
		Active:    true,
		Budget:    5.0,
		Spend:     0,
	}
	require.NoError(t, s.CreateKey(ctx, key))

	// Two calls totalling 5.0 — exactly at the budget limit
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hash-budget", Cost: 3.0}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hash-budget", Cost: 2.0}))

	updated, err := s.GetKeyByHash(ctx, "hash-budget")
	require.NoError(t, err)
	assert.InDelta(t, 5.0, updated.Spend, 1e-9)

	// Spend == Budget → auth should block (validated here at the data layer)
	assert.True(t, updated.Budget > 0 && updated.Spend >= updated.Budget,
		"key should be at budget limit after logging spend")
}

func TestStore_GetTotalSpend(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-total", KeyPrefix: "sk-to", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hash-total", Cost: 1.0}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hash-total", Cost: 0.5}))

	total, err := s.GetTotalSpend(ctx, "hash-total")
	require.NoError(t, err)
	assert.InDelta(t, 1.5, total, 1e-9)
}

func TestStore_GetTotalSpend_UnknownKey(t *testing.T) {
	s := openTestStore(t)
	total, err := s.GetTotalSpend(context.Background(), "unknown")
	require.NoError(t, err)
	assert.Equal(t, 0.0, total)
}

func TestStore_GetSpend_FilterByKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	keyA := &APIKey{KeyHash: "ha", KeyPrefix: "sk-a", Active: true}
	keyB := &APIKey{KeyHash: "hb", KeyPrefix: "sk-b", Active: true}
	require.NoError(t, s.CreateKey(ctx, keyA))
	require.NoError(t, s.CreateKey(ctx, keyB))

	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "ha", Model: "gpt-4o", Cost: 0.1}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "ha", Model: "gpt-4o", Cost: 0.2}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hb", Model: "gpt-4o", Cost: 0.5}))

	records, err := s.GetSpend(ctx, SpendFilter{KeyHash: "ha"})
	require.NoError(t, err)
	assert.Len(t, records, 2)
}

func TestStore_GetSpend_FilterByModel(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "h-model", KeyPrefix: "sk-m", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "h-model", Model: "gpt-4o", Cost: 0.1}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "h-model", Model: "claude-3", Cost: 0.2}))

	records, err := s.GetSpend(ctx, SpendFilter{Model: "gpt-4o"})
	require.NoError(t, err)
	assert.Len(t, records, 1)
	assert.Equal(t, "gpt-4o", records[0].Model)
}

func TestStore_GetSpend_FilterByDateRange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "h-date", KeyPrefix: "sk-d", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	past := time.Now().Add(-48 * time.Hour).UTC()
	now := time.Now().UTC()

	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		ID:        "old-record",
		KeyHash:   "h-date",
		Model:     "gpt-4o",
		Cost:      0.1,
		CreatedAt: past,
	}))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{
		ID:        "new-record",
		KeyHash:   "h-date",
		Model:     "gpt-4o",
		Cost:      0.2,
		CreatedAt: now,
	}))

	yesterday := time.Now().Add(-24 * time.Hour).UTC()
	records, err := s.GetSpend(ctx, SpendFilter{StartDate: &yesterday})
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "new-record", records[0].ID)
}

func TestStore_GetSpend_Pagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "h-page", KeyPrefix: "sk-pg", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	for i := 0; i < 5; i++ {
		require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "h-page", Model: "gpt-4o", Cost: 0.01}))
	}

	page, err := s.GetSpend(ctx, SpendFilter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	assert.Len(t, page, 2)
}

// === StringList serialization ===

func TestStringList_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	models := StringList{"gpt-4o", "claude-3-haiku", "gemini-1.5-pro"}
	key := &APIKey{
		KeyHash:   "hash-sl",
		KeyPrefix: "sk-sl",
		Active:    true,
		Models:    models,
	}
	require.NoError(t, s.CreateKey(ctx, key))

	got, err := s.GetKeyByHash(ctx, "hash-sl")
	require.NoError(t, err)
	assert.Equal(t, models, got.Models)
}

func TestStringList_EmptyModels(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-empty-sl", KeyPrefix: "sk-es", Active: true, Models: StringList{}}
	require.NoError(t, s.CreateKey(ctx, key))

	got, err := s.GetKeyByHash(ctx, "hash-empty-sl")
	require.NoError(t, err)
	// Empty list should not be nil after round-trip
	assert.NotNil(t, got.Models)
}

// === Unsupported driver ===

func TestOpen_UnsupportedDriver(t *testing.T) {
	_, err := Open(config.DatabaseConfig{Driver: "mysql"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported database driver")
}

// === SQL injection resistance (filters use parameterized GORM queries) ===

func TestStore_ListKeys_SQLInjectionAttemptInTeamIDFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "hi-1", KeyPrefix: "sk-i", TeamID: "team-a", Active: true}))
	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "hi-2", KeyPrefix: "sk-i", TeamID: "team-b", Active: true}))

	malicious := "team-a' OR '1'='1"
	keys, err := s.ListKeys(ctx, KeyFilter{TeamID: malicious})
	require.NoError(t, err, "malicious filter value must not break the parameterized query")
	assert.Empty(t, keys, "malicious filter should be treated as a literal string, matching nothing")

	// The table must remain intact — a classic injection would try to drop it.
	dropAttempt := "x'; DROP TABLE api_keys_local; --"
	keys, err = s.ListKeys(ctx, KeyFilter{TeamID: dropAttempt})
	require.NoError(t, err)
	assert.Empty(t, keys)

	// Confirm the table still exists and original data is untouched.
	all, err := s.ListKeys(ctx, KeyFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestStore_GetSpend_SQLInjectionAttemptInModelFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hi-spend", KeyPrefix: "sk-is", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hi-spend", Model: "gpt-4o", Cost: 1.0}))

	malicious := "gpt-4o' OR '1'='1"
	records, err := s.GetSpend(ctx, SpendFilter{Model: malicious})
	require.NoError(t, err)
	assert.Empty(t, records)

	// Original record must still be retrievable normally.
	records, err = s.GetSpend(ctx, SpendFilter{Model: "gpt-4o"})
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

func TestStore_CreateKey_SQLInjectionAttemptInNameField(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	maliciousName := "Robert'); DROP TABLE api_keys_local;--"
	key := &APIKey{KeyHash: "hi-name", KeyPrefix: "sk-in", Name: maliciousName, Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	got, err := s.GetKeyByHash(ctx, "hi-name")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, maliciousName, got.Name, "malicious string should be stored verbatim, not executed")

	// Table must still exist and be queryable.
	all, err := s.ListKeys(ctx, KeyFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, all)
}

// === Concurrent writes ===

func TestStore_ConcurrentLogSpend_AccumulatesCorrectly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-concurrent", KeyPrefix: "sk-cc", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- s.LogSpend(ctx, SpendRecord{KeyHash: "hash-concurrent", Model: "gpt-4o", Cost: 1.0})
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	got, err := s.GetKeyByHash(ctx, "hash-concurrent")
	require.NoError(t, err)
	assert.InDelta(t, float64(n), got.Spend, 1e-6, "concurrent spend increments must not be lost to races")

	records, err := s.GetSpend(ctx, SpendFilter{KeyHash: "hash-concurrent"})
	require.NoError(t, err)
	assert.Len(t, records, n)
}

func TestStore_ConcurrentCreateKey_AllSucceedWithUniqueHashes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const n = 30
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- s.CreateKey(ctx, &APIKey{
				KeyHash:   fmt.Sprintf("concurrent-hash-%d", i),
				KeyPrefix: "sk-cc",
				Active:    true,
			})
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	all, err := s.ListKeys(ctx, KeyFilter{})
	require.NoError(t, err)
	assert.Len(t, all, n)
}

// === Pagination edge cases ===

func TestStore_ListKeys_ZeroLimitReturnsAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: fmt.Sprintf("zl-%d", i), KeyPrefix: "sk-zl", Active: true}))
	}

	keys, err := s.ListKeys(ctx, KeyFilter{Limit: 0})
	require.NoError(t, err)
	assert.Len(t, keys, 4)
}

func TestStore_ListKeys_NegativeOffsetIgnored(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: fmt.Sprintf("no-%d", i), KeyPrefix: "sk-no", Active: true}))
	}

	keys, err := s.ListKeys(ctx, KeyFilter{Offset: -5})
	require.NoError(t, err)
	assert.Len(t, keys, 3, "negative offset should be ignored, not error or panic")
}

func TestStore_ListKeys_OffsetBeyondTotalReturnsEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "ob-1", KeyPrefix: "sk-ob", Active: true}))

	keys, err := s.ListKeys(ctx, KeyFilter{Offset: 100})
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestStore_GetSpend_LimitExceedsAvailableRecords(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	key := &APIKey{KeyHash: "hash-limitex", KeyPrefix: "sk-lx", Active: true}
	require.NoError(t, s.CreateKey(ctx, key))
	require.NoError(t, s.LogSpend(ctx, SpendRecord{KeyHash: "hash-limitex", Model: "gpt-4o", Cost: 0.1}))

	records, err := s.GetSpend(ctx, SpendFilter{Limit: 1000})
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

// === Migrate idempotence ===

func TestStore_Migrate_IsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Migrate was already called once in openTestStore; calling it again
	// must not error and must not destroy existing data.
	require.NoError(t, s.CreateKey(ctx, &APIKey{KeyHash: "migrate-check", KeyPrefix: "sk-mg", Active: true}))

	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.Migrate(ctx))

	got, err := s.GetKeyByHash(ctx, "migrate-check")
	require.NoError(t, err)
	require.NotNil(t, got, "data must survive repeated Migrate calls")
	assert.Equal(t, "sk-mg", got.KeyPrefix)
}

func TestGovernanceReadinessAndRouteRevisionBackfill(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.CreateKey(ctx, &APIKey{
		KeyHash: "ready-governed", KeyPrefix: "sk-ready", Active: true, GovernanceRevision: 7,
	}))
	require.NoError(t, s.UpsertKeyRouteSettings(ctx, &KeyRouteSettings{
		KeyID: "ready-governed", TeamID: "team-ready", GovernanceRevision: 0,
	}))

	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.(*GormStore).GovernanceReadiness(ctx))
	route, err := s.GetKeyRouteSettings(ctx, "ready-governed")
	require.NoError(t, err)
	require.NotNil(t, route)
	assert.Equal(t, int64(7), route.GovernanceRevision)
}

func TestSpendRecord_ApplyIdentity(t *testing.T) {
	// Thirteen handlers used to copy these fields by hand, which is why a
	// spend row named the key and nothing else for so long.
	var record SpendRecord
	record.ApplyIdentity(&APIKey{
		KeyHash:         "hash",
		KeyPrefix:       "sk-ubq-12",
		TeamID:          "team-1",
		AgentID:         "agent-1",
		CreatedByUserID: "user-1",
	})

	if record.AgentID != "agent-1" || record.UserID != "user-1" {
		t.Fatalf("identity not copied: %+v", record)
	}
	if record.KeyPrefix != "sk-ubq-12" || record.TeamID != "team-1" {
		t.Fatalf("existing fields regressed: %+v", record)
	}
}

func TestSpendRecord_ApplyIdentity_NilKeyIsANoOp(t *testing.T) {
	record := SpendRecord{KeyPrefix: "kept"}
	record.ApplyIdentity(nil)
	if record.KeyPrefix != "kept" {
		t.Fatalf("a nil key must not wipe the row: %+v", record)
	}
}

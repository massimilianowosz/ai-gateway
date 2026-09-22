package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// --- SetConfig ---

func TestSetConfig(t *testing.T) {
	h, _ := newHandler(t)
	cfg := &config.Config{}
	h.SetConfig(cfg)
	assert.Same(t, cfg, h.cfg)
}

// --- keyPrefixes ---

func TestKeyPrefixes(t *testing.T) {
	keys := []store.APIKey{{KeyPrefix: "sk-aa"}, {KeyPrefix: "sk-bb"}}
	assert.Equal(t, "sk-aa, sk-bb", keyPrefixes(keys))
}

func TestKeyPrefixes_Empty(t *testing.T) {
	assert.Equal(t, "", keyPrefixes(nil))
}

// --- ListFeedback ---

func TestListFeedback_Empty(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.ListFeedback, http.MethodGet, "/v1/feedback", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"feedback":[]`)
}

func TestListFeedback_WithRecordsAndTeamFilter(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()
	require.NoError(t, s.StoreFeedback(ctx, &store.FeedbackRecord{
		KeyHash: "hash-1", TeamID: "team-a", TriggerType: "long_session",
	}))
	require.NoError(t, s.StoreFeedback(ctx, &store.FeedbackRecord{
		KeyHash: "hash-2", TeamID: "team-b", TriggerType: "long_session",
	}))

	rr := doRequest(h.ListFeedback, http.MethodGet, "/v1/feedback?team_id=team-a", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	// KeyHash is json:"-" (never serialized); assert on team_id instead.
	assert.Contains(t, rr.Body.String(), `"team_id":"team-a"`)
	assert.NotContains(t, rr.Body.String(), "team-b")
}

func TestListFeedback_LimitClampedWhenInvalid(t *testing.T) {
	h, _ := newHandler(t)
	// A non-numeric or out-of-range limit should be ignored, falling back to
	// the default rather than erroring.
	rr := doRequest(h.ListFeedback, http.MethodGet, "/v1/feedback?limit=abc", nil)
	assert.Equal(t, http.StatusOK, rr.Code)

	rr = doRequest(h.ListFeedback, http.MethodGet, "/v1/feedback?limit=99999", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
}

// --- GetSpendRecords ---

func TestGetSpendRecords_Empty(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetSpendRecords, http.MethodGet, "/v1/spend/records", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"page":1`)
	assert.Contains(t, rr.Body.String(), `"page_size":100`)
}

func TestGetSpendRecords_WithFiltersAndPagination(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()
	require.NoError(t, s.LogSpend(ctx, store.SpendRecord{Model: "gpt-4o", TeamID: "team-1", Cost: 0.01}))

	rr := doRequest(h.GetSpendRecords, http.MethodGet,
		"/v1/spend/records?model=gpt-4o&team_id=team-1&start_date=2020-01-01&end_date=2099-01-01&page=1&page_size=10", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "gpt-4o")
}

func TestGetSpendRecords_InvalidStartDate(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetSpendRecords, http.MethodGet, "/v1/spend/records?start_date=not-a-date", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "start_date")
}

func TestGetSpendRecords_InvalidEndDate(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetSpendRecords, http.MethodGet, "/v1/spend/records?end_date=not-a-date", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "end_date")
}

func TestGetSpendRecords_AmbiguousKeyIdentifier(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()
	// Two keys sharing the same Name make "duplicate-name" resolve ambiguously.
	require.NoError(t, s.CreateKey(ctx, &store.APIKey{KeyHash: "h1", KeyPrefix: "sk-aa", Name: "duplicate-name"}))
	require.NoError(t, s.CreateKey(ctx, &store.APIKey{KeyHash: "h2", KeyPrefix: "sk-bb", Name: "duplicate-name"}))

	rr := doRequest(h.GetSpendRecords, http.MethodGet, "/v1/spend/records?key=duplicate-name", nil)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "sk-aa")
	assert.Contains(t, rr.Body.String(), "sk-bb")
}

func TestGetSpendRecords_KeyNotFound(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetSpendRecords, http.MethodGet, "/v1/spend/records?key=nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// --- Route settings (team-level) ---

func TestGetRouteSettings_MissingTeamID(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetRouteSettings_TeamDefaultsWhenNoSettingsSaved(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-x", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"team_id":"team-x"`)
	assert.Contains(t, rr.Body.String(), `"enabled":null`)
}

func TestUpdateThenGetRouteSettings_TeamLevel(t *testing.T) {
	h, _ := newHandler(t)

	updateRR := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id": "team-y",
		"enabled": true,
		"levels": []map[string]string{
			{"name": "trivial", "model": "gpt-4o-mini"},
		},
	}))
	assert.Equal(t, http.StatusOK, updateRR.Code)
	assert.Contains(t, updateRR.Body.String(), `"enabled":true`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-y", nil)
	assert.Equal(t, http.StatusOK, getRR.Code)
	assert.Contains(t, getRR.Body.String(), `"enabled":true`)
	assert.Contains(t, getRR.Body.String(), "trivial")
}

func TestUpdateRouteSettings_MissingTeamID(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestUpdateRouteSettings_InvalidJSON(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", badJSON())
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// --- Route settings (per-key) ---

func TestUpdateThenGetRouteSettings_KeyLevel(t *testing.T) {
	h, _ := newHandler(t)

	updateRR := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":          "team-z",
		"key_id":           "key-1",
		"enabled":          true,
		"feedback_enabled": true,
	}))
	assert.Equal(t, http.StatusOK, updateRR.Code)
	assert.Contains(t, updateRR.Body.String(), `"key_id":"key-1"`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-z&key_id=key-1", nil)
	assert.Equal(t, http.StatusOK, getRR.Code)
	assert.Contains(t, getRR.Body.String(), `"key_id":"key-1"`)
	assert.Contains(t, getRR.Body.String(), `"enabled":true`)
}

func TestUpdateRouteSettings_GovernedKeyRequiresBreakGlass(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{
		KeyHash: "governed-route-hash", KeyPrefix: "sk-govroute", TeamID: "team-governed",
		Active: true, GovernanceRevision: 1,
	}
	require.NoError(t, database.CreateKey(context.Background(), key))
	body := map[string]any{"team_id": key.TeamID, "key_id": key.KeyHash, "enabled": true}

	blocked := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(body))
	assert.Equal(t, http.StatusConflict, blocked.Code)

	req := httptest.NewRequest(http.MethodPost, "/v1/route-settings", jsonBody(body))
	req.Header.Set(governanceBreakGlassHeader, "acknowledged")
	req.Header.Set(governanceBreakGlassReason, "restore service during incident")
	rr := httptest.NewRecorder()
	h.UpdateRouteSettings(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestGetRouteSettings_KeyLevelNoSettingsSaved(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-z&key_id=nope", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"key_id":"nope"`)
	assert.Contains(t, rr.Body.String(), `"enabled":null`)
}

func TestUpdateRouteSettings_KeyLevelReset(t *testing.T) {
	h, _ := newHandler(t)

	// First save something for the key.
	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id": "team-reset",
		"key_id":  "key-reset",
		"enabled": true,
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	// reset_all triggers the reset/delete branch.
	rr = doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":   "team-reset",
		"key_id":    "key-reset",
		"reset_all": true,
	}))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"enabled":null`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-reset&key_id=key-reset", nil)
	assert.Contains(t, getRR.Body.String(), `"enabled":null`)
}

func TestUpdateRouteSettings_EnabledNilAndEmptyLevelsWithoutResetAllDoesNotDeleteTheRow(t *testing.T) {
	// This is exactly the payload policy_gateway_sync sends on every push when
	// no model_routing policy applies (enabled: nil, levels: []) alongside a
	// cache/compression override — it must set that override, never fall into
	// the full-row reset that used to be inferred from these same values.
	h, _ := newHandler(t)

	cacheEnabled := false
	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":       "team-sync",
		"key_id":        "key-sync",
		"enabled":       nil,
		"levels":        []store.RouteLevel{},
		"cache_enabled": cacheEnabled,
	}))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"cache_enabled":false`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-sync&key_id=key-sync", nil)
	assert.Contains(t, getRR.Body.String(), `"cache_enabled":false`,
		"the row must exist with the cache override applied, not have been deleted")
}

func TestUpdateThenGetRouteSettings_KeyLevelCacheAndCompressionOverride(t *testing.T) {
	h, _ := newHandler(t)

	updateRR := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":             "team-cc",
		"key_id":              "key-cc",
		"cache_enabled":       false,
		"compression_enabled": false,
	}))
	assert.Equal(t, http.StatusOK, updateRR.Code)
	assert.Contains(t, updateRR.Body.String(), `"cache_enabled":false`)
	assert.Contains(t, updateRR.Body.String(), `"compression_enabled":false`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-cc&key_id=key-cc", nil)
	assert.Equal(t, http.StatusOK, getRR.Code)
	assert.Contains(t, getRR.Body.String(), `"cache_enabled":false`)
	assert.Contains(t, getRR.Body.String(), `"compression_enabled":false`)
	assert.Contains(t, getRR.Body.String(), `"team_cache_enabled":true`,
		"a fresh team with no settings row defaults its cache toggle to true")
}

func TestUpdateRouteSettings_KeyLevelCacheOverrideReset(t *testing.T) {
	h, _ := newHandler(t)

	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":       "team-cc-reset",
		"key_id":        "key-cc-reset",
		"cache_enabled": false,
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	rr = doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":             "team-cc-reset",
		"key_id":              "key-cc-reset",
		"reset_cache_enabled": true,
	}))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"cache_enabled":null`)
}

func TestUpdateThenGetRouteSettings_KeyLevelThresholdAndCompressionKnobs(t *testing.T) {
	h, _ := newHandler(t)

	updateRR := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":                       "team-knobs",
		"key_id":                        "key-knobs",
		"cache_direct_threshold":        0.5,
		"cache_reuse_threshold":         0.4,
		"cache_tweak_threshold":         0.3,
		"compression_threshold":         100,
		"compression_token_budget":      2000,
		"compression_step_window":       2,
		"compression_append_only_state": true,
	}))
	assert.Equal(t, http.StatusOK, updateRR.Code)
	assert.Contains(t, updateRR.Body.String(), `"cache_direct_threshold":0.5`)
	assert.Contains(t, updateRR.Body.String(), `"compression_token_budget":2000`)
	assert.Contains(t, updateRR.Body.String(), `"compression_append_only_state":true`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-knobs&key_id=key-knobs", nil)
	assert.Equal(t, http.StatusOK, getRR.Code)
	assert.Contains(t, getRR.Body.String(), `"cache_reuse_threshold":0.4`)
	assert.Contains(t, getRR.Body.String(), `"compression_step_window":2`)
}

func TestUpdateRouteSettings_ThresholdAndCompressionKnobsGroupReset(t *testing.T) {
	h, _ := newHandler(t)

	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":                "team-knobs-reset",
		"key_id":                 "key-knobs-reset",
		"cache_direct_threshold": 0.5,
		"compression_threshold":  100,
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	rr = doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":                     "team-knobs-reset",
		"key_id":                      "key-knobs-reset",
		"reset_cache_thresholds":      true,
		"reset_compression_overrides": true,
	}))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"cache_direct_threshold":null`)
	assert.Contains(t, rr.Body.String(), `"compression_threshold":null`)
}

func TestUpdateThenGetRouteSettings_KeyLevelGuardrailOverride(t *testing.T) {
	h, _ := newHandler(t)

	updateRR := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":                 "team-guard",
		"key_id":                  "key-guard",
		"required_guardrails":     []string{"injection", "pii"},
		"guardrail_non_derogable": true,
	}))
	assert.Equal(t, http.StatusOK, updateRR.Code)
	assert.Contains(t, updateRR.Body.String(), `"required_guardrails":["injection","pii"]`)
	assert.Contains(t, updateRR.Body.String(), `"non_derogable":true`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-guard&key_id=key-guard", nil)
	assert.Equal(t, http.StatusOK, getRR.Code)
	assert.Contains(t, getRR.Body.String(), `"required_guardrails":["injection","pii"]`)
}

func TestUpdateRouteSettings_GuardrailOverrideReset(t *testing.T) {
	h, _ := newHandler(t)

	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":             "team-guard-reset",
		"key_id":              "key-guard-reset",
		"required_guardrails": []string{"injection"},
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	rr = doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":                  "team-guard-reset",
		"key_id":                   "key-guard-reset",
		"reset_guardrail_override": true,
	}))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"guardrail_override":null`)
}

func TestUpdateThenGetRouteSettings_KeyLevelLogRetentionDays(t *testing.T) {
	h, _ := newHandler(t)

	updateRR := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":            "team-retention",
		"key_id":             "key-retention",
		"log_retention_days": 30,
	}))
	assert.Equal(t, http.StatusOK, updateRR.Code)
	assert.Contains(t, updateRR.Body.String(), `"log_retention_days":30`)

	getRR := doRequest(h.GetRouteSettings, http.MethodGet, "/v1/route-settings?team_id=team-retention&key_id=key-retention", nil)
	assert.Equal(t, http.StatusOK, getRR.Code)
	assert.Contains(t, getRR.Body.String(), `"log_retention_days":30`)
}

func TestUpdateRouteSettings_LogRetentionDaysReset(t *testing.T) {
	h, _ := newHandler(t)

	rr := doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":            "team-retention-reset",
		"key_id":             "key-retention-reset",
		"log_retention_days": 30,
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	rr = doRequest(h.UpdateRouteSettings, http.MethodPost, "/v1/route-settings", jsonBody(map[string]any{
		"team_id":                  "team-retention-reset",
		"key_id":                   "key-retention-reset",
		"reset_log_retention_days": true,
	}))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"log_retention_days":null`)
}

// --- Users ---

func TestUpdateUser(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: "user-1", Email: "a@example.com", TeamID: "team-1"}))

	rr := doRequest(h.UpdateUser, http.MethodPost, "/v1/users/update", jsonBody(map[string]any{
		"id": "user-1",
		"params": map[string]any{
			"name": "New Name",
			"role": "admin",
		},
	}))
	assert.Equal(t, http.StatusOK, rr.Code)

	got, err := s.GetUser(ctx, "user-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "New Name", got.Name)
	assert.Equal(t, "admin", got.Role)
}

func TestUpdateUser_MissingID(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateUser, http.MethodPost, "/v1/users/update", jsonBody(map[string]any{}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestUpdateUser_InvalidJSON(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateUser, http.MethodPost, "/v1/users/update", badJSON())
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestDeleteUser(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: "user-del", Email: "b@example.com", TeamID: "team-1"}))

	rr := doRequest(h.DeleteUser, http.MethodPost, "/v1/users/delete", jsonBody(map[string]any{"id": "user-del"}))
	assert.Equal(t, http.StatusOK, rr.Code)

	got, err := s.GetUser(ctx, "user-del")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDeleteUser_MissingID(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.DeleteUser, http.MethodPost, "/v1/users/delete", jsonBody(map[string]any{}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestDeleteUser_InvalidJSON(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.DeleteUser, http.MethodPost, "/v1/users/delete", badJSON())
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// sanity check that doRequest/newHandler still behave as expected for a
// plain httptest.NewRecorder without going through httptest.NewServer.
func TestDoRequestHelper_SmokeTest(t *testing.T) {
	h, _ := newHandler(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/feedback", nil)
	h.ListFeedback(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

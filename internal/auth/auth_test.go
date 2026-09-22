package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// mockStore is a minimal in-memory store stub for auth tests.
type mockStore struct {
	key           *store.APIKey
	team          *store.Team
	routeSettings *store.KeyRouteSettings
	err           error
}

func (m *mockStore) CreateKey(_ context.Context, _ *store.APIKey) error { return m.err }
func (m *mockStore) GetKeyByHash(_ context.Context, _ string) (*store.APIKey, error) {
	return m.key, m.err
}
func (m *mockStore) GetKeyGovernanceState(_ context.Context, _ string) (*store.APIKey, *store.KeyRouteSettings, error) {
	return m.key, m.routeSettings, m.err
}
func (m *mockStore) ListKeys(_ context.Context, _ store.KeyFilter) ([]store.APIKey, error) {
	return nil, m.err
}
func (m *mockStore) UpdateKey(_ context.Context, _ string, _ store.UpdateKeyParams) error {
	return m.err
}
func (m *mockStore) DeleteKey(_ context.Context, _ string) error { return m.err }

func (m *mockStore) CreateTeam(_ context.Context, _ *store.Team) error { return m.err }
func (m *mockStore) GetTeam(_ context.Context, _ string) (*store.Team, error) {
	return m.team, m.err
}
func (m *mockStore) ListTeams(_ context.Context, _ store.TeamFilter) ([]store.Team, error) {
	return nil, m.err
}
func (m *mockStore) UpdateTeam(_ context.Context, _ string, _ store.UpdateTeamParams) error {
	return m.err
}
func (m *mockStore) DeleteTeam(_ context.Context, _ string) error { return m.err }

func (m *mockStore) CreateUser(_ context.Context, _ *store.User) error { return m.err }
func (m *mockStore) GetUser(_ context.Context, _ string) (*store.User, error) {
	return nil, m.err
}
func (m *mockStore) GetUserByEmail(_ context.Context, _ string) (*store.User, error) {
	return nil, m.err
}
func (m *mockStore) ListUsers(_ context.Context, _ store.UserFilter) ([]store.User, error) {
	return nil, m.err
}
func (m *mockStore) UpdateUser(_ context.Context, _ string, _ store.UpdateUserParams) error {
	return m.err
}
func (m *mockStore) DeleteUser(_ context.Context, _ string) error { return m.err }

func (m *mockStore) LogSpend(_ context.Context, _ store.SpendRecord) error { return m.err }
func (m *mockStore) GetSpend(_ context.Context, _ store.SpendFilter) ([]store.SpendRecord, error) {
	return nil, m.err
}
func (m *mockStore) GetTotalSpend(_ context.Context, _ string) (float64, error) {
	return 0, m.err
}
func (m *mockStore) PurgeExpiredSpendRecords(_ context.Context) (int64, error) {
	return 0, m.err
}
func (m *mockStore) LogCacheMetric(_ context.Context, _ store.CacheMetric) error { return m.err }
func (m *mockStore) ListCacheMetrics(_ context.Context, _ int) ([]store.CacheMetric, error) {
	return nil, m.err
}
func (m *mockStore) DeleteCacheMetrics(_ context.Context) error { return m.err }
func (m *mockStore) LogHiveStateMetric(_ context.Context, _ store.HiveStateMetric) error {
	return m.err
}
func (m *mockStore) ListHiveStateMetrics(_ context.Context, _ int) ([]store.HiveStateMetric, error) {
	return nil, m.err
}
func (m *mockStore) GetSpendSummary(_ context.Context, _ store.SpendFilter) ([]store.SpendSummary, error) {
	return nil, m.err
}
func (m *mockStore) LogKeyEvent(_ context.Context, _ store.KeyEvent) error { return nil }

func (m *mockStore) GetTenantSettings(_ context.Context, _ string) (*store.TenantSettings, error) {
	return nil, m.err
}
func (m *mockStore) UpsertTenantSettings(_ context.Context, _ *store.TenantSettings) error {
	return m.err
}
func (m *mockStore) GetKeyRouteSettings(_ context.Context, _ string) (*store.KeyRouteSettings, error) {
	return m.routeSettings, m.err
}
func (m *mockStore) UpsertKeyRouteSettings(_ context.Context, _ *store.KeyRouteSettings) error {
	return m.err
}
func (m *mockStore) DeleteKeyRouteSettings(_ context.Context, _ string) error { return m.err }
func (m *mockStore) StoreFeedback(_ context.Context, _ *store.FeedbackRecord) error {
	return m.err
}
func (m *mockStore) ListFeedback(_ context.Context, _ string, _ int) ([]store.FeedbackRecord, error) {
	return nil, m.err
}
func (m *mockStore) GetFeedbackFiredToday(_ context.Context, _ string) ([]string, int, error) {
	return nil, 0, m.err
}
func (m *mockStore) SetKeyFeedbackEnabled(_ context.Context, _ string, _ bool) error {
	return m.err
}
func (m *mockStore) GetFeedbackSessionSignals(_ context.Context, _ string) (*store.FeedbackSessionSignals, error) {
	return nil, nil
}
func (m *mockStore) Migrate(_ context.Context) error { return m.err }
func (m *mockStore) Close() error                    { return nil }

// activeKey returns a basic active virtual key with no budget/expiry restrictions.
func activeKey() *store.APIKey {
	return &store.APIKey{
		ID:        "key-1",
		KeyHash:   HashKey("sk-virtual"),
		KeyPrefix: "sk-vi",
		Name:      "test key",
		Active:    true,
		Budget:    100.0, // default budget for tests
	}
}

func TestAuthenticate_MasterKey(t *testing.T) {
	m := NewMiddleware("sk-master", nil)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := KeyFromContext(r.Context())
		assert.Equal(t, "sk-master", key)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-master")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_MissingHeader(t *testing.T) {
	m := NewMiddleware("sk-master", nil)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "missing Authorization header")
}

func TestAuthenticate_InvalidKey(t *testing.T) {
	m := NewMiddleware("sk-master", nil)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid API key")
}

func TestAuthenticate_MalformedHeader(t *testing.T) {
	m := NewMiddleware("sk-master", nil)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	tests := []struct {
		name  string
		value string
	}{
		{"no bearer prefix", "sk-master"},
		{"wrong prefix", "Basic sk-master"},
		{"empty value", "Bearer "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			req.Header.Set("Authorization", tt.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestUpstreamTokenForwarding_NoHeaderIsNoOp(t *testing.T) {
	mw := NewUpstreamTokenForwardingMiddleware(UpstreamTokenForwardingOptions{})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Empty(t, UpstreamTokenForProvider(r.Context(), "github_copilot"))
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, called)
}

func TestUpstreamTokenForwarding_DisabledRejectsAndStripsHeader(t *testing.T) {
	mw := NewUpstreamTokenForwardingMiddleware(UpstreamTokenForwardingOptions{})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set(defaultUpstreamTokenHeader, "Bearer tid_test")
	req.Header.Set(upstreamProviderHeader, "github_copilot")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, req.Header.Get(defaultUpstreamTokenHeader))
	assert.Contains(t, rec.Body.String(), "upstream token forwarding is disabled")
}

func TestUpstreamTokenForwarding_EnabledScopesTokenToAllowedProvider(t *testing.T) {
	mw := NewUpstreamTokenForwardingMiddleware(UpstreamTokenForwardingOptions{
		Enabled:          true,
		AllowedProviders: []string{"github_copilot"},
	})
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.Equal(t, "tid_test", UpstreamTokenForProvider(r.Context(), "github_copilot"))
		assert.Empty(t, UpstreamTokenForProvider(r.Context(), "openai_compatible"))
		assert.Empty(t, r.Header.Get(defaultUpstreamTokenHeader))
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set(defaultUpstreamTokenHeader, "Bearer tid_test")
	req.Header.Set(upstreamProviderHeader, "github_copilot")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, called)
}

func TestUpstreamTokenForwarding_RejectsDisallowedProvider(t *testing.T) {
	mw := NewUpstreamTokenForwardingMiddleware(UpstreamTokenForwardingOptions{
		Enabled:          true,
		AllowedProviders: []string{"github_copilot"},
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set(defaultUpstreamTokenHeader, "Bearer tid_test")
	req.Header.Set(upstreamProviderHeader, "openai_compatible")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "upstream provider is not allowed")
}

func TestUpstreamTokenForwarding_RequiresBearerToken(t *testing.T) {
	mw := NewUpstreamTokenForwardingMiddleware(UpstreamTokenForwardingOptions{
		Enabled:          true,
		AllowedProviders: []string{"github_copilot"},
	})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set(defaultUpstreamTokenHeader, "tid_test")
	req.Header.Set(upstreamProviderHeader, "github_copilot")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Bearer token")
}

// --- Virtual key tests ---

func TestAuthenticate_VirtualKey_Valid(t *testing.T) {
	key := activeKey()
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)

	called := false
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		info := KeyInfoFromContext(r.Context())
		require.NotNil(t, info)
		assert.Equal(t, "key-1", info.ID)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, called)
}

func TestAuthenticate_VirtualKey_NotFound(t *testing.T) {
	ms := &mockStore{key: nil}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-unknown")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid API key")
}

func TestAuthenticate_VirtualKey_Inactive(t *testing.T) {
	key := activeKey()
	key.Active = false
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "disabled")
}

func TestAuthenticate_GovernanceBlocked_RejectsWithReason(t *testing.T) {
	key := activeKey()
	key.GovernanceBlocked = true
	key.GovernanceBlockReason = "invalid_enforcing_composition:model_routing"
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "model_routing")
	assert.Contains(t, rec.Body.String(), "governance_blocked")
}

func TestAuthenticate_GovernanceBlocked_TakesEffectRegardlessOfBudget(t *testing.T) {
	// A key with ample budget (the flat/BYOK-style exemption CheckBudget
	// would otherwise grant later) must still be refused here — the
	// governance gate runs at Authenticate, before CheckBudget ever sees
	// the request, so it cannot be bypassed by any budget/payment state.
	key := activeKey()
	key.Budget = 0 // BudgetUnfunded on its own would not explain this rejection
	key.GovernanceBlocked = true
	key.GovernanceBlockReason = ""
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "governance_blocked")
	// An empty reason still produces a stated message, never a blank one.
	assert.Contains(t, rec.Body.String(), "policy composition invalid")
}

func TestAuthenticate_NotGovernanceBlocked_IsUnaffected(t *testing.T) {
	key := activeKey()
	key.GovernanceBlocked = false
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_GovernedScopeWithoutSnapshotFailsClosed(t *testing.T) {
	key := activeKey()
	key.GovernanceRequired = true
	key.GovernanceRevision = 0
	ms := &mockStore{key: key}
	handler := NewMiddleware("sk-master", ms).Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a governed scope without a snapshot must not reach the handler")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "governance_not_ready")
}

func TestAuthenticate_LegacyScopeWithoutSnapshotRemainsCompatible(t *testing.T) {
	key := activeKey()
	key.GovernanceRequired = false
	key.GovernanceRevision = 0
	ms := &mockStore{key: key}
	handler := NewMiddleware("sk-master", ms).Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_RejectsMismatchedGovernanceProjectionRevision(t *testing.T) {
	key := activeKey()
	key.GovernanceRevision = 4
	ms := &mockStore{key: key, routeSettings: &store.KeyRouteSettings{GovernanceRevision: 3}}
	handler := NewMiddleware("sk-master", ms).Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not be called with a mixed governance revision")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "retry")
}

func TestAuthenticate_CapturesMatchingRouteProjectionForTheWholeRequest(t *testing.T) {
	key := activeKey()
	key.GovernanceRevision = 5
	cacheDisabled := false
	route := &store.KeyRouteSettings{GovernanceRevision: 5, CacheEnabled: &cacheDisabled}
	ms := &mockStore{key: key, routeSettings: route}
	var captured *store.KeyRouteSettings
	handler := NewMiddleware("sk-master", ms).Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = KeyRouteSettingsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Same(t, route, captured)
}

func TestRecheckGovernance_StopsARevocationBeforeTheExternalHandler(t *testing.T) {
	admitted := activeKey()
	admitted.GovernanceRevision = 2
	admitted.GovernanceDigest = "old"
	current := *admitted
	current.GovernanceBlocked = true
	ms := &mockStore{key: &current, routeSettings: &store.KeyRouteSettings{GovernanceRevision: 2}}
	handler := NewMiddleware("sk-master", ms).RecheckGovernance(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("external handler must not run after revocation")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(ContextWithKeyInfo(req.Context(), admitted))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "governance_blocked")
}

func TestRecheckGovernance_RetriesWhenRevisionChangedAfterAdmission(t *testing.T) {
	admitted := activeKey()
	admitted.GovernanceRevision = 2
	admitted.GovernanceDigest = "old"
	current := *admitted
	current.GovernanceRevision = 3
	current.GovernanceDigest = "new"
	ms := &mockStore{key: &current, routeSettings: &store.KeyRouteSettings{GovernanceRevision: 3}}
	handler := NewMiddleware("sk-master", ms).RecheckGovernance(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("external handler must retry under the new revision")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(ContextWithKeyInfo(req.Context(), admitted))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("Retry-After"))
}

func TestAuthenticate_VirtualKey_Expired(t *testing.T) {
	key := activeKey()
	past := time.Now().Add(-time.Hour)
	key.ExpiresAt = &past
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "expired")
}

func TestAuthenticate_VirtualKey_NotExpired(t *testing.T) {
	key := activeKey()
	future := time.Now().Add(time.Hour)
	key.ExpiresAt = &future
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Budget enforcement tests ---

func TestAuthenticate_KeyBudget_BelowLimit(t *testing.T) {
	key := activeKey()
	key.Budget = 10.0
	key.Spend = 5.0
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// A funded key with headroom passes.
func TestAuthenticate_KeyWithHeadroom_IsAllowed(t *testing.T) {
	key := activeKey()
	key.Budget = 10
	key.Spend = 4
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)
	called := false
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Team budget enforcement tests ---

func TestAuthenticate_TeamBudget_Exceeded(t *testing.T) {
	key := activeKey()
	key.TeamID = "team-1"
	team := &store.Team{
		ID:     "team-1",
		Name:   "test team",
		Budget: 5.0,
		Spend:  5.0,
	}
	ms := &mockStore{key: key, team: team}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusPaymentRequired, rec.Code)
	assert.Contains(t, rec.Body.String(), "budget_exceeded")
}

func TestAuthenticate_TeamBudget_BelowLimit(t *testing.T) {
	key := activeKey()
	key.TeamID = "team-1"
	team := &store.Team{
		ID:     "team-1",
		Name:   "test team",
		Budget: 100.0,
		Spend:  10.0,
	}
	ms := &mockStore{key: key, team: team}
	m := NewMiddleware("sk-master", ms)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuthenticate_TeamDisabled(t *testing.T) {
	key := activeKey()
	key.TeamID = "team-1"
	// Team not found = treated as disabled
	ms := &mockStore{key: key, team: nil}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "team_disabled")
}

func TestAuthenticate_TeamNotFound(t *testing.T) {
	key := activeKey()
	key.TeamID = "team-missing"
	ms := &mockStore{key: key, team: nil}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHashKey(t *testing.T) {
	h1 := HashKey("sk-test")
	h2 := HashKey("sk-test")
	h3 := HashKey("sk-other")

	assert.Equal(t, h1, h2, "same input must produce same hash")
	assert.NotEqual(t, h1, h3, "different inputs must produce different hashes")
	assert.Len(t, h1, 64, "SHA-256 hex must be 64 chars")
}

// --- Security: master key comparison must be constant-time but still correct ---

func TestAuthenticate_MasterKey_ConstantTimeCompareStillWorks(t *testing.T) {
	m := NewMiddleware("sk-master-key-of-a-certain-length", nil)

	tests := []struct {
		name       string
		key        string
		wantStatus int
	}{
		{"exact match", "sk-master-key-of-a-certain-length", http.StatusOK},
		{"shorter than master key", "sk-master", http.StatusUnauthorized},
		{"longer than master key", "sk-master-key-of-a-certain-length-extra", http.StatusUnauthorized},
		{"same length, different content", "sk-master-key-of-a-certaix-length", http.StatusUnauthorized},
		{"empty", "", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tt.key != "" {
				req.Header.Set("Authorization", "Bearer "+tt.key)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, tt.wantStatus, rec.Code)
		})
	}
}

// --- Store error path ---

func TestAuthenticate_StoreLookupError_DoesNotLeakDetails(t *testing.T) {
	ms := &mockStore{err: fmt.Errorf("connection refused: pq: dial tcp timeout")}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "internal error validating key")
	assert.NotContains(t, rec.Body.String(), "connection refused")
	assert.NotContains(t, rec.Body.String(), "pq:")
}

func TestAuthenticate_VirtualKey_ExpiresExactlyNow_IsBlocked(t *testing.T) {
	key := activeKey()
	now := time.Now()
	key.ExpiresAt = &now
	ms := &mockStore{key: key}
	m := NewMiddleware("sk-master", ms)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()

	// time.Now() inside Authenticate will be a few nanoseconds after `now`,
	// so time.Now().After(*ExpiresAt) is true and the key must be rejected.
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "expired")
}

// --- Pass-through mode ---

func TestAuthenticate_PassThrough_SkipsBudgetAndModelChecks(t *testing.T) {
	m := NewPassThroughMiddleware()
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "any-client-token", KeyFromContext(r.Context()))
		assert.Equal(t, "any-client-token", UpstreamTokenFromContext(r.Context()))
		// No team/budget info is ever consulted in pass-through mode.
		assert.Nil(t, TeamFromContext(r.Context()))
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer any-client-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Model whitelist / GrantedModels bypass semantics ---

func TestIsModelAllowed_KeyWhitelistBlocksModelsNotListed(t *testing.T) {
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{Models: []string{"gpt-4"}})
	assert.True(t, IsModelAllowed(ctx, "gpt-4", false))
	assert.False(t, IsModelAllowed(ctx, "gpt-4o", false))
}

func TestIsModelAllowed_DenyTakesPrecedenceOverAllowAndGrant(t *testing.T) {
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{
		Models:       []string{"gpt-4o"},
		DeniedModels: []string{"gpt-4o"},
	})
	ctx = ContextWithTeam(ctx, &store.Team{GrantedModels: []string{"gpt-4o"}})
	assert.False(t, IsModelAllowed(ctx, "gpt-4o", false))
}

func TestContextWithCanonicalModels_NormalizesDeniedModels(t *testing.T) {
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{DeniedModels: []string{"gpt-4o"}})
	canonical := ContextWithCanonicalModels(ctx, func(model string) string {
		if model == "gpt-4o" {
			return "azure-gpt-4o"
		}
		return model
	})
	assert.Equal(t, store.StringList{"azure-gpt-4o"}, KeyInfoFromContext(canonical).DeniedModels)
}

func TestIsModelAllowed_EmptyKeyWhitelistAllowsAll(t *testing.T) {
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{})
	assert.True(t, IsModelAllowed(ctx, "any-model", false))
}

func TestIsModelAllowed_TeamGrantedModelsBypassAllowedModels(t *testing.T) {
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{})
	ctx = context.WithValue(ctx, teamInfoKey, &store.Team{
		AllowedModels: []string{"gpt-4o"},
		GrantedModels: []string{"claude-3-opus"},
	})

	// GrantedModels always bypasses AllowedModels, even though it's not in it.
	assert.True(t, IsModelAllowed(ctx, "claude-3-opus", false))
	assert.True(t, IsModelAllowed(ctx, "gpt-4o", false))
	assert.False(t, IsModelAllowed(ctx, "gemini-1.5-pro", false))
}

func TestIsModelAllowed_KeyWhitelistTakesPrecedenceOverTeamGrantedModels(t *testing.T) {
	// A key restricted to "gpt-4" must NOT gain access to a model via the
	// team's GrantedModels bypass — the key-level whitelist is checked first
	// and returns false before team-level rules are ever evaluated.
	ctx := ContextWithKeyInfo(context.Background(), &store.APIKey{Models: []string{"gpt-4"}})
	ctx = context.WithValue(ctx, teamInfoKey, &store.Team{
		GrantedModels: []string{"gpt-4o"},
	})

	assert.False(t, IsModelAllowed(ctx, "gpt-4o", false))
	assert.True(t, IsModelAllowed(ctx, "gpt-4", false))
}

func TestContextWithCanonicalModels_NormalizesCopiesOfKeyAndTeam(t *testing.T) {
	key := &store.APIKey{Models: []string{"gpt-4o", "azure-gpt-4o"}}
	team := &store.Team{
		AllowedModels: []string{"gpt-4o"},
		GrantedModels: []string{"gpt-4o"},
	}
	ctx := ContextWithKeyInfo(context.Background(), key)
	ctx = context.WithValue(ctx, teamInfoKey, team)
	canonicalize := func(model string) string {
		if model == "gpt-4o" {
			return "azure-gpt-4o"
		}
		return model
	}

	canonicalCtx := ContextWithCanonicalModels(ctx, canonicalize)

	assert.Equal(t, store.StringList{"azure-gpt-4o"}, KeyInfoFromContext(canonicalCtx).Models)
	assert.Equal(t, store.StringList{"azure-gpt-4o"}, TeamFromContext(canonicalCtx).AllowedModels)
	assert.Equal(t, store.StringList{"azure-gpt-4o"}, TeamFromContext(canonicalCtx).GrantedModels)
	assert.Equal(t, store.StringList{"gpt-4o", "azure-gpt-4o"}, key.Models, "stored key must not be mutated")
	assert.Equal(t, store.StringList{"gpt-4o"}, team.AllowedModels, "stored team must not be mutated")
}

// --- Upstream token header stripping order ---

func TestUpstreamTokenForwarding_AccountIDHeaderAlwaysStrippedEvenWhenDisabled(t *testing.T) {
	mw := NewUpstreamTokenForwardingMiddleware(UpstreamTokenForwardingOptions{})
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set(defaultUpstreamTokenHeader, "Bearer tid_test")
	req.Header.Set(defaultUpstreamAccountIDHeader, "acct_123")
	req.Header.Set(upstreamProviderHeader, "github_copilot")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, req.Header.Get(defaultUpstreamAccountIDHeader))
	assert.Empty(t, req.Header.Get(defaultUpstreamTokenHeader))
}

func TestContextWithUpstreamToken_RoundTrips(t *testing.T) {
	ctx := ContextWithUpstreamToken(context.Background(), "raw-token")
	assert.Equal(t, "raw-token", UpstreamTokenFromContext(ctx))
}

// A key whose stored row carries no prefix still has to be attributable: the
// dashboard aggregates a tenant's spend, savings and compression by joining on
// key_prefix, so a missing one means money spent that no tenant is credited
// with. Rows created outside the gateway have arrived without it.
func TestAuthenticate_DerivesAMissingKeyPrefixFromTheKey(t *testing.T) {
	raw := "sk-ubq-0123456789abcdef"
	db := &mockStore{key: &store.APIKey{
		KeyHash:   HashKey(raw),
		KeyPrefix: "", // as inserted by a path that did not set it
		Active:    true,
		Budget:    10,
	}}

	var seen *store.APIKey
	h := NewMiddleware("sk-master", db).Authenticate(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = KeyInfoFromContext(r.Context())
		}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil {
		t.Fatal("la richiesta non è stata autenticata")
	}
	if seen.KeyPrefix != raw[:10] {
		t.Errorf("key_prefix = %q, atteso %q: la spesa resterebbe non attribuibile",
			seen.KeyPrefix, raw[:10])
	}
}

// A stored prefix is what the tenant sees in the portal, so a request must not
// quietly replace it.
func TestAuthenticate_KeepsAStoredKeyPrefix(t *testing.T) {
	raw := "sk-ubq-0123456789abcdef"
	db := &mockStore{key: &store.APIKey{
		KeyHash:   HashKey(raw),
		KeyPrefix: "sk-ubq-stor",
		Active:    true,
		Budget:    10,
	}}

	var seen *store.APIKey
	h := NewMiddleware("sk-master", db).Authenticate(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = KeyInfoFromContext(r.Context())
		}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen == nil || seen.KeyPrefix != "sk-ubq-stor" {
		t.Errorf("il prefisso memorizzato è stato sovrascritto: %+v", seen)
	}
}

// ── agent suspension ────────────────────────────────────────────

// agentAwareStore is a store that can answer for agents. mockStore cannot, on
// purpose: the two together cover both sides of the optional interface.
type agentAwareStore struct {
	*mockStore
	agent    *store.Agent
	agentErr error
}

func (s *agentAwareStore) GetAgent(_ context.Context, _ string) (*store.Agent, error) {
	return s.agent, s.agentErr
}

func agentBoundKey(agentID string) *store.APIKey {
	key := activeKey()
	key.AgentID = agentID
	key.TeamID = ""
	return key
}

func authenticateWith(t *testing.T, st store.Store) *httptest.ResponseRecorder {
	t.Helper()
	m := NewMiddleware("sk-master", st)
	handler := m.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-virtual")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticate_ActiveAgentIsLetThrough(t *testing.T) {
	st := &agentAwareStore{
		mockStore: &mockStore{key: agentBoundKey("agent-1")},
		agent:     &store.Agent{ID: "agent-1", Name: "Sales Agent", Status: "active"},
	}
	assert.Equal(t, http.StatusOK, authenticateWith(t, st).Code)
}

func TestAuthenticate_SuspendedAgentIsRefused(t *testing.T) {
	// The point of the whole entity: an administrator stops an agent and its
	// credentials stop working, without hunting for which keys belong to it.
	st := &agentAwareStore{
		mockStore: &mockStore{key: agentBoundKey("agent-1")},
		agent:     &store.Agent{ID: "agent-1", Name: "Sales Agent", Status: "suspended"},
	}

	rec := authenticateWith(t, st)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "agent_unavailable")
	assert.Contains(t, rec.Body.String(), "suspended")
}

func TestAuthenticate_ArchivedAgentIsRefused(t *testing.T) {
	st := &agentAwareStore{
		mockStore: &mockStore{key: agentBoundKey("agent-1")},
		agent:     &store.Agent{ID: "agent-1", Name: "Old Agent", Status: "archived"},
	}
	assert.Equal(t, http.StatusForbidden, authenticateWith(t, st).Code)
}

func TestAuthenticate_MissingAgentIsRefusedNotIgnored(t *testing.T) {
	// A dangling reference is not permission: the credential names an owner
	// that is not there to answer for it.
	st := &agentAwareStore{mockStore: &mockStore{key: agentBoundKey("agent-gone")}, agent: nil}

	rec := authenticateWith(t, st)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "no longer exists")
}

func TestAuthenticate_AgentLookupFailureDoesNotLetTrafficThrough(t *testing.T) {
	st := &agentAwareStore{
		mockStore: &mockStore{key: agentBoundKey("agent-1")},
		agentErr:  fmt.Errorf("database is away"),
	}
	assert.Equal(t, http.StatusInternalServerError, authenticateWith(t, st).Code)
}

func TestAuthenticate_KeyWithoutAnAgentIsUnaffected(t *testing.T) {
	key := activeKey()
	key.TeamID = ""
	st := &agentAwareStore{
		mockStore: &mockStore{key: key},
		agent:     &store.Agent{ID: "agent-1", Status: "suspended"},
	}
	assert.Equal(t, http.StatusOK, authenticateWith(t, st).Code)
}

func TestAuthenticate_StoreThatCannotReadAgentsDoesNotBlock(t *testing.T) {
	// The check rides on an optional interface, so a store without it — a
	// test double, an alternative backend — keeps serving rather than
	// refusing every agent-bound key it cannot vouch for.
	st := &mockStore{key: agentBoundKey("agent-1")}
	assert.Equal(t, http.StatusOK, authenticateWith(t, st).Code)
}

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/agenttoken"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ── helpers ─────────────────────────────────────────────────────────────────

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "test.db"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type captureEmitter struct {
	events []string
	data   []any
}

type testSnapshotVerifier struct{}

func (testSnapshotVerifier) VerifySnapshot(_ context.Context, token string) (*agenttoken.SnapshotClaims, error) {
	var claims agenttoken.SnapshotClaims
	if err := json.Unmarshal([]byte(token), &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func (c *captureEmitter) Emit(event string, data any) {
	c.events = append(c.events, event)
	c.data = append(c.data, data)
}

func newHandler(t *testing.T) (*Handler, store.Store) {
	t.Helper()
	s := openTestStore(t)
	em := &captureEmitter{}
	h := NewHandler(s, "master-secret", nil, em)
	h.snapshotVerifier = testSnapshotVerifier{}
	return h, s
}

func jsonBody(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func badJSON() *bytes.Reader {
	return bytes.NewReader([]byte("not valid json {{"))
}

func doRequest(handler http.HandlerFunc, method, path string, body *bytes.Reader) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, body)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

// ── RequireMasterKey ────────────────────────────────────────────────────────

func TestRequireMasterKey_WrongKey_Returns401(t *testing.T) {
	h, _ := newHandler(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(h.RequireMasterKey(inner))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestRequireMasterKey_CorrectKey_Passes(t *testing.T) {
	h, _ := newHandler(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(h.RequireMasterKey(inner))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Authorization", "Bearer master-secret")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// ── GenerateKey ─────────────────────────────────────────────────────────────

func TestGenerateKey_Success(t *testing.T) {
	h, _ := newHandler(t)

	em := h.emitter.(*captureEmitter)

	rr := doRequest(h.GenerateKey, http.MethodPost, "/admin/keys", jsonBody(store.CreateKeyParams{
		Name:      "test-key",
		Budget:    50.0,
		RateLimit: 100,
		Models:    []string{"gpt-4o"},
	}))

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	rawKey, _ := resp["key"].(string)
	assert.True(t, strings.HasPrefix(rawKey, "sk-ubq-"), "key should start with sk-ubq-")
	assert.Equal(t, "test-key", resp["name"])
	assert.InDelta(t, 50.0, resp["budget"], 0.01)
	assert.InDelta(t, float64(100), resp["rate_limit"], 0.01)

	// webhook emitted
	require.Len(t, em.events, 1)
	assert.Equal(t, "key_created", em.events[0])
}

func TestGenerateKey_WithTeamAndExpiry(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()

	// create a team first
	team := &store.Team{Name: "alpha"}
	require.NoError(t, s.CreateTeam(ctx, team))

	exp := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	rr := doRequest(h.GenerateKey, http.MethodPost, "/admin/keys", jsonBody(store.CreateKeyParams{
		Budget:    10,
		Name:      "exp-key",
		TeamID:    team.ID,
		ExpiresAt: &exp,
	}))

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, team.ID, resp["team_id"])
}

func TestGenerateKey_InvalidJSON_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.GenerateKey, http.MethodPost, "/admin/keys", bytes.NewReader([]byte("not-json")))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGenerateKey_PersistsGovernedProfile(t *testing.T) {
	h, database := newHandler(t)
	rr := doRequest(h.GenerateKey, http.MethodPost, "/admin/keys", jsonBody(store.CreateKeyParams{
		Name: "governed-agent", GovernanceRequired: true,
	}))
	require.Equal(t, http.StatusOK, rr.Code)
	var response map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&response))
	key, err := database.GetKeyByHash(context.Background(), auth.HashKey(response["key"].(string)))
	require.NoError(t, err)
	require.NotNil(t, key)
	assert.True(t, key.GovernanceRequired)
}

// ── GetKeyInfo ───────────────────────────────────────────────────────────────

func createTestKey(t *testing.T, h *Handler, name string) (rawKey string, keyID string) {
	t.Helper()
	rr := doRequest(h.GenerateKey, http.MethodPost, "/admin/keys", jsonBody(store.CreateKeyParams{Name: name, Budget: 10}))
	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	return resp["key"].(string), resp["id"].(string)
}

func TestGetKeyInfo_ByRawKey(t *testing.T) {
	h, _ := newHandler(t)
	rawKey, id := createTestKey(t, h, "mykey")

	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key="+rawKey, nil)
	rr := httptest.NewRecorder()
	h.GetKeyInfo(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, id, resp["id"])
	assert.Equal(t, "mykey", resp["name"])
}

func TestGetKeyInfo_ByPrefix(t *testing.T) {
	h, _ := newHandler(t)
	rawKey, id := createTestKey(t, h, "prefix-key")

	prefix := rawKey[:11]
	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key="+prefix, nil)
	rr := httptest.NewRecorder()
	h.GetKeyInfo(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, id, resp["id"])
}

func TestGetKeyInfo_NotFound_Returns404(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key=sk-ubq-nonexistent", nil)
	rr := httptest.NewRecorder()
	h.GetKeyInfo(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestGetKeyInfo_MissingParam_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	rr := httptest.NewRecorder()
	h.GetKeyInfo(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// ── UpdateKey ────────────────────────────────────────────────────────────────

func TestUpdateKey_ChangeBudget(t *testing.T) {
	h, _ := newHandler(t)
	rawKey, _ := createTestKey(t, h, "update-me")

	rr := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(map[string]any{
		"key":    rawKey,
		"params": map[string]any{"budget": 99.9},
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify via GetKeyInfo
	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key="+rawKey, nil)
	rrGet := httptest.NewRecorder()
	h.GetKeyInfo(rrGet, req)
	require.Equal(t, http.StatusOK, rrGet.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rrGet.Body).Decode(&resp))
	assert.InDelta(t, 99.9, resp["budget"], 0.01)
}

func TestUpdateKey_GovernedFieldsRequireAuditedBreakGlass(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{
		KeyHash: "governed-update-hash", KeyPrefix: "sk-govupd", Name: "governed",
		Active: true, Budget: 10, GovernanceRevision: 3,
	}
	require.NoError(t, database.CreateKey(context.Background(), key))
	body := map[string]any{"key": key.KeyHash, "params": map[string]any{"budget": 99.0}}

	blocked := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(body))
	assert.Equal(t, http.StatusConflict, blocked.Code)
	stored, err := database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, 10.0, stored.Budget)

	req := httptest.NewRequest(http.MethodPatch, "/admin/keys", jsonBody(body))
	req.Header.Set(governanceBreakGlassHeader, "acknowledged")
	req.Header.Set(governanceBreakGlassReason, "emergency provider incident")
	rr := httptest.NewRecorder()
	h.UpdateKey(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	stored, err = database.GetKeyByHash(context.Background(), key.KeyHash)
	require.NoError(t, err)
	assert.Equal(t, 99.0, stored.Budget)
	events, err := database.(*store.GormStore).ListKeyEvents(context.Background(), 10)
	require.NoError(t, err)
	assert.Condition(t, func() bool {
		for _, event := range events {
			if event.Event == "governance_break_glass" && strings.Contains(event.Details, "emergency provider incident") {
				return true
			}
		}
		return false
	}, "break-glass use and reason must be present in the key audit trail")
}

func TestUpdateKey_AllowsIndependentKillSwitchOnGovernedKey(t *testing.T) {
	h, database := newHandler(t)
	key := &store.APIKey{KeyHash: "governed-kill-hash", KeyPrefix: "sk-govkill", Active: true, GovernanceRevision: 2}
	require.NoError(t, database.CreateKey(context.Background(), key))
	response := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(map[string]any{
		"key": key.KeyHash, "params": map[string]any{"active": false},
	}))
	assert.Equal(t, http.StatusOK, response.Code)
}

func TestUpdateKey_Deactivate(t *testing.T) {
	h, _ := newHandler(t)
	rawKey, _ := createTestKey(t, h, "deactivate-me")

	active := false
	rr := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(map[string]any{
		"key":    rawKey,
		"params": map[string]any{"active": &active},
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify via GetKeyInfo
	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key="+rawKey, nil)
	rrGet := httptest.NewRecorder()
	h.GetKeyInfo(rrGet, req)
	require.Equal(t, http.StatusOK, rrGet.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rrGet.Body).Decode(&resp))
	assert.Equal(t, false, resp["active"])
}

func TestUpdateKey_SetRequireEUResidency(t *testing.T) {
	h, _ := newHandler(t)
	rawKey, _ := createTestKey(t, h, "residency-me")

	required := true
	rr := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(map[string]any{
		"key":    rawKey,
		"params": map[string]any{"require_eu_residency": &required},
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key="+rawKey, nil)
	rrGet := httptest.NewRecorder()
	h.GetKeyInfo(rrGet, req)
	require.Equal(t, http.StatusOK, rrGet.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rrGet.Body).Decode(&resp))
	assert.Equal(t, true, resp["require_eu_residency"])
}

func TestUpdateKey_NotFound_Returns404(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(map[string]any{
		"key":    "sk-ubq-notexists",
		"params": map[string]any{"budget": 1.0},
	}))
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestUpdateKey_MissingKey_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateKey, http.MethodPatch, "/admin/keys", jsonBody(map[string]any{
		"params": map[string]any{"budget": 1.0},
	}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// ── DeleteKey ────────────────────────────────────────────────────────────────

func TestDeleteKey_Success(t *testing.T) {
	h, _ := newHandler(t)
	rawKey, _ := createTestKey(t, h, "delete-me")
	em := h.emitter.(*captureEmitter)
	em.events = nil // reset after creation

	rr := doRequest(h.DeleteKey, http.MethodDelete, "/admin/keys", jsonBody(map[string]any{
		"key": rawKey,
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	// Verify gone via GetKeyInfo → 404
	req := httptest.NewRequest(http.MethodGet, "/admin/keys?key="+rawKey, nil)
	rrGet := httptest.NewRecorder()
	h.GetKeyInfo(rrGet, req)
	assert.Equal(t, http.StatusNotFound, rrGet.Code)

	// webhook emitted
	require.Len(t, em.events, 1)
	assert.Equal(t, "key_deleted", em.events[0])
}

func TestDeleteKey_NotFound_Returns404(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.DeleteKey, http.MethodDelete, "/admin/keys", jsonBody(map[string]any{
		"key": "sk-ubq-nonexistent",
	}))
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// ── ListKeys ──────────────────────────────────────────────────────────────────

func TestListKeys_All(t *testing.T) {
	h, _ := newHandler(t)
	createTestKey(t, h, "a")
	createTestKey(t, h, "b")

	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	rr := httptest.NewRecorder()
	h.ListKeys(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	keys, _ := resp["keys"].([]any)
	assert.GreaterOrEqual(t, len(keys), 2)
}

func TestListKeys_FilterByTeam(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()

	team := &store.Team{Name: "beta"}
	require.NoError(t, s.CreateTeam(ctx, team))

	rr1 := doRequest(h.GenerateKey, http.MethodPost, "/admin/keys", jsonBody(store.CreateKeyParams{
		Budget: 10,
		Name:   "team-key", TeamID: team.ID,
	}))
	require.Equal(t, http.StatusOK, rr1.Code)
	createTestKey(t, h, "other-key") // no team

	req := httptest.NewRequest(http.MethodGet, "/admin/keys?team_id="+team.ID, nil)
	rr := httptest.NewRecorder()
	h.ListKeys(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	keys, _ := resp["keys"].([]any)
	assert.Len(t, keys, 1)
}

// ── GetSpendLogs ──────────────────────────────────────────────────────────────

func TestGetSpendLogs_Empty(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/spend", nil)
	rr := httptest.NewRecorder()
	h.GetSpendLogs(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
}

func TestGetSpendLogs_FilterByKey(t *testing.T) {
	h, s := newHandler(t)
	rawKey, _ := createTestKey(t, h, "spend-key")
	ctx := context.Background()

	// Look up by hash via the auth package
	keyHash := auth.HashKey(rawKey)

	// Log some spend
	require.NoError(t, s.LogSpend(ctx, store.SpendRecord{
		KeyHash: keyHash,
		Model:   "gpt-4o",
		Cost:    0.01,
	}))

	req := httptest.NewRequest(http.MethodGet, "/admin/spend?key="+rawKey, nil)
	rr := httptest.NewRecorder()
	h.GetSpendLogs(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	records, _ := resp["spend_logs"].([]any)
	assert.Len(t, records, 1)
}

// Creating and revoking a key are the only administrative acts that hand
// someone the power to spend money, and until this trail existed they left no
// record at all: six keys once spent under a customer's tenant and were
// deleted, and answering "who made these" meant inferring it from the shape of
// the traffic they left behind.
func TestAdmin_KeyLifecycleIsRecorded(t *testing.T) {
	h, s := newHandler(t)

	req := httptest.NewRequest("POST", "/v1/key/generate",
		jsonBody(map[string]any{"name": "reporting", "budget": 5}))
	req.Header.Set(actorHeader, "ada@example.com")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	created := httptest.NewRecorder()
	h.GenerateKey(created, req)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())

	var body struct {
		Key string `json:"key"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &body))

	events, err := s.(*store.GormStore).ListKeyEvents(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "created", events[0].Event)
	assert.Equal(t, "ada@example.com", events[0].Actor,
		"without who acted the trail does not answer the question it exists for")
	assert.Equal(t, "203.0.113.7", events[0].RemoteIP,
		"behind a proxy RemoteAddr is the proxy; the forwarded address is the caller")
	assert.Equal(t, "reporting", events[0].KeyName)

	deleted := doRequest(h.DeleteKey, "POST", "/v1/key/delete",
		jsonBody(map[string]any{"key": body.Key}))
	require.Equal(t, http.StatusOK, deleted.Code, deleted.Body.String())

	events, err = s.(*store.GormStore).ListKeyEvents(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
	last := events[1]
	assert.Equal(t, "deleted", last.Event)
	// The row is gone by now, so the trail has to carry what identified it.
	assert.NotEmpty(t, last.KeyPrefix)
	assert.Equal(t, "reporting", last.KeyName)
	assert.Equal(t, "master-key", last.Actor,
		"with no actor header the operator's own key is the honest answer")
}

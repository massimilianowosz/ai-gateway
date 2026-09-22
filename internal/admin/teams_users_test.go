package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// ── Teams ────────────────────────────────────────────────────────────────────

func TestCreateTeam_Success(t *testing.T) {
	h, _ := newHandler(t)

	rr := doRequest(h.CreateTeam, http.MethodPost, "/admin/teams", jsonBody(store.CreateTeamParams{
		Name:   "red-team",
		Budget: 200.0,
	}))

	require.Equal(t, http.StatusOK, rr.Code)
	var resp store.Team
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "red-team", resp.Name)
	assert.InDelta(t, 200.0, resp.Budget, 0.01)
	assert.NotEmpty(t, resp.ID)
}

func TestCreateTeam_MissingName_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.CreateTeam, http.MethodPost, "/admin/teams", jsonBody(store.CreateTeamParams{}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestCreateTeam_InvalidJSON_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.CreateTeam, http.MethodPost, "/admin/teams", badJSON())
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetTeam_Found(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()

	team := &store.Team{Name: "find-me"}
	require.NoError(t, s.CreateTeam(ctx, team))

	req := httptest.NewRequest(http.MethodGet, "/admin/teams?id="+team.ID, nil)
	rr := httptest.NewRecorder()
	h.GetTeam(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp store.Team
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, team.ID, resp.ID)
	assert.Equal(t, "find-me", resp.Name)
}

func TestGetTeam_NotFound_Returns404(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/teams?id=nonexistent", nil)
	rr := httptest.NewRecorder()
	h.GetTeam(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestGetTeam_MissingID_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/teams", nil)
	rr := httptest.NewRecorder()
	h.GetTeam(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestListTeams_ReturnsAll(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()

	require.NoError(t, s.CreateTeam(ctx, &store.Team{Name: "t1"}))
	require.NoError(t, s.CreateTeam(ctx, &store.Team{Name: "t2"}))

	req := httptest.NewRequest(http.MethodGet, "/admin/teams", nil)
	rr := httptest.NewRecorder()
	h.ListTeams(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	teams, _ := resp["teams"].([]any)
	assert.GreaterOrEqual(t, len(teams), 2)
}

func TestUpdateTeam_ChangeBudget(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()

	team := &store.Team{Name: "update-team"}
	require.NoError(t, s.CreateTeam(ctx, team))

	rr := doRequest(h.UpdateTeam, http.MethodPatch, "/admin/teams", jsonBody(map[string]any{
		"id":     team.ID,
		"params": map[string]any{"budget": 777.0},
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	updated, err := s.GetTeam(ctx, team.ID)
	require.NoError(t, err)
	assert.InDelta(t, 777.0, updated.Budget, 0.01)
}

func TestUpdateTeam_MissingID_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.UpdateTeam, http.MethodPatch, "/admin/teams", jsonBody(map[string]any{
		"params": map[string]any{"budget": 1.0},
	}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestDeleteTeam_Success(t *testing.T) {
	h, s := newHandler(t)
	ctx := context.Background()

	team := &store.Team{Name: "del-team"}
	require.NoError(t, s.CreateTeam(ctx, team))

	rr := doRequest(h.DeleteTeam, http.MethodDelete, "/admin/teams", jsonBody(map[string]any{
		"id": team.ID,
	}))
	require.Equal(t, http.StatusOK, rr.Code)

	gone, err := s.GetTeam(ctx, team.ID)
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestDeleteTeam_MissingID_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	rr := doRequest(h.DeleteTeam, http.MethodDelete, "/admin/teams", jsonBody(map[string]any{}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// ── Users ─────────────────────────────────────────────────────────────────────

func createTestTeam(t *testing.T, s store.Store, name string) *store.Team {
	t.Helper()
	team := &store.Team{Name: name}
	require.NoError(t, s.CreateTeam(context.Background(), team))
	return team
}

func TestCreateUser_Success(t *testing.T) {
	h, s := newHandler(t)
	team := createTestTeam(t, s, "user-team")

	rr := doRequest(h.CreateUser, http.MethodPost, "/admin/users", jsonBody(store.CreateUserParams{
		Email:  "alice@example.com",
		Name:   "Alice",
		TeamID: team.ID,
	}))

	require.Equal(t, http.StatusOK, rr.Code)
	var resp store.User
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "alice@example.com", resp.Email)
	assert.Equal(t, "member", resp.Role) // default role
	assert.True(t, resp.Active)
}

func TestCreateUser_WithExplicitRole(t *testing.T) {
	h, s := newHandler(t)
	team := createTestTeam(t, s, "role-team")

	rr := doRequest(h.CreateUser, http.MethodPost, "/admin/users", jsonBody(store.CreateUserParams{
		Email:  "bob@example.com",
		TeamID: team.ID,
		Role:   "admin",
	}))

	require.Equal(t, http.StatusOK, rr.Code)
	var resp store.User
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "admin", resp.Role)
}

func TestCreateUser_MissingEmail_Returns400(t *testing.T) {
	h, s := newHandler(t)
	team := createTestTeam(t, s, "t")

	rr := doRequest(h.CreateUser, http.MethodPost, "/admin/users", jsonBody(store.CreateUserParams{
		TeamID: team.ID,
	}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestCreateUser_MissingTeamID_Returns400(t *testing.T) {
	h, _ := newHandler(t)

	rr := doRequest(h.CreateUser, http.MethodPost, "/admin/users", jsonBody(store.CreateUserParams{
		Email: "no-team@example.com",
	}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetUser_ByID(t *testing.T) {
	h, s := newHandler(t)
	team := createTestTeam(t, s, "get-u-team")
	ctx := context.Background()

	user := &store.User{Email: "getme@example.com", TeamID: team.ID, Active: true, Role: "member"}
	require.NoError(t, s.CreateUser(ctx, user))

	req := httptest.NewRequest(http.MethodGet, "/admin/users?id="+user.ID, nil)
	rr := httptest.NewRecorder()
	h.GetUser(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp store.User
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, user.ID, resp.ID)
}

func TestGetUser_ByEmail(t *testing.T) {
	h, s := newHandler(t)
	team := createTestTeam(t, s, "email-team")
	ctx := context.Background()

	user := &store.User{Email: "byemail@example.com", TeamID: team.ID, Active: true, Role: "member"}
	require.NoError(t, s.CreateUser(ctx, user))

	req := httptest.NewRequest(http.MethodGet, "/admin/users?email=byemail@example.com", nil)
	rr := httptest.NewRecorder()
	h.GetUser(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp store.User
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "byemail@example.com", resp.Email)
}

func TestGetUser_NotFound_Returns404(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/users?id=nobody", nil)
	rr := httptest.NewRecorder()
	h.GetUser(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestGetUser_MissingParam_Returns400(t *testing.T) {
	h, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	rr := httptest.NewRecorder()
	h.GetUser(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestListUsers_FilterByTeam(t *testing.T) {
	h, s := newHandler(t)
	team1 := createTestTeam(t, s, "tm1")
	team2 := createTestTeam(t, s, "tm2")
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{Email: "u1@x.com", TeamID: team1.ID, Role: "member", Active: true}))
	require.NoError(t, s.CreateUser(ctx, &store.User{Email: "u2@x.com", TeamID: team2.ID, Role: "member", Active: true}))

	req := httptest.NewRequest(http.MethodGet, "/admin/users?team_id="+team1.ID, nil)
	rr := httptest.NewRecorder()
	h.ListUsers(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	users, _ := resp["users"].([]any)
	assert.Len(t, users, 1)
}

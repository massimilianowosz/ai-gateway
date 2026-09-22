package admin

import (
	"encoding/json"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

// --- Teams ---

// CreateTeam creates a new team.
func (h *Handler) CreateTeam(w http.ResponseWriter, r *http.Request) {
	var params store.CreateTeamParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if params.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}

	team := &store.Team{
		Name:   params.Name,
		Budget: params.Budget,
	}

	if err := h.store.CreateTeam(r.Context(), team); err != nil {
		h.logger.Error("admin: create team failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to create team"})
		return
	}

	writeJSON(w, http.StatusOK, team)
}

// GetTeam returns team info.
func (h *Handler) GetTeam(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id query parameter required"})
		return
	}

	team, err := h.store.GetTeam(r.Context(), id)
	if err != nil {
		h.logger.Error("admin: get team failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	if team == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "team not found"})
		return
	}

	writeJSON(w, http.StatusOK, team)
}

// ListTeams lists all teams.
func (h *Handler) ListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := h.store.ListTeams(r.Context(), store.TeamFilter{})
	if err != nil {
		h.logger.Error("admin: list teams failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

// UpdateTeam updates a team.
func (h *Handler) UpdateTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string                 `json:"id"`
		Params store.UpdateTeamParams `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}

	if err := h.store.UpdateTeam(r.Context(), req.ID, req.Params); err != nil {
		h.logger.Error("admin: update team failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

// DeleteTeam deletes a team.
func (h *Handler) DeleteTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}

	if err := h.store.DeleteTeam(r.Context(), req.ID); err != nil {
		h.logger.Error("admin: delete team failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// --- Users ---

// CreateUser creates a new user.
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var params store.CreateUserParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if params.Email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email is required"})
		return
	}
	if params.TeamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "team_id is required"})
		return
	}

	role := params.Role
	if role == "" {
		role = "member"
	}

	user := &store.User{
		Email:  params.Email,
		Name:   params.Name,
		TeamID: params.TeamID,
		Role:   role,
		Active: true,
	}

	if err := h.store.CreateUser(r.Context(), user); err != nil {
		h.logger.Error("admin: create user failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to create user"})
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// GetUser returns user info.
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	email := r.URL.Query().Get("email")

	var user *store.User
	var err error

	switch {
	case id != "":
		user, err = h.store.GetUser(r.Context(), id)
	case email != "":
		user, err = h.store.GetUserByEmail(r.Context(), email)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id or email query parameter required"})
		return
	}

	if err != nil {
		h.logger.Error("admin: get user failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	if user == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "user not found"})
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// ListUsers lists users, optionally filtered by team.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	filter := store.UserFilter{
		TeamID: r.URL.Query().Get("team_id"),
	}
	users, err := h.store.ListUsers(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin: list users failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// UpdateUser updates a user.
func (h *Handler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string                 `json:"id"`
		Params store.UpdateUserParams `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}

	if err := h.store.UpdateUser(r.Context(), req.ID, req.Params); err != nil {
		h.logger.Error("admin: update user failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

// DeleteUser deletes a user.
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}

	if err := h.store.DeleteUser(r.Context(), req.ID); err != nil {
		h.logger.Error("admin: delete user failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

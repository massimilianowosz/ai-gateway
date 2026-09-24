package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/agenttoken"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivetrace"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/webhook"
)

type eventEmitter interface {
	Emit(event string, data any)
}

// Handler handles admin API endpoints.
type Handler struct {
	store            store.Store
	masterKey        string
	logger           *slog.Logger
	emitter          eventEmitter
	cfg              *config.Config
	snapshotVerifier interface {
		VerifySnapshot(context.Context, string) (*agenttoken.SnapshotClaims, error)
	}
	adminSessionValidator func(*http.Request) bool
	traffic               hivetrace.TrafficStore
	trafficHub            *hivetrace.Hub
}

// SetAdminSessionValidator lets the embedded appliance console authorize an
// HttpOnly management session. Bearer master-key access remains available for
// the CLI and automation APIs.
func (h *Handler) SetAdminSessionValidator(validator func(*http.Request) bool) {
	h.adminSessionValidator = validator
}

// NewHandler creates an admin API handler.
func NewHandler(db store.Store, masterKey string, logger *slog.Logger, emitters ...eventEmitter) *Handler {
	var emitter eventEmitter
	if len(emitters) > 0 {
		emitter = emitters[0]
	}
	return &Handler{store: db, masterKey: masterKey, logger: logger, emitter: emitter}
}

// SetConfig sets the gateway config on the handler (for reading global defaults).
func (h *Handler) SetConfig(cfg *config.Config) {
	h.cfg = cfg
	h.snapshotVerifier = agenttoken.New(cfg.Server.AgentTokenJWKSURL, h.logger)
}

// RequireMasterKey middleware ensures only master key can access admin endpoints.
func (h *Handler) RequireMasterKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.adminSessionValidator != nil && h.adminSessionValidator(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("Authorization")
		if len(key) > 7 && key[:7] == "Bearer " {
			key = key[7:]
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(h.masterKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "master key required for admin endpoints",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GenerateKey creates a new virtual API key.
func (h *Handler) GenerateKey(w http.ResponseWriter, r *http.Request) {
	var params store.CreateKeyParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}

	// No budget check here. Whether a call needs a budget depends on the model
	// it names — decided per request by auth.CheckBudget, where the billing
	// mode is known. A key with no budget and no whitelist is not unusable: an
	// empty whitelist means every model, which includes the ones the gateway
	// does not pay for, and those need no budget. Refusing it here forced the
	// caller to enumerate the OAuth models by hand, which is what a whitelist
	// exists to avoid when those models are meant to be open.

	// Generate random key: sk-ubq-<32 hex chars>
	rawKey := generateRawKey()
	keyHash := auth.HashKey(rawKey)
	keyPrefix := rawKey[:11]

	key := &store.APIKey{
		KeyHash:            keyHash,
		KeyPrefix:          keyPrefix,
		Name:               params.Name,
		TeamID:             params.TeamID,
		Budget:             params.Budget,
		RateLimit:          params.RateLimit,
		Models:             store.StringList(params.Models),
		Active:             true,
		ExpiresAt:          params.ExpiresAt,
		GovernanceRequired: params.GovernanceRequired,
	}

	if err := h.store.CreateKey(r.Context(), key); err != nil {
		h.logger.Error("admin: create key failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to create key"})
		return
	}

	h.recordKeyEvent(r, "created", key, "")

	if h.emitter != nil {
		h.emitter.Emit(webhook.EventKeyCreated, map[string]any{
			"id":         key.ID,
			"key_prefix": key.KeyPrefix,
			"name":       key.Name,
			"team_id":    key.TeamID,
			"budget":     key.Budget,
			"rate_limit": key.RateLimit,
			"models":     key.Models,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"key":        rawKey,
		"key_prefix": keyPrefix,
		"id":         key.ID,
		"name":       key.Name,
		"team_id":    key.TeamID,
		"budget":     key.Budget,
		"rate_limit": key.RateLimit,
		"models":     key.Models,
		"expires_at": key.ExpiresAt,
		"created_at": key.CreatedAt,
	})
}

// GetKeyInfo returns info about a specific key.
func (h *Handler) GetKeyInfo(w http.ResponseWriter, r *http.Request) {
	keyParam := r.URL.Query().Get("key")
	if keyParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key query parameter required"})
		return
	}

	key, err := h.resolveKeyIdentifier(r.Context(), keyParam)
	if err != nil {
		h.writeKeyResolutionError(w, "get key", err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

// UpdateKey updates an existing key.
func (h *Handler) UpdateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key    string                `json:"key"`
		Params store.UpdateKeyParams `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key field required"})
		return
	}

	key, err := h.resolveKeyIdentifier(r.Context(), req.Key)
	if err != nil {
		h.writeKeyResolutionError(w, "update key", err)
		return
	}
	if governedKeyFieldsChanged(req.Params) && !h.allowGovernanceBreakGlass(w, r, key, updateSummary(req.Params)) {
		return
	}

	if err := h.store.UpdateKey(r.Context(), key.KeyHash, req.Params); err != nil {
		h.logger.Error("admin: update key failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}

	h.recordKeyEvent(r, "updated", key, updateSummary(req.Params))

	writeJSON(w, http.StatusOK, map[string]any{"status": "updated"})
}

func governedKeyFieldsChanged(params store.UpdateKeyParams) bool {
	return params.Budget != nil || params.RateLimit != nil || params.Models != nil ||
		params.DeniedModels != nil || params.AllowedProviders != nil || params.DeniedProviders != nil ||
		params.ContextInstructions != nil || params.ContextInstructionsDigest != nil ||
		params.RequireEUResidency != nil || params.GovernanceBlocked != nil ||
		params.GovernanceBlockReason != nil
}

const (
	governanceBreakGlassHeader = "X-Ubiquum-Governance-Break-Glass"
	governanceBreakGlassReason = "X-Ubiquum-Governance-Break-Glass-Reason"
)

func (h *Handler) allowGovernanceBreakGlass(
	w http.ResponseWriter, r *http.Request, key *store.APIKey, fields string,
) bool {
	if key == nil || key.GovernanceRevision == 0 {
		return true
	}
	reason := strings.TrimSpace(r.Header.Get(governanceBreakGlassReason))
	acknowledged := strings.EqualFold(strings.TrimSpace(r.Header.Get(governanceBreakGlassHeader)), "acknowledged")
	if !acknowledged || reason == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "governed fields are snapshot-controlled; use the governance API or an audited break-glass request",
		})
		return false
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > 200 {
		reason = reason[:200]
	}
	if err := h.store.LogKeyEvent(r.Context(), keyEventFor(
		r, "governance_break_glass", key, "fields="+fields+"; reason="+reason,
	)); err != nil {
		if h.logger != nil {
			h.logger.Error("admin: break-glass audit failed", "error", err, "key_prefix", key.KeyPrefix)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "break-glass audit unavailable"})
		return false
	}
	return true
}

// DeleteKey deletes a key.
func (h *Handler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key field required"})
		return
	}

	key, err := h.resolveKeyIdentifier(r.Context(), req.Key)
	if err != nil {
		h.writeKeyResolutionError(w, "delete key", err)
		return
	}

	// Recorded before the row disappears: after the delete there is nothing
	// left to describe the key that was revoked.
	h.recordKeyEvent(r, "deleted", key, "")

	if err := h.store.DeleteKey(r.Context(), key.KeyHash); err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "key not found"})
			return
		}
		h.logger.Error("admin: delete key failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}

	if h.emitter != nil {
		h.emitter.Emit(webhook.EventKeyDeleted, map[string]any{
			"id":         key.ID,
			"key_prefix": key.KeyPrefix,
			"name":       key.Name,
			"team_id":    key.TeamID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// ListKeys lists all keys, optionally filtered.
func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	filter := store.KeyFilter{
		TeamID: r.URL.Query().Get("team_id"),
	}

	keys, err := h.store.ListKeys(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin: list keys failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// ListFeedback returns collected feedback records, optionally filtered by team.
func (h *Handler) ListFeedback(w http.ResponseWriter, r *http.Request) {
	teamID := r.URL.Query().Get("team_id")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	records, err := h.store.ListFeedback(r.Context(), teamID, limit)
	if err != nil {
		h.logger.Error("admin: list feedback failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"feedback": records})
}

// GetSpendLogs returns aggregated spend summary grouped by model.
func (h *Handler) GetSpendLogs(w http.ResponseWriter, r *http.Request) {
	filter := store.SpendFilter{
		Model:  r.URL.Query().Get("model"),
		TeamID: r.URL.Query().Get("team_id"),
	}

	keyParam := r.URL.Query().Get("key")
	if keyParam != "" {
		key, err := h.resolveKeyIdentifier(r.Context(), keyParam)
		if err != nil {
			h.writeKeyResolutionError(w, "get spend logs", err)
			return
		}
		filter.KeyHash = key.KeyHash
	}

	summary, err := h.store.GetSpendSummary(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin: get spend logs failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"spend_logs": summary})
}

// GetSpendRecords returns raw individual spend records with pagination.
// Query params: key, team_id, model, start_date, end_date, page, page_size
func (h *Handler) GetSpendRecords(w http.ResponseWriter, r *http.Request) {
	filter := store.SpendFilter{
		Model:  r.URL.Query().Get("model"),
		TeamID: r.URL.Query().Get("team_id"),
	}

	keyParam := r.URL.Query().Get("key")
	if keyParam != "" {
		key, err := h.resolveKeyIdentifier(r.Context(), keyParam)
		if err != nil {
			h.writeKeyResolutionError(w, "get spend records", err)
			return
		}
		filter.KeyHash = key.KeyHash
	}

	if sd := r.URL.Query().Get("start_date"); sd != "" {
		t, err := time.Parse("2006-01-02", sd)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid start_date, use YYYY-MM-DD"})
			return
		}
		filter.StartDate = &t
	}
	if ed := r.URL.Query().Get("end_date"); ed != "" {
		t, err := time.Parse("2006-01-02", ed)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid end_date, use YYYY-MM-DD"})
			return
		}
		// end_date is inclusive: set to end of day
		eod := t.Add(24*time.Hour - time.Nanosecond)
		filter.EndDate = &eod
	}

	// Pagination
	page := 1
	pageSize := 100
	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if ps := r.URL.Query().Get("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 1000 {
			pageSize = v
		}
	}
	filter.Limit = pageSize
	filter.Offset = (page - 1) * pageSize

	records, err := h.store.GetSpend(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin: get spend records failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":      records,
		"page":      page,
		"page_size": pageSize,
	})
}

var (
	errKeyNotFound  = errors.New("key not found")
	errKeyAmbiguous = errors.New("key identifier is ambiguous")
)

type keyResolutionError struct {
	kind    error
	message string
}

func (e *keyResolutionError) Error() string {
	return e.message
}

func (e *keyResolutionError) Unwrap() error {
	return e.kind
}

func (h *Handler) resolveKeyIdentifier(ctx context.Context, identifier string) (*store.APIKey, error) {
	hash := auth.HashKey(identifier)
	key, err := h.store.GetKeyByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if key != nil {
		return key, nil
	}

	// Try identifier as-is (portal may pass the stored hash directly)
	key, err = h.store.GetKeyByHash(ctx, identifier)
	if err != nil {
		return nil, err
	}
	if key != nil {
		return key, nil
	}

	keys, err := h.store.ListKeys(ctx, store.KeyFilter{})
	if err != nil {
		return nil, err
	}

	var matches []store.APIKey
	for _, candidate := range keys {
		switch {
		case candidate.ID == identifier:
			matches = append(matches, candidate)
		case candidate.KeyPrefix == identifier:
			matches = append(matches, candidate)
		case strings.HasPrefix(identifier, "sk-ubq-") && strings.HasPrefix(candidate.KeyPrefix, identifier):
			matches = append(matches, candidate)
		case candidate.Name == identifier:
			matches = append(matches, candidate)
		}
	}

	switch len(matches) {
	case 0:
		return nil, &keyResolutionError{kind: errKeyNotFound, message: fmt.Sprintf("key %q not found", identifier)}
	case 1:
		return &matches[0], nil
	default:
		return nil, &keyResolutionError{
			kind:    errKeyAmbiguous,
			message: fmt.Sprintf("key %q matches multiple keys; use one of these prefixes: %s", identifier, keyPrefixes(matches)),
		}
	}
}

func (h *Handler) writeKeyResolutionError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, errKeyNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, errKeyAmbiguous):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		h.logger.Error("admin: "+op+" failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}
}

func keyPrefixes(keys []store.APIKey) string {
	prefixes := make([]string, 0, len(keys))
	for _, key := range keys {
		prefixes = append(prefixes, key.KeyPrefix)
	}
	return strings.Join(prefixes, ", ")
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func generateRawKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sk-ubq-" + hex.EncodeToString(b)
}

// actorHeader carries the human behind an admin call. The API authenticates
// with the master key, which identifies the operator and nobody in particular;
// a caller that knows who clicked — the portal does — says so here.
const actorHeader = "X-Ubiquum-Actor"

// recordKeyEvent writes one line of the key audit trail.
//
// It never blocks the operation it describes: refusing to create a key
// because the log is unavailable would trade a working system for a tidy
// record. A failure is logged loudly instead, because a trail with silent
// holes invites trust it has not earned.
func (h *Handler) recordKeyEvent(r *http.Request, event string, key *store.APIKey, details string) {
	if h.store == nil || key == nil {
		return
	}
	err := h.store.LogKeyEvent(r.Context(), keyEventFor(r, event, key, details))
	if err != nil && h.logger != nil {
		h.logger.Error("admin: key event not recorded",
			"error", err, "event", event, "key_prefix", key.KeyPrefix)
	}
}

func keyEventFor(r *http.Request, event string, key *store.APIKey, details string) store.KeyEvent {
	actor := strings.TrimSpace(r.Header.Get(actorHeader))
	if actor == "" {
		actor = "master-key"
	}
	return store.KeyEvent{
		Event:     event,
		KeyHash:   key.KeyHash,
		KeyPrefix: key.KeyPrefix,
		KeyName:   key.Name,
		TeamID:    key.TeamID,
		Actor:     actor,
		RemoteIP:  clientIP(r),
		Budget:    key.Budget,
		Details:   details,
		CreatedAt: time.Now(),
	}
}

// clientIP prefers the forwarded address: the gateway sits behind a proxy, so
// RemoteAddr is the proxy on every request.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if comma := strings.Index(forwarded, ","); comma > 0 {
			return strings.TrimSpace(forwarded[:comma])
		}
		return strings.TrimSpace(forwarded)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// updateSummary names the fields an update touched, so the trail says what
// changed rather than only that something did. Values are deliberately left
// out: budgets and model lists belong in the key itself, not in a log that
// will be read by whoever is investigating.
func updateSummary(params store.UpdateKeyParams) string {
	var changed []string
	if params.Name != nil {
		changed = append(changed, "name")
	}
	if params.Budget != nil {
		changed = append(changed, "budget")
	}
	if params.RateLimit != nil {
		changed = append(changed, "rate_limit")
	}
	if params.Models != nil {
		changed = append(changed, "models")
	}
	if params.DeniedModels != nil {
		changed = append(changed, "denied_models")
	}
	if params.AllowedProviders != nil {
		changed = append(changed, "allowed_providers")
	}
	if params.DeniedProviders != nil {
		changed = append(changed, "denied_providers")
	}
	if params.Active != nil {
		changed = append(changed, "active")
	}
	if params.ExpiresAt != nil {
		changed = append(changed, "expires_at")
	}
	if params.ContextInstructions != nil {
		changed = append(changed, "context_instructions")
	}
	if len(changed) == 0 {
		return ""
	}
	return "changed: " + strings.Join(changed, ", ")
}

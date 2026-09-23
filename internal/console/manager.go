package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

const sessionCookie = "ubiquum_admin"

//go:embed assets/*
var assets embed.FS

type sessionClaims struct {
	Expires int64  `json:"exp"`
	CSRF    string `json:"csrf"`
}

type loginWindow struct {
	Started time.Time
	Count   int
}

type Manager struct {
	cfg          config.ConsoleConfig
	passwordHash [32]byte
	signingKey   [32]byte
	vault        *Vault
	registry     *provider.Registry
	factory      provider.ProviderFactory
	logger       *slog.Logger
	loginMu      sync.Mutex
	loginWindows map[string]loginWindow
}

func NewManager(cfg config.ConsoleConfig, masterKey string, vault *Vault, registry *provider.Registry, factory provider.ProviderFactory, logger *slog.Logger) (*Manager, error) {
	m := &Manager{cfg: cfg, vault: vault, registry: registry, factory: factory, logger: logger, loginWindows: map[string]loginWindow{}}
	if cfg.AdminPasswordSHA256 != "" {
		decoded, err := hex.DecodeString(strings.TrimSpace(cfg.AdminPasswordSHA256))
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("console.admin_password_sha256 must be a 64-character SHA-256 hex digest")
		}
		copy(m.passwordHash[:], decoded)
	} else {
		m.passwordHash = sha256.Sum256([]byte(masterKey))
	}
	m.signingKey = sha256.Sum256([]byte("ubiquum-console-session\x00" + masterKey))
	return m, nil
}

func (m *Manager) Register(mux *http.ServeMux, requireAdmin func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusTemporaryRedirect)
	})
	staticFS, _ := fs.Sub(assets, "assets")
	staticHandler := http.StripPrefix("/console/", http.FileServer(http.FS(staticFS)))
	mux.Handle("GET /console/", securityHeaders(staticHandler))
	mux.HandleFunc("POST /console/api/session", m.login)
	mux.HandleFunc("GET /console/api/session", m.sessionInfo)
	mux.HandleFunc("DELETE /console/api/session", m.logout)
	mux.Handle("GET /console/api/models", requireAdmin(http.HandlerFunc(m.listModels)))
	mux.Handle("GET /console/api/connections", requireAdmin(http.HandlerFunc(m.listConnections)))
	mux.Handle("POST /console/api/connections", requireAdmin(http.HandlerFunc(m.createConnection)))
	mux.Handle("DELETE /console/api/connections/{id}", requireAdmin(http.HandlerFunc(m.deleteConnection)))
}

// ValidateAdminSession is passed to the existing admin API middleware. Unsafe
// methods additionally require the per-session CSRF token.
func (m *Manager) ValidateAdminSession(r *http.Request) bool {
	claims, ok := m.claimsFromRequest(r)
	if !ok {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		provided := r.Header.Get("X-Ubiquum-CSRF")
		return provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(claims.CSRF)) == 1
	}
}

func (m *Manager) login(w http.ResponseWriter, r *http.Request) {
	if !m.allowLogin(remoteIP(r)) {
		writeConsoleJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeConsoleJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	candidate := sha256.Sum256([]byte(body.Password))
	if subtle.ConstantTimeCompare(candidate[:], m.passwordHash[:]) != 1 {
		time.Sleep(150 * time.Millisecond)
		writeConsoleJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	claims := sessionClaims{Expires: time.Now().Add(m.cfg.SessionTTL).Unix(), CSRF: randomToken(24)}
	token, _ := m.signClaims(claims)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: m.cfg.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode,
		MaxAge: int(m.cfg.SessionTTL.Seconds()),
	})
	writeConsoleJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": claims.CSRF, "expires_at": claims.Expires})
}

func (m *Manager) sessionInfo(w http.ResponseWriter, r *http.Request) {
	claims, ok := m.claimsFromRequest(r)
	if !ok {
		writeConsoleJSON(w, http.StatusUnauthorized, map[string]bool{"authenticated": false})
		return
	}
	writeConsoleJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": claims.CSRF, "expires_at": claims.Expires})
}

func (m *Manager) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) listModels(w http.ResponseWriter, _ *http.Request) {
	models := []string{}
	if m.registry != nil {
		models = m.registry.ListConsoleModels()
	}
	writeConsoleJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (m *Manager) listConnections(w http.ResponseWriter, _ *http.Request) {
	writeConsoleJSON(w, http.StatusOK, map[string]any{"connections": m.vault.List()})
}

func (m *Manager) createConnection(w http.ResponseWriter, r *http.Request) {
	var connection Connection
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&connection); err != nil {
		writeConsoleJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// Validate provider construction before any secret is committed to disk.
	probe := connection
	probe.ID = "validation"
	for _, model := range probe.ModelConfigs() {
		if _, err := m.factory.Create(model); err != nil {
			writeConsoleJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
	}
	view, err := m.vault.Create(connection)
	if err != nil {
		writeConsoleJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	for _, saved := range m.vault.Connections() {
		if saved.ID == view.ID {
			if err := InstallConnection(m.registry, m.factory, saved); err != nil {
				_, _ = m.vault.Delete(saved.ID)
				writeConsoleJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
				return
			}
			break
		}
	}
	m.logger.Info("BYOK connection created", "connection_id", view.ID, "provider", view.ProviderType, "models", len(view.Models))
	writeConsoleJSON(w, http.StatusCreated, view)
}

func (m *Manager) deleteConnection(w http.ResponseWriter, r *http.Request) {
	connection, err := m.vault.Delete(r.PathValue("id"))
	if err != nil {
		writeConsoleJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	RemoveConnection(m.registry, connection)
	m.logger.Info("BYOK connection deleted", "connection_id", connection.ID, "provider", connection.ProviderType)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) claimsFromRequest(r *http.Request) (sessionClaims, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return sessionClaims{}, false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return sessionClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return sessionClaims{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sessionClaims{}, false
	}
	mac := hmac.New(sha256.New, m.signingKey[:])
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return sessionClaims{}, false
	}
	var claims sessionClaims
	if json.Unmarshal(payload, &claims) != nil || claims.Expires <= time.Now().Unix() || claims.CSRF == "" {
		return sessionClaims{}, false
	}
	return claims, true
}

func (m *Manager) signClaims(claims sessionClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, m.signingKey[:])
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (m *Manager) allowLogin(ip string) bool {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	now := time.Now()
	window := m.loginWindows[ip]
	if window.Started.IsZero() || now.Sub(window.Started) >= time.Minute {
		m.loginWindows[ip] = loginWindow{Started: now, Count: 1}
		return true
	}
	if window.Count >= 8 {
		return false
	}
	window.Count++
	m.loginWindows[ip] = window
	return true
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func randomToken(size int) string {
	b := make([]byte, size)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeConsoleJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

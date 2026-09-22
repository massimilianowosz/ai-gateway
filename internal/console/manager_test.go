package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func TestConsoleSessionRequiresCSRFForWrites(t *testing.T) {
	password := "correct horse battery staple"
	digest := sha256.Sum256([]byte(password))
	manager, err := NewManager(config.ConsoleConfig{
		SessionTTL: time.Hour, AdminPasswordSHA256: hex.EncodeToString(digest[:]),
	}, "master", nil, nil, nil, slog.Default())
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]string{"password": password})
	login := httptest.NewRecorder()
	manager.login(login, httptest.NewRequest(http.MethodPost, "/console/api/session", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, login.Code)
	require.NotEmpty(t, login.Result().Cookies())
	var response map[string]any
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &response))
	csrf := response["csrf"].(string)

	read := httptest.NewRequest(http.MethodGet, "/v1/key/list", nil)
	read.AddCookie(login.Result().Cookies()[0])
	require.True(t, manager.ValidateAdminSession(read))

	write := httptest.NewRequest(http.MethodPost, "/v1/key/update", nil)
	write.AddCookie(login.Result().Cookies()[0])
	require.False(t, manager.ValidateAdminSession(write))
	write.Header.Set("X-Ubiquum-CSRF", csrf)
	require.True(t, manager.ValidateAdminSession(write))
}

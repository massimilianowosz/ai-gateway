package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func TestVaultEncryptsSecretsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "credentials.vault")
	keyPath := filepath.Join(dir, "credentials.key")
	vault, err := OpenVault(vaultPath, keyPath)
	require.NoError(t, err)

	view, err := vault.Create(Connection{
		Name: "Production", ProviderType: "openai", APIKey: "super-secret-provider-key",
		Models: []ManagedModel{{Name: "gpt-main", ProviderModel: "gpt-upstream"}},
	})
	require.NoError(t, err)
	require.True(t, view.HasAPIKey)
	require.NotEmpty(t, view.ID)

	onDisk, err := os.ReadFile(vaultPath)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(onDisk), "super-secret-provider-key"))
	require.False(t, strings.Contains(string(onDisk), "Production"))

	reopened, err := OpenVault(vaultPath, keyPath)
	require.NoError(t, err)
	require.Len(t, reopened.Connections(), 1)
	require.Equal(t, "super-secret-provider-key", reopened.Connections()[0].APIKey)
	require.Equal(t, config.BillingModeMetered, reopened.Connections()[0].ModelConfigs()[0].BillingMode)
}

func TestVaultRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "credentials.vault")
	vault, err := OpenVault(vaultPath, filepath.Join(dir, "first.key"))
	require.NoError(t, err)
	_, err = vault.Create(Connection{Name: "One", ProviderType: "openai", APIKey: "secret", Models: []ManagedModel{{Name: "one", ProviderModel: "one"}}})
	require.NoError(t, err)

	_, err = OpenVault(vaultPath, filepath.Join(dir, "second.key"))
	require.ErrorContains(t, err, "cannot be decrypted")
}

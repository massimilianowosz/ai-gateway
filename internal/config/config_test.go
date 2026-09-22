package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_ValidConfig(t *testing.T) {
	content := `
server:
  port: 8080
  master_key: sk-test-key
  max_request_size_mb: 16

database:
  url: postgresql://localhost:5432/test
  pool_size: 5
  batch_write_interval: 30s

providers:
  azure-prod:
    type: azure_openai
    api_base: https://example.openai.azure.com
    api_key: test-key
    api_version: "2024-10-21"
  vertex-eu:
    type: vertex
    project: my-project
    location: europe-west1

models:
  - name: gpt-4o
    provider: azure-prod
    provider_model: gpt-4o-2024-11-20

  - name: claude-sonnet-4
    provider: vertex-eu
    provider_model: claude-sonnet-4@20250514

model_aliases:
  openai-gpt-4o: gpt-4o

router:
  strategy: shuffle
  retries: 2
  retry_delay: 3s
  circuit_breaker:
    threshold: 5
    recovery: 120s

pricing:
  remote_url: https://example.com/prices.json
  disable_remote_refresh: true
`
	f := writeTempFile(t, content)
	cfg, err := Load(f)
	require.NoError(t, err)

	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, "sk-test-key", cfg.Server.MasterKey)
	assert.Equal(t, 16, cfg.Server.MaxRequestSizeMB)
	assert.Equal(t, "postgresql://localhost:5432/test", cfg.Database.URL)
	assert.Equal(t, 5, cfg.Database.PoolSize)
	assert.Equal(t, 30*time.Second, cfg.Database.BatchWriteInterval)
	assert.Len(t, cfg.Models, 2)
	assert.Equal(t, "gpt-4o", cfg.Models[0].Name)
	assert.Equal(t, "azure_openai", cfg.Models[0].Provider)
	assert.Equal(t, "https://example.openai.azure.com", cfg.Models[0].APIBase)
	assert.Equal(t, "test-key", cfg.Models[0].APIKey)
	assert.Equal(t, "claude-sonnet-4", cfg.Models[1].Name)
	assert.Equal(t, "vertex", cfg.Models[1].Provider)
	assert.Equal(t, "my-project", cfg.Models[1].Project)
	assert.Equal(t, map[string]string{"openai-gpt-4o": "gpt-4o"}, cfg.ModelAliases)
	assert.Equal(t, "shuffle", cfg.Router.Strategy)
	assert.Equal(t, 2, cfg.Router.Retries)
	assert.Equal(t, 3*time.Second, cfg.Router.RetryDelay)
	assert.Equal(t, 5, cfg.Router.CircuitBreaker.Threshold)
	assert.Equal(t, 120*time.Second, cfg.Router.CircuitBreaker.Recovery)
	assert.Equal(t, "https://example.com/prices.json", cfg.Pricing.RemoteURL)
	assert.True(t, cfg.Pricing.DisableRemoteRefresh)
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("TEST_MASTER_KEY", "sk-from-env")
	t.Setenv("TEST_API_KEY", "api-key-from-env")

	content := `
server:
  master_key: ${TEST_MASTER_KEY}
providers:
  openai:
    type: openai
    api_key: ${TEST_API_KEY}
models:
  - name: test-model
    provider: openai
    provider_model: gpt-4o
`
	f := writeTempFile(t, content)
	cfg, err := Load(f)
	require.NoError(t, err)

	assert.Equal(t, "sk-from-env", cfg.Server.MasterKey)
	assert.Equal(t, "api-key-from-env", cfg.Models[0].APIKey)
}

func TestLoad_RejectsMissingEnvironmentVariable(t *testing.T) {
	path := writeTempFile(t, `
server:
  master_key: ${UBIQUUM_TEST_MISSING_SECRET}
`)
	_, err := Load(path)
	require.ErrorContains(t, err, "UBIQUUM_TEST_MISSING_SECRET")
}

func TestLoad_Defaults(t *testing.T) {
	content := `
server:
  master_key: sk-test
providers:
  azure:
    type: azure_openai
models:
  - name: m1
    provider: azure
    provider_model: gpt-4o
`
	f := writeTempFile(t, content)
	cfg, err := Load(f)
	require.NoError(t, err)

	assert.Equal(t, 4000, cfg.Server.Port)
	assert.Equal(t, 32, cfg.Server.MaxRequestSizeMB)
	assert.Equal(t, 10, cfg.Database.PoolSize)
	assert.Equal(t, 60*time.Second, cfg.Database.BatchWriteInterval)
	assert.Equal(t, "shuffle", cfg.Router.Strategy)
	assert.Equal(t, 3, cfg.Router.Retries)
	assert.Equal(t, 5*time.Second, cfg.Router.RetryDelay)
}

func TestLoad_MissingMasterKey(t *testing.T) {
	content := `
models:
  - name: m1
    provider: azure_openai
    provider_model: gpt-4o
`
	f := writeTempFile(t, content)
	_, err := Load(f)
	assert.ErrorContains(t, err, "server.master_key is required")
}

func TestLoad_NoModels(t *testing.T) {
	content := `
server:
  master_key: sk-test
models: []
`
	f := writeTempFile(t, content)
	cfg, err := Load(f)
	require.NoError(t, err)
	assert.Empty(t, cfg.Models)
}

func TestLoad_InvalidStrategy(t *testing.T) {
	content := `
server:
  master_key: sk-test
providers:
  azure:
    type: azure_openai
models:
  - name: m1
    provider: azure
    provider_model: gpt-4o
router:
  strategy: invalid
`
	f := writeTempFile(t, content)
	_, err := Load(f)
	assert.ErrorContains(t, err, "router.strategy must be one of")
}

func TestLoad_UnknownProvider(t *testing.T) {
	content := `
server:
  master_key: sk-test
providers:
  openai:
    type: openai
models:
  - name: m1
    provider: missing
    provider_model: gpt-4o
`
	f := writeTempFile(t, content)
	_, err := Load(f)
	assert.ErrorContains(t, err, `models[0].provider references unknown provider "missing"`)
}

func TestLoad_InvalidModelAliases(t *testing.T) {
	tests := []struct {
		name    string
		aliases string
		message string
	}{
		{name: "unknown target", aliases: "friendly: missing", message: `references unknown model "missing"`},
		{name: "configured name collision", aliases: "m1: m2", message: "conflicts with a configured model name"},
		{name: "self reference", aliases: "friendly: friendly", message: "cannot point to itself"},
		{name: "alias chain", aliases: "friendly: intermediate\n  intermediate: m1", message: "must point directly to a configured model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := `
server:
  master_key: sk-test
providers:
  mock:
    type: echo
models:
  - name: m1
    provider: mock
    provider_model: upstream-1
  - name: m2
    provider: mock
    provider_model: upstream-2
model_aliases:
  ` + tt.aliases + "\n"
			_, err := Load(writeTempFile(t, content))
			assert.ErrorContains(t, err, tt.message)
		})
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path.yaml")
	assert.Error(t, err)
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return f.Name()
}

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

func TestIsCLICommand(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"serve", true},
		{"shell", true},
		{"init", true},
		{"start", true},
		{"stop", true},
		{"restart", true},
		{"status", true},
		{"model", true},
		{"models", true},
		{"provider", true},
		{"providers", true},
		{"use", true},
		{"key", true},
		{"keys", true},
		{"tenant", true},
		{"tenants", true},
		{"spend", true},
		{"test", true},
		{"config", true},
		{"cache", true},
		{"state", true},
		{"route", true},
		{"logs", true},
		{"help", true},
		{"version", true},
		{"-h", true},
		{"--help", true},
		{"bogus-command", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			assert.Equal(t, tt.want, isCLICommand(tt.arg))
		})
	}
}

func TestProviderIcon(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{ProviderAzureOpenAI, "🔷"},
		{ProviderVertex, "🔶"},
		{ProviderBedrock, "🟠"},
		{ProviderOpenAI, "🟢"},
		{ProviderOpenAICompatible, "🟢"},
		{ProviderGitHubCopilot, "🟣"},
		{ProviderAnthropic, "🟤"},
		{ProviderGoogleAI, "🔵"},
		{"unknown_provider", "⚪"},
		{"", "⚪"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			assert.Equal(t, tt.want, providerIcon(tt.provider))
		})
	}
}

func TestInitCacheStore_MemoryBackend(t *testing.T) {
	cfg := config.CacheConfig{Backend: "memory"}
	store, err := initCacheStore(cfg, config.DatabaseConfig{}, nil)
	require := assert.New(t)
	require.NoError(err)
	require.NotNil(store)
}

func TestInitCacheStore_PgvectorRequiresPostgres(t *testing.T) {
	cfg := config.CacheConfig{Backend: "pgvector"}
	_, err := initCacheStore(cfg, config.DatabaseConfig{Driver: "sqlite"}, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pgvector backend requires database.driver=postgres")
}

func TestInitCacheStore_UnsupportedBackend(t *testing.T) {
	cfg := config.CacheConfig{Backend: "totally-unknown"}
	_, err := initCacheStore(cfg, config.DatabaseConfig{}, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported cache backend")
}

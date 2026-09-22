package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestProviderFactory_AzureOpenAI_RequiresAPIBaseAndKey(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderAzureOpenAI})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_base is required")

	_, err = f.Create(config.ModelConfig{Provider: ProviderAzureOpenAI, APIBase: "https://x.openai.azure.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key is required")

	p, err := f.Create(config.ModelConfig{Provider: ProviderAzureOpenAI, APIBase: "https://x.openai.azure.com", APIKey: "key"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_AzureOpenAICompat_RequiresAPIBaseAndKey(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderAzureOpenAICompat})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_base is required")

	p, err := f.Create(config.ModelConfig{Provider: ProviderAzureOpenAICompat, APIBase: "https://x", APIKey: "key"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_OpenAI_RequiresAPIKey_DefaultsBase(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderOpenAI})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key is required")

	p, err := f.Create(config.ModelConfig{Provider: ProviderOpenAI, APIKey: "sk-test"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_OpenAICompatible_RequiresAPIBaseAndKey(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderOpenAICompatible})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_base is required")

	_, err = f.Create(config.ModelConfig{Provider: ProviderOpenAICompatible, APIBase: "http://localhost:11434"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key is required")

	p, err := f.Create(config.ModelConfig{Provider: ProviderOpenAICompatible, APIBase: "http://localhost:11434", APIKey: "key"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_GitHubCopilot_NoRequiredFields(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{Provider: ProviderGitHubCopilot})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_ChatGPTCodex_NoAPIKeyRequired(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{Provider: ProviderChatGPTCodex, ProviderModel: "gpt-5.1-codex"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_Vertex_RequiresProjectAndLocation(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderVertex})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project is required")

	_, err = f.Create(config.ModelConfig{Provider: ProviderVertex, Project: "proj"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "location is required")
}

func TestProviderFactory_Vertex_InlineJSONCredentials(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{
		Provider:      ProviderVertex,
		Project:       "proj",
		Location:      "us-central1",
		APIKey:        `{"type":"service_account"}`,
		ProviderModel: "gemini-1.5-pro",
	})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_Vertex_CredentialsFileNotFound(t *testing.T) {
	f := &providerFactory{}
	_, err := f.Create(config.ModelConfig{
		Provider: ProviderVertex,
		Project:  "proj",
		Location: "us-central1",
		APIKey:   "/nonexistent/path/creds.json",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading credentials file")
}

func TestProviderFactory_Vertex_CredentialsFromFile(t *testing.T) {
	f := &providerFactory{}
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")
	require.NoError(t, os.WriteFile(credsPath, []byte(`{"type":"service_account"}`), 0o600))

	p, err := f.Create(config.ModelConfig{
		Provider: ProviderVertex,
		Project:  "proj",
		Location: "us-central1",
		APIKey:   credsPath,
	})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_Anthropic_NoAPIKeyRequired(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{Provider: ProviderAnthropic, ProviderModel: "claude-sonnet-4"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_AzureAnthropic_RequiresAPIBaseAndKey(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderAzureAnthropic})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_base is required")

	_, err = f.Create(config.ModelConfig{Provider: ProviderAzureAnthropic, APIBase: "https://x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key is required")

	p, err := f.Create(config.ModelConfig{Provider: ProviderAzureAnthropic, APIBase: "https://x", APIKey: "key"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_GoogleAI_RequiresAPIKey(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderGoogleAI})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key is required")

	p, err := f.Create(config.ModelConfig{Provider: ProviderGoogleAI, APIKey: "key"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_Bedrock_RequiresRegionAndKey(t *testing.T) {
	f := &providerFactory{}

	_, err := f.Create(config.ModelConfig{Provider: ProviderBedrock})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "region is required")

	_, err = f.Create(config.ModelConfig{Provider: ProviderBedrock, Region: "us-east-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api_key is required")
}

func TestProviderFactory_Bedrock_SigV4WhenSecretPresent(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{
		Provider:      ProviderBedrock,
		Region:        "us-east-1",
		APIKey:        "AKIAEXAMPLE",
		APISecret:     "secret",
		ProviderModel: "anthropic.claude-3-sonnet",
	})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_Bedrock_BearerTokenWhenNoSecret(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{
		Provider:      ProviderBedrock,
		Region:        "us-east-1",
		APIKey:        "bearer-token",
		ProviderModel: "anthropic.claude-3-sonnet",
	})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_Echo_NoRequiredFields(t *testing.T) {
	f := &providerFactory{}
	p, err := f.Create(config.ModelConfig{Provider: ProviderEcho, ProviderModel: "echo-1"})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestProviderFactory_UnknownProviderType(t *testing.T) {
	f := &providerFactory{}
	_, err := f.Create(config.ModelConfig{Provider: "totally-unknown"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider type")
}

func TestProviderFactory_NativeResponsesDefaultsPerProvider(t *testing.T) {
	f := &providerFactory{}

	// Only the endpoints that really expose /responses may be handed a
	// Responses request verbatim; the *_compat flavours must translate.
	cases := []struct {
		name   string
		cfg    config.ModelConfig
		native bool
	}{
		{"openai", config.ModelConfig{Provider: ProviderOpenAI, APIKey: "key"}, true},
		{"openai_compatible", config.ModelConfig{Provider: ProviderOpenAICompatible, APIBase: "https://groq.example/v1", APIKey: "key"}, false},
		{"azure_openai", config.ModelConfig{Provider: ProviderAzureOpenAI, APIBase: "https://x.openai.azure.com", APIKey: "key"}, true},
		{"azure_openai_compat", config.ModelConfig{Provider: ProviderAzureOpenAICompat, APIBase: "https://x", APIKey: "key"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := f.Create(tc.cfg)
			require.NoError(t, err)
			capability, ok := p.(provider.NativeResponsesCapability)
			require.True(t, ok, "%s must report its Responses capability", tc.name)
			assert.Equal(t, tc.native, capability.SupportsNativeResponses())
		})
	}
}

func TestProviderFactory_NativeResponsesOverride(t *testing.T) {
	f := &providerFactory{}
	enabled := true

	// An OpenAI-compatible proxy that does implement /responses can opt in.
	p, err := f.Create(config.ModelConfig{
		Provider:        ProviderOpenAICompatible,
		APIBase:         "https://proxy.example/v1",
		APIKey:          "key",
		NativeResponses: &enabled,
	})
	require.NoError(t, err)
	capability, ok := p.(provider.NativeResponsesCapability)
	require.True(t, ok)
	assert.True(t, capability.SupportsNativeResponses())
}

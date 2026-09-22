package main

import (
	"fmt"
	"os"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/anthropic"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/azure"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/bedrock"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/chatgptcodex"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/copilot"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/echo"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/googleai"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/openai"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider/vertex"
)

// Provider name constants.
const (
	ProviderAzureOpenAI       = "azure_openai"
	ProviderAzureOpenAICompat = "azure_openai_compat" // keeps max_tokens (for Mistral etc.)
	ProviderAzureAnthropic    = "azure_anthropic"
	ProviderOpenAI            = "openai"
	ProviderOpenAICompatible  = "openai_compatible"
	ProviderVertex            = "vertex"
	ProviderAnthropic         = "anthropic"
	ProviderGoogleAI          = "google_ai"
	ProviderBedrock           = "bedrock"
	ProviderGitHubCopilot     = "github_copilot"
	ProviderChatGPTCodex      = "chatgpt_codex"
	ProviderEcho              = "echo"
)

// nativeResponsesEnabled resolves the optional native_responses override
// against the provider's own default.
func nativeResponsesEnabled(cfg config.ModelConfig, byDefault bool) bool {
	if cfg.NativeResponses != nil {
		return *cfg.NativeResponses
	}
	return byDefault
}

// providerFactory creates Provider instances from model config.
type providerFactory struct{}

func (f *providerFactory) Create(cfg config.ModelConfig) (provider.Provider, error) {
	switch cfg.Provider {
	case ProviderAzureOpenAI:
		if cfg.APIBase == "" {
			return nil, fmt.Errorf("api_base is required for %s provider", ProviderAzureOpenAI)
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderAzureOpenAI)
		}
		apiVersion := cfg.APIVersion
		if apiVersion == "" {
			apiVersion = "2024-10-21"
		}
		return azure.NewOpenAI(cfg.APIBase, cfg.APIKey, apiVersion,
			azure.WithNativeResponses(nativeResponsesEnabled(cfg, true))), nil

	case ProviderAzureOpenAICompat:
		if cfg.APIBase == "" {
			return nil, fmt.Errorf("api_base is required for %s provider", ProviderAzureOpenAICompat)
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderAzureOpenAICompat)
		}
		apiVersion := cfg.APIVersion
		if apiVersion == "" {
			apiVersion = "2024-10-21"
		}
		return azure.NewOpenAICompat(cfg.APIBase, cfg.APIKey, apiVersion,
			azure.WithNativeResponses(nativeResponsesEnabled(cfg, false))), nil

	case ProviderOpenAI:
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderOpenAI)
		}
		base := cfg.APIBase
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return openai.NewCompatible(base, cfg.APIKey,
			openai.WithNativeResponses(nativeResponsesEnabled(cfg, true))), nil

	case ProviderOpenAICompatible:
		if cfg.APIBase == "" {
			return nil, fmt.Errorf("api_base is required for %s provider", ProviderOpenAICompatible)
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderOpenAICompatible)
		}
		return openai.NewCompatible(cfg.APIBase, cfg.APIKey,
			openai.WithNativeResponses(nativeResponsesEnabled(cfg, false))), nil

	case ProviderGitHubCopilot:
		return copilot.New(cfg.APIBase, cfg.APIKey), nil

	case ProviderChatGPTCodex:
		// api_key is intentionally not required: this provider relies solely on
		// scoped upstream OAuth token + account id forwarding (ChatGPT Plus/Pro
		// Codex subscription pass-through via ubiquum-cli). See
		// internal/auth.UpstreamTokenForProvider / UpstreamAccountIDForProvider.
		return chatgptcodex.New(cfg.ProviderModel), nil

	case ProviderVertex:
		if cfg.Project == "" {
			return nil, fmt.Errorf("project is required for %s provider", ProviderVertex)
		}
		if cfg.Location == "" {
			return nil, fmt.Errorf("location is required for %s provider", ProviderVertex)
		}
		vcfg := vertex.Config{
			Project:  cfg.Project,
			Location: cfg.Location,
		}
		if cfg.APIKey != "" {
			if cfg.APIKey[0] == '{' {
				vcfg.CredentialsJSON = cfg.APIKey
			} else {
				creds, err := os.ReadFile(cfg.APIKey)
				if err != nil {
					return nil, fmt.Errorf("vertex: reading credentials file %q: %w", cfg.APIKey, err)
				}
				vcfg.CredentialsJSON = string(creds)
			}
		}
		return vertex.New(vcfg, cfg.ProviderModel)

	case ProviderAnthropic:
		// api_key may be empty when the deployment relies solely on scoped
		// upstream token forwarding (e.g. Claude Pro/Max OAuth pass-through
		// via ubiquum-cli). See internal/auth.UpstreamTokenForProvider.
		return anthropic.New(cfg.APIKey, cfg.ProviderModel), nil

	case ProviderAzureAnthropic:
		if cfg.APIBase == "" {
			return nil, fmt.Errorf("api_base is required for %s provider", ProviderAzureAnthropic)
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderAzureAnthropic)
		}
		return anthropic.NewAzure(cfg.APIKey, cfg.APIBase, cfg.ProviderModel), nil

	case ProviderGoogleAI:
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderGoogleAI)
		}
		return googleai.New(cfg.APIKey, cfg.ProviderModel), nil

	case ProviderBedrock:
		if cfg.Region == "" {
			return nil, fmt.Errorf("region is required for %s provider", ProviderBedrock)
		}
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("api_key is required for %s provider", ProviderBedrock)
		}
		bcfg := bedrock.Config{
			Region: cfg.Region,
		}
		if cfg.APISecret != "" {
			bcfg.AccessKeyID = cfg.APIKey
			bcfg.SecretAccessKey = cfg.APISecret
			bcfg.SessionToken = cfg.SessionToken
		} else {
			bcfg.BearerToken = cfg.APIKey
		}
		return bedrock.New(bcfg, cfg.ProviderModel)

	case ProviderEcho:
		return echo.New(cfg.ProviderModel), nil

	default:
		return nil, fmt.Errorf("unknown provider type: %q", cfg.Provider)
	}
}

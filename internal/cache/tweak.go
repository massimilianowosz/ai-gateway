package cache

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// TweakFunc adapts a cached response to a new query using a cheap LLM.
// It receives the new query, the original cached messages, and the cached response body.
// Returns the adapted response body (complete OpenAI-format JSON).
type TweakFunc func(ctx context.Context, newQuery string, cachedMessages string, cachedResponse string) (string, error)

// NewTweakFunc creates a TweakFunc that uses a chat model from the registry.
func NewTweakFunc(registry *provider.Registry, modelName string) (TweakFunc, error) {
	deps, err := registry.GetDeployments(modelName)
	if err != nil || len(deps) == 0 {
		return nil, fmt.Errorf("cache: tweak model %q not found in registry", modelName)
	}

	dep := deps[0]

	return func(ctx context.Context, newQuery, cachedMessages, cachedResponse string) (string, error) {
		// GW-02/CTX-03 product decision: an EU-only key's cached-answer
		// adaptation must never leave the EU through this auxiliary model
		// just because it has no per-key residency plumbing. Returning an
		// error here is the existing "tweak failed" path (middleware.go
		// already falls through to a normal upstream call on it), so this
		// fails closed by skipping the optimization, not by blocking or
		// sending the prompt to a non-EU deployment.
		if allEU, hasDeployments := registry.AllDeploymentsEU(modelName); !auth.IsResidencyAllowed(ctx, allEU, hasDeployments) {
			return "", fmt.Errorf("cache: tweak model %q is not available in an EU-only deployment", modelName)
		}

		// Extract the cached assistant content
		cachedContent := extractAssistantContent(cachedResponse)

		systemPrompt := `You are a response adapter. You have a cached LLM response that answered a similar but slightly different question. Adapt the cached response to correctly answer the new question. Keep the same style, tone, and format. Only change what's necessary to make it accurate for the new question. Return ONLY the adapted response text, nothing else.`

		userPrompt := fmt.Sprintf("Original conversation:\n%s\n\nCached response:\n%s\n\nNew question:\n%s\n\nAdapt the cached response to answer the new question:", cachedMessages, cachedContent, newQuery)

		resp, err := dep.Provider.Complete(ctx, &provider.CompletionRequest{
			Model: dep.ProviderModel,
			Messages: []provider.Message{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: userPrompt},
			},
		})
		if err != nil {
			return "", fmt.Errorf("tweak completion: %w", err)
		}

		// Build a response that looks like the original but with adapted content
		tweakedResponse := buildTweakedResponse(cachedResponse, resp)
		return tweakedResponse, nil
	}, nil
}

// extractAssistantContent pulls the assistant message from a cached OpenAI response.
func extractAssistantContent(response string) string {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(response), &resp); err == nil && len(resp.Choices) > 0 {
		return resp.Choices[0].Message.Content
	}
	return response
}

// buildTweakedResponse takes the original cached response structure and replaces
// the content with the tweak model's output, preserving the OpenAI format.
func buildTweakedResponse(cachedResponse string, tweakResp *provider.CompletionResponse) string {
	var original map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cachedResponse), &original); err != nil {
		// Fallback: build minimal response
		return buildMinimalResponse(tweakResp)
	}

	// Get the tweaked content
	tweakedContent := ""
	if len(tweakResp.Choices) > 0 && tweakResp.Choices[0].Message != nil {
		if s, ok := tweakResp.Choices[0].Message.Content.(string); ok {
			tweakedContent = s
		}
	}

	// Replace choices in the original structure
	finishReason := "stop"
	newChoices := []map[string]interface{}{
		{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": tweakedContent,
			},
			"finish_reason": finishReason,
		},
	}

	choicesJSON, _ := json.Marshal(newChoices)
	original["choices"] = choicesJSON

	// Update usage if available from tweak response
	if tweakResp.Usage != nil {
		usageJSON, _ := json.Marshal(tweakResp.Usage)
		original["usage"] = usageJSON
	}

	result, err := json.Marshal(original)
	if err != nil {
		return buildMinimalResponse(tweakResp)
	}
	return string(result)
}

func buildMinimalResponse(resp *provider.CompletionResponse) string {
	data, _ := json.Marshal(resp)
	return string(data)
}

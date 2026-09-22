package cache

import (
	"context"
	"fmt"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// NewEmbedFunc creates an EmbedFunc that uses a provider from the registry.
// The model must implement the Embedder interface.
func NewEmbedFunc(registry *provider.Registry, modelName string) (EmbedFunc, error) {
	deps, err := registry.GetDeployments(modelName)
	if err != nil || len(deps) == 0 {
		return nil, fmt.Errorf("cache: embedding model %q not found in registry", modelName)
	}

	dep := deps[0]
	embedder, ok := dep.Provider.(provider.Embedder)
	if !ok {
		return nil, fmt.Errorf("cache: model %q does not support embeddings (provider %s)", modelName, dep.ProviderName)
	}

	return func(ctx context.Context, text string) (EmbedResult, error) {
		resp, err := embedder.Embed(ctx, &provider.EmbeddingRequest{
			Model: dep.ProviderModel,
			Input: []string{text},
		})
		if err != nil {
			return EmbedResult{}, err
		}
		if len(resp.Data) == 0 {
			return EmbedResult{}, fmt.Errorf("cache: empty embedding returned for model %q", modelName)
		}
		vec, err := toFloat32(resp.Data[0].Embedding)
		if err != nil {
			return EmbedResult{}, err
		}
		return EmbedResult{Vector: vec, Tokens: resp.Usage.PromptTokens}, nil
	}, nil
}

// toFloat32 converts the embedding interface{} to []float32.
func toFloat32(v interface{}) ([]float32, error) {
	switch e := v.(type) {
	case []interface{}:
		out := make([]float32, len(e))
		for i, val := range e {
			switch n := val.(type) {
			case float64:
				out[i] = float32(n)
			case float32:
				out[i] = n
			default:
				return nil, fmt.Errorf("cache: unexpected embedding element type %T", val)
			}
		}
		return out, nil
	case []float64:
		out := make([]float32, len(e))
		for i, v := range e {
			out[i] = float32(v)
		}
		return out, nil
	case []float32:
		return e, nil
	default:
		return nil, fmt.Errorf("cache: unexpected embedding type %T", v)
	}
}

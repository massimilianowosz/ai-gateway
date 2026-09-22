package provider

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

type mockFactory struct{}

func (f *mockFactory) Create(cfg config.ModelConfig) (Provider, error) {
	return &mockProvider{name: cfg.Provider}, nil
}

type mockProvider struct {
	name string
}

func (p *mockProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	return nil, nil
}
func (p *mockProvider) Stream(_ context.Context, _ *CompletionRequest) (StreamReader, error) {
	return &eofReader{}, nil
}
func (p *mockProvider) Name() string { return p.name }

func TestRegistry_GetDeployment(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o-2024-11-20"},
		{Name: "claude-sonnet", Provider: "vertex", ProviderModel: "claude-sonnet-4"},
	}

	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	dep, err := reg.GetDeployment("gpt-4o")
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", dep.ModelName)
	assert.Equal(t, "gpt-4o-2024-11-20", dep.ProviderModel)

	dep, err = reg.GetDeployment("claude-sonnet")
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet", dep.ModelName)

	_, err = reg.GetDeployment("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRegistry_ListModels(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"},
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"}, // duplicate
		{Name: "claude", Provider: "vertex", ProviderModel: "claude-sonnet-4"},
	}

	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	names := reg.ListModels()
	assert.Equal(t, []string{"gpt-4o", "claude"}, names)
}

func TestRegistry_MultipleDeployments(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"},
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"},
	}

	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	// Should be able to get a deployment (random selection from 2)
	dep, err := reg.GetDeployment("gpt-4o")
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", dep.ModelName)
}

func TestRegistry_ProvidersFor_ReturnsDistinctProviderNamesAcrossDeployments(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"},
		{Name: "gpt-4o", Provider: "openai", ProviderModel: "gpt-4o"},
		{Name: "gpt-4o", Provider: "openai", ProviderModel: "gpt-4o"}, // duplicate provider
	}

	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"azure_openai", "openai"}, reg.ProvidersFor("gpt-4o"))
}

func TestRegistry_ProvidersFor_ResolvesAliasesToTheirTargetsDeployments(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"},
	}
	reg, err := NewRegistryWithAliases(models, map[string]string{"gpt-4o-alias": "gpt-4o"}, &mockFactory{})
	require.NoError(t, err)

	assert.Equal(t, []string{"azure_openai"}, reg.ProvidersFor("gpt-4o-alias"))
}

func TestRegistry_ProvidersFor_UnknownModelReturnsNil(t *testing.T) {
	reg, err := NewRegistry(nil, &mockFactory{})
	require.NoError(t, err)
	assert.Nil(t, reg.ProvidersFor("nonexistent"))
}

func TestRegistry_ProvidersFor_ProviderProfileWinsOverProviderType(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderProfile: "openai-eu", ProviderModel: "gpt-4o"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)
	assert.Equal(t, []string{"openai-eu"}, reg.ProvidersFor("gpt-4o"))
}

func TestRegistry_AllDeploymentsEU_TrueWhenEveryDeploymentIsEU(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o", IsEU: true},
		{Name: "gpt-4o", Provider: "openai", ProviderModel: "gpt-4o", IsEU: true},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	allEU, hasDeployments := reg.AllDeploymentsEU("gpt-4o")
	assert.True(t, hasDeployments)
	assert.True(t, allEU)
}

func TestRegistry_AllDeploymentsEU_FalseWhenAnyDeploymentIsNotEU(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o", IsEU: true},
		{Name: "gpt-4o", Provider: "openai", ProviderModel: "gpt-4o", IsEU: false},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	allEU, hasDeployments := reg.AllDeploymentsEU("gpt-4o")
	assert.True(t, hasDeployments)
	assert.False(t, allEU)
}

func TestRegistry_AllDeploymentsEU_UnknownModelHasNoDeployments(t *testing.T) {
	reg, err := NewRegistry(nil, &mockFactory{})
	require.NoError(t, err)

	allEU, hasDeployments := reg.AllDeploymentsEU("nonexistent")
	assert.False(t, hasDeployments)
	assert.False(t, allEU)
}

func TestRegistry_ModelAliasesResolveWithoutDuplicatingDeployments(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "azure-gpt-4o", Provider: "azure_openai", ProviderModel: "gpt-4o"},
	}
	reg, err := NewRegistryWithAliases(models, map[string]string{"gpt-4o": "azure-gpt-4o"}, &mockFactory{})
	require.NoError(t, err)

	assert.Equal(t, "azure-gpt-4o", reg.CanonicalModel("gpt-4o"))
	assert.Equal(t, "unknown", reg.CanonicalModel("unknown"))
	assert.Equal(t, []string{"gpt-4o"}, reg.ListModels())

	canonical, err := reg.GetDeployments("azure-gpt-4o")
	require.NoError(t, err)
	alias, err := reg.GetDeployments("gpt-4o")
	require.NoError(t, err)
	require.Len(t, canonical, 1)
	require.Len(t, alias, 1)
	assert.Same(t, canonical[0], alias[0])
}

func TestRegistry_GetDeploymentMatchingNeverSelectsAnIneligibleProvider(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "mixed", Provider: "openai", ProviderModel: "m"},
		{Name: "mixed", Provider: "azure_openai", ProviderModel: "m"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		dep, getErr := reg.GetDeploymentMatching("mixed", func(dep *Deployment) bool {
			return dep.ProviderName == "azure_openai"
		})
		require.NoError(t, getErr)
		assert.Equal(t, "azure_openai", dep.ProviderName)
	}
	_, err = reg.GetDeploymentMatching("mixed", func(*Deployment) bool { return false })
	assert.ErrorContains(t, err, "no authorized deployments")
}

func TestRegistry_ModelAliasesRespectCanonicalVisibilityRules(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "public-internal", Provider: "azure_openai", ProviderModel: "public"},
		{Name: "restricted-internal", Provider: "azure_openai", ProviderModel: "restricted", Restricted: true},
	}
	aliases := map[string]string{
		"public":     "public-internal",
		"restricted": "restricted-internal",
	}
	reg, err := NewRegistryWithAliases(models, aliases, &mockFactory{})
	require.NoError(t, err)

	assert.True(t, reg.IsRestricted("restricted"))
	assert.Equal(t, []string{"public"}, reg.ListVisibleModels([]string{"public"}, nil))
	assert.ElementsMatch(t,
		[]string{"public", "restricted"},
		reg.ListVisibleModels(nil, []string{"restricted"}),
	)
}

func TestRegistry_RejectsInvalidModelAliases(t *testing.T) {
	models := []config.ModelConfig{{Name: "m1", Provider: "mock", ProviderModel: "upstream"}}
	tests := []struct {
		name    string
		aliases map[string]string
	}{
		{name: "unknown target", aliases: map[string]string{"friendly": "missing"}},
		{name: "configured name collision", aliases: map[string]string{"m1": "m1"}},
		{name: "alias chain", aliases: map[string]string{"friendly": "middle", "middle": "m1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRegistryWithAliases(models, tt.aliases, &mockFactory{})
			assert.Error(t, err)
		})
	}
}

type failingFactory struct {
	failNames map[string]bool
}

func (f *failingFactory) Create(cfg config.ModelConfig) (Provider, error) {
	if f.failNames[cfg.Name] {
		return nil, fmt.Errorf("simulated init failure for %s", cfg.Name)
	}
	return &mockProvider{name: cfg.Provider}, nil
}

func TestRegistry_PartialStartupFailure_SkipsFailedModelsButKeepsOthers(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "good-model", Provider: "azure_openai"},
		{Name: "bad-model", Provider: "azure_openai"},
	}

	reg, err := NewRegistry(models, &failingFactory{failNames: map[string]bool{"bad-model": true}})
	require.NoError(t, err)

	_, err = reg.GetDeployment("good-model")
	require.NoError(t, err)

	_, err = reg.GetDeployment("bad-model")
	require.Error(t, err)

	assert.Equal(t, []string{"good-model"}, reg.ListModels())
}

func TestRegistry_AllModelsFail_ReturnsError(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "bad-1", Provider: "azure_openai"},
		{Name: "bad-2", Provider: "azure_openai"},
	}

	_, err := NewRegistry(models, &failingFactory{failNames: map[string]bool{"bad-1": true, "bad-2": true}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 2 models failed to register")
}

func TestRegistry_EmptyModelsList_NoErrorEmptyRegistry(t *testing.T) {
	reg, err := NewRegistry(nil, &mockFactory{})
	require.NoError(t, err)
	assert.Empty(t, reg.ListModels())

	_, err = reg.GetDeployment("anything")
	require.Error(t, err)
}

func TestRegistry_GetDeployments_ReturnsCopyOfAllDeploymentsForModel(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai"},
		{Name: "gpt-4o", Provider: "vertex"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	deps, err := reg.GetDeployments("gpt-4o")
	require.NoError(t, err)
	require.Len(t, deps, 2)

	// Mutating the returned slice must not affect the registry's internal state.
	deps[0] = nil
	deps2, err := reg.GetDeployments("gpt-4o")
	require.NoError(t, err)
	assert.NotNil(t, deps2[0])

	_, err = reg.GetDeployments("nonexistent")
	require.Error(t, err)
}

func TestRegistry_ListVisibleModels_PublicRestrictedAndAllowlist(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "public-model", Provider: "azure_openai"},
		{Name: "restricted-model", Provider: "azure_openai", Restricted: true},
		{Name: "another-public", Provider: "azure_openai"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	// No allowlist, no grants: only public models visible.
	visible := reg.ListVisibleModels(nil, nil)
	assert.ElementsMatch(t, []string{"public-model", "another-public"}, visible)

	// Restricted model becomes visible only when explicitly granted.
	visible = reg.ListVisibleModels(nil, []string{"restricted-model"})
	assert.ElementsMatch(t, []string{"public-model", "another-public", "restricted-model"}, visible)

	// allowedModels narrows public visibility.
	visible = reg.ListVisibleModels([]string{"public-model"}, nil)
	assert.Equal(t, []string{"public-model"}, visible)

	// allowedModels does not grant access to restricted models.
	visible = reg.ListVisibleModels([]string{"restricted-model"}, nil)
	assert.Empty(t, visible)
}

// An emptied whitelist used to be indistinguishable from an absent one, so a
// tenant with every model turned off still saw — and could call — all of them.
func TestRegistry_ListVisibleModels_EmptyAllowlistHidesEverything(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "public-model", Provider: "azure_openai"},
		{Name: "restricted-model", Provider: "azure_openai", Restricted: true},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	assert.Empty(t, reg.ListVisibleModels([]string{}, nil),
		"an empty whitelist grants nothing")
	assert.ElementsMatch(t, []string{"public-model"}, reg.ListVisibleModels(nil, nil),
		"an absent whitelist still shows the public catalog")

	// granted_models is a separate axis and survives an empty whitelist.
	assert.ElementsMatch(t, []string{"restricted-model"},
		reg.ListVisibleModels([]string{}, []string{"restricted-model"}))
}

func TestRegistry_IsRestricted(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "restricted-model", Provider: "azure_openai", Restricted: true},
		{Name: "public-model", Provider: "azure_openai"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	assert.True(t, reg.IsRestricted("restricted-model"))
	assert.False(t, reg.IsRestricted("public-model"))
	assert.False(t, reg.IsRestricted("unknown-model"))
}

func TestRegistry_AddDeployment_IsIdempotentInAllModelsList(t *testing.T) {
	reg, err := NewRegistry(nil, &mockFactory{})
	require.NoError(t, err)

	reg.AddDeployment("live-model", &Deployment{ModelName: "live-model"})
	reg.AddDeployment("live-model", &Deployment{ModelName: "live-model"})

	assert.Equal(t, []string{"live-model"}, reg.ListModels())
	deps, err := reg.GetDeployments("live-model")
	require.NoError(t, err)
	assert.Len(t, deps, 2, "AddDeployment should append deployments even though allModels stays deduped")
}

func TestRegistry_AddDeployment_ReplacesAnIdentifiedDeployment(t *testing.T) {
	// Edge runtimes re-register on every heartbeat. Appending each time grew
	// one model's deployment list without bound, all entries pointing at the
	// same endpoint.
	reg, err := NewRegistry(nil, &mockFactory{})
	require.NoError(t, err)

	reg.AddDeployment("edge/qwen", &Deployment{ID: "edge-edge/qwen", ModelName: "edge/qwen", ProviderModel: "qwen"})
	reg.AddDeployment("edge/qwen", &Deployment{ID: "edge-edge/qwen", ModelName: "edge/qwen", ProviderModel: "qwen-updated"})

	deps, err := reg.GetDeployments("edge/qwen")
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "qwen-updated", deps[0].ProviderModel)
	assert.Equal(t, []string{"edge/qwen"}, reg.ListModels())
}

func TestRegistry_RemoveDeployments(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "gpt-4o", Provider: "azure_openai"},
		{Name: "claude", Provider: "vertex"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	reg.RemoveDeployments("gpt-4o")

	assert.Equal(t, []string{"claude"}, reg.ListModels())
	_, err = reg.GetDeployment("gpt-4o")
	require.Error(t, err)
}

func TestRegistry_ProviderProfileFallsBackToProviderType(t *testing.T) {
	models := []config.ModelConfig{
		{Name: "m1", Provider: "azure_openai", ProviderProfile: "azure-eu-1"},
		{Name: "m2", Provider: "azure_openai"},
	}
	reg, err := NewRegistry(models, &mockFactory{})
	require.NoError(t, err)

	dep1, err := reg.GetDeployment("m1")
	require.NoError(t, err)
	assert.Equal(t, "azure-eu-1", dep1.ProviderName)

	dep2, err := reg.GetDeployment("m2")
	require.NoError(t, err)
	assert.Equal(t, "azure_openai", dep2.ProviderName)
}

func TestRegistry_ConcurrentReadsAndWrites(t *testing.T) {
	reg, err := NewRegistry(nil, &mockFactory{})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("model-%d", i%3)
			reg.AddDeployment(name, &Deployment{ModelName: name})
			_, _ = reg.GetDeployment(name)
			reg.ListModels()
			reg.IsRestricted(name)
		}(i)
	}
	wg.Wait()
}

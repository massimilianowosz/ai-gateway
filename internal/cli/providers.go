package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"gopkg.in/yaml.v3"
)

var cliEnvVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

func (a *App) providers(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return a.providerList(ctx)
	}
	switch args[0] {
	case "add":
		return fmt.Errorf("usage: providers add (interactive)")
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: providers rm <name>")
		}
		return fmt.Errorf("usage: providers rm (interactive)")
	default:
		return fmt.Errorf("unknown providers command %q; try: providers add|list|rm", args[0])
	}
}

func (a *App) removeProvider(reader lineReader, out io.Writer, name string) error {
	configPath := ConfigFilePath()

	// Check provider exists
	providers := configuredProviders()
	if _, ok := providers[name]; !ok {
		return fmt.Errorf("provider %q not found in config", name)
	}

	// Find models using this provider
	affectedModels := modelsUsingProvider(name)

	if len(affectedModels) > 0 {
		fmt.Fprintf(out, "  %s provider %s has %d model(s): %s\n", yellow("!"), bold(name), len(affectedModels), strings.Join(affectedModels, ", "))
		confirm, ok := reader.ReadLine(fmt.Sprintf("  Remove provider and %d model(s)? [y/N]: ", len(affectedModels)))
		if !ok || (strings.ToLower(strings.TrimSpace(confirm)) != "y" && strings.ToLower(strings.TrimSpace(confirm)) != "yes") {
			fmt.Fprintln(out, dim("  cancelled"))
			return nil
		}
	} else {
		confirm, ok := reader.ReadLine(fmt.Sprintf("  Remove provider %s? [y/N]: ", bold(name)))
		if !ok || (strings.ToLower(strings.TrimSpace(confirm)) != "y" && strings.ToLower(strings.TrimSpace(confirm)) != "yes") {
			fmt.Fprintln(out, dim("  cancelled"))
			return nil
		}
	}

	// Remove models first
	for _, modelName := range affectedModels {
		removeModelsWithProvider(configPath, name)
		_ = modelName
	}

	// Remove provider
	remaining, err := removeProviderFromConfig(configPath, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "  %s removed provider %s", green("✓"), bold(name))
	if len(affectedModels) > 0 {
		fmt.Fprintf(out, " and %d model(s)", len(affectedModels))
	}
	fmt.Fprintf(out, " (%d providers remaining)\n", remaining)
	promptRestart(reader, out)
	return nil
}

func modelsUsingProvider(providerName string) []string {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return nil
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	var names []string
	for _, m := range cfg.Models {
		if m.Provider == providerName {
			names = append(names, m.Name)
		}
	}
	return names
}

func removeModelsWithProvider(configPath, providerName string) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return
	}
	root, err := documentRoot(doc)
	if err != nil {
		return
	}
	models, err := modelsSequence(root, false)
	if err != nil || models == nil {
		return
	}
	filtered := make([]*yaml.Node, 0, len(models.Content))
	for _, node := range models.Content {
		if modelProviderFromNode(node) != providerName {
			filtered = append(filtered, node)
		}
	}
	models.Content = filtered
	_ = writeConfigDocument(configPath, doc)
}

func modelProviderFromNode(model *yaml.Node) string {
	if model == nil || model.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(model.Content); i += 2 {
		if model.Content[i].Value == "provider" {
			return model.Content[i+1].Value
		}
	}
	return ""
}

func removeProviderFromConfig(configPath, name string) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	providers, err := providersMapping(root, false)
	if err != nil {
		return 0, err
	}
	if providers == nil {
		return 0, fmt.Errorf("provider %q not found in config", name)
	}
	for i := 0; i+1 < len(providers.Content); i += 2 {
		if providers.Content[i].Value == name {
			providers.Content = append(providers.Content[:i], providers.Content[i+2:]...)
			if err := writeConfigDocument(configPath, doc); err != nil {
				return 0, err
			}
			return len(providers.Content) / 2, nil
		}
	}
	return len(providers.Content) / 2, fmt.Errorf("provider %q not found in config", name)
}

func (a *App) addProvider(reader lineReader, out io.Writer) error {
	configPath := ConfigFilePath()
	count, err := providerCountInConfig(configPath)
	if err != nil {
		return err
	}

	name, provider, done := promptProvider(reader, out, count+1)
	if done {
		return nil
	}
	total, err := upsertProviderToConfig(configPath, name, provider)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "  %s added provider %s to %s (%d providers total)\n", green("✓"), bold(name), configPath, total)
	fmt.Fprintln(out, dim("  add models with model add"))
	return nil
}

func (a *App) providerList(ctx context.Context) error {
	providers := configuredProviders()
	if len(providers) == 0 {
		fmt.Fprintln(a.out, yellow("no providers configured"))
		return nil
	}

	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tENDPOINT\tSTATUS\tDETAIL")
	for _, name := range sortedProviderNames(providers) {
		p := providers[name]
		status, detail := checkProviderStatus(ctx, p)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, p.Type, emptyDash(providerEndpoint(p)), status, detail)
	}
	return tw.Flush()
}

func checkProviderStatus(ctx context.Context, provider initProvider) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	models, err := providerModels(ctx, provider)
	if err != nil {
		if errorsIsUnsupported(err) {
			return dim("unknown"), err.Error()
		}
		return red("error"), err.Error()
	}
	return green("ok"), fmt.Sprintf("%d models", len(models))
}

func errorsIsUnsupported(err error) bool {
	return strings.HasPrefix(err.Error(), "status check unavailable")
}

func providerModels(ctx context.Context, provider initProvider) ([]string, error) {
	endpoint := providerModelsEndpoint(provider)
	if endpoint == "" {
		return nil, fmt.Errorf("status check unavailable for %s", provider.Type)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	switch provider.Type {
	case "azure_openai":
		if provider.APIKey != "" {
			req.Header.Set("api-key", provider.APIKey)
		}
	case "anthropic":
		if provider.APIKey != "" {
			req.Header.Set("x-api-key", provider.APIKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if provider.APIKey != "" && provider.APIKey != "ollama" && provider.Type != "google_ai" {
			req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(payload.Data)+len(payload.Models))
	for _, model := range payload.Data {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	for _, model := range payload.Models {
		switch {
		case model.Name != "":
			ids = append(ids, model.Name)
		case model.ID != "":
			ids = append(ids, model.ID)
		case model.DisplayName != "":
			ids = append(ids, model.DisplayName)
		}
	}
	return ids, nil
}

func providerModelsEndpoint(provider initProvider) string {
	switch provider.Type {
	case "openai":
		base := provider.APIBase
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		return strings.TrimRight(base, "/") + "/models"
	case "openai_compatible":
		if provider.APIBase == "" {
			return ""
		}
		return strings.TrimRight(provider.APIBase, "/") + "/models"
	case "azure_openai":
		if provider.APIBase == "" {
			return ""
		}
		version := provider.APIVersion
		if version == "" {
			version = "2024-10-21"
		}
		return strings.TrimRight(provider.APIBase, "/") + "/openai/models?api-version=" + version
	case "anthropic":
		base := provider.APIBase
		if base == "" {
			base = "https://api.anthropic.com"
		}
		return strings.TrimRight(base, "/") + "/v1/models"
	case "google_ai":
		if provider.APIKey == "" {
			return ""
		}
		return "https://generativelanguage.googleapis.com/v1beta/models?key=" + provider.APIKey
	default:
		return ""
	}
}

func providerEndpoint(provider initProvider) string {
	if provider.APIBase != "" {
		return provider.APIBase
	}
	switch provider.Type {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com"
	case "google_ai":
		return "https://generativelanguage.googleapis.com"
	case "bedrock":
		return provider.Region
	case "github_copilot":
		return "https://api.githubcopilot.com"
	case "vertex":
		if provider.Project == "" && provider.Location == "" {
			return ""
		}
		return strings.Trim(provider.Project+"/"+provider.Location, "/")
	default:
		return ""
	}
}

func fetchProviderModelsForConfig(provider initProvider) []string {
	models, err := providerModels(context.Background(), provider)
	if err != nil {
		return nil
	}
	sort.Strings(models)
	return models
}

func expandEnvVars(data []byte) []byte {
	return cliEnvVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		varName := cliEnvVarPattern.FindSubmatch(match)[1]
		if val, ok := os.LookupEnv(string(varName)); ok {
			return []byte(val)
		}
		return match
	})
}

func configuredProviders() map[string]initProvider {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return nil
	}
	data = expandEnvVars(data)
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	return cfg.Providers
}

type modelDisplay struct {
	Name                 string
	Provider             string
	Type                 string
	ProviderModel        string
	Endpoint             string
	InputCostPerMillion  float64 // 0 = unknown
	OutputCostPerMillion float64 // 0 = unknown
	PriceSource          string  // "config", "catalog", or ""
}

func configuredModelDisplays() []modelDisplay {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return nil
	}
	data = expandEnvVars(data)
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	out := make([]modelDisplay, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		display := modelDisplay{
			Name:          model.Name,
			Provider:      model.Provider,
			Type:          model.Provider,
			ProviderModel: model.ProviderModel,
			Endpoint:      model.APIBase,
		}
		if provider, ok := cfg.Providers[model.Provider]; ok {
			display.Type = provider.Type
			display.Endpoint = providerEndpoint(provider)
		}
		// Pricing: explicit config takes priority, then catalog lookup
		switch {
		case model.InputCostPerMillion > 0 || model.OutputCostPerMillion > 0:
			display.InputCostPerMillion = model.InputCostPerMillion
			display.OutputCostPerMillion = model.OutputCostPerMillion
			display.PriceSource = "config"
		default:
			// Try catalog: provider model ID first, then gateway name
			lookup := model.ProviderModel
			if lookup == "" {
				lookup = model.Name
			}
			if p, ok := pricing.LookupEmbedded(lookup); ok {
				display.InputCostPerMillion = p.InputCostPerToken * 1_000_000
				display.OutputCostPerMillion = p.OutputCostPerToken * 1_000_000
				display.PriceSource = "catalog"
			} else if p, ok := pricing.LookupEmbedded(model.Name); ok {
				display.InputCostPerMillion = p.InputCostPerToken * 1_000_000
				display.OutputCostPerMillion = p.OutputCostPerToken * 1_000_000
				display.PriceSource = "catalog"
			}
		}
		out = append(out, display)
	}
	return out
}

func sortedProviderNames(providers map[string]initProvider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func providerCountInConfig(configPath string) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	providers := mappingValue(root, "providers")
	if providers == nil {
		return 0, nil
	}
	if providers.Kind != yaml.MappingNode {
		return 0, fmt.Errorf("config providers must be a YAML map")
	}
	return len(providers.Content) / 2, nil
}

func upsertProviderToConfig(configPath, name string, provider initProvider) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	providers, err := providersMapping(root, true)
	if err != nil {
		return 0, err
	}
	providerNode, err := providerYAMLNode(provider)
	if err != nil {
		return 0, err
	}
	for i := 0; i+1 < len(providers.Content); i += 2 {
		if providers.Content[i].Value == name {
			providers.Content[i+1] = providerNode
			if err := writeConfigDocument(configPath, doc); err != nil {
				return 0, err
			}
			return len(providers.Content) / 2, nil
		}
	}
	providers.Content = append(providers.Content, stringNode(name), providerNode)
	if err := writeConfigDocument(configPath, doc); err != nil {
		return 0, err
	}
	return len(providers.Content) / 2, nil
}

func providersMapping(root *yaml.Node, create bool) (*yaml.Node, error) {
	providers := mappingValue(root, "providers")
	if providers == nil {
		if !create {
			return nil, nil
		}
		providers = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, stringNode("providers"), providers)
		return providers, nil
	}
	if providers.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config providers must be a YAML map")
	}
	return providers, nil
}

func providerYAMLNode(provider initProvider) (*yaml.Node, error) {
	data, err := yaml.Marshal(provider)
	if err != nil {
		return nil, fmt.Errorf("marshal provider: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse provider YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("provider YAML is empty")
	}
	return doc.Content[0], nil
}

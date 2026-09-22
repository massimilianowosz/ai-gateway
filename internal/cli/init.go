package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/pricing"
	"gopkg.in/yaml.v3"
)

type initConfig struct {
	Server struct {
		Port      int    `yaml:"port"`
		MasterKey string `yaml:"master_key"`
		Swagger   bool   `yaml:"swagger,omitempty"`
	} `yaml:"server"`
	Providers map[string]initProvider `yaml:"providers,omitempty"`
	Models    []initModel             `yaml:"models,omitempty"`
	Router    initRouter              `yaml:"router"`
	Cache     *initCache              `yaml:"cache,omitempty"`
	HiveState *initHiveState          `yaml:"hivestate,omitempty"`
}

type initCache struct {
	Enabled        bool   `yaml:"enabled"`
	Backend        string `yaml:"backend,omitempty"`
	EmbeddingModel string `yaml:"embedding_model,omitempty"`
	TweakModel     string `yaml:"tweak_model,omitempty"`
	RedisURL       string `yaml:"redis_url,omitempty"`
	QdrantURL      string `yaml:"qdrant_url,omitempty"`
}

type initHiveState struct {
	Enabled      bool           `yaml:"enabled"`
	Model        string         `yaml:"model,omitempty"`
	Threshold    int            `yaml:"threshold,omitempty"`
	MaxLatencyMs int            `yaml:"max_latency_ms,omitempty"`
	HiveRoute    *initHiveRoute `yaml:"hiveroute,omitempty"`
}

type initHiveRoute struct {
	Enabled         bool                 `yaml:"enabled"`
	Levels          []initHiveRouteLevel `yaml:"levels,omitempty"`
	ThinkingBudgets map[string]int       `yaml:"thinking_budgets,omitempty"`
}

type initHiveRouteLevel struct {
	Name        string `yaml:"name"`
	Model       string `yaml:"model"`
	Description string `yaml:"description,omitempty"`
}

type initProvider struct {
	Type         string `yaml:"type"`
	APIBase      string `yaml:"api_base,omitempty"`
	APIKey       string `yaml:"api_key,omitempty"`
	APISecret    string `yaml:"api_secret,omitempty"`
	APIVersion   string `yaml:"api_version,omitempty"`
	Project      string `yaml:"project,omitempty"`
	Location     string `yaml:"location,omitempty"`
	Region       string `yaml:"region,omitempty"`
	SessionToken string `yaml:"session_token,omitempty"`
}

type initModel struct {
	Name                 string   `yaml:"name"`
	Provider             string   `yaml:"provider"`
	ProviderModel        string   `yaml:"provider_model"`
	APIBase              string   `yaml:"api_base,omitempty"`
	APIKey               string   `yaml:"api_key,omitempty"`
	APISecret            string   `yaml:"api_secret,omitempty"`
	APIVersion           string   `yaml:"api_version,omitempty"`
	Project              string   `yaml:"project,omitempty"`
	Location             string   `yaml:"location,omitempty"`
	Region               string   `yaml:"region,omitempty"`
	SessionToken         string   `yaml:"session_token,omitempty"`
	DropParams           []string `yaml:"drop_params,omitempty"`
	InputCostPerMillion  float64  `yaml:"input_cost_per_million,omitempty"`
	OutputCostPerMillion float64  `yaml:"output_cost_per_million,omitempty"`
}

type initRouter struct {
	Strategy string `yaml:"strategy"`
	Retries  int    `yaml:"retries"`
}

func (a *App) initCmd(reader lineReader, out io.Writer) error {
	configPath := ConfigFilePath()

	// Check if config already exists
	if _, err := os.Stat(configPath); err == nil {
		ans, ok := reader.ReadLine(fmt.Sprintf("%s %s already exists. Overwrite? [y/N] ", yellow("!"), configPath))
		if !ok {
			return nil
		}
		ans = strings.TrimSpace(strings.ToLower(ans))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(out, dim("cancelled"))
			return nil
		}
	}

	cfg := initConfig{}

	// Port
	fmt.Fprintf(out, "\n%s\n", bold("Server"))
	if val, ok := reader.ReadLine("  Port [4000]: "); ok {
		if p := strings.TrimSpace(val); p != "" {
			if port, err := strconv.Atoi(p); err == nil {
				cfg.Server.Port = port
			}
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 4000
	}

	// Master key
	masterKey := generateKey("sk-ubq-")
	if val, ok := reader.ReadLine("  Master key [auto-generate]: "); ok {
		if k := strings.TrimSpace(val); k != "" {
			masterKey = k
		}
	}
	cfg.Server.MasterKey = masterKey

	// Router
	cfg.Router.Strategy = "shuffle"
	cfg.Router.Retries = 3

	// Provider profiles
	fmt.Fprintf(out, "\n%s\n", bold("Providers"))
	fmt.Fprintln(out, dim("  Add provider credentials/endpoints. Leave name empty to finish."))
	fmt.Fprintln(out)

	cfg.Providers = make(map[string]initProvider)
	for {
		name, provider, done := promptProvider(reader, out, len(cfg.Providers)+1)
		if done {
			break
		}
		cfg.Providers[name] = provider
		fmt.Fprintf(out, "  %s added provider %s (%s)\n\n", green("✓"), bold(name), provider.Type)
	}
	if len(cfg.Providers) == 0 {
		cfg.Providers = nil
	}

	// Models
	fmt.Fprintf(out, "\n%s\n", bold("Models"))
	if len(cfg.Providers) == 0 {
		fmt.Fprintln(out, dim("  No providers configured; add providers later with providers add."))
	} else {
		fmt.Fprintln(out, dim("  Add at least one model. Leave name empty to finish."))
	}
	fmt.Fprintln(out)

	for len(cfg.Providers) > 0 {
		model, done := promptModel(reader, out, len(cfg.Models)+1, cfg.Providers)
		if done {
			break
		}
		cfg.Models = append(cfg.Models, model)
		fmt.Fprintf(out, "  %s added %s (%s)\n\n", green("✓"), bold(model.Name), model.Provider)
	}

	// Write config
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// Make the output prettier with a header comment
	header := "# Ubiquum Gateway configuration\n# Generated by: ubiquum init\n# Docs: https://docs.ubiquum.io/gateway/config\n\n"
	if err := os.WriteFile(configPath, []byte(header+string(data)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	abs, _ := filepath.Abs(configPath)
	fmt.Fprintf(out, "\n%s config written to %s\n", green("✓"), bold(abs))
	fmt.Fprintf(out, "  master key: %s %s\n", bold(masterKey), dim("(save this!)"))
	fmt.Fprintf(out, "  models:     %d\n", len(cfg.Models))

	// Collect team info
	fmt.Fprintf(out, "\n%s\n", bold("Team"))
	fmt.Fprintln(out, dim("  A team groups API keys for billing/access control."))
	fmt.Fprintln(out)
	teamID := "default"
	if val, ok := reader.ReadLine("  Team name [default]: "); ok && strings.TrimSpace(val) != "" {
		teamID = strings.TrimSpace(val)
	}
	// Team budgets keep the zero-means-unlimited reading: the per-key gate is
	// what makes a key callable, and a team cap is an additional ceiling.
	var teamBudget float64
	if val, ok := reader.ReadLine("  Team budget in USD (0 = unlimited) [0]: "); ok && val != "" {
		if b, err := strconv.ParseFloat(val, 64); err == nil {
			teamBudget = b
		}
	}

	// Collect first API key info
	fmt.Fprintf(out, "\n%s\n", bold("API Key"))
	fmt.Fprintln(out, dim("  Generate the first API key for this team."))
	fmt.Fprintln(out)

	var keyReq CreateKeyRequest
	keyReq.TeamID = teamID

	// Key name
	keyName := "dev-key"
	if val, ok := reader.ReadLine("  Key name [dev-key]: "); ok && strings.TrimSpace(val) != "" {
		keyName = strings.TrimSpace(val)
	}
	keyReq.Name = keyName

	// Key budget — required: a key with none is blocked by the gateway.
	keyReq.Budget = defaultKeyBudget
	if val, ok := reader.ReadLine(fmt.Sprintf("  Key budget in USD (required) [%g]: ", defaultKeyBudget)); ok && val != "" {
		if b, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && b > 0 {
			keyReq.Budget = b
		}
	}

	// Models (with autocompletion from configured models)
	modelNames := configuredModelNames()
	if len(modelNames) > 0 {
		fmt.Fprintf(out, "  %s\n", dim("Select models (empty = all, Tab to autocomplete)"))
		for i := 1; ; i++ {
			remaining := make([]string, 0, len(modelNames))
			for _, n := range modelNames {
				found := false
				for _, sel := range keyReq.Models {
					if sel == n {
						found = true
						break
					}
				}
				if !found {
					remaining = append(remaining, n)
				}
			}
			if len(remaining) == 0 {
				break
			}
			val, ok := reader.ReadLineWithHints(fmt.Sprintf("  [model %d]: ", i), remaining)
			if !ok || val == "" {
				break
			}
			keyReq.Models = append(keyReq.Models, strings.TrimSpace(val))
		}
	}

	// Rate limit
	if val, ok := reader.ReadLine("  Rate limit (req/min, 0 = none) [0]: "); ok && val != "" {
		if r, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			keyReq.RateLimit = r
		}
	}

	// Start/restart gateway
	fmt.Fprintln(out)
	promptRestart(reader, out)

	// Create team + key via API (gateway must be running)
	time.Sleep(500 * time.Millisecond)
	client := &Client{
		baseURL: fmt.Sprintf("http://localhost:%d", cfg.Server.Port),
		apiKey:  masterKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}

	// Create team record in DB
	_, teamErr := client.CreateTeam(context.Background(), CreateTeamRequest{
		Name:   teamID,
		Budget: teamBudget,
	})
	if teamErr != nil {
		fmt.Fprintf(out, "  %s could not create team: %v\n", yellow("!"), teamErr)
	} else {
		fmt.Fprintf(out, "  %s team %s created\n", green("✓"), bold(teamID))
	}

	// Create API key for the team
	key, err := client.CreateKey(context.Background(), keyReq)
	if err != nil {
		fmt.Fprintf(out, "  %s could not create key: %v\n", yellow("!"), err)
		fmt.Fprintln(out, dim("  create it later with: teams add"))
	} else {
		if key.Key != "" {
			fmt.Fprintf(out, "  key: %s %s\n", bold(key.Key), dim("(save this!)"))
		}
	}

	return nil
}

func promptProvider(reader lineReader, out io.Writer, n int) (string, initProvider, bool) {
	var p initProvider

	name, ok := reader.ReadLine(fmt.Sprintf("  [provider %d] Name (e.g. openai): ", n))
	if !ok {
		return "", p, true
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", p, true
	}

	providerTypes := []string{"openai", "azure_openai", "anthropic", "vertex", "google_ai", "bedrock", "github_copilot", "ollama", "openai_compatible"}
	fmt.Fprintln(out, dim("    Types: "+strings.Join(providerTypes, ", ")))
	if val, ok := reader.ReadLineWithHints("    Type: ", providerTypes); ok {
		p.Type = strings.TrimSpace(val)
	}
	promptProviderSettings(reader, &p)
	return name, p, false
}

func promptProviderSettings(reader lineReader, p *initProvider) {
	switch p.Type {
	case "azure_openai":
		if val, ok := reader.ReadLine("    Endpoint (api_base): "); ok {
			p.APIBase = strings.TrimSpace(val)
		}
		if val, ok := reader.ReadLine("    API key: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}
		if val, ok := reader.ReadLine("    API version [2024-10-21]: "); ok {
			v := strings.TrimSpace(val)
			if v == "" {
				v = "2024-10-21"
			}
			p.APIVersion = v
		}

	case "openai":
		if val, ok := reader.ReadLine("    API key: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}

	case "openai_compatible":
		if val, ok := reader.ReadLine("    Base URL (api_base): "); ok {
			p.APIBase = strings.TrimSpace(val)
		}
		if val, ok := reader.ReadLine("    API key: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}

	case "github_copilot":
		if val, ok := reader.ReadLine("    Base URL [https://api.githubcopilot.com]: "); ok {
			v := strings.TrimSpace(val)
			if v == "" {
				v = "https://api.githubcopilot.com"
			}
			p.APIBase = v
		}
		if val, ok := reader.ReadLine("    API token [optional; usually forwarded by ubiquum-cli]: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}

	case "ollama":
		p.Type = "openai_compatible"
		if val, ok := reader.ReadLine("    Base URL [http://localhost:11434/v1]: "); ok {
			v := strings.TrimSpace(val)
			if v == "" {
				v = "http://localhost:11434/v1"
			}
			p.APIBase = v
		}
		p.APIKey = "ollama"

	case "anthropic", "google_ai":
		if val, ok := reader.ReadLine("    API key: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}

	case "vertex":
		if val, ok := reader.ReadLine("    GCP project: "); ok {
			p.Project = strings.TrimSpace(val)
		}
		if val, ok := reader.ReadLine("    Location [us-central1]: "); ok {
			loc := strings.TrimSpace(val)
			if loc == "" {
				loc = "us-central1"
			}
			p.Location = loc
		}
		if val, ok := reader.ReadLine("    Credentials JSON/path [ADC]: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}

	case "bedrock":
		if val, ok := reader.ReadLine("    AWS region [us-east-1]: "); ok {
			r := strings.TrimSpace(val)
			if r == "" {
				r = "us-east-1"
			}
			p.Region = r
		}
		if val, ok := reader.ReadLine("    Access key / bearer token: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}
		if val, ok := reader.ReadLine("    Secret access key [optional]: "); ok {
			p.APISecret = strings.TrimSpace(val)
		}

	default:
		if val, ok := reader.ReadLine("    API base URL: "); ok {
			p.APIBase = strings.TrimSpace(val)
		}
		if val, ok := reader.ReadLine("    API key: "); ok {
			p.APIKey = strings.TrimSpace(val)
		}
	}
}

func promptModel(reader lineReader, out io.Writer, n int, providers map[string]initProvider) (initModel, bool) {
	var m initModel

	name, ok := reader.ReadLine(fmt.Sprintf("  [model %d] Name (e.g. gpt-4o): ", n))
	if !ok {
		return m, true
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return m, true
	}
	m.Name = name

	if len(providers) > 0 {
		names := sortedProviderNames(providers)
		defaultProvider := names[0]
		if val, ok := reader.ReadLineWithHints(fmt.Sprintf("    Provider [%s]: ", defaultProvider), names); ok {
			selected := strings.TrimSpace(val)
			if selected == "" {
				selected = defaultProvider
			}
			provider, found := providers[selected]
			if found {
				m.Provider = selected
				models := fetchProviderModelsForConfig(provider)
				if val, ok := reader.ReadLineWithHints("    Model ID [Tab]: ", models); ok {
					m.ProviderModel = strings.TrimSpace(val)
				}
				promptModelPricing(reader, out, &m)
				return m, false
			}
			fmt.Fprintf(out, "    %s unknown provider %s\n", yellow("!"), selected)
			return m, true
		}
	}
	return m, true
}

// promptModelPricing asks for input/output cost per million tokens.
// It looks up the catalog default and pre-fills it so the user can just press Enter.
func promptModelPricing(reader lineReader, out io.Writer, m *initModel) {
	// Determine catalog default: try provider model ID first, then gateway name
	var defaultInput, defaultOutput float64
	lookupKey := m.ProviderModel
	if lookupKey == "" {
		lookupKey = m.Name
	}
	if p, ok := pricing.LookupEmbedded(lookupKey); ok {
		defaultInput = p.InputCostPerToken * 1_000_000
		defaultOutput = p.OutputCostPerToken * 1_000_000
	} else if p, ok := pricing.LookupEmbedded(m.Name); ok {
		defaultInput = p.InputCostPerToken * 1_000_000
		defaultOutput = p.OutputCostPerToken * 1_000_000
	}

	if defaultInput > 0 || defaultOutput > 0 {
		fmt.Fprintf(out, "    %s\n", dim(fmt.Sprintf("Pricing from catalog: $%.4f / $%.4f per 1M tokens (input/output)", defaultInput, defaultOutput)))
	} else {
		fmt.Fprintf(out, "    %s\n", dim("Pricing not found in catalog — enter manually (0 = unknown)"))
	}

	readPrice := func(label string, def float64) float64 {
		prompt := fmt.Sprintf("    %s", label)
		if def > 0 {
			prompt += fmt.Sprintf(" [%.4f]", def)
		} else {
			prompt += " [0]"
		}
		prompt += ": "
		val, ok := reader.ReadLine(prompt)
		if !ok || strings.TrimSpace(val) == "" {
			return def
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			return f
		}
		return def
	}

	m.InputCostPerMillion = readPrice("Input cost per 1M tokens ($)", defaultInput)
	m.OutputCostPerMillion = readPrice("Output cost per 1M tokens ($)", defaultOutput)
}

func (a *App) addModel(reader lineReader, out io.Writer) error {
	configPath := ConfigFilePath()

	modelCount, err := modelCountInConfig(configPath)
	if err != nil {
		return err
	}

	providers := configuredProviders()
	if len(providers) == 0 {
		return fmt.Errorf("no providers configured; run providers add first")
	}

	model, done := promptModel(reader, out, modelCount+1, providers)
	if done {
		return nil
	}
	total, err := appendModelToConfig(configPath, model)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "  %s added %s to %s (%d models total)\n", green("✓"), bold(model.Name), configPath, total)
	promptRestart(reader, out)
	return nil
}

func generateKey(prefix string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func configuredModelNames() []string {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return nil
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil
	}
	seen := make(map[string]bool, len(cfg.Models))
	names := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.Name == "" || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return names
}

func modelCountInConfig(configPath string) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	models := mappingValue(root, "models")
	if models == nil {
		return 0, nil
	}
	if models.Kind != yaml.SequenceNode {
		return 0, fmt.Errorf("config models must be a YAML list")
	}
	return len(models.Content), nil
}

func appendModelToConfig(configPath string, model initModel) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	models, err := modelsSequence(root, true)
	if err != nil {
		return 0, err
	}
	modelNode, err := modelYAMLNode(model)
	if err != nil {
		return 0, err
	}
	models.Content = append(models.Content, modelNode)
	if err := writeConfigDocument(configPath, doc); err != nil {
		return 0, err
	}
	return len(models.Content), nil
}

func removeModelFromConfig(configPath, name string) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	models, err := modelsSequence(root, false)
	if err != nil {
		return 0, err
	}
	if models == nil {
		return 0, fmt.Errorf("model %q not found in config", name)
	}
	for i, node := range models.Content {
		if modelNameFromNode(node) == name {
			models.Content = append(models.Content[:i], models.Content[i+1:]...)
			if err := writeConfigDocument(configPath, doc); err != nil {
				return 0, err
			}
			return len(models.Content), nil
		}
	}
	return len(models.Content), fmt.Errorf("model %q not found in config", name)
}

type modelUpdate struct {
	Field string
	Value string
}

func updateModelsInConfig(configPath, name string, updates []modelUpdate) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	models, err := modelsSequence(root, false)
	if err != nil {
		return 0, err
	}
	if models == nil {
		return 0, fmt.Errorf("model %q not found in config", name)
	}

	updated := 0
	for _, node := range models.Content {
		if modelNameFromNode(node) != name {
			continue
		}
		if node == nil || node.Kind != yaml.MappingNode {
			return 0, fmt.Errorf("model %q entry must be a YAML object", name)
		}
		applyModelUpdates(node, updates)
		updated++
	}
	if updated == 0 {
		return 0, fmt.Errorf("model %q not found in config", name)
	}
	if err := writeConfigDocument(configPath, doc); err != nil {
		return 0, err
	}
	return updated, nil
}

func unsetModelFieldsInConfig(configPath, name string, fields []string) (int, error) {
	doc, err := readConfigDocument(configPath)
	if err != nil {
		return 0, err
	}
	root, err := documentRoot(doc)
	if err != nil {
		return 0, err
	}
	models, err := modelsSequence(root, false)
	if err != nil {
		return 0, err
	}
	if models == nil {
		return 0, fmt.Errorf("model %q not found in config", name)
	}

	updated := 0
	for _, node := range models.Content {
		if modelNameFromNode(node) != name {
			continue
		}
		if node == nil || node.Kind != yaml.MappingNode {
			return 0, fmt.Errorf("model %q entry must be a YAML object", name)
		}
		for _, field := range fields {
			removeModelField(node, field)
		}
		updated++
	}
	if updated == 0 {
		return 0, fmt.Errorf("model %q not found in config", name)
	}
	if err := writeConfigDocument(configPath, doc); err != nil {
		return 0, err
	}
	return updated, nil
}

func applyModelUpdates(model *yaml.Node, updates []modelUpdate) {
	for _, update := range updates {
		switch update.Field {
		case "drop_params":
			setStringListField(model, update.Field, csv(update.Value))
		case "input_cost_per_million", "output_cost_per_million":
			setFloatField(model, update.Field, update.Value)
			removeMappingField(model, strings.TrimSuffix(update.Field, "_per_million")+"_per_token")
		default:
			setStringField(model, update.Field, update.Value)
		}
	}
}

func removeModelField(model *yaml.Node, field string) {
	switch field {
	case "price":
		removeMappingField(model, "input_cost_per_million")
		removeMappingField(model, "output_cost_per_million")
		removeMappingField(model, "input_cost_per_token")
		removeMappingField(model, "output_cost_per_token")
	default:
		removeMappingField(model, field)
	}
}

func setStringField(model *yaml.Node, field, value string) {
	setMappingField(model, field, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func setFloatField(model *yaml.Node, field, value string) {
	price, err := strconv.ParseFloat(value, 64)
	if err == nil {
		value = formatYAMLFloat(price)
		if price == 0 {
			removeMappingField(model, field)
			return
		}
	}
	setMappingField(model, field, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: value})
}

func formatYAMLFloat(value float64) string {
	out := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.ContainsAny(out, ".eE") {
		out += ".0"
	}
	return out
}

func setStringListField(model *yaml.Node, field string, values []string) {
	if len(values) == 0 {
		removeMappingField(model, field)
		return
	}
	list := &yaml.Node{Kind: yaml.SequenceNode}
	for _, value := range values {
		list.Content = append(list.Content, stringNode(value))
	}
	setMappingField(model, field, list)
}

func setMappingField(mapping *yaml.Node, field string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == field {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, stringNode(field), value)
}

func removeMappingField(mapping *yaml.Node, field string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == field {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func readConfigDocument(configPath string) (*yaml.Node, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config found; run /init first")
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &doc, nil
}

func writeConfigDocument(configPath string, doc *yaml.Node) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func documentRoot(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.MappingNode})
		}
		return doc.Content[0], nil
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.MappingNode
	}
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root must be a YAML object")
	}
	return doc, nil
}

func modelsSequence(root *yaml.Node, create bool) (*yaml.Node, error) {
	models := mappingValue(root, "models")
	if models == nil {
		if !create {
			return nil, nil
		}
		models = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, stringNode("models"), models)
		return models, nil
	}
	if models.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("config models must be a YAML list")
	}
	return models, nil
}

func mappingValue(root *yaml.Node, key string) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}

func modelYAMLNode(model initModel) (*yaml.Node, error) {
	data, err := yaml.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("marshal model: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse model YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("model YAML is empty")
	}
	return doc.Content[0], nil
}

func modelNameFromNode(model *yaml.Node) string {
	if model == nil || model.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(model.Content); i += 2 {
		if model.Content[i].Value == "name" {
			return model.Content[i+1].Value
		}
	}
	return ""
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

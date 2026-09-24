package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// envVarPattern matches ${VAR_NAME} patterns in config values.
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// Load reads and parses a gateway configuration file.
// Environment variables in the format ${VAR_NAME} are expanded.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// Expand environment variables. A missing secret must fail startup rather
	// than becoming the literal string "${NAME}", which would silently turn a
	// placeholder into a valid master key.
	var missing []string
	expanded := envVarPattern.ReplaceAllFunc(data, func(match []byte) []byte {
		varName := envVarPattern.FindSubmatch(match)[1]
		if val, ok := os.LookupEnv(string(varName)); ok {
			return []byte(val)
		}
		missing = append(missing, string(varName))
		return nil
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("reading config: required environment variable %s is not set", strings.Join(missing, ", "))
	}

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	applyDefaults(&cfg)

	if cfg.Server.MasterKey == "" {
		return nil, fmt.Errorf("validating config: server.master_key is required")
	}

	if err := resolveProviderProfiles(&cfg); err != nil {
		return nil, fmt.Errorf("resolving providers: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 4000
	}
	if cfg.Server.MaxRequestSizeMB == 0 {
		cfg.Server.MaxRequestSizeMB = 32
	}
	if cfg.Server.UpstreamTokenForwarding.Header == "" {
		cfg.Server.UpstreamTokenForwarding.Header = "X-Ubiquum-Upstream-Authorization"
	}
	if cfg.Server.UpstreamTokenForwarding.ProviderHeader == "" {
		cfg.Server.UpstreamTokenForwarding.ProviderHeader = "X-Ubiquum-Upstream-Provider"
	}
	if cfg.Console.SessionTTL <= 0 {
		cfg.Console.SessionTTL = 8 * time.Hour
	}
	dataDir := applianceDataDir()
	if cfg.Console.VaultPath == "" {
		cfg.Console.VaultPath = dataDir + string(os.PathSeparator) + "credentials.vault"
	}
	if cfg.Console.VaultKeyFile == "" {
		cfg.Console.VaultKeyFile = dataDir + string(os.PathSeparator) + "credentials.key"
	}
	if cfg.Database.PoolSize == 0 {
		cfg.Database.PoolSize = 10
	}
	if cfg.Database.BatchWriteInterval == 0 {
		cfg.Database.BatchWriteInterval = 60 * time.Second
	}
	if cfg.Router.Strategy == "" {
		cfg.Router.Strategy = "shuffle"
	}
	if cfg.Router.Retries == 0 {
		cfg.Router.Retries = 3
	}
	if cfg.Router.RetryDelay == 0 {
		cfg.Router.RetryDelay = 5 * time.Second
	}
	if cfg.Router.CircuitBreaker.Threshold == 0 {
		cfg.Router.CircuitBreaker.Threshold = 3
	}
	if cfg.Router.CircuitBreaker.Recovery == 0 {
		cfg.Router.CircuitBreaker.Recovery = 60 * time.Second
	}
	cfg.Cache.ApplyDefaults()
	cfg.HiveState.ApplyDefaults()
	cfg.LiveCompression.ApplyDefaults()
	cfg.Steering.ApplyDefaults()
	cfg.Feedback.ApplyDefaults()
	cfg.Workflow.ApplyDefaults()
	cfg.Responses.ApplyDefaults()
	cfg.HiveTrace.ApplyDefaults()

	for i := range cfg.Models {
		m := &cfg.Models[i]
		if m.AuthMode == "" {
			m.AuthMode = AuthModeAPIKey
		}
		if m.BillingMode == "" {
			m.BillingMode = BillingModeMetered
		}
	}
}

func applianceDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("UBIQUUM_HOME")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".ubiquum"
	}
	return home + string(os.PathSeparator) + ".ubiquum"
}

func resolveProviderProfiles(cfg *Config) error {
	for i := range cfg.Models {
		model := &cfg.Models[i]
		profileName := model.Provider
		if profileName == "" {
			continue
		}
		profile, ok := cfg.Providers[profileName]
		if !ok {
			return fmt.Errorf("models[%d].provider references unknown provider %q", i, profileName)
		}
		if profile.Type == "" {
			return fmt.Errorf("providers.%s.type is required", profileName)
		}

		model.ProviderProfile = profileName
		model.Provider = profile.Type
		if model.APIBase == "" {
			model.APIBase = profile.APIBase
		}
		if model.APIKey == "" {
			model.APIKey = profile.APIKey
		}
		if model.APISecret == "" {
			model.APISecret = profile.APISecret
		}
		if model.APIVersion == "" {
			model.APIVersion = profile.APIVersion
		}
		if model.Project == "" {
			model.Project = profile.Project
		}
		if model.Location == "" {
			model.Location = profile.Location
		}
		if model.Region == "" {
			model.Region = profile.Region
		}
		if model.SessionToken == "" {
			model.SessionToken = profile.SessionToken
		}
	}
	return nil
}

func validate(cfg *Config) error {
	if cfg.Server.MasterKey == "" {
		return fmt.Errorf("server.master_key is required")
	}
	for name, provider := range cfg.Providers {
		if name == "" {
			return fmt.Errorf("providers contains an empty name")
		}
		if provider.Type == "" {
			return fmt.Errorf("providers.%s.type is required", name)
		}
	}
	// Zero models is allowed — gateway starts and serves /health, /ready reports not-ready
	modelNames := make(map[string]struct{}, len(cfg.Models))
	for i, m := range cfg.Models {
		if m.Name == "" {
			return fmt.Errorf("models[%d].name is required", i)
		}
		if m.Provider == "" {
			return fmt.Errorf("models[%d].provider is required", i)
		}
		if m.ProviderModel == "" {
			return fmt.Errorf("models[%d].provider_model is required", i)
		}
		modelNames[m.Name] = struct{}{}
	}
	for alias, target := range cfg.ModelAliases {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("model_aliases contains an empty alias")
		}
		if strings.TrimSpace(target) == "" {
			return fmt.Errorf("model_aliases.%s target is required", alias)
		}
		if alias != strings.TrimSpace(alias) || target != strings.TrimSpace(target) {
			return fmt.Errorf("model_aliases.%s and its target must not contain surrounding whitespace", alias)
		}
		if alias == target {
			return fmt.Errorf("model_aliases.%s cannot point to itself", alias)
		}
		if _, exists := modelNames[alias]; exists {
			return fmt.Errorf("model_aliases.%s conflicts with a configured model name", alias)
		}
		if _, chained := cfg.ModelAliases[target]; chained {
			return fmt.Errorf("model_aliases.%s must point directly to a configured model, not alias %q", alias, target)
		}
		if _, exists := modelNames[target]; !exists {
			return fmt.Errorf("model_aliases.%s references unknown model %q", alias, target)
		}
	}
	validStrategies := map[string]bool{"shuffle": true, "round-robin": true, "latency": true}
	if !validStrategies[cfg.Router.Strategy] {
		return fmt.Errorf("router.strategy must be one of: shuffle, round-robin, latency")
	}
	if err := validateHiveTrace(&cfg.HiveTrace); err != nil {
		return err
	}
	return nil
}

func validateHiveTrace(c *HiveTraceConfig) error {
	if !c.Enabled {
		return nil
	}
	switch c.Backend {
	case "sql":
	case "clickhouse":
		if strings.TrimSpace(c.ClickHouseURL) == "" {
			return fmt.Errorf("hivetrace.clickhouse_url is required for backend clickhouse")
		}
	default:
		return fmt.Errorf("hivetrace.backend must be one of: sql, clickhouse")
	}
	// Storing prompts and completions verbatim is the one setting here that can
	// turn the trace store into a secret store, so refusing it takes two keys.
	if c.CaptureBodies && !c.ShouldRedact() && !c.AllowUnredacted {
		return fmt.Errorf("hivetrace.capture_bodies with redact: false also requires hivetrace.allow_unredacted: true")
	}
	return nil
}

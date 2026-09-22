package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
)

type App struct {
	client        *Client
	out           io.Writer
	interactive   bool
	currentModel  string
	currentTeam   string // display name shown in prompt
	currentTeamID string // actual UUID used in API calls
}

func NewApp(client *Client, out io.Writer, interactive bool) *App {
	return &App{client: client, out: out, interactive: interactive}
}

func Run(ctx context.Context, args []string, version string, in io.Reader, out io.Writer) error {
	args, baseURL, apiKey, err := extractConnectionArgs(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		printCLIUsage(out)
		return nil
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printCLIUsage(out)
		return nil
	}
	if args[0] == "version" {
		fmt.Fprintf(out, "ubiquum %s\n", version)
		return nil
	}

	client := NewClient(baseURL, apiKey)
	if args[0] == "shell" {
		sh := NewShell(client, version, in, out)
		return sh.Run(ctx)
	}

	app := NewApp(client, out, false)

	// Interactive commands that need stdin
	switch args[0] {
	case "init":
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
		reader := &scanReader{sc: scanner, out: out}
		return app.initCmd(reader, out)
	case "model", "models":
		if len(args) > 1 && args[1] == "add" {
			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
			reader := &scanReader{sc: scanner, out: out}
			return app.addModel(reader, out)
		}
	case "providers":
		if len(args) > 1 && args[1] == "add" {
			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
			reader := &scanReader{sc: scanner, out: out}
			return app.addProvider(reader, out)
		}
	case "cache":
		if len(args) > 1 && args[1] == "init" {
			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
			reader := &scanReader{sc: scanner, out: out}
			return app.cacheInit(reader, out)
		}
	case "state":
		if len(args) > 1 && args[1] == "init" {
			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
			reader := &scanReader{sc: scanner, out: out}
			return app.stateInit(reader, out)
		}
	}

	_, err = app.Execute(ctx, args)
	return err
}

func (a *App) Execute(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 {
		a.printHelp()
		return false, nil
	}
	switch canonicalCommand(args[0]) {
	case "exit", "quit", "q":
		fmt.Fprintln(a.out, dim("bye"))
		return true, nil
	case "help", "?":
		a.printHelp()
	case "clear":
		if a.interactive {
			fmt.Fprint(a.out, "\033[H\033[2J")
		}
	case "start":
		err := a.start(ctx, args[1:])
		a.client.ReloadKey()
		return false, err
	case "stop":
		return false, a.stop(ctx)
	case "restart":
		err := a.restart(ctx, args[1:])
		a.client.ReloadKey()
		return false, err
	case "logs":
		return false, a.logs(ctx, args[1:])
	case "config":
		return false, a.configCmd(ctx, args[1:])
	case "status":
		return false, a.status(ctx)
	case "models":
		return false, a.models(ctx, args[1:])
	case "providers":
		return false, a.providers(ctx, args[1:])
	case "use":
		return false, a.useModel(args[1:])
	case "test":
		return false, a.test(ctx, args[1:])
	case "keys":
		return false, a.keys(ctx, args[1:])
	case "teams":
		return false, a.teams(ctx, args[1:])
	case "spend":
		return false, a.spend(ctx, args[1:])
	case "cache":
		return false, a.cacheCmd(ctx, args[1:])
	case "state":
		return false, a.stateCmd(ctx, args[1:])
	case "route":
		return false, a.routeCmd(ctx, args[1:])
	default:
		return false, fmt.Errorf("unknown command %q; try help", args[0])
	}
	return false, nil
}

func canonicalCommand(cmd string) string {
	switch cmd {
	case "key":
		return "keys"
	case "model":
		return "models"
	case "provider":
		return "providers"
	case "team", "tenant", "tenants":
		return "teams"
	default:
		return cmd
	}
}

func (a *App) printHelp() {
	fmt.Fprintln(a.out, bold("Setup"))
	fmt.Fprintln(a.out, "  init                            create gateway.yaml interactively")
	fmt.Fprintln(a.out, "  config [path]                   show gateway configuration")
	fmt.Fprintln(a.out, "  config set <key> <value>        update a setting (port, master_key, strategy, retries, swagger)")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("Cache"))
	fmt.Fprintln(a.out, "  cache                           show cache status")
	fmt.Fprintln(a.out, "  cache init                      interactive cache setup wizard")
	fmt.Fprintln(a.out, "  cache enable                    enable semantic cache")
	fmt.Fprintln(a.out, "  cache disable                   disable semantic cache")
	fmt.Fprintln(a.out, "  cache set <key> <value>          update cache setting")
	fmt.Fprintln(a.out, dim("    keys: backend, embedding_model, tweak_model, redis_url, qdrant_url"))
	fmt.Fprintln(a.out, "  cache flush                     clear all cached entries")
	fmt.Fprintln(a.out, "  cache metrics [n]               show last n cache metrics (default 50)")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("State"))
	fmt.Fprintln(a.out, "  state                           show hivestate status")
	fmt.Fprintln(a.out, "  state metrics [n]               show aggregated state metrics")
	fmt.Fprintln(a.out, "  state enable | disable          toggle hivestate")
	fmt.Fprintln(a.out, "  state set <key> <value>         update setting (model, threshold, max_latency_ms)")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("Route"))
	fmt.Fprintln(a.out, "  route                           show hiveroute status and levels")
	fmt.Fprintln(a.out, "  route enable | disable          toggle hiveroute")
	fmt.Fprintln(a.out, "  route add <level> <model> [desc]  add a difficulty level")
	fmt.Fprintln(a.out, "  route rm <level>                remove a difficulty level")
	fmt.Fprintln(a.out, "  route metrics [n]               show route metrics and savings")
	fmt.Fprintln(a.out, "  route team <id>                 show per-team route config")
	fmt.Fprintln(a.out, "  route team <id> enable|disable  toggle per-team hiveroute")
	fmt.Fprintln(a.out, "  route team <id> add <lvl> <model> [desc]")
	fmt.Fprintln(a.out, "  route team <id> rm <level>      remove a team level")
	fmt.Fprintln(a.out, "  route team <id> reset           clear team overrides")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("Server"))
	fmt.Fprintln(a.out, "  start [-config path]            start the gateway in background")
	fmt.Fprintln(a.out, "  stop                            stop the running gateway")
	fmt.Fprintln(a.out, "  restart [-config path]          restart the gateway")
	fmt.Fprintln(a.out, "  status                          gateway health + readiness")
	fmt.Fprintln(a.out, "  logs [-n 30]                    show recent gateway logs")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("Models"))
	fmt.Fprintln(a.out, "  model list                      list exposed model names")
	fmt.Fprintln(a.out, "  model add                       add a model to config")
	fmt.Fprintln(a.out, "  model show <name>               inspect configured deployments")
	fmt.Fprintln(a.out, "  model set <name> <field> <val>  update model config fields")
	fmt.Fprintln(a.out, "  model unset <name> <field>      remove model config fields")
	fmt.Fprintln(a.out, "  model rm <name>                 remove a model from config")
	fmt.Fprintln(a.out, "  providers | provider            list providers with live status")
	fmt.Fprintln(a.out, "  providers add                   add a provider profile")
	fmt.Fprintln(a.out, "  providers rm <name>             remove a provider (cascades to models)")
	fmt.Fprintln(a.out, "  use <model>                     set default model for prompt text")
	fmt.Fprintln(a.out, "  test <model> <prompt>           send one chat completion")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("Keys & Teams"))
	fmt.Fprintln(a.out, "  keys | key                      list virtual keys")
	fmt.Fprintln(a.out, "  keys add [field val ...]         create a virtual API key")
	fmt.Fprintln(a.out, "  keys info <key|prefix|name>     inspect a virtual key")
	fmt.Fprintln(a.out, "  keys update <key> <field> <val> update a virtual key")
	fmt.Fprintln(a.out, "  keys enable <key>               activate a virtual key")
	fmt.Fprintln(a.out, "  keys disable <key>              deactivate a virtual key")
	fmt.Fprintln(a.out, "  keys rm <key|prefix|name>       remove a virtual key")
	fmt.Fprintln(a.out, "  teams | team                    list teams (grouped keys)")
	fmt.Fprintln(a.out, "  teams add <id> [field val ...]  create a team with API key")
	fmt.Fprintln(a.out, "  team use <name>                 set active team context")
	fmt.Fprintln(a.out, "  team none                       clear active team context")
	fmt.Fprintln(a.out, "  spend [key k] [model m]         show recent spend logs")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, dim("  fields: name, budget, rate-limit, models, metadata, active, expires"))
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, bold("Shell"))
	fmt.Fprintln(a.out, "  clear                           clear screen")
	fmt.Fprintln(a.out, "  exit                            leave shell")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, dim("Tab to autocomplete · Env: UBIQUUM_URL, UBIQUUM_MASTER_KEY"))
}

func (a *App) status(ctx context.Context) error {
	// The daemon PID file is meaningful only for standalone local mode. When
	// the CLI is pointed at an already-running gateway, report HTTP status only.
	if !connectedMode(a.client.BaseURL()) {
		printDaemonStatus(a.out)
	}

	health, healthErr := a.client.Health(ctx)
	ready, readyErr := a.client.Ready(ctx)
	if healthErr != nil {
		if isConnectionRefused(healthErr) {
			printConnError(a.out, a.client.BaseURL())
			return nil
		}
		return healthErr
	}
	fmt.Fprintf(a.out, "%s health  %s\n", green("●"), health.Status)
	if readyErr != nil {
		// Try to extract the reason from the error
		errMsg := readyErr.Error()
		if strings.Contains(errMsg, "no models") {
			fmt.Fprintf(a.out, "%s ready   not ready (no models configured)\n", yellow("●"))
			fmt.Fprintln(a.out, dim("  add models with /add then /restart"))
		} else {
			fmt.Fprintf(a.out, "%s ready   %s\n", yellow("●"), errMsg)
		}
		return nil
	}
	if ready.Status == "ready" {
		fmt.Fprintf(a.out, "%s ready   %s (%d models)\n", green("●"), ready.Status, ready.Models)
		return nil
	}
	fmt.Fprintf(a.out, "%s ready   %s %s\n", yellow("●"), ready.Status, ready.Reason)
	return nil
}

func connectedMode(baseURL string) bool {
	return baseURL != defaultBaseURL ||
		os.Getenv("UBIQUUM_URL") != "" ||
		os.Getenv("UBIQUUM_START_CMD") != "" ||
		os.Getenv("UBIQUUM_STOP_CMD") != "" ||
		os.Getenv("UBIQUUM_RESTART_CMD") != "" ||
		os.Getenv("UBIQUUM_LOGS_CMD") != ""
}

func (a *App) models(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return fmt.Errorf("usage: model add (interactive)")
		case "rm", "delete":
			if len(args) != 2 {
				return fmt.Errorf("usage: model rm <name>")
			}
			return a.modelsRemove(args[1])
		case "set", "update":
			return a.modelsSet(args[1:])
		case "unset":
			return a.modelsUnset(args[1:])
		case "show", "get", "info":
			return a.modelsShow(args[1:])
		case "list", "ls":
			// Continue below and render the model table.
		default:
			return fmt.Errorf("unknown model command %q; try: model add|list|show|set|unset|rm", args[0])
		}
	}

	configModels := configuredModelDisplays()

	// Try gateway API first (shows only running models)
	models, err := a.client.Models(ctx)
	if err != nil {
		if isConnectionRefused(err) {
			// Gateway not running — show from config only
			if len(configModels) == 0 {
				fmt.Fprintln(a.out, yellow("no models configured"))
				return nil
			}
			tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPROVIDER\tTYPE\tMODEL\tPRICE/1M (in/out)\tENDPOINT")
			lastName := ""
			for _, m := range configModels {
				displayName := m.Name
				if displayName == lastName {
					displayName = ""
				} else {
					lastName = displayName
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", displayName, m.Provider, m.Type, m.ProviderModel, formatModelPrice(m), emptyDash(m.Endpoint))
			}
			tw.Flush()
			fmt.Fprintln(a.out, dim("  (from config — gateway not running)"))
			return nil
		}
		return err
	}
	if len(models) == 0 {
		fmt.Fprintln(a.out, yellow("no models returned"))
		return nil
	}

	// Build lookup: model name → []modelDisplay
	byName := make(map[string][]modelDisplay, len(configModels))
	for _, cm := range configModels {
		byName[cm.Name] = append(byName[cm.Name], cm)
	}

	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPROVIDER\tTYPE\tMODEL\tPRICE/1M (in/out)\tENDPOINT")
	for _, m := range models {
		deployments := byName[m.ID]
		if len(deployments) == 0 {
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\n", m.ID)
			continue
		}
		for i, cm := range deployments {
			name := m.ID
			if i > 0 {
				name = ""
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", name, cm.Provider, cm.Type, cm.ProviderModel, formatModelPrice(cm), emptyDash(cm.Endpoint))
		}
	}
	return tw.Flush()
}

func (a *App) modelsRemove(name string) error {
	configPath := ConfigFilePath()
	remaining, err := removeModelFromConfig(configPath, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s removed %s from config (%d models remaining)\n", green("✓"), bold(name), remaining)
	autoRestart(a.out)
	return nil
}

func (a *App) modelsSet(args []string) error {
	name, updates, err := parseModelSetArgs(args)
	if err != nil {
		return err
	}
	updated, err := updateModelsInConfig(ConfigFilePath(), name, updates)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s updated %s (%d deployment(s))\n", green("✓"), bold(name), updated)
	autoRestart(a.out)
	return nil
}

func (a *App) modelsUnset(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: model unset <name> <field> [field ...]\n  fields: %s", strings.Join(modelUnsetFields(), ", "))
	}
	name := args[0]
	fields := make([]string, 0, len(args)-1)
	for _, raw := range args[1:] {
		field := normalizeModelField(strings.TrimLeft(raw, "-"))
		if err := validateModelUnsetField(field); err != nil {
			return err
		}
		fields = append(fields, field)
	}
	updated, err := unsetModelFieldsInConfig(ConfigFilePath(), name, fields)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s updated %s (%d deployment(s))\n", green("✓"), bold(name), updated)
	autoRestart(a.out)
	return nil
}

func (a *App) modelsShow(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: model show <name>")
	}
	models, err := configuredModelsByName(args[0])
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("model %q not found in config", args[0])
	}
	for i, model := range models {
		if len(models) > 1 {
			fmt.Fprintf(a.out, "%s %s deployment %d/%d\n", bold("model"), bold(model.Name), i+1, len(models))
		} else {
			fmt.Fprintf(a.out, "%s %s\n", bold("model"), bold(model.Name))
		}
		printModelDetail(a.out, model)
		if i+1 < len(models) {
			fmt.Fprintln(a.out)
		}
	}
	return nil
}

func configuredModelsByName(name string) ([]initModel, error) {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config found; run init first")
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	models := make([]initModel, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		if model.Name == name {
			models = append(models, model)
		}
	}
	return models, nil
}

func printModelDetail(w io.Writer, model initModel) {
	fmt.Fprintf(w, "  provider:                  %s\n", emptyDash(model.Provider))
	fmt.Fprintf(w, "  provider_model:            %s\n", emptyDash(model.ProviderModel))
	fmt.Fprintf(w, "  api_base:                  %s\n", emptyDash(model.APIBase))
	fmt.Fprintf(w, "  api_key:                   %s\n", emptyDash(maskSecret(model.APIKey)))
	fmt.Fprintf(w, "  api_secret:                %s\n", emptyDash(maskSecret(model.APISecret)))
	fmt.Fprintf(w, "  api_version:               %s\n", emptyDash(model.APIVersion))
	fmt.Fprintf(w, "  project:                   %s\n", emptyDash(model.Project))
	fmt.Fprintf(w, "  location:                  %s\n", emptyDash(model.Location))
	fmt.Fprintf(w, "  region:                    %s\n", emptyDash(model.Region))
	fmt.Fprintf(w, "  session_token:             %s\n", emptyDash(maskSecret(model.SessionToken)))
	fmt.Fprintf(w, "  drop_params:               %s\n", emptyDash(strings.Join(model.DropParams, ",")))
	fmt.Fprintf(w, "  input_cost_per_million:    %s\n", formatOptionalFloat(model.InputCostPerMillion))
	fmt.Fprintf(w, "  output_cost_per_million:   %s\n", formatOptionalFloat(model.OutputCostPerMillion))
}

func formatOptionalFloat(value float64) string {
	if value == 0 {
		return "-"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func parseModelSetArgs(args []string) (string, []modelUpdate, error) {
	if len(args) < 3 {
		return "", nil, fmt.Errorf("usage: model set <name> <field> <value> [...]\n  fields: %s", strings.Join(modelSetFields(), ", "))
	}
	name := args[0]
	updates := make([]modelUpdate, 0, len(args)/2)
	for i := 1; i < len(args); {
		key := normalizeModelField(strings.TrimLeft(args[i], "-"))
		if key == "price" {
			if i+2 >= len(args) {
				return "", nil, fmt.Errorf("usage: model set <name> price <input-per-1m> <output-per-1m>")
			}
			input := args[i+1]
			output := args[i+2]
			if err := validateModelUpdateField("input_cost_per_million", input); err != nil {
				return "", nil, err
			}
			if err := validateModelUpdateField("output_cost_per_million", output); err != nil {
				return "", nil, err
			}
			updates = append(updates,
				modelUpdate{Field: "input_cost_per_million", Value: input},
				modelUpdate{Field: "output_cost_per_million", Value: output},
			)
			i += 3
			continue
		}
		if i+1 >= len(args) {
			return "", nil, fmt.Errorf("missing value for model field %q", key)
		}
		value := args[i+1]
		if err := validateModelUpdateField(key, value); err != nil {
			return "", nil, err
		}
		updates = append(updates, modelUpdate{Field: key, Value: value})
		i += 2
	}
	return name, updates, nil
}

func validateModelUpdateField(key, value string) error {
	switch key {
	case "name", "provider", "provider_model", "api_base", "api_key", "api_secret", "api_version", "project", "location", "region", "session_token", "drop_params":
		return nil
	case "input_cost_per_million", "output_cost_per_million":
		_, err := parseModelPriceArg(key, value)
		return err
	default:
		return fmt.Errorf("unknown model field %q; available: %s", key, strings.Join(modelUpdateFields(), ", "))
	}
}

func validateModelUnsetField(key string) error {
	if key == "name" {
		return fmt.Errorf("model name cannot be unset")
	}
	for _, field := range modelUnsetFields() {
		if key == field {
			return nil
		}
	}
	return fmt.Errorf("unknown model field %q; available: %s", key, strings.Join(modelUnsetFields(), ", "))
}

func normalizeModelField(field string) string {
	switch field {
	case "provider-model":
		return "provider_model"
	case "api-base":
		return "api_base"
	case "api-key":
		return "api_key"
	case "api-secret":
		return "api_secret"
	case "api-version":
		return "api_version"
	case "session-token":
		return "session_token"
	case "drop-params":
		return "drop_params"
	case "input-price", "input_cost", "input-cost":
		return "input_cost_per_million"
	case "output-price", "output_cost", "output-cost":
		return "output_cost_per_million"
	default:
		return field
	}
}

func modelSetFields() []string {
	fields := []string{"price"}
	fields = append(fields, modelUpdateFields()...)
	return fields
}

func modelUnsetFields() []string {
	fields := []string{"price"}
	fields = append(fields, modelUpdateFields()[1:]...)
	return fields
}

func modelUpdateFields() []string {
	return []string{
		"name", "provider", "provider_model", "api_base", "api_key", "api_secret",
		"api_version", "project", "location", "region", "session_token", "drop_params",
		"input_cost_per_million", "output_cost_per_million",
	}
}

func parseModelPriceArg(label, value string) (float64, error) {
	price, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %s", label, value)
	}
	if price < 0 {
		return 0, fmt.Errorf("%s must be >= 0", label)
	}
	return price, nil
}

func (a *App) useModel(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: use <model>")
	}
	a.currentModel = args[0]
	fmt.Fprintf(a.out, "%s default model: %s\n", green("✓"), bold(args[0]))
	return nil
}

// teamUse sets the active team context. It accepts a team name or ID.
// When given a name, it resolves it to an ID via the gateway API.
func (a *App) teamUse(args []string) error {
	if len(args) == 0 {
		if a.currentTeam == "" {
			fmt.Fprintln(a.out, dim("no active team (usage: team use <name|id>"))
		} else {
			fmt.Fprintf(a.out, "%s active team: %s\n", green("●"), bold(a.currentTeam))
		}
		return nil
	}
	if args[0] == "--clear" || args[0] == "none" || args[0] == "-" || args[0] == "off" {
		a.currentTeam = ""
		a.currentTeamID = ""
		fmt.Fprintln(a.out, dim("team context cleared"))
		return nil
	}

	arg := strings.TrimSpace(args[0])
	// Try to resolve the argument as a team name → ID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if teams, err := a.client.ListTeams(ctx); err == nil {
		for _, t := range teams {
			if strings.EqualFold(t.Name, arg) || t.ID == arg {
				a.currentTeam = t.Name
				a.currentTeamID = t.ID
				fmt.Fprintf(a.out, "%s active team: %s\n", green("✓"), bold(t.Name))
				fmt.Fprintln(a.out, dim("  keys, spend commands will now scope to this team"))
				return nil
			}
		}
	}
	// Fallback: store as-is (gateway might be offline)
	a.currentTeam = arg
	a.currentTeamID = arg
	fmt.Fprintf(a.out, "%s active team: %s\n", green("✓"), bold(arg))
	fmt.Fprintln(a.out, dim("  keys, spend commands will now scope to this team"))
	return nil
}

func (a *App) keys(ctx context.Context, args []string) error {
	if len(args) == 0 {
		// Apply team context if set
		if a.currentTeamID != "" {
			return a.keyList(ctx, []string{"--team", a.currentTeamID})
		}
		return a.keyList(ctx, nil)
	}
	switch args[0] {
	case "add", "create":
		return a.keyCreate(ctx, args[1:])
	case "list":
		return a.keyList(ctx, args[1:])
	case "info":
		if len(args) != 2 {
			return fmt.Errorf("usage: keys info <key>")
		}
		return a.keyInfo(ctx, args[1])
	case "update":
		return a.keyUpdate(ctx, args[1:])
	case "enable":
		if len(args) != 2 {
			return fmt.Errorf("usage: keys enable <key>")
		}
		return a.keySetActive(ctx, args[1], true)
	case "disable":
		if len(args) != 2 {
			return fmt.Errorf("usage: keys disable <key>")
		}
		return a.keySetActive(ctx, args[1], false)
	case "rm", "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: keys rm <key>")
		}
		return a.keyDelete(ctx, args[1])
	default:
		return fmt.Errorf("unknown keys command %q; try: keys add|update|enable|disable|info|rm", args[0])
	}
}

func (a *App) keyInfo(ctx context.Context, identifier string) error {
	keys, err := a.client.ListKeys(ctx, "")
	if err == nil {
		key, found, resolveErr := resolveKeyReference(keys, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		if found {
			printKeyDetail(a.out, key, false)
			return nil
		}
	}

	key, err := a.client.KeyInfo(ctx, identifier)
	if err != nil {
		return err
	}
	printKeyDetail(a.out, key, false)
	return nil
}

func (a *App) keyDelete(ctx context.Context, identifier string) error {
	var target APIKey
	targetFound := false

	keys, err := a.client.ListKeys(ctx, "")
	if err == nil {
		resolved, found, resolveErr := resolveKeyReference(keys, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		target = resolved
		targetFound = found
	}

	if err := a.client.DeleteKey(ctx, identifier); err != nil {
		return err
	}

	if targetFound {
		keysAfter, err := a.client.ListKeys(ctx, "")
		if err != nil {
			return fmt.Errorf("delete accepted, but verification failed: %w", err)
		}
		if containsKey(keysAfter, target) {
			return fmt.Errorf("delete accepted but %s is still present; restart the gateway with ./bin/ubiquum restart, then retry with prefix %s", keyLabel(target), target.KeyPrefix)
		}
		fmt.Fprintf(a.out, "%s key deleted: %s\n", green("✓"), keyLabel(target))
		return nil
	}

	fmt.Fprintln(a.out, green("✓"), "key deleted")
	return nil
}

func (a *App) keyUpdate(ctx context.Context, args []string) error {
	if len(args) < 3 || len(args)%2 == 0 {
		return fmt.Errorf("usage: keys update <key> <field> <value> [...]\n  fields: name, budget, rate-limit, models, metadata, active, expires")
	}
	identifier := args[0]

	// Resolve key reference (prefix, name, or full key)
	rawKey := identifier
	keys, err := a.client.ListKeys(ctx, "")
	if err == nil {
		if resolved, found, _ := resolveKeyReference(keys, identifier); found && resolved.Key != "" {
			rawKey = resolved.Key
		}
	}

	var params UpdateKeyParams
	hasChange := false
	pairs := args[1:]
	for i := 0; i < len(pairs); i += 2 {
		key := strings.TrimLeft(pairs[i], "-")
		val := pairs[i+1]
		switch key {
		case "name":
			params.Name = &val
			hasChange = true
		case "budget":
			b, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return fmt.Errorf("invalid budget: %s", val)
			}
			params.Budget = &b
			hasChange = true
		case "rate-limit", "ratelimit":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid rate-limit: %s", val)
			}
			params.RateLimit = &n
			hasChange = true
		case "models":
			params.Models = csv(val)
			hasChange = true
		case "active":
			b := val == "true" || val == "1" || val == "yes"
			params.Active = &b
			hasChange = true
		case "expires":
			t, err := time.Parse("2006-01-02", val)
			if err != nil {
				return fmt.Errorf("invalid date %q (use YYYY-MM-DD)", val)
			}
			params.ExpiresAt = &t
			hasChange = true
		default:
			return fmt.Errorf("unknown field %q; available: name, budget, rate-limit, models, active, expires", key)
		}
	}
	if !hasChange {
		return fmt.Errorf("nothing to update")
	}

	if err := a.client.UpdateKey(ctx, rawKey, params); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s key updated\n", green("✓"))
	return nil
}

func (a *App) keySetActive(ctx context.Context, identifier string, active bool) error {
	rawKey := identifier
	keys, err := a.client.ListKeys(ctx, "")
	if err == nil {
		if resolved, found, _ := resolveKeyReference(keys, identifier); found && resolved.Key != "" {
			rawKey = resolved.Key
		}
	}
	params := UpdateKeyParams{Active: &active}
	if err := a.client.UpdateKey(ctx, rawKey, params); err != nil {
		return err
	}
	if active {
		fmt.Fprintf(a.out, "%s key enabled\n", green("✓"))
	} else {
		fmt.Fprintf(a.out, "%s key disabled\n", green("✓"))
	}
	return nil
}

func (a *App) keyCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("key create")
	var req CreateKeyRequest
	var models string
	fs.StringVar(&req.Name, "name", "", "key name")
	fs.StringVar(&req.TeamID, "team", "", "team id")
	fs.Float64Var(&req.Budget, "budget", 0, "budget")
	fs.IntVar(&req.RateLimit, "rate-limit", 0, "rate limit")
	fs.StringVar(&models, "models", "", "comma-separated allowed models")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req.Models = csv(models)
	// Inherit current team context if --team was not explicitly specified
	if req.TeamID == "" && a.currentTeamID != "" {
		req.TeamID = a.currentTeamID
	}

	key, err := a.client.CreateKey(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, green("✓"), "key created")
	printKeyDetail(a.out, key, key.Key != "")
	if key.Key != "" {
		fmt.Fprintln(a.out, dim("  copy it now; future key views are masked"))
	}
	return nil
}

func (a *App) keyCreateInteractive(ctx context.Context, reader lineReader, out io.Writer) error {
	fmt.Fprintln(out)
	var req CreateKeyRequest

	// Name
	if val, ok := reader.ReadLine("  Name [dev-key]: "); ok && val != "" {
		req.Name = val
	} else {
		req.Name = "dev-key"
	}

	// Budget
	// A key with no budget cannot call unless every model it may use is billed
	// upstream. Ask for one and default to something usable rather than zero;
	// 0 is accepted for a key scoped to OAuth pass-through models.
	req.Budget = defaultKeyBudget
	if val, ok := reader.ReadLine(fmt.Sprintf("  Budget in USD, 0 for an OAuth-only key [%g]: ", defaultKeyBudget)); ok && val != "" {
		if b, err := strconv.ParseFloat(val, 64); err == nil && b >= 0 {
			req.Budget = b
		}
	}

	// Models (with autocompletion, add one at a time)
	displays := configuredModelDisplays()
	modelNames := make([]string, len(displays))
	for i, d := range displays {
		modelNames[i] = d.Name
	}
	if len(modelNames) > 0 {
		fmt.Fprintf(out, "  %s\n", dim("Select models (empty = all, Tab to autocomplete)"))
		for i := 1; ; i++ {
			remaining := make([]string, 0, len(modelNames))
			for _, n := range modelNames {
				found := false
				for _, sel := range req.Models {
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
			req.Models = append(req.Models, strings.TrimSpace(val))
		}
	}

	// Rate limit
	if val, ok := reader.ReadLine("  Rate limit (req/min, 0 = none) [0]: "); ok && val != "" {
		if r, err := strconv.Atoi(val); err == nil {
			req.RateLimit = r
		}
	}

	// Team: skip if already set via "team use"
	if a.currentTeamID != "" {
		req.TeamID = a.currentTeamID
		fmt.Fprintf(out, "  %s\n", dim("Team: "+a.currentTeam+" (from active context)"))
	} else {
		// Team (mandatory, with autocomplete)
		var teamHints []string
		if teams, err := a.client.ListTeams(ctx); err == nil {
			for _, t := range teams {
				teamHints = append(teamHints, t.Name)
			}
		}
		// Fallback: collect team IDs from existing keys
		if len(teamHints) == 0 {
			if keys, err := a.client.ListKeys(ctx, ""); err == nil {
				seen := map[string]bool{}
				for _, k := range keys {
					if k.TeamID != "" && !seen[k.TeamID] {
						seen[k.TeamID] = true
						teamHints = append(teamHints, k.TeamID)
					}
				}
			}
		}
		for {
			val, ok := reader.ReadLineWithHints("  Team: ", teamHints)
			if !ok {
				return fmt.Errorf("cancelled")
			}
			if val != "" {
				// Resolve name → ID
				selected := strings.TrimSpace(val)
				if teams, err := a.client.ListTeams(ctx); err == nil {
					for _, t := range teams {
						if strings.EqualFold(t.Name, selected) {
							selected = t.ID
							break
						}
					}
				}
				req.TeamID = selected
				break
			}
			fmt.Fprintln(out, yellow("  ⚠ team is required"))
		}
	}

	key, err := a.client.CreateKey(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, green("✓"), "key created")
	printKeyDetail(out, key, key.Key != "")
	if key.Key != "" {
		fmt.Fprintln(out, dim("  copy it now; future key views are masked"))
	}
	return nil
}

func (a *App) keyList(ctx context.Context, args []string) error {
	fs := newFlagSet("key list")
	var team string
	fs.StringVar(&team, "team", "", "team id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys, err := a.client.ListKeys(ctx, team)
	if err != nil {
		return err
	}
	printKeyTable(a.out, keys)
	return nil
}

func (a *App) teams(ctx context.Context, args []string) error {
	if len(args) == 0 {
		keys, err := a.client.ListKeys(ctx, "")
		if err != nil {
			return err
		}
		printTeamTable(a.out, keys)
		return nil
	}
	switch args[0] {
	case "add", "create":
		return a.teamCreate(ctx, args[1:])
	case "use":
		return a.teamUse(args[1:])
	case "none", "off", "clear":
		return a.teamUse([]string{"--clear"})
	case "rm", "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: teams rm <team-id>")
		}
		return a.teamDelete(ctx, args[1])
	case "list":
		keys, err := a.client.ListKeys(ctx, "")
		if err != nil {
			return err
		}
		printTeamTable(a.out, keys)
		return nil
	default:
		return fmt.Errorf("unknown teams command %q; try: team use <name> | team none | team add | team rm | team list", args[0])
	}
}

func (a *App) teamCreate(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: teams add <id> [budget n] [models a,b]")
	}
	teamID := args[0]
	fs := newFlagSet("team create")
	var budget float64
	var models, name string
	fs.Float64Var(&budget, "budget", 0, "budget")
	fs.StringVar(&models, "models", "", "comma-separated allowed models")
	fs.StringVar(&name, "name", "", "key name")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if name == "" {
		name = teamID + ":default"
	}

	// Create team record in DB
	_, teamErr := a.client.CreateTeam(ctx, CreateTeamRequest{
		Name:   teamID,
		Budget: budget,
	})
	if teamErr != nil {
		fmt.Fprintf(a.out, "%s could not create team record: %v\n", yellow("!"), teamErr)
	}

	key, err := a.client.CreateKey(ctx, CreateKeyRequest{
		Name:   name,
		TeamID: teamID,
		Budget: budget,
		Models: csv(models),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s team %s ready\n", green("✓"), bold(teamID))
	printKeyDetail(a.out, key, key.Key != "")
	if key.Key != "" {
		fmt.Fprintln(a.out, dim("  copy it now; future key views are masked"))
	}
	return nil
}

func (a *App) teamAddInteractive(ctx context.Context, reader lineReader, out io.Writer) error {
	fmt.Fprintln(out)

	// Team ID
	teamID, ok := reader.ReadLine("  Team name (e.g. acme): ")
	if !ok || teamID == "" {
		return fmt.Errorf("team name is required")
	}

	// Budget
	// Required: a key with no budget is blocked by the gateway.
	budget := defaultKeyBudget
	if val, ok := reader.ReadLine(fmt.Sprintf("  Budget in USD (required) [%g]: ", defaultKeyBudget)); ok && val != "" {
		if b, err := strconv.ParseFloat(val, 64); err == nil && b > 0 {
			budget = b
		}
	}

	// Models with autocompletion
	var models []string
	displays := configuredModelDisplays()
	modelNames := make([]string, len(displays))
	for i, d := range displays {
		modelNames[i] = d.Name
	}
	if len(modelNames) > 0 {
		fmt.Fprintf(out, "  %s\n", dim("Select models (empty = all, Tab to autocomplete)"))
		for i := 1; ; i++ {
			remaining := make([]string, 0, len(modelNames))
			for _, n := range modelNames {
				found := false
				for _, sel := range models {
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
			models = append(models, strings.TrimSpace(val))
		}
	}

	name := teamID + ":default"

	// Create team record in DB
	_, teamErr := a.client.CreateTeam(ctx, CreateTeamRequest{
		Name:   teamID,
		Budget: budget,
	})
	if teamErr != nil {
		fmt.Fprintf(out, "%s could not create team record: %v\n", yellow("!"), teamErr)
	}

	key, err := a.client.CreateKey(ctx, CreateKeyRequest{
		Name:   name,
		TeamID: teamID,
		Budget: budget,
		Models: models,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s team %s ready\n", green("✓"), bold(teamID))
	printKeyDetail(out, key, key.Key != "")
	if key.Key != "" {
		fmt.Fprintln(out, dim("  copy it now; future key views are masked"))
	}
	return nil
}

func (a *App) teamDelete(ctx context.Context, teamID string) error {
	// Delete all keys belonging to this team
	keys, err := a.client.ListKeys(ctx, teamID)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("no keys found for team %q", teamID)
	}
	for _, k := range keys {
		if err := a.client.DeleteKey(ctx, k.KeyPrefix); err != nil {
			return fmt.Errorf("failed to delete key %s: %w", k.KeyPrefix, err)
		}
	}
	fmt.Fprintf(a.out, "%s team %s removed (%d keys deleted)\n", green("✓"), bold(teamID), len(keys))
	return nil
}

// spend shows spend logs, respecting the active team context.
func (a *App) spend(ctx context.Context, args []string) error {
	fs := newFlagSet("spend")
	var key, model string
	fs.StringVar(&key, "key", "", "raw API key")
	fs.StringVar(&model, "model", "", "model name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	records, err := a.client.SpendLogs(ctx, key, model)
	if err != nil {
		return err
	}
	printSpendTable(a.out, records)
	return nil
}

func (a *App) test(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: /test <model> <prompt>")
	}
	model := args[0]
	prompt := strings.Join(args[1:], " ")

	// Spinner while waiting
	done := make(chan struct{})
	cleared := make(chan struct{})
	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				fmt.Fprint(a.out, "\r\033[K")
				close(cleared)
				return
			case <-ticker.C:
				fmt.Fprintf(a.out, "\r%s", dim(frames[i%len(frames)]))
				i++
			}
		}
	}()

	resp, err := a.client.Chat(ctx, model, prompt)
	close(done)
	<-cleared
	if err != nil {
		return err
	}

	// Show routing info
	if resp.Failed != "" {
		for _, f := range strings.Split(resp.Failed, ",") {
			fmt.Fprintf(a.out, "%s %s did not respond\n", yellow("↻"), f)
		}
	}
	if resp.Provider != "" {
		fmt.Fprintf(a.out, "%s routed to %s", dim("→"), bold(resp.Provider))
		if resp.Attempts != "" {
			fmt.Fprintf(a.out, " %s", dim(fmt.Sprintf("(%s attempts)", resp.Attempts)))
		}
		fmt.Fprintln(a.out)
	}

	if len(resp.Choices) == 0 {
		fmt.Fprintln(a.out, yellow("no choices returned"))
		return nil
	}
	content := resp.Choices[0].Message.Content
	switch v := content.(type) {
	case string:
		fmt.Fprintln(a.out, blue(v))
	default:
		data, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(a.out, string(data))
	}
	return nil
}

func printKeyDetail(w io.Writer, key APIKey, showSecret bool) {
	fmt.Fprintf(w, "  id:         %s\n", key.ID)
	if key.Key != "" {
		if showSecret {
			fmt.Fprintf(w, "  key:        %s %s\n", bold(key.Key), dim("(shown once)"))
		} else {
			fmt.Fprintf(w, "  key:        %s\n", maskSecret(key.Key))
		}
	}
	fmt.Fprintf(w, "  prefix:     %s\n", key.KeyPrefix)
	fmt.Fprintf(w, "  name:       %s\n", emptyDash(key.Name))
	fmt.Fprintf(w, "  team:       %s\n", emptyDash(key.TeamID))
	fmt.Fprintf(w, "  budget:     %.6f\n", key.Budget)
	fmt.Fprintf(w, "  spend:      %.6f\n", key.Spend)
	fmt.Fprintf(w, "  models:     %s\n", emptyDash(strings.Join(key.Models, ",")))
}

func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return strings.Repeat("•", len(value))
	}
	return value[:11] + strings.Repeat("•", max(4, len(value)-15)) + value[len(value)-4:]
}

func printKeyTable(w io.Writer, keys []APIKey) {
	if len(keys) == 0 {
		fmt.Fprintln(w, yellow("no keys found"))
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PREFIX\tNAME\tTEAM\tBUDGET\tSPEND\tACTIVE\tMODELS")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.4f\t%.4f\t%t\t%s\n",
			k.KeyPrefix, emptyDash(k.Name), emptyDash(k.TeamID), k.Budget, k.Spend, k.Active, emptyDash(strings.Join(k.Models, ",")))
	}
	tw.Flush()
}

func resolveKeyReference(keys []APIKey, identifier string) (APIKey, bool, error) {
	var matches []APIKey
	seen := map[string]bool{}
	for _, key := range keys {
		if keyMatchesIdentifier(key, identifier) && !seen[key.ID] {
			matches = append(matches, key)
			seen[key.ID] = true
		}
	}
	switch len(matches) {
	case 0:
		return APIKey{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return APIKey{}, false, fmt.Errorf("key %q is ambiguous; use one of these prefixes: %s", identifier, keyPrefixList(matches))
	}
}

func keyMatchesIdentifier(key APIKey, identifier string) bool {
	switch {
	case identifier == "":
		return false
	case key.ID == identifier:
		return true
	case key.KeyPrefix == identifier:
		return true
	case key.Name == identifier:
		return true
	case strings.HasPrefix(identifier, "sk-ubq-") && key.KeyPrefix != "":
		return strings.HasPrefix(key.KeyPrefix, identifier) || strings.HasPrefix(identifier, key.KeyPrefix)
	default:
		return false
	}
}

func containsKey(keys []APIKey, needle APIKey) bool {
	for _, key := range keys {
		if key.ID != "" && key.ID == needle.ID {
			return true
		}
		if key.ID == "" && key.KeyPrefix == needle.KeyPrefix {
			return true
		}
	}
	return false
}

func keyLabel(key APIKey) string {
	if key.Name != "" {
		return fmt.Sprintf("%s (%s)", key.Name, key.KeyPrefix)
	}
	return key.KeyPrefix
}

func keyPrefixList(keys []APIKey) string {
	prefixes := make([]string, 0, len(keys))
	for _, key := range keys {
		prefixes = append(prefixes, key.KeyPrefix)
	}
	sort.Strings(prefixes)
	return strings.Join(prefixes, ", ")
}

func printTeamTable(w io.Writer, keys []APIKey) {
	type row struct {
		keys   int
		active int
		budget float64
		spend  float64
		models map[string]bool
	}
	rows := map[string]*row{}
	for _, k := range keys {
		id := k.TeamID
		if id == "" {
			id = "-"
		}
		r := rows[id]
		if r == nil {
			r = &row{models: map[string]bool{}}
			rows[id] = r
		}
		r.keys++
		if k.Active {
			r.active++
		}
		r.budget += k.Budget
		r.spend += k.Spend
		for _, m := range k.Models {
			r.models[m] = true
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, yellow("no teams found"))
		return
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TEAM\tKEYS\tACTIVE\tBUDGET\tSPEND\tMODELS")
	for _, name := range names {
		r := rows[name]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.4f\t%.4f\t%s\n", name, r.keys, r.active, r.budget, r.spend, emptyDash(strings.Join(sortedSet(r.models), ",")))
	}
	tw.Flush()
}

func printSpendTable(w io.Writer, records []SpendSummary) {
	if len(records) == 0 {
		fmt.Fprintln(w, yellow("no spend logs found"))
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tPROVIDER\tREQUESTS\tTOKENS\tCOST\tAVG MS")
	for _, s := range records {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.6f\t%.0f\n",
			s.Model, s.Provider, s.Requests, s.Tokens, s.Cost, s.AvgMs)
	}
	tw.Flush()
}

func printCLIUsage(w io.Writer) {
	fmt.Fprintln(w, bold("ubiquum")+" — LLM Gateway CLI")
	fmt.Fprintln(w)
	fmt.Fprintln(w, bold("Usage:"))
	fmt.Fprintln(w, "  ubiquum                         open interactive shell")
	fmt.Fprintln(w, "  ubiquum init                    create gateway.yaml wizard")
	fmt.Fprintln(w, "  ubiquum serve [-config path]    start gateway (foreground)")
	fmt.Fprintln(w, "  ubiquum start [-config path]    start gateway (background)")
	fmt.Fprintln(w, "  ubiquum stop                    stop background gateway")
	fmt.Fprintln(w, "  ubiquum restart [-config path]  restart background gateway")
	fmt.Fprintln(w, "  ubiquum status [--url u]        check health/readiness")
	fmt.Fprintln(w, "  ubiquum model list              list models")
	fmt.Fprintln(w, "  ubiquum model show <name>       inspect model config")
	fmt.Fprintln(w, "  ubiquum model set <name> f v    update model config fields")
	fmt.Fprintln(w, "  ubiquum model unset <name> f    remove model config fields")
	fmt.Fprintln(w, "  ubiquum providers               list providers + status")
	fmt.Fprintln(w, "  ubiquum logs [-n 30]            show recent gateway logs")
	fmt.Fprintln(w, "  ubiquum config [path]           show configuration")
	fmt.Fprintln(w, "  ubiquum keys add|list|info      manage virtual keys")
	fmt.Fprintln(w, "  ubiquum tenants create|list     lightweight tenant workflow")
	fmt.Fprintln(w, "  ubiquum spend [--key k]         show spend logs")
	fmt.Fprintln(w, "  ubiquum test <model> <prompt>   send a chat completion")
	fmt.Fprintln(w)
	fmt.Fprintln(w, dim("Flags: --url, --admin-key (or set UBIQUUM_URL, UBIQUUM_MASTER_KEY)"))
}

func extractConnectionArgs(args []string) ([]string, string, string, error) {
	baseURL := firstEnv("UBIQUUM_URL")
	apiKey := firstEnv("UBIQUUM_MASTER_KEY", "GATEWAY_MASTER_KEY")
	allowKeyAlias := len(args) == 0 || args[0] != "spend"
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--url":
			i++
			if i >= len(args) {
				return nil, "", "", fmt.Errorf("--url requires a value")
			}
			baseURL = args[i]
		case strings.HasPrefix(arg, "--url="):
			baseURL = strings.TrimPrefix(arg, "--url=")
		case arg == "--admin-key" || arg == "--master-key" || (arg == "--key" && allowKeyAlias):
			i++
			if i >= len(args) {
				return nil, "", "", fmt.Errorf("%s requires a value", arg)
			}
			apiKey = args[i]
		case strings.HasPrefix(arg, "--admin-key="):
			apiKey = strings.TrimPrefix(arg, "--admin-key=")
		case strings.HasPrefix(arg, "--master-key="):
			apiKey = strings.TrimPrefix(arg, "--master-key=")
		case strings.HasPrefix(arg, "--key=") && allowKeyAlias:
			apiKey = strings.TrimPrefix(arg, "--key=")
		default:
			clean = append(clean, arg)
		}
	}

	// Auto-read master_key from config file if not provided via flag/env
	if apiKey == "" {
		apiKey = readMasterKeyFromConfig()
	}

	// Auto-read url from config file if not provided via flag/env
	if baseURL == "" {
		baseURL = readURLFromConfig()
	}

	return clean, baseURL, apiKey, nil
}

// readMasterKeyFromConfig reads the master_key from ~/.ubiquum/gateway.yaml.
func readMasterKeyFromConfig() string {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return ""
	}
	// Quick parse — just find master_key line
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "master_key:") {
			val := strings.TrimPrefix(line, "master_key:")
			val = strings.TrimSpace(val)
			val = strings.Trim(val, "\"'")
			return val
		}
	}
	return ""
}

// readURLFromConfig reads the cli.url from ~/.ubiquum/gateway.yaml.
func readURLFromConfig() string {
	data, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "url:") {
			val := strings.TrimPrefix(line, "url:")
			val = strings.TrimSpace(val)
			val = strings.Trim(val, "\"'")
			return val
		}
	}
	return ""
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if val := os.Getenv(name); val != "" {
			return val
		}
	}
	return ""
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func csv(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func emptyDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// formatModelPrice formats the input/output cost per 1M tokens for display.
// Shows "in/out" values; appends "*" when from config override vs catalog.
func formatModelPrice(m modelDisplay) string {
	if m.InputCostPerMillion == 0 && m.OutputCostPerMillion == 0 {
		return "-"
	}
	s := fmt.Sprintf("%.2f / %.2f", m.InputCostPerMillion, m.OutputCostPerMillion)
	if m.PriceSource == "catalog" {
		s = dim(s)
	}
	return s
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "connect: connection refused")
}

// printConnError prints a helpful message when the gateway can't be reached.
// It checks whether the PID is alive to distinguish "crashed" from "not started".
func printConnError(w io.Writer, baseURL string) {
	pid, err := readPID()
	if err == nil && isProcessAlive(pid) {
		fmt.Fprintf(w, "%s gateway not responding — it may have crashed. check: logs\n", red("●"))
	} else {
		fmt.Fprintf(w, "%s gateway not running at %s — start it first: start\n", yellow("●"), baseURL)
	}
}

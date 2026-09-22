package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func (a *App) routeCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "status" {
		return a.routeStatus()
	}
	switch args[0] {
	case "enable":
		return a.routeToggle(true)
	case "disable":
		return a.routeToggle(false)
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("usage: route add <level> <model> [description]")
		}
		desc := ""
		if len(args) > 3 {
			desc = strings.Join(args[3:], " ")
		}
		return a.routeAddLevel(args[1], args[2], desc)
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: route rm <level>")
		}
		return a.routeRemoveLevel(args[1])
	case "metrics":
		limit := 100
		if len(args) > 1 {
			if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
				limit = n
			}
		}
		return a.routeMetrics(ctx, limit)
	case "team":
		return a.routeTeamCmd(ctx, args[1:])
	default:
		return fmt.Errorf("unknown route subcommand %q; try: enable, disable, add, rm, metrics, status", args[0])
	}
}

func (a *App) routeStatus() error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(a.out, "%s hiveroute not configured (no config file)\n", dim("○"))
		return nil
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.HiveState == nil || cfg.HiveState.HiveRoute == nil || !cfg.HiveState.HiveRoute.Enabled {
		fmt.Fprintf(a.out, "%s hiveroute disabled\n", dim("○"))
		fmt.Fprintf(a.out, "  run %s to enable\n", bold("route enable"))
		return nil
	}
	hr := cfg.HiveState.HiveRoute
	fmt.Fprintf(a.out, "%s hiveroute enabled\n", green("●"))
	fmt.Fprintln(a.out)
	if len(hr.Levels) == 0 {
		fmt.Fprintf(a.out, "  %s\n", dim("no levels configured"))
		fmt.Fprintf(a.out, "  run %s to add a level\n", bold("route add <level> <model> [description]"))
		return nil
	}
	fmt.Fprintf(a.out, "  %-12s %-20s %s\n", bold("LEVEL"), bold("MODEL"), bold("DESCRIPTION"))
	for _, l := range hr.Levels {
		fmt.Fprintf(a.out, "  %-12s %-20s %s\n", l.Name, l.Model, dim(l.Description))
	}
	fmt.Fprintln(a.out)
	if len(hr.ThinkingBudgets) > 0 {
		fmt.Fprintf(a.out, "  %s\n", bold("Thinking Budgets:"))
		for k, v := range hr.ThinkingBudgets {
			fmt.Fprintf(a.out, "    %-10s %d tokens\n", k, v)
		}
		fmt.Fprintln(a.out)
	}
	return nil
}

func (a *App) routeToggle(enable bool) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run init first")
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.HiveState == nil {
		cfg.HiveState = &initHiveState{}
	}
	if cfg.HiveState.HiveRoute == nil {
		cfg.HiveState.HiveRoute = &initHiveRoute{}
	}
	cfg.HiveState.HiveRoute.Enabled = enable
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return err
	}
	// The daemon holds its config in memory, so a write alone changes nothing
	// while `route status` reports the new state. Every other config-mutating
	// command restarts; these did not.
	autoRestart(a.out)
	if enable {
		fmt.Fprintf(a.out, "%s hiveroute enabled\n", green("✓"))
	} else {
		fmt.Fprintf(a.out, "%s hiveroute disabled\n", green("✓"))
	}
	return nil
}

func (a *App) routeAddLevel(name, model, desc string) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run init first")
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.HiveState == nil {
		cfg.HiveState = &initHiveState{}
	}
	if cfg.HiveState.HiveRoute == nil {
		cfg.HiveState.HiveRoute = &initHiveRoute{Enabled: true}
	}
	// Check if level already exists
	for i, l := range cfg.HiveState.HiveRoute.Levels {
		if strings.EqualFold(l.Name, name) {
			cfg.HiveState.HiveRoute.Levels[i].Model = model
			if desc != "" {
				cfg.HiveState.HiveRoute.Levels[i].Description = desc
			}
			out, err := yaml.Marshal(&cfg)
			if err != nil {
				return err
			}
			if err := os.WriteFile(configPath, out, 0644); err != nil {
				return err
			}
			autoRestart(a.out)
			fmt.Fprintf(a.out, "%s updated level %s → %s\n", green("✓"), bold(name), model)
			return nil
		}
	}
	cfg.HiveState.HiveRoute.Levels = append(cfg.HiveState.HiveRoute.Levels, initHiveRouteLevel{
		Name:        name,
		Model:       model,
		Description: desc,
	})
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return err
	}
	autoRestart(a.out)
	fmt.Fprintf(a.out, "%s added level %s → %s\n", green("✓"), bold(name), model)
	return nil
}

func (a *App) routeRemoveLevel(name string) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run init first")
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.HiveState == nil || cfg.HiveState.HiveRoute == nil {
		return fmt.Errorf("hiveroute not configured")
	}
	found := false
	levels := cfg.HiveState.HiveRoute.Levels[:0]
	for _, l := range cfg.HiveState.HiveRoute.Levels {
		if strings.EqualFold(l.Name, name) {
			found = true
			continue
		}
		levels = append(levels, l)
	}
	if !found {
		return fmt.Errorf("level %q not found", name)
	}
	cfg.HiveState.HiveRoute.Levels = levels
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return err
	}
	autoRestart(a.out)
	fmt.Fprintf(a.out, "%s removed level %s\n", green("✓"), bold(name))
	return nil
}

func (a *App) routeMetrics(ctx context.Context, limit int) error {
	metrics, err := a.client.StateMetrics(ctx, limit)
	if err != nil {
		return fmt.Errorf("fetch metrics: %w", err)
	}

	// Filter to only routed requests
	var routed []StateMetricResponse
	for _, m := range metrics {
		if m.RouteLevel != "" {
			routed = append(routed, m)
		}
	}

	if len(routed) == 0 {
		fmt.Fprintf(a.out, "%s no route decisions recorded yet\n", dim("○"))
		return nil
	}

	// Aggregate by model
	type modelStats struct {
		calls     int
		savedCost float64
		efforts   map[string]int
	}
	byModel := make(map[string]*modelStats)
	byLevel := make(map[string]int)
	var totalSaved float64

	for _, m := range routed {
		byLevel[m.RouteLevel]++
		if _, ok := byModel[m.RouteModel]; !ok {
			byModel[m.RouteModel] = &modelStats{efforts: make(map[string]int)}
		}
		s := byModel[m.RouteModel]
		s.calls++
		s.savedCost += m.RouteSavedCost
		totalSaved += m.RouteSavedCost
		if m.RouteEffort != "" {
			s.efforts[m.RouteEffort]++
		}
	}

	// Print summary
	fmt.Fprintf(a.out, "\n  %s\n", bold("HiveRoute Metrics"))
	fmt.Fprintf(a.out, "  %s\n", strings.Repeat("─", 50))
	fmt.Fprintf(a.out, "  Total routed:    %d / %d requests\n", len(routed), len(metrics))
	fmt.Fprintf(a.out, "  Total saved:     %s\n", green(fmt.Sprintf("$%.4f", totalSaved)))
	fmt.Fprintln(a.out)

	// By difficulty level
	fmt.Fprintf(a.out, "  %s\n", bold("By Difficulty Level:"))
	for level, count := range byLevel {
		pct := float64(count) / float64(len(routed)) * 100
		fmt.Fprintf(a.out, "    %-12s %4d  (%4.1f%%)\n", level, count, pct)
	}
	fmt.Fprintln(a.out)

	// By target model
	fmt.Fprintf(a.out, "  %s\n", bold("By Target Model:"))
	fmt.Fprintf(a.out, "  %-20s %6s %10s %s\n", "MODEL", "CALLS", "SAVED", "EFFORT DISTRIBUTION")
	for model, s := range byModel {
		effortStr := ""
		for e, c := range s.efforts {
			if effortStr != "" {
				effortStr += " "
			}
			effortStr += fmt.Sprintf("%s:%d", e, c)
		}
		fmt.Fprintf(a.out, "  %-20s %6d %10s %s\n", model, s.calls, fmt.Sprintf("$%.4f", s.savedCost), dim(effortStr))
	}
	fmt.Fprintln(a.out)

	fmt.Fprintf(a.out, "  %s\n\n", dim(fmt.Sprintf("showing last %d entries", len(metrics))))
	return nil
}

// routeTeamCmd handles per-team route configuration.
// Usage:
//
//	route team <team-id>                        show team route config
//	route team <team-id> enable | disable       toggle per-team hiveroute
//	route team <team-id> add <level> <model> [desc]  add/update a level
//	route team <team-id> rm <level>             remove a level
//	route team <team-id> reset                  remove all team overrides (use global)
func (a *App) routeTeamCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: route team <team-id> [enable|disable|add|rm|reset]")
	}
	teamID := args[0]
	if len(args) == 1 {
		return a.routeTeamStatus(ctx, teamID)
	}
	switch args[1] {
	case "enable":
		return a.routeTeamToggle(ctx, teamID, true)
	case "disable":
		return a.routeTeamToggle(ctx, teamID, false)
	case "add":
		if len(args) < 4 {
			return fmt.Errorf("usage: route team <team-id> add <level> <model> [description]")
		}
		desc := ""
		if len(args) > 4 {
			desc = strings.Join(args[4:], " ")
		}
		return a.routeTeamAddLevel(ctx, teamID, args[2], args[3], desc)
	case "rm", "remove":
		if len(args) < 3 {
			return fmt.Errorf("usage: route team <team-id> rm <level>")
		}
		return a.routeTeamRemoveLevel(ctx, teamID, args[2])
	case "reset":
		return a.routeTeamReset(ctx, teamID)
	default:
		return fmt.Errorf("unknown team route subcommand %q", args[1])
	}
}

func (a *App) routeTeamStatus(ctx context.Context, teamID string) error {
	settings, err := a.client.GetRouteSettings(ctx, teamID)
	if err != nil {
		return fmt.Errorf("fetch settings: %w", err)
	}

	fmt.Fprintf(a.out, "\n  %s %s\n", bold("HiveRoute for team"), bold(teamID))
	fmt.Fprintf(a.out, "  %s\n", strings.Repeat("─", 50))

	if settings.Enabled == nil {
		fmt.Fprintf(a.out, "  Enabled: %s (inherits global)\n", dim("not set"))
	} else if *settings.Enabled {
		fmt.Fprintf(a.out, "  Enabled: %s\n", green("yes"))
	} else {
		fmt.Fprintf(a.out, "  Enabled: %s\n", dim("no"))
	}

	if len(settings.Levels) == 0 {
		fmt.Fprintf(a.out, "  Levels:  %s (inherits global)\n", dim("not set"))
	} else {
		fmt.Fprintln(a.out)
		fmt.Fprintf(a.out, "  %-12s %-20s %s\n", bold("LEVEL"), bold("MODEL"), bold("DESCRIPTION"))
		for _, l := range settings.Levels {
			fmt.Fprintf(a.out, "  %-12s %-20s %s\n", l.Name, l.Model, dim(l.Description))
		}
	}
	fmt.Fprintln(a.out)
	return nil
}

func (a *App) routeTeamToggle(ctx context.Context, teamID string, enable bool) error {
	_, err := a.client.UpdateRouteSettings(ctx, map[string]any{
		"team_id": teamID,
		"enabled": enable,
	})
	if err != nil {
		return err
	}
	if enable {
		fmt.Fprintf(a.out, "%s hiveroute enabled for team %s\n", green("✓"), bold(teamID))
	} else {
		fmt.Fprintf(a.out, "%s hiveroute disabled for team %s\n", green("✓"), bold(teamID))
	}
	return nil
}

func (a *App) routeTeamAddLevel(ctx context.Context, teamID, name, model, desc string) error {
	// Get existing settings
	settings, err := a.client.GetRouteSettings(ctx, teamID)
	if err != nil {
		return err
	}

	// Update or add level
	found := false
	for i, l := range settings.Levels {
		if strings.EqualFold(l.Name, name) {
			settings.Levels[i].Model = model
			if desc != "" {
				settings.Levels[i].Description = desc
			}
			found = true
			break
		}
	}
	if !found {
		settings.Levels = append(settings.Levels, RouteSettingsLevel{Name: name, Model: model, Description: desc})
	}

	// Convert to API format
	levels := make([]map[string]string, len(settings.Levels))
	for i, l := range settings.Levels {
		levels[i] = map[string]string{"name": l.Name, "model": l.Model, "description": l.Description}
	}

	_, err = a.client.UpdateRouteSettings(ctx, map[string]any{
		"team_id": teamID,
		"levels":  levels,
	})
	if err != nil {
		return err
	}
	if found {
		fmt.Fprintf(a.out, "%s updated level %s → %s for team %s\n", green("✓"), bold(name), model, bold(teamID))
	} else {
		fmt.Fprintf(a.out, "%s added level %s → %s for team %s\n", green("✓"), bold(name), model, bold(teamID))
	}
	return nil
}

func (a *App) routeTeamRemoveLevel(ctx context.Context, teamID, name string) error {
	settings, err := a.client.GetRouteSettings(ctx, teamID)
	if err != nil {
		return err
	}

	found := false
	// Initialised, not nil: a nil slice serializes to JSON null, and the server
	// reads null as "field absent" and leaves the levels untouched — while this
	// command printed success. Removing the last level was the only case that
	// produced an empty slice, so it was the only one that silently did nothing.
	levels := []map[string]string{}
	for _, l := range settings.Levels {
		if strings.EqualFold(l.Name, name) {
			found = true
			continue
		}
		levels = append(levels, map[string]string{"name": l.Name, "model": l.Model, "description": l.Description})
	}
	if !found {
		return fmt.Errorf("level %q not found for team %s", name, teamID)
	}

	_, err = a.client.UpdateRouteSettings(ctx, map[string]any{
		"team_id": teamID,
		"levels":  levels,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s removed level %s for team %s\n", green("✓"), bold(name), bold(teamID))
	return nil
}

func (a *App) routeTeamReset(ctx context.Context, teamID string) error {
	_, err := a.client.UpdateRouteSettings(ctx, map[string]any{
		"team_id":       teamID,
		"reset_enabled": true,
		"levels":        []map[string]string{},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s reset route config for team %s (will use global defaults)\n", green("✓"), bold(teamID))
	return nil
}

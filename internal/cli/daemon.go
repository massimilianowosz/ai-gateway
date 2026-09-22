package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

func ubiquumHome() string {
	return UbiquumHome()
}

// UbiquumHome returns the path to the .ubiquum directory.
func UbiquumHome() string {
	dir := os.Getenv("UBIQUUM_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".ubiquum")
	}
	return dir
}

func ConfigFilePath() string {
	return filepath.Join(ubiquumHome(), "gateway.yaml")
}

func pidFilePath() string {
	return filepath.Join(ubiquumHome(), "gateway.pid")
}

func logFilePath() string {
	return filepath.Join(ubiquumHome(), "gateway.log")
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file: %w", err)
	}
	return pid, nil
}

func writePID(pid int) error {
	dir := filepath.Dir(pidFilePath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(pidFilePath(), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func removePID() {
	os.Remove(pidFilePath())
}

func (a *App) runOrchestratorCommand(ctx context.Context, envName, action string) (bool, error) {
	command := strings.TrimSpace(os.Getenv(envName))
	if command == "" {
		return false, nil
	}

	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Stdout = a.out
	cmd.Stderr = a.out
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return true, fmt.Errorf("%s command failed: %w", action, err)
	}
	return true, nil
}

func isProcessAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		// On Windows, FindProcess always succeeds. Use kill(0) signal.
		proc, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		return proc.Signal(syscall.Signal(0)) == nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// promptRestart checks if the gateway is running and offers to restart it.
// If the gateway is not running, it prints a hint instead.
func promptRestart(reader lineReader, out io.Writer) {
	pid, err := readPID()
	if err != nil || !isProcessAlive(pid) {
		// Gateway not running — offer to start
		answer, ok := reader.ReadLine("  Start gateway now? [Y/n]: ")
		if !ok {
			return
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			fmt.Fprintln(out, dim("  start manually when ready"))
			return
		}
		doStart(out)
		return
	}
	answer, ok := reader.ReadLine("  Restart gateway now? [Y/n]: ")
	if !ok {
		return
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "" && answer != "y" && answer != "yes" {
		fmt.Fprintln(out, dim("  restart manually when ready"))
		return
	}
	doRestart(out)
}

// autoRestart restarts the gateway silently if it's running.
func autoRestart(out io.Writer) {
	pid, err := readPID()
	if err != nil || !isProcessAlive(pid) {
		return
	}
	doRestart(out)
}

func doRestart(out io.Writer) {
	pid, _ := readPID()
	if pid > 0 {
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Signal(syscall.SIGTERM)
			time.Sleep(500 * time.Millisecond)
		}
	}
	removePID()
	doStart(out)
}

func doStart(out io.Writer) {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}

	logPath := logFilePath()
	dir := filepath.Dir(logPath)
	_ = os.MkdirAll(dir, 0o755)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(out, "  %s start failed: %v\n", red("✗"), err)
		return
	}

	cmd := exec.Command(exe, "serve", "-config", ConfigFilePath())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		fmt.Fprintf(out, "  %s start failed: %v\n", red("✗"), err)
		return
	}
	logFile.Close()

	_ = writePID(cmd.Process.Pid)
	time.Sleep(500 * time.Millisecond)
	if !isProcessAlive(cmd.Process.Pid) {
		fmt.Fprintf(out, "  %s gateway exited unexpectedly; check logs\n", red("✗"))
		return
	}
	fmt.Fprintf(out, "  %s gateway started (pid %d)\n", green("✓"), cmd.Process.Pid)
}

func (a *App) start(ctx context.Context, args []string) error {
	if handled, err := a.runOrchestratorCommand(ctx, "UBIQUUM_START_CMD", "start"); handled {
		return err
	}

	// Check if already running
	if pid, err := readPID(); err == nil && isProcessAlive(pid) {
		return fmt.Errorf("gateway already running (pid %d); use /restart to bounce", pid)
	}

	fs := newFlagSet("start")
	configPath := ConfigFilePath()
	fs.StringVar(&configPath, "config", configPath, "path to configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Find the ubiquum binary
	exe, err := os.Executable()
	if err != nil {
		exe = "ubiquum"
	}

	logPath := logFilePath()
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command(exe, "serve", "-config", configPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start gateway: %w", err)
	}
	logFile.Close()

	if err := writePID(cmd.Process.Pid); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}

	// Wait and verify the process is alive
	for i := 0; i < 10; i++ {
		time.Sleep(200 * time.Millisecond)
		if !isProcessAlive(cmd.Process.Pid) {
			removePID()
			// Show last few log lines to explain why
			if logData, err := os.ReadFile(logPath); err == nil {
				lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
				start := len(lines) - 5
				if start < 0 {
					start = 0
				}
				fmt.Fprintf(a.out, "%s gateway crashed at startup:\n", red("●"))
				for _, l := range lines[start:] {
					fmt.Fprintf(a.out, "  %s\n", dim(l))
				}
			}
			return fmt.Errorf("gateway failed to start; fix config and retry")
		}
	}

	fmt.Fprintf(a.out, "%s gateway started (pid %d)\n", green("●"), cmd.Process.Pid)
	fmt.Fprintf(a.out, "  config: %s\n", configPath)
	fmt.Fprintf(a.out, "  logs:   %s\n", dim(logPath))
	a.client.ReloadKey()
	return nil
}

func (a *App) stop(ctx context.Context) error {
	if handled, err := a.runOrchestratorCommand(ctx, "UBIQUUM_STOP_CMD", "stop"); handled {
		return err
	}

	pid, err := readPID()
	if err != nil {
		return fmt.Errorf("gateway not running (no pid file)")
	}
	if !isProcessAlive(pid) {
		removePID()
		return fmt.Errorf("gateway not running (stale pid %d)", pid)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM: %w", err)
	}

	// Wait up to 5 seconds for graceful shutdown
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isProcessAlive(pid) {
			removePID()
			fmt.Fprintf(a.out, "%s gateway stopped (was pid %d)\n", green("●"), pid)
			return nil
		}
	}

	// Force kill
	_ = proc.Signal(syscall.SIGKILL)
	removePID()
	fmt.Fprintf(a.out, "%s gateway killed (pid %d, did not exit gracefully)\n", yellow("●"), pid)
	return nil
}

func (a *App) restart(ctx context.Context, args []string) error {
	if handled, err := a.runOrchestratorCommand(ctx, "UBIQUUM_RESTART_CMD", "restart"); handled {
		return err
	}

	// Try to stop (ignore errors if not running)
	if pid, err := readPID(); err == nil && isProcessAlive(pid) {
		if err := a.stop(ctx); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return a.start(ctx, args)
}

func (a *App) logs(ctx context.Context, args []string) error {
	if handled, err := a.runOrchestratorCommand(ctx, "UBIQUUM_LOGS_CMD", "logs"); handled {
		return err
	}

	// Parse: "logs" (follow), "logs n 20" (last 20 lines)
	lines := 0
	follow := true
	for i := 0; i < len(args); i++ {
		key := strings.TrimLeft(args[i], "-")
		switch key {
		case "n":
			if i+1 < len(args) {
				i++
				n, err := strconv.Atoi(args[i])
				if err != nil {
					return fmt.Errorf("invalid -n value: %s", args[i])
				}
				lines = n
				follow = false
			}
		case "f":
			follow = true
		}
	}

	logPath := logFilePath()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		// No local log file — fetch from remote gateway
		return a.logsRemote(ctx)
	}

	if !follow {
		return a.logsLast(logPath, lines)
	}
	return a.logsFollow(ctx, logPath)
}

func (a *App) logsRemote(ctx context.Context) error {
	fmt.Fprintln(a.out, dim("following remote gateway logs (ctrl-c to stop)"))
	var lastLine string
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		body, err := a.client.GetRaw(ctx, "/v1/logs")
		if err != nil {
			return fmt.Errorf("failed to fetch remote logs: %w", err)
		}
		text := strings.TrimRight(string(body), "\n")
		var lines []string
		if text != "" {
			lines = strings.Split(text, "\n")
		}

		// The remote endpoint always returns the last N lines of a fixed-size ring
		// buffer, so once the buffer fills up its length stops growing even though
		// its content keeps changing. Diff by content (last seen line) instead of
		// by count, otherwise new log lines stop showing up after the buffer fills.
		startIdx := 0
		if lastLine != "" {
			found := false
			for i := len(lines) - 1; i >= 0; i-- {
				if lines[i] == lastLine {
					startIdx = i + 1
					found = true
					break
				}
			}
			if !found {
				// Buffer rotated past the last line we printed — print the whole
				// window rather than silently dropping lines.
				startIdx = 0
			}
		}
		for _, line := range lines[startIdx:] {
			a.printLogLine(line)
		}
		if len(lines) > 0 {
			lastLine = lines[len(lines)-1]
		}
		time.Sleep(2 * time.Second)
	}
}

func (a *App) logsLast(logPath string, lines int) error {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Errorf("read logs: %w", err)
	}
	allLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	start := len(allLines) - lines
	if start < 0 {
		start = 0
	}
	for _, l := range allLines[start:] {
		a.printLogLine(l)
	}
	return nil
}

func (a *App) logsFollow(ctx context.Context, logPath string) error {
	f, err := os.Open(logPath)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	// Seek to end
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	fmt.Fprintln(a.out, dim("following "+logPath+" (ctrl-c to stop)"))

	buf := make([]byte, 4096)
	var partial string
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			chunk := partial + string(buf[:n])
			lines := strings.Split(chunk, "\n")
			// Last element may be incomplete
			partial = lines[len(lines)-1]
			for _, l := range lines[:len(lines)-1] {
				a.printLogLine(l)
			}
		}
		if err == io.EOF || n == 0 {
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func (a *App) printLogLine(line string) {
	if line == "" {
		return
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		fmt.Fprintln(a.out, dim(line))
		return
	}

	// Extract time
	ts := ""
	if t, ok := entry["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			ts = parsed.Format("15:04:05")
		} else {
			ts = t
		}
	}

	level := ""
	if l, ok := entry["level"].(string); ok {
		level = l
	}

	msg := ""
	if m, ok := entry["msg"].(string); ok {
		msg = m
	}

	// For request logs, show tabular format
	if msg == "request" {
		method, _ := entry["method"].(string)
		path, _ := entry["path"].(string)
		status := int(getFloat(entry, "status"))
		dur := int(getFloat(entry, "duration_ms"))

		statusStr := fmt.Sprintf("%d", status)
		switch {
		case status >= 500:
			statusStr = red(statusStr)
		case status >= 400:
			statusStr = yellow(statusStr)
		default:
			statusStr = green(statusStr)
		}

		fmt.Fprintf(a.out, "%s  %s  %-4s %s  %dms\n",
			dim(ts), statusStr, method, path, dur)
		return
	}

	// Other log lines
	var levelStr string
	switch level {
	case "ERROR":
		levelStr = red("ERR")
	case "WARN":
		levelStr = yellow("WRN")
	case "INFO":
		levelStr = green("INF")
	case "DEBUG":
		levelStr = dim("DBG")
	default:
		levelStr = dim(level)
	}
	extra := fmtExtraValues(entry)
	if extra != "" {
		fmt.Fprintf(a.out, "%s  %s  %s  %s\n", dim(ts), levelStr, msg, dim(extra))
	} else {
		fmt.Fprintf(a.out, "%s  %s  %s\n", dim(ts), levelStr, msg)
	}
}

func getFloat(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func fmtExtraValues(entry map[string]any) string {
	skip := map[string]bool{"time": true, "level": true, "msg": true, "method": true, "path": true, "status": true, "duration_ms": true}
	var parts []string
	for k, v := range entry {
		if skip[k] {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func (a *App) cacheCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "status" {
		return a.cacheStatus()
	}
	switch args[0] {
	case "init":
		// In interactive shell, this is handled by the shell dispatcher.
		// If we reach here, it means non-interactive mode without stdin wiring.
		return fmt.Errorf("cache init requires interactive mode; use individual 'cache set' commands instead")
	case "enable":
		return a.cacheSetField("enabled", "true")
	case "disable":
		return a.cacheSetField("enabled", "false")
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: cache set <key> <value>\n  keys: backend, embedding_model, tweak_model, redis_url, qdrant_url")
		}
		return a.cacheSetField(args[1], strings.Join(args[2:], " "))
	case "flush":
		return a.cacheFlush(ctx)
	case "metrics":
		limit := 50
		if len(args) >= 2 {
			if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
				limit = n
			}
		}
		return a.cacheMetrics(ctx, limit)
	default:
		return fmt.Errorf("unknown cache subcommand %q; try: init, enable, disable, set, status, flush, metrics", args[0])
	}
}

func (a *App) cacheStatus() error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(a.out, "%s cache not configured (no config file)\n", dim("○"))
		return nil
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.Cache == nil || !cfg.Cache.Enabled {
		fmt.Fprintf(a.out, "%s cache disabled\n", dim("○"))
		fmt.Fprintf(a.out, "  run %s to set up\n", bold("cache init"))
		return nil
	}
	fmt.Fprintf(a.out, "%s cache enabled\n", green("●"))
	fmt.Fprintf(a.out, "  backend:         %s\n", valueOrDefault(cfg.Cache.Backend, "memory"))
	fmt.Fprintf(a.out, "  embedding_model: %s\n", valueOrDefault(cfg.Cache.EmbeddingModel, dim("(not set)")))
	if cfg.Cache.TweakModel != "" {
		fmt.Fprintf(a.out, "  tweak_model:     %s\n", cfg.Cache.TweakModel)
	}
	if cfg.Cache.RedisURL != "" {
		fmt.Fprintf(a.out, "  redis_url:       %s\n", cfg.Cache.RedisURL)
	}
	if cfg.Cache.QdrantURL != "" {
		fmt.Fprintf(a.out, "  qdrant_url:      %s\n", cfg.Cache.QdrantURL)
	}
	return nil
}

func (a *App) cacheSetField(key, value string) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run init first")
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.Cache == nil {
		cfg.Cache = &initCache{}
	}

	switch key {
	case "enabled":
		cfg.Cache.Enabled = value == "true" || value == "1" || value == "yes" || value == "on"
	case "backend":
		switch value {
		case "memory", "redis", "qdrant", "pgvector":
			cfg.Cache.Backend = value
		default:
			return fmt.Errorf("unknown backend %q; options: memory, redis, qdrant, pgvector", value)
		}
	case "embedding_model":
		cfg.Cache.EmbeddingModel = value
	case "tweak_model":
		cfg.Cache.TweakModel = value
	case "redis_url":
		cfg.Cache.RedisURL = value
	case "qdrant_url":
		cfg.Cache.QdrantURL = value
	default:
		return fmt.Errorf("unknown cache key %q; available: backend, embedding_model, tweak_model, redis_url, qdrant_url", key)
	}

	out2, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	header := "# Ubiquum Gateway configuration\n# Docs: https://docs.ubiquum.io/gateway/config\n\n"
	if err := os.WriteFile(configPath, []byte(header+string(out2)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(a.out, "%s cache.%s = %s\n", green("✓"), bold(key), value)
	autoRestart(a.out)
	return nil
}

func (a *App) cacheFlush(ctx context.Context) error {
	if err := a.client.CacheFlush(ctx); err != nil {
		return fmt.Errorf("flush failed: %w", err)
	}
	fmt.Fprintf(a.out, "%s cache flushed\n", green("✓"))
	return nil
}

func (a *App) cacheMetrics(ctx context.Context, limit int) error {
	metrics, err := a.client.CacheMetrics(ctx, limit)
	if err != nil {
		return fmt.Errorf("fetch metrics: %w", err)
	}
	if len(metrics) == 0 {
		fmt.Fprintf(a.out, "%s no cache metrics recorded yet\n", dim("○"))
		return nil
	}

	// Aggregate by band
	type bandStats struct {
		count       int
		tokensSaved int
		totalLatMs  int64
		totalScore  float64
	}
	bands := make(map[string]*bandStats)
	bandOrder := []string{"DIRECT", "REUSE", "TWEAK", "MISS"}
	for _, b := range bandOrder {
		bands[b] = &bandStats{}
	}
	var totalSaved int
	for _, m := range metrics {
		s, ok := bands[m.Band]
		if !ok {
			s = &bandStats{}
			bands[m.Band] = s
			bandOrder = append(bandOrder, m.Band)
		}
		s.count++
		s.tokensSaved += m.TokensSaved
		s.totalLatMs += m.LatencyMs
		s.totalScore += m.Score
		totalSaved += m.TokensSaved
	}

	// Summary
	fmt.Fprintf(a.out, "\n  %s\n", bold("Cache Metrics Summary"))
	fmt.Fprintf(a.out, "  %s\n", strings.Repeat("─", 50))
	fmt.Fprintf(a.out, "  Total:        %d\n", len(metrics))
	fmt.Fprintln(a.out)

	// Per-band breakdown
	fmt.Fprintf(a.out, "  %-10s %8s %10s %8s %10s\n", "BAND", "COUNT", "TOKENS ↓", "AVG MS", "AVG SCORE")
	fmt.Fprintf(a.out, "  %s\n", strings.Repeat("─", 50))
	for _, band := range bandOrder {
		s := bands[band]
		if s.count == 0 {
			continue
		}
		avgMs := s.totalLatMs / int64(s.count)
		avgScore := s.totalScore / float64(s.count)

		bandPadded := fmt.Sprintf("%-10s", band)
		switch band {
		case "DIRECT", "REUSE":
			bandPadded = green(bandPadded)
		case "TWEAK":
			bandPadded = bold(bandPadded)
		case "MISS":
			bandPadded = dim(bandPadded)
		}
		fmt.Fprintf(a.out, "  %s %8d %10d %8d %10.4f\n",
			bandPadded, s.count, s.tokensSaved, avgMs, avgScore)
	}
	fmt.Fprintln(a.out)

	// Totals
	hits := bands["DIRECT"].count + bands["REUSE"].count + bands["TWEAK"].count
	total := len(metrics)
	if total > 0 {
		hitRate := float64(hits) / float64(total) * 100
		fmt.Fprintf(a.out, "  Hit rate:     %s (%d/%d)\n", green(fmt.Sprintf("%.1f%%", hitRate)), hits, total)
		fmt.Fprintf(a.out, "  Tokens saved: %d\n", totalSaved)
	}
	fmt.Fprintf(a.out, "\n  %s\n\n", dim(fmt.Sprintf("showing last %d entries", len(metrics))))
	return nil
}

func (a *App) cacheInit(reader lineReader, out io.Writer) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run 'init' first to create gateway.yaml")
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.Cache == nil {
		cfg.Cache = &initCache{}
	}

	fmt.Fprintf(out, "\n%s\n\n", bold("Semantic Cache Setup"))

	// Backend
	ans, ok := reader.ReadLine("  Backend [memory]: ")
	if !ok {
		return nil
	}
	backend := strings.TrimSpace(ans)
	if backend == "" {
		backend = "memory"
	}
	switch backend {
	case "memory", "redis", "qdrant", "pgvector":
		cfg.Cache.Backend = backend
	default:
		return fmt.Errorf("unknown backend %q; options: memory, redis, qdrant, pgvector", backend)
	}

	// Embedding model
	models := configuredModelNames()
	hint := ""
	if len(models) > 0 {
		hint = " (" + strings.Join(models, ", ") + ")"
	}
	ans, ok = reader.ReadLineWithHints(fmt.Sprintf("  Embedding model%s: ", hint), models)
	if !ok {
		return nil
	}
	if em := strings.TrimSpace(ans); em != "" {
		cfg.Cache.EmbeddingModel = em
	}

	// Tweak model (optional)
	ans, ok = reader.ReadLineWithHints("  Tweak model (optional, for TWEAK band) [skip]: ", models)
	if !ok {
		return nil
	}
	if tm := strings.TrimSpace(ans); tm != "" && tm != "skip" {
		cfg.Cache.TweakModel = tm
	}

	// Backend-specific URLs
	switch backend {
	case "redis":
		ans, ok = reader.ReadLine("  Redis URL [redis://localhost:6379]: ")
		if !ok {
			return nil
		}
		url := strings.TrimSpace(ans)
		if url == "" {
			url = "redis://localhost:6379"
		}
		cfg.Cache.RedisURL = url
	case "qdrant":
		ans, ok = reader.ReadLine("  Qdrant URL [http://localhost:6333]: ")
		if !ok {
			return nil
		}
		url := strings.TrimSpace(ans)
		if url == "" {
			url = "http://localhost:6333"
		}
		cfg.Cache.QdrantURL = url
	}

	cfg.Cache.Enabled = true

	// Write
	out2, err2 := yaml.Marshal(cfg)
	if err2 != nil {
		return fmt.Errorf("marshal config: %w", err2)
	}
	header := "# Ubiquum Gateway configuration\n# Docs: https://docs.ubiquum.io/gateway/config\n\n"
	if err := os.WriteFile(configPath, []byte(header+string(out2)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s cache configured and enabled\n", green("✓"))
	fmt.Fprintf(out, "  backend:         %s\n", cfg.Cache.Backend)
	fmt.Fprintf(out, "  embedding_model: %s\n", cfg.Cache.EmbeddingModel)
	if cfg.Cache.TweakModel != "" {
		fmt.Fprintf(out, "  tweak_model:     %s\n", cfg.Cache.TweakModel)
	}
	fmt.Fprintln(out)
	autoRestart(out)
	return nil
}

// --- HiveState CLI commands ---

func (a *App) stateCmd(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "status" {
		return a.stateStatus()
	}
	switch args[0] {
	case "init":
		return fmt.Errorf("state init requires interactive mode; use individual 'state set' commands instead")
	case "enable":
		return a.stateSetField("enabled", "true")
	case "disable":
		return a.stateSetField("enabled", "false")
	case "metrics":
		limit := 50
		if len(args) > 1 {
			if n, err := strconv.Atoi(args[1]); err == nil && n > 0 {
				limit = n
			}
		}
		return a.stateMetrics(ctx, limit)
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: state set <key> <value>\n  keys: model, threshold, max_latency_ms")
		}
		return a.stateSetField(args[1], strings.Join(args[2:], " "))
	default:
		return fmt.Errorf("unknown state subcommand %q; try: init, enable, disable, set, metrics, status", args[0])
	}
}

func (a *App) stateStatus() error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(a.out, "%s hivestate not configured (no config file)\n", dim("○"))
		return nil
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.HiveState == nil || !cfg.HiveState.Enabled {
		fmt.Fprintf(a.out, "%s hivestate disabled\n", dim("○"))
		fmt.Fprintf(a.out, "  run %s to set up\n", bold("state init"))
		return nil
	}
	fmt.Fprintf(a.out, "%s hivestate enabled\n", green("●"))
	fmt.Fprintf(a.out, "  model:          %s\n", valueOrDefault(cfg.HiveState.Model, dim("(not set)")))
	if cfg.HiveState.Threshold > 0 {
		fmt.Fprintf(a.out, "  threshold:      %d tokens\n", cfg.HiveState.Threshold)
	}
	if cfg.HiveState.MaxLatencyMs > 0 {
		fmt.Fprintf(a.out, "  max_latency_ms: %d\n", cfg.HiveState.MaxLatencyMs)
	}
	return nil
}

func (a *App) stateMetrics(ctx context.Context, limit int) error {
	metrics, err := a.client.StateMetrics(ctx, limit)
	if err != nil {
		return fmt.Errorf("fetch state metrics: %w", err)
	}
	if len(metrics) == 0 {
		fmt.Fprintf(a.out, "%s no state metrics recorded yet\n", dim("○"))
		return nil
	}

	// Aggregate stats
	var totalState, totalNoop int
	var totalOriginal, totalResult int
	var totalLatency int64
	var fallbacks int
	intentCounts := make(map[string]int)
	for _, m := range metrics {
		if m.Mode == "state" {
			totalState++
			totalOriginal += m.OriginalTokens
			totalResult += m.ResultTokens
			totalLatency += m.LatencyMs
			if m.Intent != "" {
				intentCounts[m.Intent]++
			}
		} else {
			totalNoop++
		}
		if m.FallbackReason != "" {
			fallbacks++
		}
	}

	// Summary
	fmt.Fprintf(a.out, "\n  %s\n", bold("HiveState Metrics Summary"))
	fmt.Fprintf(a.out, "  %s\n", strings.Repeat("─", 40))
	fmt.Fprintf(a.out, "  Total:        %d\n", len(metrics))
	fmt.Fprintf(a.out, "  STATE:        %s\n", green(fmt.Sprintf("%d", totalState)))
	fmt.Fprintf(a.out, "  NOOP:         %s\n", dim(fmt.Sprintf("%d", totalNoop)))
	if fallbacks > 0 {
		fmt.Fprintf(a.out, "  Fallbacks:    %s\n", yellow(fmt.Sprintf("%d", fallbacks)))
	}
	fmt.Fprintln(a.out)

	// Token reduction
	if totalOriginal > 0 {
		reduction := 1.0 - float64(totalResult)/float64(totalOriginal)
		fmt.Fprintf(a.out, "  Original:     %d tokens\n", totalOriginal)
		fmt.Fprintf(a.out, "  Compressed:   %d tokens\n", totalResult)
		fmt.Fprintf(a.out, "  Reduction:    %s\n", green(fmt.Sprintf("%.1f%%", reduction*100)))
		fmt.Fprintf(a.out, "  Tokens saved: %d\n", totalOriginal-totalResult)
		fmt.Fprintln(a.out)
	}

	// Latency
	if totalState > 0 {
		avgMs := totalLatency / int64(totalState)
		fmt.Fprintf(a.out, "  Avg latency:  %dms\n", avgMs)
		fmt.Fprintln(a.out)
	}

	// Top intents
	if len(intentCounts) > 0 {
		fmt.Fprintf(a.out, "  %s\n", bold("Top Intents:"))
		type kv struct {
			k string
			v int
		}
		var sorted []kv
		for k, v := range intentCounts {
			sorted = append(sorted, kv{k, v})
		}
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].v > sorted[i].v {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		max := 10
		if len(sorted) < max {
			max = len(sorted)
		}
		for _, s := range sorted[:max] {
			fmt.Fprintf(a.out, "    %-30s %d\n", s.k, s.v)
		}
		fmt.Fprintln(a.out)
	}

	fmt.Fprintf(a.out, "  %s\n\n", dim(fmt.Sprintf("showing last %d entries", len(metrics))))
	return nil
}

func (a *App) stateSetField(key, value string) error {
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

	switch key {
	case "enabled":
		cfg.HiveState.Enabled = value == "true" || value == "1" || value == "yes" || value == "on"
	case "model":
		cfg.HiveState.Model = value
	case "threshold":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("threshold must be a positive integer")
		}
		cfg.HiveState.Threshold = n
	case "max_latency_ms":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("max_latency_ms must be a positive integer")
		}
		cfg.HiveState.MaxLatencyMs = n
	default:
		return fmt.Errorf("unknown hivestate key %q; available: model, threshold, max_latency_ms", key)
	}

	out2, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	header := "# Ubiquum Gateway configuration\n# Docs: https://docs.ubiquum.io/gateway/config\n\n"
	if err := os.WriteFile(configPath, []byte(header+string(out2)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(a.out, "%s hivestate.%s = %s\n", green("✓"), bold(key), value)
	autoRestart(a.out)
	return nil
}

func (a *App) stateInit(reader lineReader, out io.Writer) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run 'init' first to create gateway.yaml")
	}
	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.HiveState == nil {
		cfg.HiveState = &initHiveState{}
	}

	fmt.Fprintf(out, "\n%s\n\n", bold("🧠 HiveState Setup"))
	fmt.Fprintf(out, "  HiveState extracts conversational state from long histories\n")
	fmt.Fprintf(out, "  and forwards only intent + constraints to the upstream model.\n\n")

	// Model selection
	models := configuredModelNames()
	hint := ""
	if len(models) > 0 {
		hint = " (" + strings.Join(models, ", ") + ")"
	}
	ans, ok := reader.ReadLineWithHints(fmt.Sprintf("  State extraction model%s: ", hint), models)
	if !ok {
		return nil
	}
	if m := strings.TrimSpace(ans); m != "" {
		cfg.HiveState.Model = m
	}
	if cfg.HiveState.Model == "" {
		return fmt.Errorf("a model is required for HiveState; configure one in models:[] first")
	}

	// Threshold
	ans, ok = reader.ReadLine("  Activation threshold (tokens) [4000]: ")
	if !ok {
		return nil
	}
	if t := strings.TrimSpace(ans); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return fmt.Errorf("threshold must be a positive integer")
		}
		cfg.HiveState.Threshold = n
	} else {
		cfg.HiveState.Threshold = 4000
	}

	// Max latency
	ans, ok = reader.ReadLine("  Max extraction latency (ms) [3000]: ")
	if !ok {
		return nil
	}
	if l := strings.TrimSpace(ans); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			return fmt.Errorf("max_latency_ms must be a positive integer")
		}
		cfg.HiveState.MaxLatencyMs = n
	} else {
		cfg.HiveState.MaxLatencyMs = 3000
	}

	cfg.HiveState.Enabled = true

	// Write
	out2, err2 := yaml.Marshal(cfg)
	if err2 != nil {
		return fmt.Errorf("marshal config: %w", err2)
	}
	header := "# Ubiquum Gateway configuration\n# Docs: https://docs.ubiquum.io/gateway/config\n\n"
	if err := os.WriteFile(configPath, []byte(header+string(out2)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s hivestate configured and enabled\n", green("✓"))
	fmt.Fprintf(out, "  model:          %s\n", cfg.HiveState.Model)
	fmt.Fprintf(out, "  threshold:      %d tokens\n", cfg.HiveState.Threshold)
	fmt.Fprintf(out, "  max_latency_ms: %d\n", cfg.HiveState.MaxLatencyMs)
	fmt.Fprintln(out)
	autoRestart(out)
	return nil
}

func valueOrDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func (a *App) configCmd(ctx context.Context, args []string) error {
	// config set <key> <value>
	if len(args) >= 3 && args[0] == "set" {
		return a.configSet(args[1], strings.Join(args[2:], " "))
	}

	configPath := ConfigFilePath()
	if len(args) > 0 && args[0] != "set" {
		configPath = args[0]
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	fmt.Fprintf(a.out, "%s config loaded: %s (%d bytes)\n", green("✓"), configPath, len(data))
	fmt.Fprintln(a.out, dim(string(data)))
	return nil
}

func (a *App) configSet(key, value string) error {
	configPath := ConfigFilePath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("no config found; run init first")
	}

	var cfg initConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	switch key {
	case "port":
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid port: %s", value)
		}
		cfg.Server.Port = port
	case "master_key":
		cfg.Server.MasterKey = value
	case "strategy":
		switch value {
		case "shuffle", "round-robin", "latency":
			cfg.Router.Strategy = value
		default:
			return fmt.Errorf("unknown strategy %q; options: shuffle, round-robin, latency", value)
		}
	case "retries":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid retries: %s", value)
		}
		cfg.Router.Retries = n
	case "swagger":
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			cfg.Server.Swagger = true
		case "false", "0", "no", "off":
			cfg.Server.Swagger = false
		default:
			return fmt.Errorf("invalid swagger value %q; use true or false", value)
		}
	default:
		return fmt.Errorf("unknown setting %q; available: port, master_key, strategy, retries, swagger\n  for cache settings use: cache set <key> <value>", key)
	}

	out2, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	header := "# Ubiquum Gateway configuration\n# Docs: https://docs.ubiquum.io/gateway/config\n\n"
	if err := os.WriteFile(configPath, []byte(header+string(out2)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(a.out, "%s %s = %s\n", green("✓"), bold(key), value)
	autoRestart(a.out)
	if key == "master_key" {
		a.client.ReloadKey()
	}
	return nil
}

func printDaemonStatus(w io.Writer) {
	pid, err := readPID()
	if err != nil {
		fmt.Fprintf(w, "%s gateway not running\n", dim("○"))
		return
	}
	if !isProcessAlive(pid) {
		removePID()
		fmt.Fprintf(w, "%s gateway not running (stale pid cleaned)\n", dim("○"))
		return
	}
	fmt.Fprintf(w, "%s gateway running (pid %d)\n", green("●"), pid)
}

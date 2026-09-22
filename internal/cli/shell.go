package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chzyer/readline"
)

// lineReader abstracts line input for both interactive (readline) and piped (scanner) modes.
type lineReader interface {
	ReadLine(prompt string) (string, bool)
	ReadLineWithHints(prompt string, hints []string) (string, bool)
}

// rlReader wraps chzyer/readline for interactive input.
type rlReader struct {
	rl *readline.Instance
}

func (r *rlReader) ReadLine(prompt string) (string, bool) {
	r.rl.SetPrompt(prompt)
	line, err := r.rl.Readline()
	if err != nil {
		return "", false
	}
	return line, true
}

func (r *rlReader) ReadLineWithHints(prompt string, hints []string) (string, bool) {
	// Build PcItems for dynamic completion (empty list = no completions)
	var completer *readline.PrefixCompleter
	if len(hints) > 0 {
		items := make([]readline.PrefixCompleterInterface, len(hints))
		for i, h := range hints {
			items[i] = readline.PcItem(h)
		}
		completer = readline.NewPrefixCompleter(items...)
	} else {
		completer = readline.NewPrefixCompleter()
	}
	prev := r.rl.Config.AutoComplete
	r.rl.Config.AutoComplete = completer
	defer func() { r.rl.Config.AutoComplete = prev }()

	r.rl.SetPrompt(prompt)
	line, err := r.rl.Readline()
	if err != nil {
		return "", false
	}
	return line, true
}

// scanReader wraps bufio.Scanner for piped/non-TTY input.
type scanReader struct {
	sc  *bufio.Scanner
	out io.Writer
}

func (r *scanReader) ReadLine(prompt string) (string, bool) {
	fmt.Fprint(r.out, prompt)
	if !r.sc.Scan() {
		return "", false
	}
	return r.sc.Text(), true
}

func (r *scanReader) ReadLineWithHints(prompt string, _ []string) (string, bool) {
	return r.ReadLine(prompt)
}

var commandOrder = []string{
	"status", "model", "models", "use", "test", "logs", "cache", "state", "route",
	"start", "stop", "restart", "keys", "key",
	"providers", "provider", "teams", "team", "spend", "init", "config",
	"help", "clear", "exit",
}

var commandSet = func() map[string]bool {
	set := make(map[string]bool, len(commandOrder))
	for _, cmd := range commandOrder {
		set[cmd] = true
	}
	set["quit"] = true
	set["q"] = true
	set["?"] = true
	return set
}()

type Shell struct {
	app     *App
	in      io.Reader
	out     io.Writer
	version string
	source  *completionSource
}

func NewShell(client *Client, version string, in io.Reader, out io.Writer) *Shell {
	return &Shell{
		app:     NewApp(client, out, true),
		in:      in,
		out:     out,
		version: version,
		source:  &completionSource{client: client},
	}
}

func (s *Shell) Run(ctx context.Context) error {
	printWelcome(s.out, s.version, s.app.client.BaseURL(), s.app.client.HasKey())

	if !isTerminal() {
		return s.runPlain(ctx)
	}
	return s.runInteractive(ctx)
}

func (s *Shell) runInteractive(ctx context.Context) error {
	historyPath := filepath.Join(ubiquumHome(), "history")
	_ = os.MkdirAll(ubiquumHome(), 0o755)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:            "ubiquum › ",
		AutoComplete:      newShellCompleter(s.source),
		Painter:           shellPainter{source: s.source},
		HistoryFile:       historyPath,
		HistorySearchFold: true,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	reader := &rlReader{rl: rl}

	for {
		rl.SetPrompt(shellPrompt(s.app.currentModel, s.app.currentTeam))
		line, err := rl.Readline()
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		exit := s.handleCommand(ctx, line, reader)
		if exit {
			break
		}
	}

	return nil
}

func (s *Shell) runPlain(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	reader := &scanReader{sc: scanner, out: s.out}

	for {
		fmt.Fprint(s.out, stripANSI(shellPrompt(s.app.currentModel, s.app.currentTeam)))
		if !scanner.Scan() {
			fmt.Fprintln(s.out)
			return scanner.Err()
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		exit := s.handleCommand(ctx, input, reader)
		if exit {
			return nil
		}
	}
}

func (s *Shell) handleCommand(ctx context.Context, input string, reader lineReader) bool {
	input = strings.TrimSpace(input)
	isSlashCommand := strings.HasPrefix(input, "/")
	args, err := shellArgs(input)
	if err != nil {
		fmt.Fprintln(s.out, red("error:"), err)
		return false
	}
	if len(args) == 0 {
		return false
	}

	if !isSlashCommand && !commandSet[args[0]] {
		fmt.Fprintf(s.out, "%s unknown command %q; try help\n", red("error:"), args[0])
		return false
	}

	switch canonicalCommand(args[0]) {
	case "init":
		if err := s.app.initCmd(reader, s.out); err != nil {
			fmt.Fprintln(s.out, red("error:"), err)
		}
		s.app.client.ReloadKey()
		return false
	case "models":
		if len(args) > 1 && args[1] == "add" {
			if err := s.app.addModel(reader, s.out); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
	case "providers":
		if len(args) > 1 && args[1] == "add" {
			if err := s.app.addProvider(reader, s.out); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
		if len(args) > 2 && args[1] == "rm" {
			if err := s.app.removeProvider(reader, s.out, args[2]); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
	case "cache":
		if len(args) > 1 && args[1] == "init" {
			if err := s.app.cacheInit(reader, s.out); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
	case "state":
		if len(args) > 1 && args[1] == "init" {
			if err := s.app.stateInit(reader, s.out); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
	case "keys":
		if len(args) > 1 && (args[1] == "add" || args[1] == "create") && len(args) == 2 {
			if err := s.app.keyCreateInteractive(ctx, reader, s.out); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
	case "teams":
		if len(args) > 1 && (args[1] == "add" || args[1] == "create") && len(args) == 2 {
			if err := s.app.teamAddInteractive(ctx, reader, s.out); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
		if len(args) == 3 && args[1] == "use" {
			if err := s.app.teamUse(args[2:]); err != nil {
				fmt.Fprintln(s.out, red("error:"), err)
			}
			return false
		}
	}

	exit, err := s.app.Execute(ctx, args)
	if err != nil {
		fmt.Fprintln(s.out, red("error:"), err)
		return false
	}
	return exit
}

func shellPrompt(model, team string) string {
	label := orange("ubiquum")
	if team != "" {
		label += " " + blue("["+team+"]")
	}
	if model != "" {
		label += " " + dim("("+model+")")
	}
	return label + " › "
}

func newShellCompleter(source *completionSource) readline.AutoCompleter {
	return readline.SegmentFunc(func(segments [][]rune, _ int) [][]rune {
		return completionCandidates(runeSegments(segments), source)
	})
}

func runeSegments(segments [][]rune) []string {
	out := make([]string, len(segments))
	for i, seg := range segments {
		out[i] = string(seg)
	}
	return out
}

func completionCandidates(parts []string, source *completionSource) [][]rune {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		return stringCandidates(commandCandidates(parts[0]))
	}

	cmd := strings.TrimPrefix(parts[0], "/")
	current := parts[len(parts)-1]
	prev := ""
	if len(parts) > 1 {
		prev = parts[len(parts)-2]
	}

	var candidates []string
	switch canonicalCommand(cmd) {
	case "models":
		candidates = modelCandidates(parts, current, prev)
	case "providers":
		if len(parts) == 2 {
			candidates = []string{"add", "list", "rm"}
		} else if len(parts) == 3 && parts[1] == "rm" {
			candidates = sortedProviderNames(configuredProviders())
		}
	case "use":
		if len(parts) == 2 {
			candidates = configuredModelNames()
		}
	case "test":
		if len(parts) == 2 {
			candidates = configuredModelNames()
		}
	case "keys":
		candidates = keyCandidates(parts, current, prev, source)
	case "teams":
		candidates = teamCandidates(parts, current, prev, source)
	case "spend":
		candidates = spendCandidates(parts, current, prev, source)
	case "logs":
		candidates = []string{"-n", "-f"}
	case "start", "restart":
		candidates = []string{"-config"}
	case "config":
		if len(parts) == 2 {
			candidates = []string{"set"}
		} else if len(parts) == 3 && parts[1] == "set" {
			candidates = []string{"port", "master_key", "strategy", "retries", "swagger"}
		} else if len(parts) == 4 && parts[1] == "set" && parts[2] == "strategy" {
			candidates = []string{"shuffle", "round-robin", "latency"}
		}
	case "cache":
		if len(parts) == 2 {
			candidates = []string{"init", "enable", "disable", "set", "status", "flush", "metrics"}
		} else if len(parts) == 3 && parts[1] == "set" {
			candidates = []string{"backend", "embedding_model", "tweak_model", "redis_url", "qdrant_url"}
		} else if len(parts) == 4 && parts[1] == "set" && parts[2] == "backend" {
			candidates = []string{"memory", "redis", "qdrant", "pgvector"}
		} else if len(parts) == 4 && parts[1] == "set" && (parts[2] == "embedding_model" || parts[2] == "tweak_model") {
			candidates = configuredModelNames()
		}
	case "state":
		if len(parts) == 2 {
			candidates = []string{"init", "enable", "disable", "set", "status"}
		} else if len(parts) == 3 && parts[1] == "set" {
			candidates = []string{"model", "threshold", "max_latency_ms"}
		} else if len(parts) == 4 && parts[1] == "set" && parts[2] == "model" {
			candidates = configuredModelNames()
		}
	case "route":
		if len(parts) == 2 {
			candidates = []string{"enable", "disable", "add", "rm", "metrics", "team"}
		} else if len(parts) == 3 && parts[1] == "add" {
			candidates = []string{"complex", "standard", "trivial"}
		} else if len(parts) == 3 && (parts[1] == "add" || parts[1] == "rm") {
			candidates = []string{"complex", "standard", "trivial"}
		} else if len(parts) == 4 && parts[1] == "add" {
			candidates = configuredModelNames()
		} else if len(parts) >= 3 && parts[1] == "team" {
			if len(parts) == 4 {
				candidates = []string{"enable", "disable", "add", "rm", "reset"}
			} else if len(parts) == 5 && parts[3] == "add" {
				candidates = []string{"complex", "standard", "trivial"}
			} else if len(parts) == 6 && parts[3] == "add" {
				candidates = configuredModelNames()
			} else if len(parts) == 5 && parts[3] == "rm" {
				candidates = []string{"complex", "standard", "trivial"}
			}
		}
	}
	return stringCandidates(filterPrefix(candidates, current))
}

func modelCandidates(parts []string, current, prev string) []string {
	if len(parts) == 2 {
		return []string{"add", "list", "show", "set", "unset", "rm"}
	}
	switch parts[1] {
	case "rm", "delete", "show", "get", "info":
		if len(parts) == 3 {
			return configuredModelNames()
		}
	case "set", "update":
		if len(parts) == 3 {
			return configuredModelNames()
		}
		if prev == "provider" {
			return sortedProviderNames(configuredProviders())
		}
		if len(parts) >= 4 {
			return modelSetFields()
		}
	case "unset":
		if len(parts) == 3 {
			return configuredModelNames()
		}
		if len(parts) >= 4 {
			return modelUnsetFields()
		}
	}
	return filterPrefix(nil, current)
}

func keyCandidates(parts []string, current, prev string, source *completionSource) []string {
	if len(parts) == 2 {
		return []string{"add", "list", "info", "update", "enable", "disable", "rm"}
	}
	if prev == "models" {
		return configuredModelNames()
	}
	if prev == "team" {
		return teamIdentifiers(source)
	}
	switch parts[1] {
	case "add", "create":
		return []string{"name", "team", "budget", "rate-limit", "models", "metadata"}
	case "update":
		if len(parts) == 3 {
			return keyIdentifiers(source)
		}
		return []string{"name", "budget", "rate-limit", "models", "metadata", "active", "expires"}
	case "enable", "disable":
		if len(parts) == 3 {
			return keyIdentifiers(source)
		}
	case "rm", "delete", "info":
		if len(parts) == 3 {
			return keyIdentifiers(source)
		}
	case "list":
		return []string{"team"}
	default:
		return filterPrefix(nil, current)
	}
	return nil
}

func teamCandidates(parts []string, current, prev string, source *completionSource) []string {
	if len(parts) == 2 {
		return []string{"add", "use", "none", "off", "rm", "list"}
	}
	if parts[1] == "use" && len(parts) == 3 {
		return teamIdentifiers(source)
	}
	if prev == "models" {
		return configuredModelNames()
	}
	if len(parts) >= 3 && (parts[1] == "add" || parts[1] == "create") {
		return []string{"budget", "models", "name"}
	}
	if parts[1] == "rm" && len(parts) == 3 {
		return teamIdentifiers(source)
	}
	return filterPrefix(nil, current)
}

func spendCandidates(_ []string, current, prev string, source *completionSource) []string {
	if prev == "model" {
		return configuredModelNames()
	}
	if prev == "key" {
		return keyIdentifiers(source)
	}
	return filterPrefix([]string{"key", "model"}, current)
}

func commandCandidates(current string) []string {
	prefix := ""
	needle := current
	if strings.HasPrefix(current, "/") {
		prefix = "/"
		needle = strings.TrimPrefix(current, "/")
	}
	candidates := make([]string, 0, len(commandOrder))
	for _, cmd := range commandOrder {
		if strings.HasPrefix(cmd, needle) {
			candidates = append(candidates, prefix+cmd)
		}
	}
	return candidates
}

func filterPrefix(values []string, current string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.HasPrefix(value, current) {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func stringCandidates(values []string) [][]rune {
	out := make([][]rune, 0, len(values))
	for _, value := range values {
		out = append(out, []rune(value))
	}
	return out
}

type shellPainter struct {
	source *completionSource
}

func (p shellPainter) Paint(line []rune, pos int) []rune {
	if pos != len(line) || len(line) == 0 {
		return line
	}
	suffix := suggestionSuffix(string(line), p.source)
	if suffix == "" {
		return line
	}
	ghost := dim(suffix)
	moveBack := fmt.Sprintf("\x1b[%dD", visibleLen(suffix))
	return []rune(string(line) + ghost + moveBack)
}

func suggestionSuffix(line string, source *completionSource) string {
	if strings.TrimSpace(line) == "" || strings.HasSuffix(line, " ") || strings.HasSuffix(line, "\t") {
		return ""
	}
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return ""
	}
	var candidates []string
	current := parts[len(parts)-1]
	if len(parts) == 1 {
		candidates = commandCandidates(current)
	} else {
		candidates = candidatesAsStrings(completionCandidates(parts, source))
	}
	for _, cand := range candidates {
		if strings.HasPrefix(cand, current) && cand != current {
			return cand[len(current):]
		}
	}
	return ""
}

func candidatesAsStrings(values [][]rune) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

type completionSource struct {
	client       *Client
	keys         []APIKey
	fetched      time.Time
	teams        []TeamInfo
	teamsFetched time.Time
}

func keyIdentifiers(source *completionSource) []string {
	keys := cachedKeys(source)
	nameCounts := make(map[string]int, len(keys))
	for _, key := range keys {
		if key.Name != "" {
			nameCounts[key.Name]++
		}
	}

	seen := map[string]bool{}
	var out []string
	for _, key := range keys {
		if key.Name != "" && nameCounts[key.Name] == 1 {
			addIdentifier(&out, seen, key.Name)
		} else {
			addIdentifier(&out, seen, key.KeyPrefix)
		}
	}
	sort.Strings(out)
	return out
}

func teamIdentifiers(source *completionSource) []string {
	teams := cachedTeams(source)
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		// Prefer name for display; fall back to ID if name is empty
		label := t.Name
		if label == "" {
			label = t.ID
		}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func cachedTeams(source *completionSource) []TeamInfo {
	if source == nil {
		return nil
	}
	if source.client == nil || !source.client.HasKey() {
		return source.teams
	}
	if time.Since(source.teamsFetched) < 2*time.Second {
		return source.teams
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	teams, err := source.client.ListTeams(ctx)
	if err != nil {
		return source.teams
	}
	source.teams = teams
	source.teamsFetched = time.Now()
	return teams
}

func addIdentifier(out *[]string, seen map[string]bool, value string) {
	if value == "" || seen[value] {
		return
	}
	seen[value] = true
	*out = append(*out, value)
}

func cachedKeys(source *completionSource) []APIKey {
	if source == nil {
		return nil
	}
	if source.client == nil || !source.client.HasKey() {
		return source.keys
	}
	if time.Since(source.fetched) < 2*time.Second {
		return source.keys
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	keys, err := source.client.ListKeys(ctx, "")
	if err != nil {
		return source.keys
	}
	source.keys = keys
	source.fetched = time.Now()
	return keys
}

func shellArgs(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "/") {
		line = strings.TrimSpace(line[1:])
	}
	return splitCommandLine(line)
}

func splitCommandLine(s string) ([]string, error) {
	var args []string
	var b strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if b.Len() > 0 {
			args = append(args, b.String())
			b.Reset()
		}
	}

	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return args, nil
}

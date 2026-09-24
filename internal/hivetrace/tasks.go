package hivetrace

import (
	"path"
	"strings"
)

// Task types. The set matches the classifier benchmark, so a deterministic
// label and a model label are comparable.
const (
	TaskCoding    = "coding"
	TaskDebugging = "debugging"
	TaskOps       = "ops"
	TaskResearch  = "research"
	TaskData      = "data"
	TaskWriting   = "writing"
)

// Where a task type came from, so the console never presents a guess as a fact.
const (
	TaskViaFiles = "files"
	TaskViaTools = "tools"
)

// classifyTask names the kind of work from what the session touched. It only
// answers when the evidence does: debugging, for one, looks exactly like
// coding in a file list, so it is left to the model rather than guessed.
func classifyTask(s *SessionSummary) (taskType, via string) {
	counts := map[string]int{}
	for _, p := range s.FilesWritten {
		if kind := fileTaskKind(p); kind != "" {
			counts[kind]++
		}
	}
	best, bestN := "", 0
	// Ties go to the earlier kind: a session that wrote one Go file and one
	// README was coding with a note, not writing with a snippet.
	for _, kind := range []string{TaskCoding, TaskOps, TaskData, TaskWriting} {
		if counts[kind] > bestN {
			best, bestN = kind, counts[kind]
		}
	}
	if best != "" {
		return best, TaskViaFiles
	}
	if len(s.FilesWritten) > 0 {
		return "", ""
	}

	for _, srv := range s.MCPServers {
		if isDataServer(srv.Server) {
			return TaskData, TaskViaTools
		}
	}
	for _, t := range s.Tools {
		if webTools[strings.ToLower(t.Tool)] {
			return TaskResearch, TaskViaTools
		}
	}
	read := 0
	for _, p := range s.FilesRead {
		if !agentOwnFile(p) {
			read++
		}
	}
	if read >= 3 {
		return TaskResearch, TaskViaFiles
	}
	return "", ""
}

// agentOwnFile reports the agent's own skills, rules and memory, which it
// loads whatever the task and so say nothing about it.
func agentOwnFile(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	for _, dir := range []string{"/.codex/", "/.claude/", "/.cursor/", "/.copilot/", "/.github/instructions/", "/.gemini/"} {
		if strings.Contains(p, dir) {
			return true
		}
	}
	base := path.Base(p)
	return base == "AGENTS.md" || base == "CLAUDE.md"
}

var webTools = map[string]bool{"websearch": true, "web_search": true, "webfetch": true, "web_fetch": true}

var dataServers = []string{"sqlite", "postgres", "mysql", "mariadb", "bigquery", "snowflake", "clickhouse", "mongo", "duckdb", "redshift", "databricks", "supabase"}

func isDataServer(name string) bool {
	name = strings.ToLower(name)
	for _, s := range dataServers {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}

var (
	codeExts = map[string]bool{
		".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".jsx": true,
		".java": true, ".kt": true, ".scala": true, ".rs": true, ".c": true, ".h": true, ".cc": true, ".cpp": true,
		".hpp": true, ".cs": true, ".rb": true, ".php": true, ".swift": true, ".m": true, ".dart": true, ".lua": true,
		".sh": true, ".bash": true, ".zsh": true, ".ps1": true, ".css": true, ".scss": true, ".html": true,
		".vue": true, ".svelte": true, ".ex": true, ".exs": true, ".erl": true, ".clj": true, ".r": true,
	}
	opsExts   = map[string]bool{".yaml": true, ".yml": true, ".tf": true, ".tfvars": true, ".hcl": true, ".toml": true, ".ini": true, ".conf": true}
	opsFiles  = map[string]bool{"dockerfile": true, "makefile": true, "jenkinsfile": true, "procfile": true, "vagrantfile": true}
	dataExts  = map[string]bool{".csv": true, ".tsv": true, ".parquet": true, ".xlsx": true, ".xls": true, ".ipynb": true, ".sql": true, ".jsonl": true, ".db": true, ".sqlite": true}
	proseExts = map[string]bool{".md": true, ".mdx": true, ".txt": true, ".rst": true, ".adoc": true, ".tex": true, ".docx": true, ".odt": true, ".rtf": true}
)

// fileTaskKind reads the kind of work a written file implies from its name.
// JSON and anything unrecognised say nothing: a JSON file is as often config
// as data, and an unknown extension is no evidence at all.
func fileTaskKind(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	base := strings.ToLower(path.Base(p))
	ext := path.Ext(base)
	switch {
	case opsFiles[base] || strings.HasPrefix(base, "dockerfile") || strings.Contains("/"+p, "/.github/workflows/"):
		return TaskOps
	case strings.HasSuffix(base, "_test.go") || codeExts[ext]:
		return TaskCoding
	case opsExts[ext]:
		return TaskOps
	case dataExts[ext]:
		return TaskData
	case proseExts[ext]:
		return TaskWriting
	}
	return ""
}

// measureDifficulty rates a session 1–5 by the work it took: turns that were
// served, tool calls, and files changed. Failed turns do not count — a burst
// of rate limits is not a hard task — and neither does wall time, which in an
// agent session is mostly the person being away. 0 means nothing was served.
func measureDifficulty(s *SessionSummary) int {
	served := s.Requests - s.Errors
	if served <= 0 {
		return 0
	}
	score := band(served, 2, 8, 25, 80) + band(s.ToolCalls, 0, 5, 20, 60) + band(len(s.FilesWritten), 0, 1, 4, 9)
	d := 1 + (score+1)/3 // mean of the three bands, rounded
	if d > 5 {
		d = 5
	}
	return d
}

// band places n on a 0–4 scale by the upper bounds of the first four steps.
func band(n int, bounds ...int) int {
	for i, b := range bounds {
		if n <= b {
			return i
		}
	}
	return len(bounds)
}

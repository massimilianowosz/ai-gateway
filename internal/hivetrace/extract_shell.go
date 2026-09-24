package hivetrace

import (
	"regexp"
	"strings"
)

// File access inside a shell command.
//
// Agents that work through a terminal never name a file in a structured
// argument: Codex wraps a command in JavaScript (`tools.exec_command({cmd:
// "wc -l docs/paper.md"})`) and Claude Code puts it in Bash's command string.
// Their sessions therefore showed no file activity at all while visibly
// reading half a repository.
//
// The parsing is deliberately narrow. Deciding whether an arbitrary command
// writes a file is not something a heuristic can do honestly, so only commands
// whose purpose is unambiguous are read, and only their obvious path operands
// are taken. Under-reporting is the correct failure here: a file list that
// invents a write is worse than one that misses a read.

// shellTools are the tools whose arguments carry a command line rather than a
// path. The bare lowercased name, so an MCP bridge resolves the same.
var shellTools = map[string]bool{
	"bash": true, "shell": true, "exec": true, "exec_command": true,
	"run_terminal_cmd": true, "run_in_terminal": true, "execute_command": true,
	"run_shell_command": true, "terminal": true,
}

// shellReaders map a command to the access it represents. Anything absent is
// skipped rather than guessed at.
var shellReaders = map[string]string{
	"cat": FileOpRead, "head": FileOpRead, "tail": FileOpRead, "wc": FileOpRead,
	"less": FileOpRead, "more": FileOpRead, "nl": FileOpRead, "od": FileOpRead,
	"xxd": FileOpRead, "sed": FileOpRead, "awk": FileOpRead, "jq": FileOpRead,
	"grep": FileOpSearch, "rg": FileOpSearch, "ag": FileOpSearch,
	"find": FileOpSearch, "fd": FileOpSearch, "ls": FileOpSearch,
}

// shellWriters map a command to the operand that it modifies. The value is
// the index of the first path operand the command writes to, counting only
// non-flag operands: cp and mv write their last one, the rest write all of
// theirs.
var shellWriters = map[string]bool{
	"touch": true, "tee": true, "cp": true, "mv": true, "rm": true,
	"mkdir": false, // a directory is not a file access worth recording
}

// patternFirst are the search commands whose first operand is the pattern, not
// a path. Counting it as a file would fill the list with regexes.
var patternFirst = map[string]bool{
	"grep": true, "rg": true, "ag": true, "awk": true, "sed": true, "jq": true,
}

// commandArg pulls the command out of the shapes agents use to carry one:
// Codex's `cmd:"…"` inside JavaScript, and the plain `"command": "…"` of a
// JSON tool call.
var commandArg = regexp.MustCompile(`(?:"?(?:cmd|command)"?\s*:\s*)"((?:[^"\\]|\\.)*)"`)

// shellCommands returns every command line found in a tool's arguments.
func shellCommands(arguments string) []string {
	var out []string
	for _, m := range commandArg.FindAllStringSubmatch(arguments, -1) {
		if unquoted := unescape(m[1]); unquoted != "" {
			out = append(out, unquoted)
		}
	}
	return out
}

func unescape(s string) string {
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t")
	return r.Replace(s)
}

// bridgedCall matches the way Codex reaches a connector.
//
// It does not issue a tool call for Miro or Google Drive. It declares them as
// TypeScript on a `tools` object and invokes them from inside exec, so the
// only call the gateway sees is a shell one and every connector a session
// touched was invisible — the traffic that most needs an audit trail, since
// it leaves the machine.
var bridgedCall = regexp.MustCompile(`tools\.(mcp__[A-Za-z0-9_]+)\s*\(`)

// codexAppsBridge is the namespace Codex puts every connector behind. It names
// the bridge, not the service.
const codexAppsBridge = "codex_apps"

// codexPluginPrefixes are the plugin names that contain an underscore, so the
// service cannot be read as "everything before the first one": google_drive
// would come out as google, and the console would label Drive traffic with
// Google's mark and name. There is no syntax that separates a plugin from its
// tool — miro_board_show and google_drive_fetch are shaped identically — so
// the multi-word ones are listed rather than inferred.
var codexPluginPrefixes = []string{
	"google_drive",
	"google_calendar",
	"google_sheets",
	"google_docs",
	"microsoft_365",
	"plugin_management",
}

// bridgedToolCalls returns a call for every connector tool invoked inside a
// shell command's arguments, with the arguments it was given so the file it
// touched can still be recovered.
func bridgedToolCalls(arguments string) []toolCall {
	var out []toolCall
	seen := make(map[string]struct{})

	for _, loc := range bridgedCall.FindAllStringSubmatchIndex(arguments, -1) {
		name := arguments[loc[2]:loc[3]]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}

		tool, server, _ := parseToolName(name)
		if server == codexAppsBridge {
			server = codexPluginOf(tool)
		}
		out = append(out, toolCall{
			ToolInvocation: ToolInvocation{
				Name:   name,
				Tool:   tool,
				Server: server,
				Source: ToolSourceMCP,
			},
			arguments: callArguments(arguments, loc[1]),
		})
	}
	return out
}

// codexPluginOf names the service a Codex tool belongs to.
func codexPluginOf(tool string) string {
	for _, p := range codexPluginPrefixes {
		if strings.HasPrefix(tool, p+"_") {
			return p
		}
	}
	if i := strings.Index(tool, "_"); i > 0 {
		return tool[:i]
	}
	return tool
}

// callArguments returns the argument text of a call that opens at start.
//
// The arguments are a JavaScript object literal, not JSON, so they are kept as
// written and read later by key rather than parsed. A bound rather than a
// matching bracket: a truncated stream is common and a half-read object is
// still enough to recover a path from.
func callArguments(s string, start int) string {
	const window = 600
	end := start + window
	if end > len(s) {
		end = len(s)
	}
	if close := strings.Index(s[start:end], ");"); close > 0 {
		end = start + close
	}
	return s[start:end]
}

// patchFile matches the directives of the patch envelope Codex writes with.
// It edits files by handing exec a patch document rather than by running a
// write command, so nothing in the command line names the file.
var patchFile = regexp.MustCompile(`(?m)^\*\*\* (Add File|Update File|Delete File|Move to): (.+)$`)

// patchFileAccess reads the files a patch envelope touches. A delete is
// recorded as a write: FileAccess has no third state, and "the agent changed
// this file" is the fact that matters to an operator reviewing a session.
func patchFileAccess(arguments string) []FileAccess {
	var out []FileAccess
	for _, m := range patchFile.FindAllStringSubmatch(unescape(arguments), -1) {
		path := strings.TrimSpace(m[2])
		if path == "" {
			continue
		}
		out = append(out, FileAccess{Path: path, Operation: FileOpWrite, Tool: "apply_patch"})
	}
	return out
}

// shellFileAccess derives file accesses from one command line.
func shellFileAccess(command string) []FileAccess {
	var out []FileAccess
	// && || ; | all separate one command from the next, and each half may
	// touch its own files.
	for _, part := range splitAny(command, []string{"&&", "||", ";", "|"}) {
		fields := shellFields(part)
		if len(fields) == 0 {
			continue
		}
		out = append(out, redirectTargets(fields)...)
		name := commandName(fields)
		if writesOperands, isWriter := shellWriters[name]; isWriter {
			if writesOperands {
				out = append(out, writeOperands(name, fields)...)
			}
			continue
		}
		op, ok := shellReaders[name]
		if !ok {
			continue
		}
		operands := 0
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") {
				continue
			}
			operands++
			// The pattern of a search command is not a file.
			if operands == 1 && patternFirst[name] {
				continue
			}
			if looksLikePath(f) {
				out = append(out, FileAccess{Path: f, Operation: op, Tool: name})
			}
		}
	}
	return out
}

// commandName skips the wrappers that precede the real command.
func commandName(fields []string) string {
	for _, f := range fields {
		if strings.Contains(f, "=") && !strings.Contains(f, "/") {
			continue // VAR=value prefix
		}
		if f == "sudo" || f == "command" || f == "env" || f == "time" {
			continue
		}
		return strings.ToLower(f[strings.LastIndex(f, "/")+1:])
	}
	return ""
}

// looksLikePath keeps the operands that are plausibly files: a path separator
// or a file extension. A bare word is far more likely to be a subcommand or a
// pattern than a file in the working directory.
func looksLikePath(s string) bool {
	s = strings.Trim(s, `'"`)
	if s == "" || strings.HasPrefix(s, "$") {
		return false
	}
	if strings.Contains(s, "/") {
		return true
	}
	dot := strings.LastIndex(s, ".")
	return dot > 0 && dot < len(s)-1
}

// shellFields splits on whitespace while keeping quoted runs together.
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func splitAny(s string, seps []string) []string {
	parts := []string{s}
	for _, sep := range seps {
		var next []string
		for _, p := range parts {
			next = append(next, strings.Split(p, sep)...)
		}
		parts = next
	}
	return parts
}

// redirectTargets reads the files a shell redirection writes to. A `2>&1` is a
// descriptor, not a path, and is left alone.
func redirectTargets(fields []string) []FileAccess {
	var out []FileAccess
	for i, f := range fields {
		if !isRedirect(f) || i+1 >= len(fields) {
			continue
		}
		target := fields[i+1]
		if strings.HasPrefix(target, "&") || !looksLikePath(target) {
			continue
		}
		out = append(out, FileAccess{Path: target, Operation: FileOpWrite, Tool: "redirect"})
	}
	return out
}

// isRedirect reports whether a field is an output redirection operator, with
// or without the file descriptor that may precede it.
func isRedirect(f string) bool {
	f = strings.TrimSuffix(strings.TrimSuffix(f, ">"), ">")
	if f == "" {
		return true
	}
	for _, r := range f {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// writeOperands reads the paths a write command acts on. cp and mv write only
// their destination; the rest write every operand they are given.
func writeOperands(name string, fields []string) []FileAccess {
	var paths []string
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") || !looksLikePath(f) {
			continue
		}
		paths = append(paths, f)
	}
	if len(paths) == 0 {
		return nil
	}
	if name == "cp" || name == "mv" {
		paths = paths[len(paths)-1:]
	}
	out := make([]FileAccess, 0, len(paths))
	for _, p := range paths {
		out = append(out, FileAccess{Path: p, Operation: FileOpWrite, Tool: name})
	}
	return out
}

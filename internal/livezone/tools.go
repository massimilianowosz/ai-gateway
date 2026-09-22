package livezone

import (
	"path"
	"strings"
)

// Tool results the gateway must never rewrite.
//
// A coding agent quotes these back verbatim — line numbers from a Read, match
// positions from a Grep, the exact text it is about to Edit. Compressing them
// does not cost tokens, it corrupts the agent's next action: a stale line
// number produces a wrong patch, a truncated search result produces a missed
// case. The saving is not worth the class of failure.
//
// Shell output is deliberately absent. Build logs, test output and command
// results are the large, repetitive payloads this package exists for, and an
// agent reads them for their conclusions rather than quoting them by offset.
// Operators who disagree can add "Bash" to ProtectedTools in config.
var defaultProtectedTools = map[string]struct{}{
	"read": {}, "glob": {}, "grep": {}, "write": {}, "edit": {},
	"multiedit": {}, "notebookedit": {},
	"websearch": {}, "webfetch": {}, "web_search": {}, "web_fetch": {},
	"str_replace_editor": {}, "str_replace_based_edit_tool": {}, "text_editor": {},
	"view": {}, "create": {}, "insert": {},
}

// IsProtected reports whether a tool's output must be forwarded untouched.
//
// Matching is case-insensitive and unwraps the MCP naming conventions
// (mcp__server__tool, mcp_server_tool), so a protected tool stays protected
// when it arrives through an MCP bridge. Callers may extend the set; an entry
// ending in "*" matches by prefix, which lets an operator protect a whole MCP
// server with one line.
func IsProtected(toolName string, extra []string) bool {
	if toolName == "" {
		// An unnamed tool result cannot be checked against the list, so it is
		// treated as protected. Compression is an optimisation; skipping it
		// costs nothing that matters.
		return true
	}
	for _, alias := range toolAliases(toolName) {
		if _, ok := defaultProtectedTools[alias]; ok {
			return true
		}
		for _, e := range extra {
			e = strings.ToLower(strings.TrimSpace(e))
			if e == "" {
				continue
			}
			if strings.HasSuffix(e, "*") {
				if strings.HasPrefix(alias, strings.TrimSuffix(e, "*")) {
					return true
				}
				continue
			}
			if alias == e {
				return true
			}
		}
	}
	return false
}

// toolAliases returns the lowercase spellings a tool name may arrive under.
func toolAliases(name string) []string {
	lower := strings.ToLower(strings.TrimSpace(name))
	aliases := []string{lower}

	// mcp__server__tool  →  also match "tool" and the single-underscore form.
	if strings.HasPrefix(lower, "mcp__") {
		if parts := strings.SplitN(lower, "__", 3); len(parts) == 3 && parts[2] != "" {
			aliases = append(aliases, parts[2], "mcp_"+parts[1]+"_"+parts[2])
		}
	} else if strings.HasPrefix(lower, "mcp_") {
		if parts := strings.SplitN(lower, "_", 3); len(parts) == 3 && parts[2] != "" {
			aliases = append(aliases, parts[2], "mcp__"+parts[1]+"__"+parts[2])
		}
	}

	// Some clients namespace tools with a path or dot prefix.
	if i := strings.LastIndexAny(lower, "./"); i >= 0 && i+1 < len(lower) {
		aliases = append(aliases, path.Base(strings.ReplaceAll(lower, ".", "/")))
	}
	return aliases
}

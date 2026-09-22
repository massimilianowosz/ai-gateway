package hivestate

import (
	"fmt"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
)

const stateExtractionSystemPrompt = `You are a state-extraction engine. Read the conversation and produce a JSON snapshot that allows another AI to continue seamlessly WITHOUT the original messages.

Output ONLY valid JSON:
{
  "intent": "<verb_noun>",
  "difficulty": "<DIFFICULTY_LEVEL>",
  "reasoning_effort": "low|medium|high",
  "active_constraints": {
    "identifiers": { ... },
    "values": { ... },
    "actions_taken": [ {"tool": "...", "input": "...", "result": "...", "success": true}, ... ],
    "files_modified": [ {"path": "...", "change": "..."}, ... ],
    "progress": { "done": [...], "current": "...", "next": [...] },
    "requirements": { ... },
    "errors_encountered": [ {"error": "...", "resolution": "..."}, ... ]
  },
  "conversation_status": "in_progress|completed|clarifying"
}

## intent
The current action. Use snake_case verb_noun (e.g. setup_nextjs_project, fix_build_error, migrate_database).

## difficulty
Classify the CURRENT TASK difficulty. Consider:
- Number of files/modules involved
- Conceptual complexity (simple CRUD vs distributed systems design)
- Ambiguity of requirements
- Error debugging depth (surface typo vs deep race condition)
- Whether the task requires architectural decisions

## reasoning_effort
How much thinking the AI needs for the NEXT step:
- "low": straightforward execution, no ambiguity (apply a known fix, simple edit, answer a factual question)
- "medium": moderate reasoning needed (implement a feature with clear requirements, debug with good error messages)
- "high": deep analysis required (architecture decisions, complex debugging with unclear cause, multi-step refactoring)

## active_constraints
Pack ALL task-critical information here:

**identifiers** — Every ID, reference, name, path, or unique handle mentioned.

**values** — Every specific value: versions, configs, URLs, ports, env vars, package names.

**actions_taken** — EVERY tool call / command executed, in chronological order:
  [{"tool": "Bash", "input": "npx create-next-app@latest myapp --ts", "result": "project created successfully", "success": true},
   {"tool": "Bash", "input": "npx shadcn@latest init", "result": "configured with Radix + Nova preset", "success": true},
   {"tool": "Bash", "input": "ls -la public/", "result": "index.html(2.1K), app.js(45K), styles.css(8K), favicon.ico", "success": true},
   {"tool": "Read file", "input": "src/app/page.tsx", "result": "React component: exports default Page, uses Button from @/components/ui/button, has form with email input", "success": true},
   {"tool": "Read file", "input": "tailwind.config.ts", "result": "content paths: ./src/**/*.{ts,tsx}, theme extends colors with 'brand' palette, plugins: [animate]", "success": true},
   {"tool": "WriteFile", "input": "src/app/page.tsx", "result": "created main page with Button component", "success": true},
   {"tool": "Bash", "input": "npm run build", "result": "error: Cannot find module '@/components/ui/button'", "success": false}]
  - Include the FULL command for Bash calls (not summarized)
  - Include file paths for Read/Write operations
  - Include whether it succeeded or failed
  - Include the key part of the output (error messages verbatim, success confirmation)
  - For READ operations: "result" MUST describe what the file CONTAINS (exports, structure, key values, patterns) — NEVER write just "file contents" or "file read successfully"
  - For WRITE operations: "result" must describe what was written/changed
  - For BASH: "result" MUST include the actual output data. For ls/find: list the filenames. For installs: the version installed. For errors: the error text verbatim.
  - FORBIDDEN generic results: "file contents", "list of files", "command output", "successfully executed", "file read", "search results", "directory listing", "detailed list of...", "contents of...". ANY description of what the output IS rather than what it CONTAINS is forbidden. The "result" must be THE DATA ITSELF (filenames, values, errors), not a label describing the data.

  BAD: {"result": "detailed list of files in public folder"}
  BAD: {"result": "list of project files"}
  BAD: {"result": "file contents showing React component"}
  GOOD: {"result": "index.html, app.js(45K), styles.css, gateway.yaml"}
  GOOD: {"result": "exports: App component, uses fetch('/api/chat'), state: messages[]"}
  GOOD: {"result": "v18.2.0"}

**files_modified** — Files created or edited during the session:
  [{"path": "src/app/page.tsx", "change": "created with shadcn Button import"},
   {"path": "tailwind.config.ts", "change": "added content paths for components"}]

**progress** — Task status:
  done: completed steps with outcomes
  current: what is being handled right now
  next: known remaining steps

**requirements** — What the user wants that hasn't been fulfilled yet.

**errors_encountered** — Errors that happened and how they were resolved (or not):
  [{"error": "Module not found: @/components/ui/button", "resolution": "ran npx shadcn@latest add button"}]

## RULES
1. TOOL CALLS are the #1 priority. Every [TOOL_CALL ...] and [TOOL_OUTPUT ...] / [TOOL_ERROR ...] MUST appear in actions_taken.
2. Preserve FULL commands — do not summarize "npm install ..." as "installed deps". Keep the exact command.
3. Include every file path, package name, version, and config value — these are not recoverable if lost.
4. Error messages must be captured VERBATIM (they contain critical debugging info).
5. Never invent data. Raw JSON only. No markdown, no comments.
6. Order actions_taken chronologically — the sequence matters for understanding what happened.`

// hiveRouteDifficultyPromptSuffix returns an additional prompt section
// describing the available difficulty levels for HiveRoute classification.
func hiveRouteDifficultyPromptSuffix(levels []config.HiveRouteLevel) string {
	if len(levels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## DIFFICULTY LEVELS (choose exactly one for the \"difficulty\" field):\n")
	for _, l := range levels {
		fmt.Fprintf(&b, "- %q: %s\n", l.Name, l.Description)
	}
	return b.String()
}

// deltaExtractionPromptSuffix turns the snapshot contract into an append-only one.
//
// Two failure modes matter here. If the model repeats what the prior state
// already says, the log grows quadratically and the whole scheme costs more
// than it saves. If it silently drops a fact that changed, the reader keeps
// believing the stale version, because nothing is ever edited in place — a
// correction can only be expressed by restating the fact.
const deltaExtractionPromptSuffix = `

## APPEND-ONLY MODE
You are shown the state extracted from EARLIER turns, then only the NEW turns.
Your output describes the NEW turns alone. It will be appended after the
earlier state, which is permanent and cannot be edited.

- Do NOT repeat identifiers, values, actions, files or errors that the earlier
  state already records. They are still in effect; restating them wastes the
  entire benefit of this mode.
- DO restate a fact when the new turns CHANGED it (a value was updated, an
  error was fixed, a file was modified again). A restated fact overrides the
  earlier one. This is the only way to correct the record.
- "intent", "difficulty", "reasoning_effort" and "conversation_status" describe
  the situation as it stands NOW. Always emit all four, even when unchanged:
  the reader takes them from the most recent entry.
- "progress.done" lists only steps completed in the NEW turns. "current" and
  "next" describe the situation now and replace the earlier ones.
- Emit no key at all rather than an empty object or list.`

func buildDeltaExtractionPrompt(priorState string, newMessages []Message, lastUser Message, levels []config.HiveRouteLevel) string {
	prompt := "State extracted from earlier turns (read-only, already recorded):\n" +
		priorState + "\n\nNew turns to describe:\n"
	prompt += renderHistory(newMessages)
	prompt += fmt.Sprintf("\nLatest user message:\n[user]: %s\n", lastUser.Content)
	prompt += hiveRouteDifficultyPromptSuffix(levels)
	prompt += "\nOutput JSON describing ONLY the new turns."
	return prompt
}

// renderHistory formats messages for the extractor, truncating the long ones.
// Very long messages are raw file dumps or tool output: the head and tail carry
// the identity of the content, the middle rarely does.
func renderHistory(history []Message) string {
	var b strings.Builder
	for _, m := range history {
		content := m.Content
		switch {
		case len(content) > 8000:
			content = content[:2000] + "\n...[truncated]...\n" + content[len(content)-500:]
		case len(content) > 4000:
			content = content[:2500] + "\n...[truncated]...\n" + content[len(content)-500:]
		}
		fmt.Fprintf(&b, "[%s]: %s\n", m.Role, content)
	}
	return b.String()
}

func buildExtractionPrompt(history []Message, lastUser Message, levels []config.HiveRouteLevel) string {
	prompt := "Conversation history:\n"
	prompt += renderHistory(history)
	prompt += fmt.Sprintf("\nLatest user message:\n[user]: %s\n", lastUser.Content)
	prompt += "\nExtract the current task state as JSON:"
	if suffix := hiveRouteDifficultyPromptSuffix(levels); suffix != "" {
		prompt += suffix
	}
	return prompt
}

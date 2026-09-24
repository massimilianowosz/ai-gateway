package hivetrace

import (
	"encoding/json"
	"strings"
)

// Agent identification, deliberately not a classification problem.
//
// Which agent made a call is already stated in the traffic: the User-Agent
// names the product, and when it does not, the tool vocabulary does - agents
// ship a fixed toolset and no two of them chose the same names. Asking a model
// to guess would turn a fact into a probability, and an operator filtering a
// findings list by "Claude Code" needs a fact.
//
// What the traffic cannot state is the name of a bespoke script. urllib and
// httpx are runtimes, not agents, so those are reported as the library they
// are, and AgentVia records how the answer was reached, so the console can
// show "claude-code" and "custom (python-httpx)" without pretending the second
// one is a product name.

const (
	// AgentViaUserAgent means the client named itself. Treat as fact.
	AgentViaUserAgent = "user-agent"
	// AgentViaTools means the client did not name itself but its toolset is
	// a known agent's. Strong, but a caller can imitate a toolset.
	AgentViaTools = "tools"
	// AgentViaCredential means the agent was read off the subscription the
	// caller presented. Codex omits its User-Agent on some turns, and a
	// ChatGPT Codex token is only issued to Codex's own OAuth flow, so the
	// credential names the product when nothing else does.
	AgentViaCredential = "credential"
	// AgentViaLibrary means only the HTTP client is known: a bespoke agent,
	// or something that is not an agent at all.
	AgentViaLibrary = "library"
)

// upstreamAgents maps a declared upstream provider to the agent it implies.
// Only unambiguous ones are listed: "anthropic" is left out because any client
// can spend an Anthropic subscription, while a chatgpt_codex token exists only
// because Codex's own login produced it.
var upstreamAgents = map[string]string{
	"chatgpt_codex":  "codex",
	"github_copilot": "copilot",
}

// AgentUnknown is the agent of a call that carried no usable User-Agent.
const AgentUnknown = ""

// agentSignature matches one agent product.
type agentSignature struct {
	agent string
	// products are lowercase User-Agent product tokens, matched by prefix so
	// a rename from "codex" to "codex_cli_rs" does not need a new entry.
	products []string
	// tools are names unique enough that seeing minTools of them identifies
	// the agent on its own. Shared names (read_file, Bash) are omitted: they
	// would match half the field.
	tools    []string
	minTools int
}

// Ordered: the first match wins, so put the specific before the general.
var agentSignatures = []agentSignature{
	{
		agent:    "claude-code",
		products: []string{"claude-cli", "claude-code"},
		tools:    []string{"TodoWrite", "ExitPlanMode", "NotebookEdit", "MultiEdit", "BashOutput", "KillShell", "AskUserQuestion", "SlashCommand"},
		minTools: 1,
	},
	{
		agent:    "codex",
		products: []string{"codex"},
		tools:    []string{"apply_patch", "update_plan"},
		minTools: 1,
	},
	{
		agent:    "copilot",
		products: []string{"githubcopilotchat", "copilot", "vscode"},
		tools:    []string{"replace_string_in_file", "semantic_search", "manage_todo_list", "run_in_terminal", "grep_search", "file_search", "insert_edit_into_file"},
		minTools: 2,
	},
	{
		agent:    "cursor",
		products: []string{"cursor"},
		tools:    []string{"codebase_search", "run_terminal_cmd", "edit_file"},
		minTools: 2,
	},
	{
		agent:    "cline",
		products: []string{"cline", "roo-cline", "roo-code"},
		tools:    []string{"replace_in_file", "write_to_file", "browser_action", "attempt_completion"},
		minTools: 2,
	},
	{
		agent:    "gemini-cli",
		products: []string{"geminicli", "gemini-cli"},
		tools:    []string{"run_shell_command", "read_many_files", "write_file", "replace"},
		minTools: 2,
	},
	{
		agent:    "windsurf",
		products: []string{"windsurf", "codeium"},
	},
	{
		agent:    "opencode",
		products: []string{"opencode"},
	},
	{
		agent:    "aider",
		products: []string{"aider"},
	},
	{
		agent:    "goose",
		products: []string{"goose"},
	},
	{
		agent:    "continue",
		products: []string{"continue"},
	},
	{
		agent:    "zed",
		products: []string{"zed"},
	},
	{
		agent:    "kilocode",
		products: []string{"kilo-code", "kilocode"},
	},
	{
		agent:    "openhands",
		products: []string{"openhands", "opendevin"},
	},
	{
		agent:    "crush",
		products: []string{"crush", "charmbracelet"},
	},
	{
		agent:    "modelhive-cli",
		products: []string{"modelhive"},
	},
}

// agentLibraries maps a User-Agent product to the runtime behind a bespoke
// caller. Naming the library is the honest answer: it says the traffic is
// somebody's own code, and says what it was written with.
var agentLibraries = map[string]string{
	"openai-python":    "openai-python",
	"openai-node":      "openai-node",
	"anthropic-python": "anthropic-python",
	"anthropic-sdk-go": "anthropic-go",
	"anthropic-sdk":    "anthropic-sdk",
	"langchain":        "langchain",
	"langgraph":        "langgraph",
	"llama_index":      "llamaindex",
	"llamaindex":       "llamaindex",
	"litellm":          "litellm",
	"haystack":         "haystack",
	"autogen":          "autogen",
	"crewai":           "crewai",
	"python-urllib":    "python-urllib",
	"python-requests":  "python-requests",
	"httpx":            "python-httpx",
	"aiohttp":          "python-aiohttp",
	"node-fetch":       "node-fetch",
	"axios":            "axios",
	"undici":           "undici",
	"go-http-client":   "go-http",
	"okhttp":           "okhttp",
	"curl":             "curl",
	"wget":             "wget",
	"postmanruntime":   "postman",
	"insomnia":         "insomnia",
}

// fingerprintAgent names the agent behind a turn.
//
// declared are the tool names the request offered the model, called are the
// ones it used. The declared set is the better signal of the two: an agent
// sends its whole toolset on every turn, including the first, while the called
// set is empty until the model decides to reach for something.
//
// It returns the agent, and how it was decided. An empty agent means the turn
// carried nothing to go on, which is a legitimate answer and better than a
// guess.
func fingerprintAgent(product, app string, declared []string, called []ToolInvocation, upstream string) (string, string) {
	if agent := matchProduct(app); agent != "" {
		return agent, AgentViaUserAgent
	}
	if agent := matchProduct(product); agent != "" {
		return agent, AgentViaUserAgent
	}
	if agent := matchTools(declared, called); agent != "" {
		return agent, AgentViaTools
	}
	if agent, ok := upstreamAgents[strings.ToLower(strings.TrimSpace(upstream))]; ok {
		return agent, AgentViaCredential
	}
	if lib, ok := agentLibraries[strings.ToLower(strings.TrimSpace(product))]; ok {
		return lib, AgentViaLibrary
	}
	return AgentUnknown, ""
}

// declaredToolNames lists the tools a request offered the model. The three API
// flavours spell the same list differently; anything unparseable yields none,
// because a fingerprint that guesses is worse than one that abstains.
func declaredToolNames(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var req struct {
		Tools []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		name := t.Name
		if name == "" {
			name = t.Function.Name
		}
		if name != "" {
			tool, _, _ := parseToolName(name)
			out = append(out, tool)
		}
	}
	return out
}

// agentEvidenceRank orders the ways an agent can be identified, strongest
// first, so a session keeps the best answer any of its turns produced.
func agentEvidenceRank(via string) int {
	switch via {
	case AgentViaUserAgent:
		return 4
	case AgentViaTools:
		return 3
	case AgentViaCredential:
		return 2
	case AgentViaLibrary:
		return 1
	default:
		return 0
	}
}

func betterAgentEvidence(candidate, current string) bool {
	return agentEvidenceRank(candidate) > agentEvidenceRank(current)
}

func matchProduct(product string) string {
	p := strings.ToLower(strings.TrimSpace(product))
	if p == "" {
		return ""
	}
	for _, sig := range agentSignatures {
		for _, want := range sig.products {
			if strings.HasPrefix(p, want) {
				return sig.agent
			}
		}
	}
	return ""
}

func matchTools(declared []string, called []ToolInvocation) string {
	if len(declared) == 0 && len(called) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(declared)+len(called))
	for _, name := range declared {
		seen[name] = true
	}
	for _, t := range called {
		// Only native tools identify the caller: an MCP server's tool names
		// belong to the server and any agent can mount it.
		if t.Source == ToolSourceMCP {
			continue
		}
		seen[t.Tool] = true
	}
	for _, sig := range agentSignatures {
		if sig.minTools == 0 {
			continue
		}
		hits := 0
		for _, name := range sig.tools {
			if seen[name] {
				hits++
			}
		}
		if hits >= sig.minTools {
			return sig.agent
		}
	}
	return ""
}

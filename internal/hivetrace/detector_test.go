package hivetrace

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/hivestate"
)

func findingFor(findings []Finding, kind, typ string) (Finding, bool) {
	for _, f := range findings {
		if f.Kind == kind && f.Type == typ {
			return f, true
		}
	}
	return Finding{}, false
}

// The headline case: a credential pasted into a prompt is an exposure the
// organisation caused, and must be reported even though nothing blocked it.
func TestDetector_SecretPastedInUserMessage(t *testing.T) {
	det := newDetector()
	text := "here is my key ghp_abcdefghijklmnopqrstuvwxyz0123456789 please use it"

	findings := det.findings(text, OriginRequest, nil, 0)

	f, ok := findingFor(findings, KindSecret, "GITHUB_TOKEN")
	require.True(t, ok, "expected a GITHUB_TOKEN finding, got %+v", findings)
	assert.Equal(t, OriginRequest, f.Origin)
	assert.Equal(t, 1, f.Occurrences)
}

func TestDetector_SecretInModelResponse(t *testing.T) {
	det := newDetector()
	findings := det.findings("sure, use AKIAIOSFODNN7EXAMPLE", OriginResponse, nil, 0)

	f, ok := findingFor(findings, KindSecret, "AWS_ACCESS_KEY")
	require.True(t, ok)
	assert.Equal(t, OriginResponse, f.Origin)
}

func TestDetector_PIIAlongsideSecrets(t *testing.T) {
	det := newDetector()
	findings := det.findings("mail me at ada@example.com and ada2@example.com", OriginRequest, nil, 0)

	f, ok := findingFor(findings, KindPII, "EMAIL_ADDRESS")
	require.True(t, ok)
	assert.Equal(t, 2, f.Occurrences, "every occurrence is counted, not just the first")
}

func TestDetector_CountsEachKindSeparately(t *testing.T) {
	det := newDetector()
	findings := det.findings("ghp_abcdefghijklmnopqrstuvwxyz0123456789 ada@example.com", OriginRequest, nil, 0)

	_, hasSecret := findingFor(findings, KindSecret, "GITHUB_TOKEN")
	_, hasPII := findingFor(findings, KindPII, "EMAIL_ADDRESS")
	assert.True(t, hasSecret)
	assert.True(t, hasPII)
}

func TestDetector_HonoursTenantEntitySelection(t *testing.T) {
	det := newDetector()
	only := []string{"PHONE_NUMBER"}

	findings := det.findings("ada@example.com", OriginRequest, &only, 0)

	_, hasEmail := findingFor(findings, KindPII, "EMAIL_ADDRESS")
	assert.False(t, hasEmail, "a detector the tenant switched off must not be reported")
}

func TestDetector_CleanTextProducesNothing(t *testing.T) {
	det := newDetector()
	assert.Empty(t, det.findings("refactor the retry loop in the router", OriginRequest, nil, 0))
	assert.Empty(t, det.findings("", OriginResponse, nil, 0))
}

func TestDetector_RedactRemovesBothKinds(t *testing.T) {
	det := newDetector()
	text := "key ghp_abcdefghijklmnopqrstuvwxyz0123456789 mail ada@example.com"

	out, replaced := det.redact(text, nil)

	assert.NotContains(t, out, "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	assert.NotContains(t, out, "ada@example.com")
	assert.Contains(t, out, "[GITHUB_TOKEN]")
	assert.Contains(t, out, "[EMAIL_ADDRESS]")
	assert.Contains(t, replaced, "GITHUB_TOKEN")
	assert.Contains(t, replaced, "EMAIL_ADDRESS")
}

// The request carries the whole history. Scanning all of it would re-report a
// turn-one exposure on every later turn; only the tail is new.
func TestNewInputText_OnlyTheTurnThatJustArrived(t *testing.T) {
	messages := []hivestate.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "old secret ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Role: "assistant", Content: "noted"},
		{Role: "user", Content: "now read /src/a.go"},
	}

	text := newInputText(messages)

	assert.Equal(t, "now read /src/a.go", text)
	assert.NotContains(t, text, "ghp_")
}

func TestNewInputText_FirstTurnIncludesSystemPrompt(t *testing.T) {
	messages := []hivestate.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}

	text := newInputText(messages)

	assert.Contains(t, text, "you are helpful")
	assert.Contains(t, text, "hello")
}

// A tool result the agent hands back can carry the contents of a .env file,
// and it lands after the assistant turn that requested it.
func TestNewInputText_IncludesToolResults(t *testing.T) {
	messages := []hivestate.Message{
		{Role: "user", Content: "read the env file"},
		{Role: "assistant", Content: "[TOOL_CALL Read] {\"file_path\":\".env\"}"},
		{Role: "tool", Content: "AWS_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE", ToolCallID: "call_1"},
	}

	text := newInputText(messages)

	assert.Contains(t, text, "AKIAIOSFODNN7EXAMPLE")
	det := newDetector()
	_, ok := findingFor(det.findings(text, OriginRequest, nil, 0), KindSecret, "AWS_ACCESS_KEY")
	assert.True(t, ok, "a secret read back through a tool result must be flagged")
}

func TestExtractResponseText_ReassemblesAcrossChunks(t *testing.T) {
	// A credential split across two SSE frames is invisible to a per-chunk
	// scan; reassembly is what makes the detector see what the user sees.
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"token ghp_abcdefghijkl"}}]}`,
		`data: {"choices":[{"delta":{"content":"mnopqrstuvwxyz0123456789"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))

	text := extractResponseText(APIChatCompletions, body)

	assert.Equal(t, "token ghp_abcdefghijklmnopqrstuvwxyz0123456789", text)
	det := newDetector()
	_, ok := findingFor(det.findings(text, OriginResponse, nil, 0), KindSecret, "GITHUB_TOKEN")
	assert.True(t, ok)
}

func TestExtractResponseText_PerSurface(t *testing.T) {
	anthropic := []byte(strings.Join([]string{
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		``,
	}, "\n\n"))
	assert.Equal(t, "hello world", extractResponseText(APIMessages, anthropic))

	responses := []byte(strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello "}`,
		`data: {"type":"response.output_text.delta","delta":"there"}`,
		``,
	}, "\n\n"))
	assert.Equal(t, "hello there", extractResponseText(APIResponses, responses))

	plain := []byte(`{"choices":[{"message":{"content":"done"}}]}`)
	assert.Equal(t, "done", extractResponseText(APIChatCompletions, plain))
}

// Deltas alone reconstruct the answer; absorbing the terminal event too would
// duplicate every word.
func TestExtractResponseText_DoesNotDoubleCountTerminalEvent(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"answer"}`,
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`,
		``,
	}, "\n\n"))

	assert.Equal(t, "answer", extractResponseText(APIResponses, body))
}

func TestAPIFor(t *testing.T) {
	api, flavor := apiFor("/v1/messages")
	assert.Equal(t, APIMessages, api)
	assert.Equal(t, hivestate.APIAnthropic, flavor)

	api, flavor = apiFor("/v1/responses")
	assert.Equal(t, APIResponses, api)
	assert.Equal(t, hivestate.APIResponses, flavor)

	api, flavor = apiFor("/v1/chat/completions")
	assert.Equal(t, APIChatCompletions, api)
	assert.Equal(t, hivestate.APIOpenAI, flavor)
}

// The samples are opt-in. Off, a finding says what kind of thing it saw and
// nothing about the thing itself.
func TestFindings_NoSamplesUnlessAsked(t *testing.T) {
	det := newDetector()
	findings := det.findings("write to ada@example.com", OriginRequest, nil, 0)

	require.Len(t, findings, 1)
	assert.Empty(t, findings[0].Samples)
}

// On, the value is kept so an operator can tell a real address from a
// placeholder — which a count alone cannot.
func TestFindings_SamplesAreDistinctAndBounded(t *testing.T) {
	det := newDetector()
	text := "ada@example.com bob@example.com ada@example.com cy@example.com dot@example.com"
	findings := det.findings(text, OriginRequest, nil, 3)

	require.Len(t, findings, 1)
	assert.Equal(t, 5, findings[0].Occurrences)
	assert.Len(t, findings[0].Samples, 3, "samples stay bounded")
	// Four addresses, one of them twice: the count is occurrences, the list is
	// distinct values, and a reader must be able to tell those apart.
	assert.Greater(t, findings[0].Occurrences, len(findings[0].Samples))
	assert.Contains(t, findings[0].Samples, "ada@example.com")
	assert.Equal(t, len(findings[0].Samples), len(dedupe(findings[0].Samples)),
		"a repeated value must not fill the list")
}

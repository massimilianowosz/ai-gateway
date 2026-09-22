package vertex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newProviderWithStaticToken(t *testing.T, token string, expiry time.Time) *Provider {
	t.Helper()
	return &Provider{
		project:  "proj",
		location: "us-central1",
		modelID:  "gemini-2.5-flash",
		client:   http.DefaultClient,
		credentials: &google.Credentials{
			TokenSource: oauth2.StaticTokenSource(&oauth2.Token{
				AccessToken: token,
				Expiry:      expiry,
			}),
		},
	}
}

func TestGetAccessToken_CachesValidToken(t *testing.T) {
	p := newProviderWithStaticToken(t, "first-token", time.Now().Add(time.Hour))

	tok1, err := p.getAccessToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "first-token", tok1)

	// Mutate the underlying token source's would-be next value indirectly by
	// changing the cached fields directly, then confirm getAccessToken still
	// returns the cached value without calling TokenSource again.
	p.accessToken = "manually-cached"
	tok2, err := p.getAccessToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "manually-cached", tok2, "must return cached token when not expired")
}

func TestGetAccessToken_RefreshesWhenExpired(t *testing.T) {
	p := newProviderWithStaticToken(t, "unused", time.Now().Add(time.Hour))
	// Force an already-expired cached token.
	p.accessToken = "stale-token"
	p.tokenExpiry = time.Now().Add(-time.Minute)

	tok, err := p.getAccessToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "unused", tok, "expired cached token must trigger a refresh from TokenSource")
}

func TestGetAccessToken_RefreshesWithin60sMargin(t *testing.T) {
	p := newProviderWithStaticToken(t, "refreshed", time.Now().Add(time.Hour))
	// Cached token expires in 30s, inside the 60s safety margin.
	p.accessToken = "about-to-expire"
	p.tokenExpiry = time.Now().Add(30 * time.Second)

	tok, err := p.getAccessToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "refreshed", tok)
}

func TestGeminiURL_Format(t *testing.T) {
	p := &Provider{project: "my-proj", location: "europe-west1", modelID: "gemini-2.5-flash"}
	url := p.geminiURL("generateContent")
	assert.Equal(t, "https://europe-west1-aiplatform.googleapis.com/v1/projects/my-proj/locations/europe-west1/publishers/google/models/gemini-2.5-flash:generateContent", url)
}

func TestPartnerURL_Format(t *testing.T) {
	p := &Provider{project: "my-proj", location: "us-east5", modelID: "claude-sonnet-4", partnerPub: "anthropic"}
	url := p.partnerURL("rawPredict")
	assert.Equal(t, "https://us-east5-aiplatform.googleapis.com/v1/projects/my-proj/locations/us-east5/publishers/anthropic/models/claude-sonnet-4:rawPredict", url)
}

func TestURL_GlobalLocationHasNoRegionPrefix(t *testing.T) {
	g := &Provider{project: "my-proj", location: "global", modelID: "gemini-3-flash-preview"}
	assert.Equal(t,
		"https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/publishers/google/models/gemini-3-flash-preview:generateContent",
		g.geminiURL("generateContent"))

	a := &Provider{project: "my-proj", location: "global", modelID: "claude-opus-4-6", partnerPub: "anthropic"}
	assert.Equal(t,
		"https://aiplatform.googleapis.com/v1/projects/my-proj/locations/global/publishers/anthropic/models/claude-opus-4-6:rawPredict",
		a.partnerURL("rawPredict"))
}

func TestParseVertexError_StructuredJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    403,
				"message": "Permission denied on project",
				"status":  "PERMISSION_DENIED",
			},
		})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	upstreamErr := parseVertexError(resp)
	require.Error(t, upstreamErr)
	assert.Contains(t, upstreamErr.Error(), "Permission denied on project")
}

func TestParseVertexError_UnstructuredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("upstream unavailable"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	upstreamErr := parseVertexError(resp)
	require.Error(t, upstreamErr)
	assert.Contains(t, upstreamErr.Error(), "upstream unavailable")
}

func TestNew_MissingProject(t *testing.T) {
	_, err := New(Config{Location: "us-central1"}, "gemini-2.5-flash")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project is required")
}

func TestNew_MissingLocation(t *testing.T) {
	_, err := New(Config{Project: "proj"}, "gemini-2.5-flash")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "location is required")
}

func TestNew_InvalidCredentialsJSON(t *testing.T) {
	_, err := New(Config{
		Project:         "proj",
		Location:        "us-central1",
		CredentialsJSON: "not valid json",
	}, "gemini-2.5-flash")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to initialize credentials")
}

func TestNew_DetectsPartnerModel(t *testing.T) {
	p := &Provider{modelID: "claude-sonnet-4"}
	if isAnthropicModel(p.modelID) {
		p.isPartner = true
		p.partnerPub = "anthropic"
	}
	assert.True(t, p.isPartner)
	assert.Equal(t, "anthropic", p.partnerPub)
}

func TestGeminiStreamReader_TextAndToolCallAndFinish(t *testing.T) {
	sseBody := `data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":""}]}

data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"loc":"Rome"}}}]},"finishReason":"STOP"}]}

`
	r := &geminiStreamReader{
		reader:  bufio.NewReader(strings.NewReader(sseBody)),
		body:    io.NopCloser(strings.NewReader("")),
		headers: http.Header{},
		model:   "gemini-2.5-flash",
	}

	chunk1, err := r.Next()
	require.NoError(t, err)
	var c1 struct {
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(chunk1, &c1))
	assert.Equal(t, "assistant", c1.Choices[0].Delta.Role)
	assert.Equal(t, "Hi", c1.Choices[0].Delta.Content)

	chunk2, err := r.Next()
	require.NoError(t, err)
	var c2 struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(chunk2, &c2))
	require.Len(t, c2.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "get_weather", c2.Choices[0].Delta.ToolCalls[0].Function.Name)
	require.NotNil(t, c2.Choices[0].FinishReason)
	assert.Equal(t, "stop", *c2.Choices[0].FinishReason)

	_, err = r.Next()
	assert.Equal(t, io.EOF, err)
}

func TestGeminiStreamReader_SkipsMalformedJSON(t *testing.T) {
	sseBody := "data: {not valid json}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"\"}]}\n\n"
	r := &geminiStreamReader{
		reader:  bufio.NewReader(strings.NewReader(sseBody)),
		body:    io.NopCloser(strings.NewReader("")),
		headers: http.Header{},
		model:   "gemini-2.5-flash",
	}

	chunk, err := r.Next()
	require.NoError(t, err)
	var c struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(chunk, &c))
	assert.Equal(t, "ok", c.Choices[0].Delta.Content)
}

func TestAnthropicStreamReader_MessageStartTextDeltaToolUseAndStop(t *testing.T) {
	sseBody := `data: {"type":"message_start"}

data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"tu_1","name":"search"}}

data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}

data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"\"rome\"}"}}

data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

data: {"type":"message_stop"}

`
	r := &anthropicStreamReader{
		reader:  bufio.NewReader(strings.NewReader(sseBody)),
		body:    io.NopCloser(strings.NewReader("")),
		headers: http.Header{},
		model:   "claude-sonnet-4",
	}

	type deltaShape struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	type chunkShape struct {
		Choices []struct {
			Delta        deltaShape `json:"delta"`
			FinishReason *string    `json:"finish_reason"`
		} `json:"choices"`
	}

	var chunks []chunkShape
	for {
		raw, err := r.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var c chunkShape
		require.NoError(t, json.Unmarshal(raw, &c))
		chunks = append(chunks, c)
	}

	// message_start, content_block_start(tool_use), 2x input_json_delta,
	// text_delta, message_delta(stop_reason) = 6 chunks before message_stop (EOF).
	require.Len(t, chunks, 6)
	assert.Equal(t, "assistant", chunks[0].Choices[0].Delta.Role)
	require.Len(t, chunks[1].Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "search", chunks[1].Choices[0].Delta.ToolCalls[0].Function.Name)
	require.Len(t, chunks[2].Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, `{"q":`, chunks[2].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	require.Len(t, chunks[3].Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, `"rome"}`, chunks[3].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "done", chunks[4].Choices[0].Delta.Content)
	require.NotNil(t, chunks[5].Choices[0].FinishReason)
	assert.Equal(t, "tool_calls", *chunks[5].Choices[0].FinishReason)
}

func TestConvertEmbeddingInput_StringAndSlice(t *testing.T) {
	single := convertEmbeddingInput("hello")
	require.Len(t, single, 1)
	assert.Equal(t, "hello", single[0]["content"])

	multi := convertEmbeddingInput([]interface{}{"a", "b"})
	require.Len(t, multi, 2)
	assert.Equal(t, "a", multi[0]["content"])
	assert.Equal(t, "b", multi[1]["content"])
}

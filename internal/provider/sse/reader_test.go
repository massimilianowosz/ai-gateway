package sse

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

func TestReader_Next_ParsesDataLines(t *testing.T) {
	input := "data: {\"id\":\"1\"}\n\ndata: {\"id\":\"2\"}\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk1, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"id":"1"}`, string(chunk1))

	chunk2, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"id":"2"}`, string(chunk2))

	_, err = reader.Next()
	assert.Equal(t, io.EOF, err)
}

func TestReader_Next_DoneSentinel(t *testing.T) {
	input := "data: {\"text\":\"hello\"}\n\ndata: [DONE]\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Contains(t, string(chunk), "hello")

	_, err = reader.Next()
	assert.Equal(t, io.EOF, err)
}

func TestReader_Next_SkipsNonDataLines(t *testing.T) {
	input := "event: message\nid: 123\nretry: 5000\ndata: {\"ok\":true}\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(chunk))
}

func TestReader_Next_EmptyStream(t *testing.T) {
	reader := NewReader(io.NopCloser(strings.NewReader("")), http.Header{})

	_, err := reader.Next()
	assert.Equal(t, io.EOF, err)
}

func TestReader_Next_BlankLinesIgnored(t *testing.T) {
	input := "\n\n\ndata: {\"x\":1}\n\n\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"x":1}`, string(chunk))

	_, err = reader.Next()
	assert.Equal(t, io.EOF, err)
}

func TestReader_Close(t *testing.T) {
	reader := NewReader(io.NopCloser(strings.NewReader("")), http.Header{"X-Test": {"val"}})
	assert.NoError(t, reader.Close())
}

func TestReader_Headers(t *testing.T) {
	headers := http.Header{"X-Request-Id": {"abc123"}}
	reader := NewReader(io.NopCloser(strings.NewReader("")), headers)
	assert.Equal(t, "abc123", reader.Headers().Get("X-Request-Id"))
}

func TestParseOpenAIError_Structured(t *testing.T) {
	body := `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit"}}`
	resp := &http.Response{
		StatusCode: 429,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	err := ParseOpenAIError(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Rate limit exceeded")
	assert.Contains(t, err.Error(), "429")
}

func TestParseOpenAIError_Unstructured(t *testing.T) {
	body := `Server error`
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	err := ParseOpenAIError(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Server error")
}

func TestReader_Next_LargeChunk(t *testing.T) {
	large := strings.Repeat("x", 200*1024)
	input := "data: " + large + "\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, large, string(chunk))
}

func TestReader_Next_MultibyteUTF8Preserved(t *testing.T) {
	input := "data: {\"text\":\"héllo wörld 你好 🎉\"}\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"text":"héllo wörld 你好 🎉"}`, string(chunk))
}

func TestReader_Next_HandlesCRLFLineEndings(t *testing.T) {
	input := "data: {\"a\":1}\r\n\r\ndata: {\"a\":2}\r\n\r\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk1, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, string(chunk1))

	chunk2, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"a":2}`, string(chunk2))
}

func TestReader_Next_BOMAtStreamStart(t *testing.T) {
	// A UTF-8 BOM before the first "data:" line should not prevent parsing;
	// it just gets treated (and skipped) as part of the first, non-"data:",
	// blank-after-trim line.
	input := "\xEF\xBB\xBF\ndata: {\"ok\":true}\n\n"
	reader := NewReader(io.NopCloser(strings.NewReader(input)), http.Header{})

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(chunk))
}

func TestParseOpenAIError_TruncatesBodyAt4096Bytes(t *testing.T) {
	huge := strings.Repeat("a", 10000)
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(strings.NewReader(huge)),
	}

	err := ParseOpenAIError(resp)
	require.Error(t, err)
	upstreamErr, ok := err.(*provider.UpstreamError)
	require.True(t, ok)
	assert.LessOrEqual(t, len(upstreamErr.Message), 4096)
}

func TestParseOpenAIError_ValidJSONButMissingMessageFallsBackToRawBody(t *testing.T) {
	body := `{"error":{"type":"server_error"}}`
	resp := &http.Response{
		StatusCode: 502,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	err := ParseOpenAIError(resp)
	require.Error(t, err)
	upstreamErr, ok := err.(*provider.UpstreamError)
	require.True(t, ok)
	assert.Equal(t, body, upstreamErr.Message, "empty error.message should fall back to raw body")
}

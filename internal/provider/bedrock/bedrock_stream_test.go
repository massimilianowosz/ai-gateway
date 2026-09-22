package bedrock

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// encodeEventStreamFrame builds a minimal AWS EventStream binary frame
// carrying a single ":event-type" string header and a JSON payload. CRCs are
// zero-filled since bedrockStreamReader does not validate them.
func encodeEventStreamFrame(t *testing.T, eventType string, payload []byte) []byte {
	t.Helper()

	var headers bytes.Buffer
	name := ":event-type"
	headers.WriteByte(byte(len(name)))
	headers.WriteString(name)
	headers.WriteByte(7) // string type
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(eventType)))
	headers.Write(lenBuf[:])
	headers.WriteString(eventType)

	headersLen := headers.Len()
	remaining := headersLen + len(payload) + 4 // + message CRC
	totalLen := 12 + remaining

	var frame bytes.Buffer
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(totalLen))
	frame.Write(u32[:])
	binary.BigEndian.PutUint32(u32[:], uint32(headersLen))
	frame.Write(u32[:])
	frame.Write([]byte{0, 0, 0, 0}) // prelude crc (unchecked)
	frame.Write(headers.Bytes())
	frame.Write(payload)
	frame.Write([]byte{0, 0, 0, 0}) // message crc (unchecked)

	return frame.Bytes()
}

func newTestStreamReader(t *testing.T, frames ...[]byte) *bedrockStreamReader {
	t.Helper()
	var buf bytes.Buffer
	for _, f := range frames {
		buf.Write(f)
	}
	return &bedrockStreamReader{
		reader:  bufio.NewReader(&buf),
		body:    io.NopCloser(&buf),
		headers: http.Header{},
		model:   "bedrock-claude-sonnet-4",
	}
}

func TestBedrockStreamReader_FullEventSequence(t *testing.T) {
	messageStart, _ := json.Marshal(map[string]string{"role": "assistant"})
	textDelta, _ := json.Marshal(map[string]interface{}{
		"contentBlockIndex": 0,
		"delta":             map[string]string{"text": "Hello"},
	})
	toolStart, _ := json.Marshal(map[string]interface{}{
		"contentBlockIndex": 1,
		"start": map[string]interface{}{
			"toolUse": map[string]string{"toolUseId": "tu_1", "name": "get_weather"},
		},
	})
	toolArgDelta, _ := json.Marshal(map[string]interface{}{
		"contentBlockIndex": 1,
		"delta": map[string]interface{}{
			"toolUse": map[string]string{"input": `{"city":"Rome"}`},
		},
	})
	messageStop, _ := json.Marshal(map[string]string{"stopReason": "tool_use"})

	r := newTestStreamReader(t,
		encodeEventStreamFrame(t, "messageStart", messageStart),
		encodeEventStreamFrame(t, "contentBlockDelta", textDelta),
		encodeEventStreamFrame(t, "contentBlockStart", toolStart),
		encodeEventStreamFrame(t, "contentBlockDelta", toolArgDelta),
		encodeEventStreamFrame(t, "contentBlockStop", nil),
		encodeEventStreamFrame(t, "messageStop", messageStop),
		encodeEventStreamFrame(t, "metadata", []byte(`{}`)),
	)

	type parsedChunk struct {
		Choices []struct {
			Delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}

	var chunks []parsedChunk
	for {
		raw, err := r.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var c parsedChunk
		require.NoError(t, json.Unmarshal(raw, &c))
		chunks = append(chunks, c)
	}

	require.Len(t, chunks, 5) // messageStart, textDelta, toolStart, toolArgDelta, messageStop (contentBlockStop/metadata emit no chunk / EOF)
	assert.Equal(t, "assistant", chunks[0].Choices[0].Delta.Role)
	assert.Equal(t, "Hello", chunks[1].Choices[0].Delta.Content)
	require.Len(t, chunks[2].Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "get_weather", chunks[2].Choices[0].Delta.ToolCalls[0].Function.Name)
	require.Len(t, chunks[3].Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, `{"city":"Rome"}`, chunks[3].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	require.NotNil(t, chunks[4].Choices[0].FinishReason)
	assert.Equal(t, "tool_calls", *chunks[4].Choices[0].FinishReason)
}

func TestBedrockStreamReader_MessageStopMapsFinishReason(t *testing.T) {
	messageStop, _ := json.Marshal(map[string]string{"stopReason": "max_tokens"})
	metadata, _ := json.Marshal(map[string]interface{}{})

	r := newTestStreamReader(t,
		encodeEventStreamFrame(t, "messageStop", messageStop),
		encodeEventStreamFrame(t, "metadata", metadata),
	)

	raw, err := r.Next()
	require.NoError(t, err)

	var c struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(raw, &c))
	require.NotNil(t, c.Choices[0].FinishReason)
	assert.Equal(t, "length", *c.Choices[0].FinishReason)

	_, err = r.Next()
	assert.Equal(t, io.EOF, err)
}

func TestBedrockStreamReader_FrameTooLarge(t *testing.T) {
	var prelude bytes.Buffer
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(2<<20)) // > 1MB max
	prelude.Write(u32[:])
	binary.BigEndian.PutUint32(u32[:], 0)
	prelude.Write(u32[:])
	prelude.Write([]byte{0, 0, 0, 0})

	r := &bedrockStreamReader{
		reader:  bufio.NewReader(bytes.NewReader(prelude.Bytes())),
		body:    io.NopCloser(bytes.NewReader(nil)),
		headers: http.Header{},
		model:   "m",
	}

	_, err := r.Next()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frame too large")
}

func TestBedrockStreamReader_TruncatedStreamReturnsError(t *testing.T) {
	r := &bedrockStreamReader{
		reader:  bufio.NewReader(bytes.NewReader([]byte{0, 0, 0, 5})), // incomplete prelude
		body:    io.NopCloser(bytes.NewReader(nil)),
		headers: http.Header{},
		model:   "m",
	}

	_, err := r.Next()
	require.Error(t, err)
	assert.NotEqual(t, io.EOF, err)
}

func TestBedrockStreamReader_CloseAndHeaders(t *testing.T) {
	h := http.Header{"X-Test": []string{"1"}}
	r := &bedrockStreamReader{
		reader:  bufio.NewReader(bytes.NewReader(nil)),
		body:    io.NopCloser(bytes.NewReader(nil)),
		headers: h,
		model:   "m",
	}
	assert.Equal(t, h, r.Headers())
	assert.NoError(t, r.Close())
}

func TestAuthRequest_SigV4SignsAgainstRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
		assert.NotEmpty(t, r.Header.Get("X-Amz-Date"))
		assert.NotEmpty(t, r.Header.Get("X-Amz-Content-Sha256"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, err := New(Config{
		Region:          "us-east-1",
		AccessKeyID:     "AKIA-test",
		SecretAccessKey: "secret",
	}, "test-model")
	require.NoError(t, err)

	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	require.NoError(t, p.authRequest(req, []byte(`{}`)))

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestParseBedrockError_StructuredType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "The model returned an error",
			"__type":  "ValidationException",
		})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	upstreamErr := parseBedrockError(resp)
	var ue *provider.UpstreamError
	require.ErrorAs(t, upstreamErr, &ue)
	assert.Equal(t, http.StatusBadRequest, ue.StatusCode)
	assert.Equal(t, "The model returned an error", ue.Message)
	assert.Equal(t, "ValidationException", ue.Type)
}

func TestParseBedrockError_UnstructuredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	upstreamErr := parseBedrockError(resp)
	var ue *provider.UpstreamError
	require.ErrorAs(t, upstreamErr, &ue)
	assert.Equal(t, "internal server error", ue.Message)
}

func TestNewProvider_SigV4_MissingSecretAccessKey(t *testing.T) {
	_, err := New(Config{
		Region:      "us-east-1",
		AccessKeyID: "AKIA-test",
	}, "test-model")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret_access_key is required")
}

func TestNewProvider_MissingRegion(t *testing.T) {
	_, err := New(Config{
		BearerToken: "ABSK-token",
	}, "test-model")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "region is required")
}

func TestConvertContentBlocks_ImageDataURI(t *testing.T) {
	msg := provider.Message{
		Role: "user",
		Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "what is this?"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{
				"url": "data:image/png;base64,QUFB",
			}},
		},
	}

	blocks := convertContentBlocks(msg)
	require.Len(t, blocks, 2)
	assert.Equal(t, "what is this?", blocks[0].Text)
	require.NotNil(t, blocks[1].Image)
	assert.Equal(t, "png", blocks[1].Image.Format)
	assert.Equal(t, "QUFB", blocks[1].Image.Source.Bytes)
}

func TestExtractStopSequences_Variants(t *testing.T) {
	assert.Equal(t, []string{"STOP"}, extractStopSequences("STOP"))
	assert.Equal(t, []string{"A", "B"}, extractStopSequences([]interface{}{"A", "B"}))
	assert.Nil(t, extractStopSequences(42))
}

func TestProvider_Complete_ContextCancelled(t *testing.T) {
	p, err := New(Config{
		Region:      "us-east-1",
		BearerToken: "ABSK-test",
	}, "test-model")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = p.Complete(ctx, &provider.CompletionRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Error(t, err)
}

package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// Reader implements provider.StreamReader for standard SSE responses.
// Used by OpenAI-compatible and Azure providers that follow the
// "data: {...}\n\n" / "data: [DONE]\n\n" protocol.
type Reader struct {
	scanner *bufio.Reader
	body    io.ReadCloser
	headers http.Header
}

// NewReader wraps an HTTP response body as an SSE StreamReader.
func NewReader(body io.ReadCloser, headers http.Header) *Reader {
	return &Reader{
		scanner: bufio.NewReader(body),
		body:    body,
		headers: headers,
	}
}

func (r *Reader) Next() ([]byte, error) {
	for {
		line, err := ReadLine(r.scanner)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading SSE stream: %w", err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		if bytes.HasPrefix(line, []byte("data: ")) {
			data := bytes.TrimPrefix(line, []byte("data: "))
			if bytes.Equal(data, []byte("[DONE]")) {
				return nil, io.EOF
			}
			return data, nil
		}
		// Skip non-data SSE fields (event:, id:, retry:)
	}
}

func (r *Reader) Close() error         { return r.body.Close() }
func (r *Reader) Headers() http.Header { return r.headers }

// ParseOpenAIError parses an OpenAI-format error response body into an UpstreamError.
// Works for Azure OpenAI, OpenAI, and any API that returns {"error": {"message": ...}}.
func ParseOpenAIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
		return &provider.UpstreamError{
			StatusCode: resp.StatusCode,
			Message:    errResp.Error.Message,
			Type:       errResp.Error.Type,
			Code:       errResp.Error.Code,
		}
	}
	return &provider.UpstreamError{
		StatusCode: resp.StatusCode,
		Message:    string(body),
	}
}

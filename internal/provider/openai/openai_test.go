package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

func TestNewCompatible_Name(t *testing.T) {
	p := NewCompatible("https://example.com/v1", "key")
	assert.Equal(t, "openai_compatible", p.Name())
}

func TestCompatible_Complete_UsesConfiguredAPIKey(t *testing.T) {
	var capturedAuth, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedPath = r.URL.Path

		var req provider.CompletionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.False(t, req.Stream)
		assert.Equal(t, "gpt-4o", req.Model)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{
			ID:      "chatcmpl-1",
			Model:   "gpt-4o",
			Choices: []provider.Choice{{Index: 0, Message: &provider.Message{Role: "assistant", Content: "hi"}}},
			Usage:   &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "configured-key")
	resp, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "Bearer configured-key", capturedAuth)
	assert.Equal(t, "/chat/completions", capturedPath)
	assert.Equal(t, "chatcmpl-1", resp.ID)
	assert.Equal(t, 2, resp.Usage.TotalTokens)
}

func TestCompatible_Complete_PrefersUpstreamTokenOverConfiguredKey(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "chatcmpl-1", Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "hi"}}}})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "configured-key")
	ctx := auth.ContextWithUpstreamToken(context.Background(), "upstream-token")
	_, err := p.Complete(ctx, &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})

	require.NoError(t, err)
	assert.Equal(t, "Bearer upstream-token", capturedAuth)
}

func TestCompatible_ForwardsTenantOnlyWhenAsked(t *testing.T) {
	// The Edge proxy resolves a tenant-scoped runtime behind a global model
	// name, so it needs to know who is calling. Nobody else does: the header is
	// an identity assertion, and a third-party provider must never receive it.
	var captured string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get(TenantHeader)
		_, present = r.Header[http.CanonicalHeaderKey(TenantHeader)]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "chatcmpl-1", Choices: []provider.Choice{{Message: &provider.Message{Role: "assistant", Content: "hi"}}}})
	}))
	defer srv.Close()

	ctx := auth.ContextWithTeam(context.Background(), &store.Team{ID: "team-acme"})
	req := &provider.CompletionRequest{Model: "edge/qwen", Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	_, err := NewCompatible(srv.URL, "k").Complete(ctx, req)
	require.NoError(t, err)
	assert.False(t, present, "a plain provider must not learn the caller's tenant")

	_, err = NewCompatible(srv.URL, "k", WithTenantForwarding()).Complete(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "team-acme", captured)

	// No authenticated team: send nothing rather than an empty assertion, so
	// the backend refuses the request instead of routing it on a blank tenant.
	_, err = NewCompatible(srv.URL, "k", WithTenantForwarding()).Complete(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, present)
}

func TestCompatible_MaxTokensConvertedToMaxCompletionTokens(t *testing.T) {
	var captured provider.CompletionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Decode into a generic map since CompletionRequest's UnmarshalJSON
		// would re-populate MaxTokens from max_tokens if still present.
		var raw map[string]interface{}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &raw)
		json.Unmarshal(body, &captured)

		_, hasMaxTokens := raw["max_tokens"]
		assert.False(t, hasMaxTokens, "max_tokens must be stripped from the outgoing request")
		assert.Equal(t, float64(256), raw["max_completion_tokens"])

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.CompletionResponse{ID: "x"})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	maxTokens := 256
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:     "gpt-4o",
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &maxTokens,
	})
	require.NoError(t, err)
}

func TestCompatible_Stream_SetsStreamTrueAndParsesChunks(t *testing.T) {
	sseData := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req provider.CompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		assert.True(t, req.Stream)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(sseData))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	defer reader.Close()

	chunk, err := reader.Next()
	require.NoError(t, err)
	assert.Contains(t, string(chunk), "Hi")

	_, err = reader.Next()
	assert.ErrorIs(t, err, io.EOF)
}

func TestCompatible_Complete_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "rate limited", "type": "rate_limit_error"},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	_, err := p.Complete(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})

	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusTooManyRequests, upstreamErr.StatusCode)
}

func TestCompatible_Stream_ErrorResponseClosesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "boom", "type": "server_error"},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	reader, err := p.Stream(context.Background(), &provider.CompletionRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	require.Nil(t, reader)
	require.Error(t, err)
	var upstreamErr *provider.UpstreamError
	require.ErrorAs(t, err, &upstreamErr)
	assert.Equal(t, http.StatusInternalServerError, upstreamErr.StatusCode)
}

func TestCompatible_Embed(t *testing.T) {
	var capturedAuth, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.EmbeddingResponse{
			Object: "list",
			Model:  "text-embedding-3-small",
			Data:   []provider.EmbeddingObject{{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}}},
			Usage:  provider.EmbeddingUsage{PromptTokens: 3, TotalTokens: 3},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	resp, err := p.Embed(context.Background(), &provider.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hello"})

	require.NoError(t, err)
	assert.Equal(t, "/embeddings", capturedPath)
	assert.Equal(t, "Bearer key", capturedAuth)
	assert.Len(t, resp.Data, 1)
}

func TestCompatible_Embed_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "bad input", "type": "invalid_request_error"},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	_, err := p.Embed(context.Background(), &provider.EmbeddingRequest{Model: "text-embedding-3-small", Input: "hello"})
	require.Error(t, err)
}

func TestCompatible_Moderate(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.ModerationResponse{
			ID:    "modr-1",
			Model: "omni-moderation-latest",
			Results: []provider.ModerationResult{{
				Flagged:    true,
				Categories: map[string]bool{"violence": true},
			}},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	resp, err := p.Moderate(context.Background(), &provider.ModerationRequest{Model: "omni-moderation-latest", Input: "some text"})

	require.NoError(t, err)
	assert.Equal(t, "/moderations", capturedPath)
	assert.True(t, resp.Results[0].Flagged)
}

func TestCompatible_GenerateImage(t *testing.T) {
	var capturedPath string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"url":"https://img.example.com/1.png"}]}`))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	respBody, err := p.GenerateImage(context.Background(), []byte(`{"prompt":"a cat"}`))

	require.NoError(t, err)
	assert.Equal(t, "/images/generations", capturedPath)
	assert.JSONEq(t, `{"prompt":"a cat"}`, string(capturedBody))
	assert.Contains(t, string(respBody), "img.example.com")
}

func TestCompatible_Speak_DefaultsContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/audio/speech", r.URL.Path)
		// Explicitly set an empty Content-Type to prevent Go's automatic
		// sniffing from populating one, so we can exercise the provider's
		// own fallback to "audio/mpeg" when upstream sends none.
		w.Header().Set("Content-Type", "")
		w.Write([]byte("fake-audio-bytes"))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	audio, contentType, err := p.Speak(context.Background(), []byte(`{"input":"hello","voice":"alloy"}`))

	require.NoError(t, err)
	assert.Equal(t, "fake-audio-bytes", string(audio))
	assert.Equal(t, "audio/mpeg", contentType)
}

func TestCompatible_Speak_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "invalid api key", "type": "authentication_error"},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	_, _, err := p.Speak(context.Background(), []byte(`{}`))
	require.Error(t, err)
}

func TestCompatible_Transcribe_SendsMultipartForm(t *testing.T) {
	var capturedModel, capturedLanguage, capturedFormat, capturedFilename, capturedFileContent string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(10<<20))
		capturedModel = r.FormValue("model")
		capturedLanguage = r.FormValue("language")
		capturedFormat = r.FormValue("response_format")

		file, header, err := r.FormFile("file")
		require.NoError(t, err)
		defer file.Close()
		capturedFilename = header.Filename
		data, _ := io.ReadAll(file)
		capturedFileContent = string(data)

		w.Write([]byte(`{"text":"transcribed text"}`))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	resp, err := p.Transcribe(context.Background(), &provider.TranscriptionRequest{
		Model:    "whisper-1",
		File:     []byte("fake-audio-data"),
		Filename: "audio.mp3",
		Language: "en",
		Format:   "json",
	})

	require.NoError(t, err)
	assert.Contains(t, string(resp), "transcribed text")
	assert.Equal(t, "whisper-1", capturedModel)
	assert.Equal(t, "en", capturedLanguage)
	assert.Equal(t, "json", capturedFormat)
	assert.Equal(t, "audio.mp3", capturedFilename)
	assert.Equal(t, "fake-audio-data", capturedFileContent)
}

func TestCompatible_Forward_PassesThroughContentTypeAndBody(t *testing.T) {
	var capturedContentType string
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedContentType = r.Header.Get("Content-Type")
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("raw-response"))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	respBody, respContentType, err := p.Forward(context.Background(), "/custom/endpoint", "application/custom", bytes.NewReader([]byte("payload")))

	require.NoError(t, err)
	assert.Equal(t, "application/custom", capturedContentType)
	assert.Equal(t, "payload", string(capturedBody))
	assert.Equal(t, "raw-response", string(respBody))
	assert.Equal(t, "application/octet-stream", respContentType)
}

func TestCompatible_Forward_DefaultsToJSONContentType(t *testing.T) {
	var capturedContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedContentType = r.Header.Get("Content-Type")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	_, _, err := p.Forward(context.Background(), "/custom", "", bytes.NewReader([]byte("{}")))

	require.NoError(t, err)
	assert.Equal(t, "application/json", capturedContentType)
}

func TestCompatible_Forward_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "not found", "type": "invalid_request_error"},
		})
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	_, _, err := p.Forward(context.Background(), "/missing", "application/json", bytes.NewReader([]byte("{}")))
	require.Error(t, err)
}

func TestCompatible_DoFileRequestStreamsBodyAndPreservesQuery(t *testing.T) {
	var capturedBody, capturedAuth, capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/files", r.URL.Path)
		capturedQuery = r.URL.RawQuery
		capturedAuth = r.Header.Get("Authorization")
		capturedBody = string(mustReadAll(t, r.Body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"file-provider"}`))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	resp, err := p.DoFileRequest(context.Background(), provider.FileRequest{
		Method:        http.MethodPost,
		Path:          "/files",
		RawQuery:      "purpose=user_data",
		ContentType:   "multipart/form-data; boundary=test",
		ContentLength: int64(len("streamed-payload")),
		Body:          strings.NewReader("streamed-payload"),
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "purpose=user_data", capturedQuery)
	assert.Equal(t, "Bearer key", capturedAuth)
	assert.Equal(t, "streamed-payload", capturedBody)
}

func TestCompatible_DoResponsesRequestUsesNativeEndpoint(t *testing.T) {
	var capturedBody, capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/responses", r.URL.Path)
		capturedAuth = r.Header.Get("Authorization")
		capturedBody = string(mustReadAll(t, r.Body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-provider"}`))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	body := `{"model":"gpt-4o-mini","input":"hello"}`
	resp, err := p.DoResponsesRequest(context.Background(), strings.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "Bearer key", capturedAuth)
	assert.JSONEq(t, body, capturedBody)
}

// ensure multipart writer usage in the provider doesn't leak unclosed parts
// (sanity check that the constructed request is well-formed multipart data)
func TestCompatible_Transcribe_ProducesValidMultipart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		assert.Equal(t, "multipart/form-data", mediaType)

		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := mr.NextPart()
		require.NoError(t, err)
		assert.NotEmpty(t, part.FormName())

		w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	p := NewCompatible(srv.URL, "key")
	_, err := p.Transcribe(context.Background(), &provider.TranscriptionRequest{
		Model:    "whisper-1",
		File:     []byte("data"),
		Filename: "a.wav",
	})
	require.NoError(t, err)
}

func mustReadAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return data
}

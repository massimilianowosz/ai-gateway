package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/config"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

type mockFileProvider struct {
	lastFileRequest provider.FileRequest
	uploadBody      []byte
	content         []byte
	contentType     string
	lastCompletion  *provider.CompletionRequest
}

type mockNativeFileProvider struct {
	*mockFileProvider
	nativeBody map[string]interface{}
}

func (m *mockFileProvider) Name() string { return "mock-files" }

func (m *mockFileProvider) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.lastCompletion = req
	stop := "stop"
	return &provider.CompletionResponse{
		ID:      "chatcmpl-files",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []provider.Choice{{
			Index:        0,
			Message:      &provider.Message{Role: "assistant", Content: "ok"},
			FinishReason: &stop,
		}},
	}, nil
}

func (m *mockFileProvider) Stream(_ context.Context, _ *provider.CompletionRequest) (provider.StreamReader, error) {
	return &mockAnthropicStreamReader{}, nil
}

func (m *mockFileProvider) DoFileRequest(_ context.Context, req provider.FileRequest) (*http.Response, error) {
	m.lastFileRequest = req
	switch {
	case req.Method == http.MethodPost && req.Path == "/files":
		m.uploadBody, _ = io.ReadAll(req.Body)
		return jsonHTTPResponse(http.StatusOK, "application/json", `{
			"id":"file-upstream-123",
			"object":"file",
			"bytes":128,
			"created_at":1700000000,
			"filename":"assessment.pdf",
			"purpose":"user_data"
		}`), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/content"):
		contentType := m.contentType
		if contentType == "" {
			contentType = "application/pdf"
		}
		return bytesHTTPResponse(http.StatusOK, contentType, m.content), nil
	case req.Method == http.MethodDelete:
		return jsonHTTPResponse(http.StatusOK, "application/json", `{"id":"file-upstream-123","deleted":true}`), nil
	default:
		return jsonHTTPResponse(http.StatusNotFound, "application/json", `{"error":{"message":"not found","type":"invalid_request_error"}}`), nil
	}
}

func (m *mockNativeFileProvider) DoResponsesRequest(_ context.Context, body io.Reader, _ int64) (*http.Response, error) {
	if err := json.NewDecoder(body).Decode(&m.nativeBody); err != nil {
		return nil, err
	}
	return jsonHTTPResponse(http.StatusOK, "application/json", `{
		"id":"resp-native",
		"object":"response",
		"created_at":1700000000,
		"status":"completed",
		"model":"provider-model",
		"parallel_tool_calls":true,
		"output":[{
			"id":"msg-native",
			"type":"message",
			"status":"completed",
			"role":"assistant",
			"content":[{"type":"output_text","text":"native ok","annotations":[]}]
		}],
		"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}
	}`), nil
}

func jsonHTTPResponse(status int, contentType, body string) *http.Response {
	return bytesHTTPResponse(status, contentType, []byte(body))
}

func bytesHTTPResponse(status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func openFilesTestStore(t *testing.T) store.Store {
	t.Helper()
	db, err := store.Open(config.DatabaseConfig{
		Driver: "sqlite",
		URL:    filepath.Join(t.TempDir(), "files.db"),
	})
	require.NoError(t, err)
	require.NoError(t, db.Migrate(context.Background()))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func authenticatedFileRequest(req *http.Request, teamID string) *http.Request {
	return req.WithContext(auth.ContextWithKeyInfo(req.Context(), &store.APIKey{
		Budget:  100,
		KeyHash: "key-hash",
		TeamID:  teamID,
	}))
}

func uploadTestFile(t *testing.T, handler *FilesHandler, teamID string) fileObject {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("purpose", "user_data"))
	part, err := writer.CreateFormFile("file", "assessment.pdf")
	require.NoError(t, err)
	_, err = part.Write([]byte("%PDF-1.7 assessment"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set(fileModelHeader, "azure-gpt-4o-mini")
	req = authenticatedFileRequest(req, teamID)
	w := httptest.NewRecorder()
	handler.ServeCollection(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var result fileObject
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	return result
}

func TestFilesHandler_UploadsToProviderAndStoresMetadataOnly(t *testing.T) {
	mock := &mockFileProvider{}
	registry := newTestRegistry("azure-gpt-4o-mini", mock)
	db := openFilesTestStore(t)
	handler := NewFilesHandler(registry, db, slog.Default())

	uploaded := uploadTestFile(t, handler, "tenant-a")
	assert.True(t, strings.HasPrefix(uploaded.ID, "file-mh-"))
	assert.Equal(t, "assessment.pdf", uploaded.Filename)
	assert.Contains(t, string(mock.uploadBody), "%PDF-1.7 assessment")

	fileStore := db.(store.FileStore)
	mapping, err := fileStore.GetProviderFile(context.Background(), uploaded.ID, "team:tenant-a")
	require.NoError(t, err)
	require.NotNil(t, mapping)
	assert.Equal(t, "file-upstream-123", mapping.UpstreamID)
	assert.Equal(t, int64(128), mapping.Bytes)

	// ProviderFile contains metadata only: there is deliberately no blob field.
	raw, err := json.Marshal(mapping)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "%PDF")

	otherTenant, err := fileStore.GetProviderFile(context.Background(), uploaded.ID, "team:tenant-b")
	require.NoError(t, err)
	assert.Nil(t, otherTenant)
}

func TestFilesHandler_ListRetrieveContentAndDelete(t *testing.T) {
	mock := &mockFileProvider{content: []byte("%PDF-provider-content")}
	registry := newTestRegistry("azure-gpt-4o-mini", mock)
	db := openFilesTestStore(t)
	handler := NewFilesHandler(registry, db, slog.Default())
	uploaded := uploadTestFile(t, handler, "tenant-a")

	listReq := authenticatedFileRequest(httptest.NewRequest(http.MethodGet, "/v1/files", nil), "tenant-a")
	listW := httptest.NewRecorder()
	handler.ServeCollection(listW, listReq)
	require.Equal(t, http.StatusOK, listW.Code)
	var list fileList
	require.NoError(t, json.Unmarshal(listW.Body.Bytes(), &list))
	require.Len(t, list.Data, 1)
	assert.Equal(t, uploaded.ID, list.Data[0].ID)

	getReq := authenticatedFileRequest(httptest.NewRequest(http.MethodGet, "/v1/files/"+uploaded.ID, nil), "tenant-a")
	getReq.SetPathValue("file_id", uploaded.ID)
	getW := httptest.NewRecorder()
	handler.ServeObject(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)

	contentReq := authenticatedFileRequest(httptest.NewRequest(http.MethodGet, "/v1/files/"+uploaded.ID+"/content", nil), "tenant-a")
	contentReq.SetPathValue("file_id", uploaded.ID)
	contentW := httptest.NewRecorder()
	handler.ServeContent(contentW, contentReq)
	require.Equal(t, http.StatusOK, contentW.Code)
	assert.Equal(t, "%PDF-provider-content", contentW.Body.String())

	deleteReq := authenticatedFileRequest(httptest.NewRequest(http.MethodDelete, "/v1/files/"+uploaded.ID, nil), "tenant-a")
	deleteReq.SetPathValue("file_id", uploaded.ID)
	deleteW := httptest.NewRecorder()
	handler.ServeObject(deleteW, deleteReq)
	require.Equal(t, http.StatusOK, deleteW.Code)

	fileStore := db.(store.FileStore)
	mapping, err := fileStore.GetProviderFile(context.Background(), uploaded.ID, "team:tenant-a")
	require.NoError(t, err)
	assert.Nil(t, mapping)
}

func TestResponsesHandler_ResolvesPDFFileIDAndPinsDeployment(t *testing.T) {
	mock := &mockFileProvider{
		content:     []byte("%PDF-provider-content"),
		contentType: "application/pdf",
	}
	registry := newTestRegistry("azure-gpt-4o-mini", mock)
	db := openFilesTestStore(t)
	filesHandler := NewFilesHandler(registry, db, slog.Default())
	uploaded := uploadTestFile(t, filesHandler, "tenant-a")

	handler := NewResponsesHandler(registry, nil, slog.Default(), db, nil, nil)
	body := `{
		"model":"azure-gpt-4o-mini",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_file","file_id":"` + uploaded.ID + `"},
				{"type":"input_text","text":"Riassumi il PDF"}
			]
		}]
	}`
	req := authenticatedFileRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)), "tenant-a")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, mock.lastCompletion)
	require.Len(t, mock.lastCompletion.Messages, 1)

	parts, ok := mock.lastCompletion.Messages[0].Content.([]interface{})
	require.True(t, ok)
	filePart := parts[0].(map[string]interface{})["file"].(map[string]interface{})
	assert.NotContains(t, filePart, "file_id")
	assert.Equal(t, "data:application/pdf;base64,JVBERi1wcm92aWRlci1jb250ZW50", filePart["file_data"])
	assert.Equal(t, "assessment.pdf", filePart["filename"])
}

func TestResponsesHandler_ForwardsUploadedFileToNativeResponsesAPI(t *testing.T) {
	mock := &mockNativeFileProvider{mockFileProvider: &mockFileProvider{}}
	registry := newTestRegistry("azure-gpt-4o-mini", mock)
	db := openFilesTestStore(t)
	filesHandler := NewFilesHandler(registry, db, slog.Default())
	uploaded := uploadTestFile(t, filesHandler, "tenant-a")

	handler := NewResponsesHandler(registry, nil, slog.Default(), db, nil, nil)
	body := `{
		"model":"azure-gpt-4o-mini",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_file","file_id":"` + uploaded.ID + `"},
				{"type":"input_text","text":"Riassumi il PDF"}
			]
		}]
	}`
	req := authenticatedFileRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)), "tenant-a")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	input := mock.nativeBody["input"].([]interface{})
	content := input[0].(map[string]interface{})["content"].([]interface{})
	filePart := content[0].(map[string]interface{})
	assert.Equal(t, "file-upstream-123", filePart["file_id"])

	var response map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, "azure-gpt-4o-mini", response["model"])
	assert.Equal(t, "mock", w.Header().Get("X-Ubiquum-Provider"))
}

func TestCompletionHandler_ResolvesChatFileIDAndEnforcesTenantScope(t *testing.T) {
	mock := &mockFileProvider{
		content:     []byte("%PDF-provider-content"),
		contentType: "application/pdf",
	}
	registry := newTestRegistry("azure-gpt-4o-mini", mock)
	db := openFilesTestStore(t)
	filesHandler := NewFilesHandler(registry, db, slog.Default())
	uploaded := uploadTestFile(t, filesHandler, "tenant-a")

	handler := NewCompletionHandler(registry, nil, slog.Default(), db, nil, nil)
	body := `{
		"model":"azure-gpt-4o-mini",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"file","file":{"file_id":"` + uploaded.ID + `"}},
				{"type":"text","text":"Riassumi il PDF"}
			]
		}]
	}`

	otherTenantReq := authenticatedFileRequest(
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)),
		"tenant-b",
	)
	otherTenantW := httptest.NewRecorder()
	handler.ServeHTTP(otherTenantW, otherTenantReq)
	require.Equal(t, http.StatusBadRequest, otherTenantW.Code)
	assert.Contains(t, otherTenantW.Body.String(), "not found or is not accessible")

	ownerReq := authenticatedFileRequest(
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)),
		"tenant-a",
	)
	ownerW := httptest.NewRecorder()
	handler.ServeHTTP(ownerW, ownerReq)
	require.Equal(t, http.StatusOK, ownerW.Code, ownerW.Body.String())
	require.NotNil(t, mock.lastCompletion)

	parts := mock.lastCompletion.Messages[0].Content.([]interface{})
	filePart := parts[0].(map[string]interface{})["file"].(map[string]interface{})
	assert.NotContains(t, filePart, "file_id")
	assert.Equal(t, "data:application/pdf;base64,JVBERi1wcm92aWRlci1jb250ZW50", filePart["file_data"])
	assert.Equal(t, "assessment.pdf", filePart["filename"])
}

func TestResponsesHandler_ResolvesUploadedImageToDataURI(t *testing.T) {
	mock := &mockFileProvider{
		content:     []byte("PNGDATA"),
		contentType: "image/png",
	}
	registry := newTestRegistry("azure-gpt-4o-mini", mock)
	db := openFilesTestStore(t)
	fileStore := db.(store.FileStore)
	require.NoError(t, fileStore.CreateProviderFile(context.Background(), &store.ProviderFile{
		ID:           "file-mh-image",
		OwnerID:      "team:tenant-a",
		Model:        "azure-gpt-4o-mini",
		DeploymentID: "azure-gpt-4o-mini-0",
		UpstreamID:   "file-upstream-image",
		Filename:     "diagram.png",
		Purpose:      "vision",
		MimeType:     "image/png",
		Bytes:        7,
		CreatedAt:    time.Now(),
	}))

	handler := NewResponsesHandler(registry, nil, slog.Default(), db, nil, nil)
	body := `{
		"model":"azure-gpt-4o-mini",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_image","file_id":"file-mh-image"},
				{"type":"input_text","text":"Descrivi l'immagine"}
			]
		}]
	}`
	req := authenticatedFileRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)), "tenant-a")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	parts := mock.lastCompletion.Messages[0].Content.([]interface{})
	imagePart := parts[0].(map[string]interface{})["image_url"].(map[string]interface{})
	assert.Equal(t, "data:image/png;base64,UE5HREFUQQ==", imagePart["url"])
	assert.NotContains(t, imagePart, "file_id")
}

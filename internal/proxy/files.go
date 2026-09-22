package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const (
	fileModelHeader                   = "X-Ubiquum-Model"
	providerFilesPath                 = "/files/"
	fileMetadataStoreUnavailableError = "file metadata store is unavailable"
)

// FilesHandler exposes the OpenAI-compatible Files API while keeping file
// contents provider-side. Ubiquum persists only a tenant-scoped id mapping.
type FilesHandler struct {
	registry *provider.Registry
	store    store.Store
	logger   *slog.Logger
}

// NewFilesHandler creates a Files API handler.
func NewFilesHandler(registry *provider.Registry, db store.Store, logger *slog.Logger) *FilesHandler {
	return &FilesHandler{registry: registry, store: db, logger: logger}
}

type upstreamFileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

func decodeUpstreamFileObject(body io.Reader) (*upstreamFileObject, error) {
	payload, err := io.ReadAll(io.LimitReader(body, 2<<20))
	if err != nil {
		return nil, errors.New("failed to read provider file response")
	}

	var upstream upstreamFileObject
	if err := json.Unmarshal(payload, &upstream); err != nil || upstream.ID == "" {
		return nil, errors.New("provider returned an invalid file object")
	}
	return &upstream, nil
}

type fileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	Status    string `json:"status,omitempty"`
}

type fileList struct {
	Object  string       `json:"object"`
	Data    []fileObject `json:"data"`
	FirstID string       `json:"first_id,omitempty"`
	LastID  string       `json:"last_id,omitempty"`
	HasMore bool         `json:"has_more"`
}

// ServeCollection handles POST /v1/files and GET /v1/files.
func (h *FilesHandler) ServeCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.create(w, r)
	case http.MethodGet:
		h.list(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ServeObject handles GET/DELETE /v1/files/{file_id}.
func (h *FilesHandler) ServeObject(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.retrieve(w, r)
	case http.MethodDelete:
		h.delete(w, r)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// ServeContent handles GET /v1/files/{file_id}/content.
func (h *FilesHandler) ServeContent(w http.ResponseWriter, r *http.Request) {
	file, dep, fileAPI, ok := h.resolveFile(w, r)
	if !ok {
		return
	}

	resp, err := fileAPI.DoFileRequest(r.Context(), provider.FileRequest{
		Method:        http.MethodGet,
		Path:          providerFilesPath + url.PathEscape(file.UpstreamID) + "/content",
		ContentLength: 0,
	})
	if err != nil {
		h.logger.Error("provider file content request failed", "file_id", file.ID, "deployment", dep.ID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayProviderFileResponse(w, resp)
		return
	}

	copyFileResponseHeaders(w.Header(), resp.Header)
	if w.Header().Get(headerContentType) == "" && file.MimeType != "" {
		w.Header().Set(headerContentType, file.MimeType)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *FilesHandler) create(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.Header.Get(fileModelHeader))
	if model == "" {
		writeError(
			w,
			http.StatusBadRequest,
			"invalid_request_error",
			"X-Ubiquum-Model is required for file uploads so Ubiquum can select the provider",
		)
		return
	}
	if !auth.IsModelAllowed(r.Context(), model, h.registry.IsRestricted(model)) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied for model %q", model))
		return
	}
	if !auth.IsProviderAllowed(r.Context(), h.registry.ProvidersFor(model)) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied for the provider serving model %q", model))
		return
	}
	if allEU, hasDeployments := h.registry.AllDeploymentsEU(model); !auth.IsResidencyAllowed(r.Context(), allEU, hasDeployments) {
		writeError(w, http.StatusForbidden, "access_denied", fmt.Sprintf("access denied: model %q is not available in an EU-only deployment", model))
		return
	}
	// A key that cannot pay — never funded, or funded and spent — may still
	// call a model the gateway does not pay for. Anything it does pay for needs
	// a budget with something left in it. Checked here, where the model is
	// known and authentication could not see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(model)); status != auth.BudgetOK {
		writeBudgetError(w, status, model)
		return
	}

	ownerID := fileOwnerID(r.Context())
	fileStore, ok := h.store.(store.FileStore)
	if !ok || ownerID == "" {
		writeError(w, http.StatusInternalServerError, "server_error", fileMetadataStoreUnavailableError)
		return
	}

	dep, err := getAuthorizedDeployment(r.Context(), h.registry, model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", model))
		return
	}
	fileAPI, ok := dep.Provider.(provider.FileAPI)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("provider for model %q does not support the Files API", model))
		return
	}

	resp, err := fileAPI.DoFileRequest(r.Context(), provider.FileRequest{
		Method:        http.MethodPost,
		Path:          "/files",
		ContentType:   r.Header.Get(headerContentType),
		ContentLength: r.ContentLength,
		Body:          r.Body,
	})
	if err != nil {
		h.logger.Error("provider file upload failed", "model", model, "deployment", dep.ID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		relayProviderFileResponse(w, resp)
		return
	}

	upstream, err := decodeUpstreamFileObject(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	createdAt := time.Unix(upstream.CreatedAt, 0)
	if upstream.CreatedAt == 0 {
		createdAt = time.Now()
	}
	var expiresAt *time.Time
	if upstream.ExpiresAt != nil {
		value := time.Unix(*upstream.ExpiresAt, 0)
		expiresAt = &value
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(upstream.Filename)))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	file := &store.ProviderFile{
		ID:           "file-mh-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		OwnerID:      ownerID,
		Model:        model,
		DeploymentID: dep.ID,
		UpstreamID:   upstream.ID,
		Filename:     upstream.Filename,
		Purpose:      upstream.Purpose,
		MimeType:     mimeType,
		Bytes:        upstream.Bytes,
		CreatedAt:    createdAt,
		ExpiresAt:    expiresAt,
	}
	if err := fileStore.CreateProviderFile(r.Context(), file); err != nil {
		h.cleanupUpstreamFile(r.Context(), fileAPI, upstream.ID)
		h.logger.Error("failed to persist provider file mapping", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "failed to persist file metadata")
		return
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	w.Header().Set(headerUbiquumProvider, dep.ProviderName)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(toFileObject(file))
}

func (h *FilesHandler) list(w http.ResponseWriter, r *http.Request) {
	fileStore, ok := h.store.(store.FileStore)
	ownerID := fileOwnerID(r.Context())
	if !ok || ownerID == "" {
		writeError(w, http.StatusInternalServerError, "server_error", fileMetadataStoreUnavailableError)
		return
	}

	limit := 10000
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 10000 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "limit must be between 1 and 10000")
			return
		}
		limit = value
	}
	order := strings.ToLower(r.URL.Query().Get("order"))
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "order must be asc or desc")
		return
	}

	files, err := fileStore.ListProviderFiles(r.Context(), ownerID, store.ProviderFileFilter{
		Purpose: r.URL.Query().Get("purpose"),
		After:   r.URL.Query().Get("after"),
		Order:   order,
		Limit:   limit + 1,
	})
	if err != nil {
		h.logger.Error("failed to list provider file mappings", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "failed to list files")
		return
	}

	hasMore := len(files) > limit
	if hasMore {
		files = files[:limit]
	}
	result := fileList{Object: "list", Data: make([]fileObject, 0, len(files)), HasMore: hasMore}
	for i := range files {
		result.Data = append(result.Data, toFileObject(&files[i]))
	}
	if len(result.Data) > 0 {
		result.FirstID = result.Data[0].ID
		result.LastID = result.Data[len(result.Data)-1].ID
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(result)
}

func (h *FilesHandler) retrieve(w http.ResponseWriter, r *http.Request) {
	file, _, _, ok := h.resolveFile(w, r)
	if !ok {
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(toFileObject(file))
}

func (h *FilesHandler) delete(w http.ResponseWriter, r *http.Request) {
	file, dep, fileAPI, ok := h.resolveFile(w, r)
	if !ok {
		return
	}

	resp, err := fileAPI.DoFileRequest(r.Context(), provider.FileRequest{
		Method:        http.MethodDelete,
		Path:          providerFilesPath + url.PathEscape(file.UpstreamID),
		ContentLength: 0,
	})
	if err != nil {
		h.logger.Error("provider file delete failed", "file_id", file.ID, "deployment", dep.ID, "error", err)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		relayProviderFileResponse(w, resp)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	fileStore := h.store.(store.FileStore)
	if err := fileStore.DeleteProviderFile(r.Context(), file.ID, file.OwnerID); err != nil {
		h.logger.Error("failed to delete provider file mapping", "file_id", file.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "failed to delete file metadata")
		return
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":      file.ID,
		"object":  "file",
		"deleted": true,
	})
}

func (h *FilesHandler) resolveFile(w http.ResponseWriter, r *http.Request) (*store.ProviderFile, *provider.Deployment, provider.FileAPI, bool) {
	fileStore, ok := h.store.(store.FileStore)
	ownerID := fileOwnerID(r.Context())
	if !ok || ownerID == "" {
		writeError(w, http.StatusInternalServerError, "server_error", fileMetadataStoreUnavailableError)
		return nil, nil, nil, false
	}

	fileID := r.PathValue("file_id")
	file, err := fileStore.GetProviderFile(r.Context(), fileID, ownerID)
	if err != nil {
		h.logger.Error("failed to resolve provider file mapping", "file_id", fileID, "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "failed to resolve file")
		return nil, nil, nil, false
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("file %q not found", fileID))
		return nil, nil, nil, false
	}

	dep, err := getAuthorizedDeploymentByID(r.Context(), h.registry, file.Model, file.DeploymentID)
	if err != nil {
		writeError(w, http.StatusGone, "invalid_request_error", "the provider deployment for this file is no longer available")
		return nil, nil, nil, false
	}
	fileAPI, ok := dep.Provider.(provider.FileAPI)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the provider for this file no longer supports the Files API")
		return nil, nil, nil, false
	}
	return file, dep, fileAPI, true
}

func (h *FilesHandler) cleanupUpstreamFile(ctx context.Context, fileAPI provider.FileAPI, upstreamID string) {
	resp, err := fileAPI.DoFileRequest(ctx, provider.FileRequest{
		Method:        http.MethodDelete,
		Path:          providerFilesPath + url.PathEscape(upstreamID),
		ContentLength: 0,
	})
	if err == nil && resp != nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}

// fileOwnerID is auth.OwnerID under the name this package has always used.
// The derivation lives in auth so the workflow middleware, which persists
// short-circuited turns into the same tables, cannot drift from it.
func fileOwnerID(ctx context.Context) string { return auth.OwnerID(ctx) }

func toFileObject(file *store.ProviderFile) fileObject {
	result := fileObject{
		ID:        file.ID,
		Object:    "file",
		Bytes:     file.Bytes,
		CreatedAt: file.CreatedAt.Unix(),
		Filename:  file.Filename,
		Purpose:   file.Purpose,
		Status:    "processed",
	}
	if file.ExpiresAt != nil {
		value := file.ExpiresAt.Unix()
		result.ExpiresAt = &value
	}
	return result
}

func relayProviderFileResponse(w http.ResponseWriter, resp *http.Response) {
	copyFileResponseHeaders(w.Header(), resp.Header)
	if w.Header().Get(headerContentType) == "" {
		w.Header().Set(headerContentType, contentTypeJSON)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func copyFileResponseHeaders(dst, src http.Header) {
	for _, name := range []string{headerContentType, "Content-Length", "Content-Disposition"} {
		if value := src.Get(name); value != "" {
			dst.Set(name, value)
		}
	}
}

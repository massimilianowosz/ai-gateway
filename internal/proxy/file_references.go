package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/store"
)

const maxInlineProviderFileBytes = int64(32 << 20)

// resolveFileReferences resolves tenant-scoped Ubiquum file ids and pins the
// request to the deployment that owns them. Provider file contents are fetched
// on demand and converted to data URIs because the Gateway translates Responses
// requests to Chat Completions, where provider-side file ids are not portable.
// Bytes exist only in request memory and are never persisted by Ubiquum.
func resolveFileReferences(
	r *http.Request,
	db store.Store,
	registry *provider.Registry,
	req *provider.CompletionRequest,
) error {
	if db == nil {
		return nil
	}
	fileStore, ok := db.(store.FileStore)
	if !ok {
		return nil
	}
	ownerID := fileOwnerID(r.Context())

	for messageIndex := range req.Messages {
		parts, ok := req.Messages[messageIndex].Content.([]interface{})
		if !ok {
			continue
		}
		if err := resolveMessageFileReferences(r, fileStore, registry, ownerID, req, parts); err != nil {
			return err
		}
	}
	return nil
}

func resolveMessageFileReferences(
	r *http.Request,
	fileStore store.FileStore,
	registry *provider.Registry,
	ownerID string,
	req *provider.CompletionRequest,
	parts []interface{},
) error {
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]interface{})
		if !ok {
			continue
		}
		if err := resolveContentPartFileReference(r, fileStore, registry, ownerID, req, part); err != nil {
			return err
		}
	}
	return nil
}

func resolveContentPartFileReference(
	r *http.Request,
	fileStore store.FileStore,
	registry *provider.Registry,
	ownerID string,
	req *provider.CompletionRequest,
	part map[string]interface{},
) error {
	partType, _ := part["type"].(string)
	switch partType {
	case "file":
		fileValue, _ := part["file"].(map[string]interface{})
		return resolveFileContentPart(r, fileStore, registry, ownerID, req, fileValue)
	case "image_url":
		imageValue, _ := part["image_url"].(map[string]interface{})
		return resolveImageContentPart(r, fileStore, registry, ownerID, req, imageValue)
	default:
		return nil
	}
}

func resolveFileContentPart(
	r *http.Request,
	fileStore store.FileStore,
	registry *provider.Registry,
	ownerID string,
	req *provider.CompletionRequest,
	fileValue map[string]interface{},
) error {
	fileID, _ := fileValue["file_id"].(string)
	if fileID == "" {
		return nil
	}
	mapping, err := resolveProviderFile(r.Context(), fileStore, ownerID, fileID, req.Model)
	if err != nil {
		return err
	}
	if err := pinFileDeployment(req, mapping.DeploymentID); err != nil {
		return err
	}
	dataURI, err := providerFileDataURI(r, registry, mapping, false)
	if err != nil {
		return err
	}
	delete(fileValue, "file_id")
	fileValue["file_data"] = dataURI
	if _, ok := fileValue["filename"]; !ok && mapping.Filename != "" {
		fileValue["filename"] = mapping.Filename
	}
	return nil
}

func resolveImageContentPart(
	r *http.Request,
	fileStore store.FileStore,
	registry *provider.Registry,
	ownerID string,
	req *provider.CompletionRequest,
	imageValue map[string]interface{},
) error {
	fileID, _ := imageValue["file_id"].(string)
	if fileID == "" {
		return nil
	}
	mapping, err := resolveProviderFile(r.Context(), fileStore, ownerID, fileID, req.Model)
	if err != nil {
		return err
	}
	if err := pinFileDeployment(req, mapping.DeploymentID); err != nil {
		return err
	}
	dataURI, err := providerFileDataURI(r, registry, mapping, true)
	if err != nil {
		return err
	}
	delete(imageValue, "file_id")
	imageValue["url"] = dataURI
	return nil
}

func resolveProviderFile(ctx context.Context, fileStore store.FileStore, ownerID, fileID, model string) (*store.ProviderFile, error) {
	if ownerID == "" {
		return nil, fmt.Errorf("file references require an authenticated Ubiquum identity")
	}
	file, err := fileStore.GetProviderFile(ctx, fileID, ownerID)
	if err != nil {
		return nil, fmt.Errorf("resolve file %q: %w", fileID, err)
	}
	if file == nil {
		return nil, fmt.Errorf("file %q was not found or is not accessible to this tenant", fileID)
	}
	if file.Model != model {
		return nil, fmt.Errorf("file %q belongs to model %q and cannot be used with model %q", fileID, file.Model, model)
	}
	return file, nil
}

func pinFileDeployment(req *provider.CompletionRequest, deploymentID string) error {
	if req.PinnedDeploymentID != "" && req.PinnedDeploymentID != deploymentID {
		return fmt.Errorf("all uploaded files in one request must belong to the same provider deployment")
	}
	req.PinnedDeploymentID = deploymentID
	return nil
}

func providerFileDataURI(
	r *http.Request,
	registry *provider.Registry,
	file *store.ProviderFile,
	requireImage bool,
) (string, error) {
	if file.Bytes > maxInlineProviderFileBytes {
		return "", fmt.Errorf("file %q exceeds the %d MB inline limit", file.ID, maxInlineProviderFileBytes>>20)
	}
	dep, err := getAuthorizedDeploymentByID(r.Context(), registry, file.Model, file.DeploymentID)
	if err != nil {
		return "", fmt.Errorf("provider deployment for file %q is unavailable", file.ID)
	}
	fileAPI, ok := dep.Provider.(provider.FileAPI)
	if !ok {
		return "", fmt.Errorf("provider for file %q does not support content retrieval", file.ID)
	}

	resp, err := fileAPI.DoFileRequest(r.Context(), provider.FileRequest{
		Method:        http.MethodGet,
		Path:          "/files/" + url.PathEscape(file.UpstreamID) + "/content",
		ContentLength: 0,
	})
	if err != nil {
		return "", fmt.Errorf("retrieve file %q: %w", file.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("retrieve file %q: provider returned HTTP %d: %s", file.ID, resp.StatusCode, strings.TrimSpace(string(message)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInlineProviderFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read file %q: %w", file.ID, err)
	}
	if int64(len(data)) > maxInlineProviderFileBytes {
		return "", fmt.Errorf("file %q exceeds the %d MB inline limit", file.ID, maxInlineProviderFileBytes>>20)
	}

	mimeType := file.MimeType
	if upstreamType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]); upstreamType != "" {
		mimeType = upstreamType
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(file.Filename)))
	}
	if requireImage && !strings.HasPrefix(mimeType, "image/") {
		return "", fmt.Errorf("file %q is %q, not an image", file.ID, mimeType)
	}

	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	qdrantCollectionsPath  = "/collections/"
	qdrantPointsDeletePath = "/points/delete"
)

// QdrantStore uses Qdrant's HTTP API for vector storage and similarity search.
type QdrantStore struct {
	baseURL    string
	collection string
	client     *http.Client
	dim        int
}

// NewQdrantStore creates a Qdrant-backed store. Creates the collection if it doesn't exist.
func NewQdrantStore(baseURL, collection string, dim int) (*QdrantStore, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	parsedURL, err := url.ParseRequestURI(baseURL)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return nil, fmt.Errorf("qdrant: invalid URL %q", baseURL)
	}
	if collection == "" {
		return nil, fmt.Errorf("qdrant: collection is required")
	}
	if dim <= 0 {
		return nil, fmt.Errorf("qdrant: vector dimension must be positive")
	}

	s := &QdrantStore{
		baseURL:    baseURL,
		collection: collection,
		client:     &http.Client{Timeout: 10 * time.Second},
		dim:        dim,
	}
	if err := s.ensureCollection(); err != nil {
		return nil, fmt.Errorf("qdrant: ensure collection: %w", err)
	}
	return s, nil
}

func (s *QdrantStore) Search(ctx context.Context, teamID, model string, queryEmbedding []float32, topK int) ([]Entry, error) {
	if err := s.validateVector(queryEmbedding); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return nil, fmt.Errorf("qdrant: topK must be positive")
	}

	body := map[string]any{
		"vector": queryEmbedding,
		"limit":  topK,
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": "team_id", "match": map[string]any{"value": teamID}},
				{"key": "model", "match": map[string]any{"value": model}},
			},
		},
		"with_payload": true,
	}

	resp, err := s.doJSON(ctx, "POST", s.collectionPath("/points/search"), body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Result []struct {
			ID      string         `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("qdrant: parse response: %w", err)
	}

	var entries []Entry
	for _, r := range result.Result {
		e := Entry{
			ID:          r.ID,
			TeamID:      getString(r.Payload, "team_id"),
			Model:       getString(r.Payload, "model"),
			Messages:    getString(r.Payload, "messages"),
			Response:    getString(r.Payload, "response"),
			TotalTokens: getInt(r.Payload, "total_tokens"),
			Meta:        getString(r.Payload, "meta"),
		}
		e.QueryEmbedding = getFloat32Slice(r.Payload, "query_embedding")
		e.CtxEmbedding = getFloat32Slice(r.Payload, "ctx_embedding")
		if t, err := time.Parse(time.RFC3339, getString(r.Payload, "created_at")); err == nil {
			e.CreatedAt = t
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *QdrantStore) Insert(ctx context.Context, entry Entry) error {
	if err := s.validateVector(entry.QueryEmbedding); err != nil {
		return err
	}

	point := map[string]any{
		"id":     entry.ID,
		"vector": entry.QueryEmbedding,
		"payload": map[string]any{
			"team_id":         entry.TeamID,
			"model":           entry.Model,
			"messages":        entry.Messages,
			"response":        entry.Response,
			"total_tokens":    entry.TotalTokens,
			"meta":            entry.Meta,
			"query_embedding": entry.QueryEmbedding,
			"ctx_embedding":   entry.CtxEmbedding,
			"created_at":      entry.CreatedAt.Format(time.RFC3339),
		},
	}
	body := map[string]any{"points": []any{point}}
	_, err := s.doJSON(ctx, "PUT", s.collectionPath("/points"), body)
	return err
}

func (s *QdrantStore) Delete(ctx context.Context, id string) error {
	body := map[string]any{
		"points": []string{id},
	}
	_, err := s.doJSON(ctx, "POST", s.collectionPath(qdrantPointsDeletePath), body)
	return err
}

func (s *QdrantStore) Evict(ctx context.Context, maxAge time.Duration, maxEntries int) error {
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339)
	body := map[string]any{
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": "created_at", "range": map[string]any{"lt": cutoff}},
			},
		},
	}
	if _, err := s.doJSON(ctx, "POST", s.collectionPath(qdrantPointsDeletePath), body); err != nil {
		return err
	}
	return s.trimToMaxEntries(ctx, maxEntries)
}

func (s *QdrantStore) Count(ctx context.Context) (int, error) {
	resp, err := s.doJSON(ctx, "POST", s.collectionPath("/points/count"), map[string]any{"exact": true})
	if err != nil {
		return 0, err
	}
	var result struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return 0, fmt.Errorf("qdrant: parse count response: %w", err)
	}
	return result.Result.Count, nil
}

func (s *QdrantStore) Flush(ctx context.Context) error {
	// Delete and recreate the collection
	if _, err := s.doJSON(ctx, "DELETE", s.collectionPath(""), nil); err != nil {
		return err
	}
	return s.ensureCollection()
}

func (s *QdrantStore) Close() error { return nil }

func (s *QdrantStore) ensureCollection() error {
	// Check if collection exists
	req, err := http.NewRequest("GET", s.baseURL+s.collectionPath(""), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if resp.StatusCode == 200 {
		return s.ensurePayloadIndexes()
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("qdrant GET collection: %d %s", resp.StatusCode, string(body))
	}

	// Create collection
	createBody := map[string]any{
		"vectors": map[string]any{
			"size":     s.dim,
			"distance": "Cosine",
		},
	}
	if _, err = s.doJSON(context.Background(), "PUT", s.collectionPath(""), createBody); err != nil {
		return err
	}
	return s.ensurePayloadIndexes()
}

func (s *QdrantStore) doJSON(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var requestBody io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("qdrant: encode request: %w", err)
		}
		requestBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, requestBody)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qdrant %s %s: %d %s", method, path, resp.StatusCode, string(body))
	}
	return body, nil
}

func (s *QdrantStore) ensurePayloadIndexes() error {
	indexes := []struct {
		field  string
		schema string
	}{
		{field: "team_id", schema: "keyword"},
		{field: "model", schema: "keyword"},
		{field: "created_at", schema: "datetime"},
	}
	for _, index := range indexes {
		body := map[string]any{
			"field_name":   index.field,
			"field_schema": index.schema,
		}
		if _, err := s.doJSON(context.Background(), "PUT", s.collectionPath("/index"), body); err != nil {
			return fmt.Errorf("qdrant: create %s payload index: %w", index.field, err)
		}
	}
	return nil
}

func (s *QdrantStore) trimToMaxEntries(ctx context.Context, maxEntries int) error {
	if maxEntries < 0 {
		return nil
	}
	count, err := s.Count(ctx)
	if err != nil {
		return err
	}
	if count <= maxEntries {
		return nil
	}

	excess := count - maxEntries
	body := map[string]any{
		"limit": excess,
		"order_by": map[string]any{
			"key":       "created_at",
			"direction": "asc",
		},
		"with_payload": false,
		"with_vector":  false,
	}
	resp, err := s.doJSON(ctx, "POST", s.collectionPath("/points/scroll"), body)
	if err != nil {
		return err
	}
	var result struct {
		Result struct {
			Points []struct {
				ID json.RawMessage `json:"id"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("qdrant: parse scroll response: %w", err)
	}
	if len(result.Result.Points) == 0 {
		return fmt.Errorf("qdrant: cannot trim %d excess entries: scroll returned no points", excess)
	}

	ids := make([]json.RawMessage, 0, len(result.Result.Points))
	for _, point := range result.Result.Points {
		if len(point.ID) > 0 && json.Valid(point.ID) {
			ids = append(ids, point.ID)
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("qdrant: cannot trim entries: scroll returned invalid point IDs")
	}
	_, err = s.doJSON(ctx, "POST", s.collectionPath(qdrantPointsDeletePath), map[string]any{"points": ids})
	return err
}

func (s *QdrantStore) collectionPath(suffix string) string {
	return qdrantCollectionsPath + s.collection + suffix
}

func (s *QdrantStore) validateVector(vector []float32) error {
	if len(vector) != s.dim {
		return fmt.Errorf("qdrant: vector dimension %d does not match collection dimension %d", len(vector), s.dim)
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func getFloat32Slice(m map[string]any, key string) []float32 {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]float32, len(arr))
	for i, v := range arr {
		if f, ok := v.(float64); ok {
			out[i] = float32(f)
		}
	}
	return out
}

package cache

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type qdrantRecordedRequest struct {
	method string
	path   string
	body   map[string]any
}

type qdrantRecorder struct {
	mu       sync.Mutex
	requests []qdrantRecordedRequest
}

func (r *qdrantRecorder) record(req *http.Request) {
	var body map[string]any
	data, _ := io.ReadAll(req.Body)
	if len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}
	r.mu.Lock()
	r.requests = append(r.requests, qdrantRecordedRequest{
		method: req.Method,
		path:   req.URL.Path,
		body:   body,
	})
	r.mu.Unlock()
}

func (r *qdrantRecorder) snapshot() []qdrantRecordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]qdrantRecordedRequest(nil), r.requests...)
}

func writeQdrantJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func newQdrantCollectionSetupServer(t *testing.T, collectionExists bool) (*httptest.Server, *qdrantRecorder) {
	t.Helper()
	recorder := &qdrantRecorder{}
	server := httptest.NewServer(qdrantCollectionSetupHandler(recorder, collectionExists))
	t.Cleanup(server.Close)
	return server, recorder
}

func qdrantCollectionSetupHandler(recorder *qdrantRecorder, collectionExists bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/collections/cache":
			if collectionExists {
				writeQdrantJSON(w, http.StatusOK, `{"result":true}`)
				return
			}
			writeQdrantJSON(w, http.StatusNotFound, `{"status":"not found"}`)
		case req.Method == http.MethodPut && req.URL.Path == "/collections/cache":
			writeQdrantJSON(w, http.StatusOK, `{"result":true}`)
		case req.Method == http.MethodPut && req.URL.Path == "/collections/cache/index":
			writeQdrantJSON(w, http.StatusOK, `{"result":true}`)
		default:
			writeQdrantJSON(w, http.StatusNotFound, `{}`)
		}
	})
}

func assertQdrantCollectionSetup(t *testing.T, requests []qdrantRecordedRequest, collectionExists bool) {
	t.Helper()
	var collectionCreates int
	indexes := map[string]string{}
	for _, req := range requests {
		if req.method == http.MethodPut && req.path == "/collections/cache" {
			collectionCreates++
			vectors := req.body["vectors"].(map[string]any)
			assert.Equal(t, float64(3), vectors["size"])
			assert.Equal(t, "Cosine", vectors["distance"])
		}
		if req.method == http.MethodPut && req.path == "/collections/cache/index" {
			indexes[req.body["field_name"].(string)] = req.body["field_schema"].(string)
		}
	}
	if collectionExists {
		assert.Zero(t, collectionCreates)
	} else {
		assert.Equal(t, 1, collectionCreates)
	}
	assert.Equal(t, map[string]string{
		"team_id":    "keyword",
		"model":      "keyword",
		"created_at": "datetime",
	}, indexes)
}

func TestNewQdrantStoreCreatesCollectionAndPayloadIndexes(t *testing.T) {
	for _, collectionExists := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing", false: "missing"}[collectionExists], func(t *testing.T) {
			server, recorder := newQdrantCollectionSetupServer(t, collectionExists)
			store, err := NewQdrantStore(server.URL+"/", "cache", 3)
			require.NoError(t, err)
			assert.Equal(t, server.URL, store.baseURL)
			assert.NoError(t, store.Close())
			assertQdrantCollectionSetup(t, recorder.snapshot(), collectionExists)
		})
	}
}

func TestNewQdrantStoreValidationAndSetupErrors(t *testing.T) {
	_, err := NewQdrantStore("://invalid", "cache", 3)
	require.ErrorContains(t, err, "invalid URL")
	_, err = NewQdrantStore("http://localhost", "", 3)
	require.ErrorContains(t, err, "collection is required")
	_, err = NewQdrantStore("http://localhost", "cache", 0)
	require.ErrorContains(t, err, "dimension must be positive")

	for _, tc := range []struct {
		name       string
		getStatus  int
		putStatus  int
		indexError bool
		want       string
	}{
		{name: "collection check", getStatus: http.StatusUnauthorized, want: "GET collection"},
		{name: "collection create", getStatus: http.StatusNotFound, putStatus: http.StatusInternalServerError, want: "PUT"},
		{name: "payload index", getStatus: http.StatusOK, indexError: true, want: "payload index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch {
				case req.Method == http.MethodGet:
					writeQdrantJSON(w, tc.getStatus, `{"status":"check"}`)
				case req.URL.Path == "/collections/cache/index" && tc.indexError:
					writeQdrantJSON(w, http.StatusInternalServerError, `{"status":"index failed"}`)
				default:
					status := tc.putStatus
					if status == 0 {
						status = http.StatusOK
					}
					writeQdrantJSON(w, status, `{"status":"write"}`)
				}
			}))
			defer server.Close()

			_, err := NewQdrantStore(server.URL, "cache", 3)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestQdrantStoreLifecycleAndBackendParity(t *testing.T) {
	recorder := &qdrantRecorder{}
	createdAt := time.Date(2026, 7, 24, 12, 30, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req)
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/collections/cache/points/search":
			writeQdrantJSON(w, http.StatusOK, `{
				"result":[{
					"id":"point-1",
					"score":0.99,
					"payload":{
						"team_id":"team-a",
						"model":"model-a",
						"messages":"messages",
						"response":"response",
						"total_tokens":42,
						"meta":"intent:test",
						"query_embedding":[1,0,0],
						"ctx_embedding":[0,1,0],
						"created_at":"2026-07-24T12:30:00Z"
					}
				}]
			}`)
		case req.Method == http.MethodPut && req.URL.Path == "/collections/cache/points":
			writeQdrantJSON(w, http.StatusOK, `{"result":"inserted"}`)
		case req.Method == http.MethodPost && req.URL.Path == "/collections/cache/points/count":
			writeQdrantJSON(w, http.StatusOK, `{"result":{"count":4}}`)
		case req.Method == http.MethodPost && req.URL.Path == "/collections/cache/points/scroll":
			writeQdrantJSON(w, http.StatusOK, `{"result":{"points":[{"id":"old-1"},{"id":2}]}}`)
		case req.Method == http.MethodPost && req.URL.Path == "/collections/cache/points/delete":
			writeQdrantJSON(w, http.StatusOK, `{"result":"deleted"}`)
		case req.Method == http.MethodDelete && req.URL.Path == "/collections/cache":
			writeQdrantJSON(w, http.StatusOK, `{"result":"collection deleted"}`)
		case req.Method == http.MethodGet && req.URL.Path == "/collections/cache":
			writeQdrantJSON(w, http.StatusOK, `{"result":true}`)
		case req.Method == http.MethodPut && req.URL.Path == "/collections/cache/index":
			writeQdrantJSON(w, http.StatusOK, `{"result":"indexed"}`)
		default:
			writeQdrantJSON(w, http.StatusNotFound, `{}`)
		}
	}))
	defer server.Close()

	store := &QdrantStore{
		baseURL:    server.URL,
		collection: "cache",
		client:     server.Client(),
		dim:        3,
	}
	ctx := context.Background()

	results, err := store.Search(ctx, "team-a", "model-a", []float32{1, 0, 0}, 5)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, Entry{
		ID:             "point-1",
		TeamID:         "team-a",
		Model:          "model-a",
		QueryEmbedding: []float32{1, 0, 0},
		CtxEmbedding:   []float32{0, 1, 0},
		Messages:       "messages",
		Response:       "response",
		TotalTokens:    42,
		Meta:           "intent:test",
		CreatedAt:      createdAt,
	}, results[0])

	entry := Entry{
		ID:             "point-new",
		TeamID:         "team-a",
		Model:          "model-a",
		QueryEmbedding: []float32{1, 0, 0},
		CtxEmbedding:   []float32{0, 1, 0},
		Messages:       "messages",
		Response:       "response",
		TotalTokens:    21,
		Meta:           "intent:inserted",
		CreatedAt:      createdAt,
	}
	require.NoError(t, store.Insert(ctx, entry))
	require.NoError(t, store.Delete(ctx, entry.ID))
	require.NoError(t, store.Evict(ctx, 24*time.Hour, 2))
	count, err := store.Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, count)
	require.NoError(t, store.Flush(ctx))

	requests := recorder.snapshot()
	var searchBody, insertBody, countBody, scrollBody map[string]any
	var deleteBodies []map[string]any
	var indexRequests int
	for _, req := range requests {
		switch req.path {
		case "/collections/cache/points/search":
			searchBody = req.body
		case "/collections/cache/points":
			insertBody = req.body
		case "/collections/cache/points/count":
			countBody = req.body
		case "/collections/cache/points/scroll":
			scrollBody = req.body
		case "/collections/cache/points/delete":
			deleteBodies = append(deleteBodies, req.body)
		case "/collections/cache/index":
			indexRequests++
		}
	}

	filter := searchBody["filter"].(map[string]any)
	must := filter["must"].([]any)
	assert.Len(t, must, 2)
	assert.Equal(t, float64(5), searchBody["limit"])

	points := insertBody["points"].([]any)
	payload := points[0].(map[string]any)["payload"].(map[string]any)
	assert.Equal(t, "intent:inserted", payload["meta"])
	assert.Equal(t, "team-a", payload["team_id"])

	assert.Equal(t, true, countBody["exact"])
	assert.Equal(t, float64(2), scrollBody["limit"])
	orderBy := scrollBody["order_by"].(map[string]any)
	assert.Equal(t, "created_at", orderBy["key"])
	assert.Equal(t, "asc", orderBy["direction"])
	require.Len(t, deleteBodies, 3)
	assert.NotNil(t, deleteBodies[0]["points"]) // explicit Delete
	assert.NotNil(t, deleteBodies[1]["filter"]) // TTL eviction
	assert.NotNil(t, deleteBodies[2]["points"]) // maxEntries trimming
	assert.Equal(t, 3, indexRequests)
}

func TestQdrantStoreValidationAndErrorPaths(t *testing.T) {
	store := &QdrantStore{baseURL: "http://localhost", collection: "cache", client: http.DefaultClient, dim: 3}
	ctx := context.Background()

	_, err := store.Search(ctx, "team", "model", []float32{1, 0}, 5)
	require.ErrorContains(t, err, "dimension")
	_, err = store.Search(ctx, "team", "model", []float32{1, 0, 0}, 0)
	require.ErrorContains(t, err, "topK")
	require.ErrorContains(t, store.Insert(ctx, Entry{QueryEmbedding: []float32{1}}), "dimension")

	_, err = store.doJSON(ctx, http.MethodPost, "/encode", map[string]any{"bad": make(chan int)})
	require.ErrorContains(t, err, "encode request")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/bad-json":
			writeQdrantJSON(w, http.StatusOK, `{`)
		case "/empty-scroll":
			writeQdrantJSON(w, http.StatusOK, `{"result":{"points":[]}}`)
		default:
			writeQdrantJSON(w, http.StatusInternalServerError, `{"status":"failed"}`)
		}
	}))
	defer server.Close()
	store.baseURL = server.URL
	store.client = server.Client()

	_, err = store.doJSON(ctx, http.MethodGet, "/server-error", nil)
	require.ErrorContains(t, err, "500")

	_, err = store.Search(ctx, "team", "model", []float32{1, 0, 0}, 5)
	require.ErrorContains(t, err, "500")
	require.ErrorContains(t, store.Insert(ctx, Entry{QueryEmbedding: []float32{1, 0, 0}}), "500")
	require.ErrorContains(t, store.Delete(ctx, "id"), "500")
	require.ErrorContains(t, store.Evict(ctx, time.Hour, 1), "500")
	_, err = store.Count(ctx)
	require.ErrorContains(t, err, "500")
	require.ErrorContains(t, store.Flush(ctx), "500")

	store.baseURL = server.URL
	_, err = store.doJSON(ctx, http.MethodGet, "/bad-json", nil)
	require.NoError(t, err)

	assert.Equal(t, "", getString(map[string]any{"key": 1}, "key"))
	assert.Equal(t, 3, getInt(map[string]any{"key": float64(3)}, "key"))
	assert.Zero(t, getInt(map[string]any{"key": "3"}, "key"))
	assert.Equal(t, []float32{1, 0, 3}, getFloat32Slice(map[string]any{
		"key": []any{float64(1), "invalid", float64(3)},
	}, "key"))
	assert.Nil(t, getFloat32Slice(map[string]any{"key": "invalid"}, "key"))
}

func TestQdrantStoreRejectsMalformedResponses(t *testing.T) {
	response := `{`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeQdrantJSON(w, http.StatusOK, response)
	}))
	defer server.Close()
	store := &QdrantStore{baseURL: server.URL, collection: "cache", client: server.Client(), dim: 3}

	_, err := store.Search(context.Background(), "team", "model", []float32{1, 0, 0}, 5)
	require.ErrorContains(t, err, "parse response")
	_, err = store.Count(context.Background())
	require.ErrorContains(t, err, "parse count")

	response = `{"result":{"count":2}}`
	err = store.trimToMaxEntries(context.Background(), 2)
	require.NoError(t, err)

	response = `{"result":{"count":3}}`
	// The count response is also returned to scroll and therefore has no
	// points, exercising the defensive empty-scroll branch.
	err = store.trimToMaxEntries(context.Background(), 2)
	require.ErrorContains(t, err, "scroll returned no points")
}

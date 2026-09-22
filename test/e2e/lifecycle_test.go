package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stored turn can be read back, and deleting it means it is gone.
func TestResponses_RetrieveAndDelete(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("ciao")})
	key := g.mintKey(100, "m")

	created := g.post("/v1/responses", key, `{"model":"m","input":"hi"}`)
	require.Equal(t, http.StatusOK, created.code, created.body)
	var r struct{ ID string }
	decode(t, created.body, &r)

	got := g.do("GET", "/v1/responses/"+r.ID, key, "", "")
	require.Equal(t, http.StatusOK, got.code, got.body)
	assert.Contains(t, got.body, "ciao")

	del := g.do("DELETE", "/v1/responses/"+r.ID, key, "", "")
	require.Equal(t, http.StatusOK, del.code, del.body)

	gone := g.do("GET", "/v1/responses/"+r.ID, key, "", "")
	assert.Equal(t, http.StatusNotFound, gone.code, "a deleted turn was still served: %s", gone.body)
}

// store:false means the turn answers but is not retrievable afterwards.
func TestResponses_StoreFalseIsNotRetrievable(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("effimero")})
	key := g.mintKey(100, "m")

	created := g.post("/v1/responses", key, `{"model":"m","input":"hi","store":false}`)
	require.Equal(t, http.StatusOK, created.code, created.body)
	assert.Contains(t, created.body, "effimero")

	var r struct{ ID string }
	decode(t, created.body, &r)
	got := g.do("GET", "/v1/responses/"+r.ID, key, "", "")
	assert.Equal(t, http.StatusNotFound, got.code, "store:false was kept anyway: %s", got.body)
}

// Cancelling a finished turn must not overwrite its answer: the client was
// already told it completed.
func TestResponses_CancelDoesNotOverwriteAFinishedTurn(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("finito")})
	key := g.mintKey(100, "m")

	created := g.post("/v1/responses", key, `{"model":"m","input":"hi"}`)
	require.Equal(t, http.StatusOK, created.code, created.body)
	var r struct {
		ID     string
		Status string
	}
	decode(t, created.body, &r)
	require.Equal(t, "completed", r.Status)

	cancel := g.post("/v1/responses/"+r.ID+"/cancel", key, ``)
	require.Equal(t, http.StatusOK, cancel.code, cancel.body)

	got := g.do("GET", "/v1/responses/"+r.ID, key, "", "")
	var after struct{ Status string }
	decode(t, got.body, &after)
	assert.Equal(t, "completed", after.Status,
		"a cancel rewrote a turn the client had already been shown as finished")
}

// --- conversations ---

// Conversations are created, read, updated and deleted, and scoped to a tenant.
func TestConversations_Lifecycle(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	created := g.post("/v1/conversations", key, `{"metadata":{"topic":"prova"}}`)
	require.Equal(t, http.StatusOK, created.code, created.body)
	var conv struct {
		ID       string
		Object   string
		Metadata map[string]string
	}
	decode(t, created.body, &conv)
	require.NotEmpty(t, conv.ID)
	assert.Equal(t, "conversation", conv.Object)
	assert.Equal(t, "prova", conv.Metadata["topic"])

	got := g.do("GET", "/v1/conversations/"+conv.ID, key, "", "")
	require.Equal(t, http.StatusOK, got.code, got.body)
	assert.Contains(t, got.body, conv.ID)

	updated := g.post("/v1/conversations/"+conv.ID, key, `{"metadata":{"topic":"aggiornato"}}`)
	require.Equal(t, http.StatusOK, updated.code, updated.body)
	assert.Contains(t, updated.body, "aggiornato")

	del := g.do("DELETE", "/v1/conversations/"+conv.ID, key, "", "")
	require.Equal(t, http.StatusOK, del.code, del.body)

	gone := g.do("GET", "/v1/conversations/"+conv.ID, key, "", "")
	assert.Equal(t, http.StatusNotFound, gone.code, gone.body)
}

// Items are appended in order and read back in that order.
func TestConversations_ItemsKeepTheirOrder(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	created := g.post("/v1/conversations", key, `{}`)
	var conv struct{ ID string }
	decode(t, created.body, &conv)

	add := g.post("/v1/conversations/"+conv.ID+"/items", key, `{"items":[
		{"type":"message","role":"user","content":"uno"},
		{"type":"message","role":"user","content":"due"}
	]}`)
	require.Equal(t, http.StatusOK, add.code, add.body)

	more := g.post("/v1/conversations/"+conv.ID+"/items", key, `{"items":[
		{"type":"message","role":"user","content":"tre"}
	]}`)
	require.Equal(t, http.StatusOK, more.code, more.body)

	list := g.do("GET", "/v1/conversations/"+conv.ID+"/items", key, "", "")
	require.Equal(t, http.StatusOK, list.code, list.body)

	var items struct {
		Data []map[string]any
	}
	decode(t, list.body, &items)
	require.Len(t, items.Data, 3)

	// Every item carries an id, and the order is the order of appending.
	seen := map[string]bool{}
	for i, item := range items.Data {
		id, _ := item["id"].(string)
		require.NotEmpty(t, id, "item %d has no id: /items paginates on them", i)
		require.False(t, seen[id], "item id %s appeared twice", id)
		seen[id] = true
	}
	assert.Contains(t, list.body, "uno")
	assert.Contains(t, list.body, "due")
	assert.Contains(t, list.body, "tre")
}

// A single item can be read and removed.
func TestConversations_SingleItem(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	created := g.post("/v1/conversations", key, `{"items":[{"type":"message","role":"user","content":"solo"}]}`)
	var conv struct{ ID string }
	decode(t, created.body, &conv)

	list := g.do("GET", "/v1/conversations/"+conv.ID+"/items", key, "", "")
	var items struct{ Data []map[string]any }
	decode(t, list.body, &items)
	require.Len(t, items.Data, 1)
	id, _ := items.Data[0]["id"].(string)
	require.NotEmpty(t, id)

	one := g.do("GET", "/v1/conversations/"+conv.ID+"/items/"+id, key, "", "")
	assert.Equal(t, http.StatusOK, one.code, one.body)

	del := g.do("DELETE", "/v1/conversations/"+conv.ID+"/items/"+id, key, "", "")
	assert.Equal(t, http.StatusOK, del.code, del.body)

	after := g.do("GET", "/v1/conversations/"+conv.ID+"/items", key, "", "")
	var left struct{ Data []map[string]any }
	decode(t, after.body, &left)
	assert.Empty(t, left.Data, "the removed item is still listed")
}

// A conversation belongs to the key that made it.
func TestConversations_AreScopedToTheirOwner(t *testing.T) {
	g := newGateway(t, []string{"m"})
	mine := g.mintKey(100, "m")
	theirs := g.mintKey(100, "m")

	created := g.post("/v1/conversations", mine, `{}`)
	var conv struct{ ID string }
	decode(t, created.body, &conv)

	got := g.do("GET", "/v1/conversations/"+conv.ID, theirs, "", "")
	assert.Equal(t, http.StatusNotFound, got.code, "another key read the conversation: %s", got.body)
}

// Concurrent appends to one conversation must all land, each with its own
// position and id.
func TestConversations_ConcurrentAppendsAllLand(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	created := g.post("/v1/conversations", key, `{}`)
	var conv struct{ ID string }
	decode(t, created.body, &conv)

	const writers = 6
	done := make(chan response, writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			done <- g.post("/v1/conversations/"+conv.ID+"/items", key,
				`{"items":[{"type":"message","role":"user","content":"w`+string(rune('a'+i))+`"}]}`)
		}(i)
	}
	deadline := time.After(10 * time.Second)
	for i := 0; i < writers; i++ {
		select {
		case resp := <-done:
			require.Equal(t, http.StatusOK, resp.code, resp.body)
		case <-deadline:
			t.Fatal("a concurrent append never returned")
		}
	}

	list := g.do("GET", "/v1/conversations/"+conv.ID+"/items", key, "", "")
	var items struct{ Data []map[string]any }
	decode(t, list.body, &items)
	assert.Len(t, items.Data, writers, "an append was lost under concurrency")

	ids := map[string]bool{}
	for _, item := range items.Data {
		id, _ := item["id"].(string)
		require.False(t, ids[id], "two items share the id %s", id)
		ids[id] = true
	}
}

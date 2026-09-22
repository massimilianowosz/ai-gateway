package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decode(t *testing.T, body string, into any) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(body), into), "body: %s", body)
}

// A turn stored under a conversation must carry both sides of the exchange, and
// the next turn on that conversation must see them.
func TestResponses_ConversationCarriesBothSidesOfTheTurn(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("mi chiamo Max")})
	key := g.mintKey(100, "m")

	conv := g.post("/v1/conversations", key, `{}`)
	require.Equal(t, http.StatusOK, conv.code, conv.body)
	var created struct{ ID string }
	decode(t, conv.body, &created)
	require.NotEmpty(t, created.ID)

	first := g.post("/v1/responses", key,
		`{"model":"m","input":"come mi chiamo?","conversation":"`+created.ID+`"}`)
	require.Equal(t, http.StatusOK, first.code, first.body)

	items := g.do("GET", "/v1/conversations/"+created.ID+"/items", key, "", "")
	require.Equal(t, http.StatusOK, items.code, items.body)

	assert.Contains(t, items.body, "come mi chiamo?", "the caller's input never joined the thread")
	assert.Contains(t, items.body, "mi chiamo Max", "the model's answer never joined the thread")
}

// previous_response_id must replay everything said before, not just the turn
// immediately prior.
func TestResponses_ChainReplaysTheWholeThread(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("primo")})
	key := g.mintKey(100, "m")

	one := g.post("/v1/responses", key, `{"model":"m","input":"turno uno"}`)
	require.Equal(t, http.StatusOK, one.code, one.body)
	var r1 struct{ ID string }
	decode(t, one.body, &r1)

	g.scripts.set("m", &script{complete: textAnswer("secondo")})
	two := g.post("/v1/responses", key,
		`{"model":"m","input":"turno due","previous_response_id":"`+r1.ID+`"}`)
	require.Equal(t, http.StatusOK, two.code, two.body)
	var r2 struct{ ID string }
	decode(t, two.body, &r2)

	// The third turn's stored input must contain the first exchange.
	g.scripts.set("m", &script{complete: textAnswer("terzo")})
	three := g.post("/v1/responses", key,
		`{"model":"m","input":"turno tre","previous_response_id":"`+r2.ID+`"}`)
	require.Equal(t, http.StatusOK, three.code, three.body)
	var r3 struct{ ID string }
	decode(t, three.body, &r3)

	items := g.do("GET", "/v1/responses/"+r3.ID+"/input_items?limit=100", key, "", "")
	require.Equal(t, http.StatusOK, items.code, items.body)
	assert.Contains(t, items.body, "turno uno", "the chain lost the first turn")
	assert.Contains(t, items.body, "turno due")
	assert.Contains(t, items.body, "turno tre")
}

// An unknown previous_response_id is rejected rather than silently starting a
// fresh thread.
func TestResponses_UnknownPreviousIsRejected(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	resp := g.post("/v1/responses", key,
		`{"model":"m","input":"ciao","previous_response_id":"resp_nonesiste"}`)
	assert.NotEqual(t, http.StatusOK, resp.code, "an unknown ancestor was accepted: %s", resp.body)
}

// conversation and previous_response_id are mutually exclusive.
func TestResponses_ConversationAndPreviousAreExclusive(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	conv := g.post("/v1/conversations", key, `{}`)
	var created struct{ ID string }
	decode(t, conv.body, &created)

	resp := g.post("/v1/responses", key,
		`{"model":"m","input":"ciao","conversation":"`+created.ID+`","previous_response_id":"resp_x"}`)
	assert.Equal(t, http.StatusBadRequest, resp.code, resp.body)
}

// A background turn answers queued, then completes, and the completed row is
// what a later GET serves.
func TestResponses_BackgroundTurnCompletes(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("fatto")})
	key := g.mintKey(100, "m")

	start := g.post("/v1/responses", key, `{"model":"m","input":"lavora","background":true}`)
	require.Equal(t, http.StatusOK, start.code, start.body)
	var queued struct {
		ID     string
		Status string
	}
	decode(t, start.body, &queued)
	assert.Equal(t, "queued", queued.Status)

	var final struct {
		Status string
		Output []map[string]any
	}
	require.Eventually(t, func() bool {
		got := g.do("GET", "/v1/responses/"+queued.ID, key, "", "")
		if got.code != http.StatusOK {
			return false
		}
		final = struct {
			Status string
			Output []map[string]any
		}{}
		return json.Unmarshal([]byte(got.body), &final) == nil && final.Status == "completed"
	}, 3*time.Second, 25*time.Millisecond, "the background turn never completed")

	assert.NotEmpty(t, final.Output)
}

// input_items paginates, and a page cut short by `before` says so.
func TestResponses_InputItemsPagination(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("ok")})
	key := g.mintKey(100, "m")

	resp := g.post("/v1/responses", key, `{"model":"m","input":[
		{"type":"message","role":"user","content":"uno"},
		{"type":"message","role":"user","content":"due"},
		{"type":"message","role":"user","content":"tre"}
	]}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	var r struct{ ID string }
	decode(t, resp.body, &r)

	all := g.do("GET", "/v1/responses/"+r.ID+"/input_items?limit=100", key, "", "")
	var list struct {
		Data    []map[string]any
		HasMore bool `json:"has_more"`
	}
	decode(t, all.body, &list)
	require.Len(t, list.Data, 3)
	assert.False(t, list.HasMore, "a complete page must not claim more")

	cursor, _ := list.Data[2]["id"].(string)
	require.NotEmpty(t, cursor)

	cut := g.do("GET", "/v1/responses/"+r.ID+"/input_items?before="+cursor, key, "", "")
	var page struct {
		Data    []map[string]any
		HasMore bool `json:"has_more"`
	}
	decode(t, cut.body, &page)
	assert.Len(t, page.Data, 2)
	assert.True(t, page.HasMore, "items exist at and after the cursor")
}

// A response belongs to the key that made it.
func TestResponses_AreScopedToTheirOwner(t *testing.T) {
	g := newGateway(t, []string{"m"})
	g.scripts.set("m", &script{complete: textAnswer("segreto")})
	mine := g.mintKey(100, "m")
	theirs := g.mintKey(100, "m")

	resp := g.post("/v1/responses", mine, `{"model":"m","input":"ciao"}`)
	require.Equal(t, http.StatusOK, resp.code, resp.body)
	var r struct{ ID string }
	decode(t, resp.body, &r)

	got := g.do("GET", "/v1/responses/"+r.ID, theirs, "", "")
	assert.Equal(t, http.StatusNotFound, got.code, "another key could read the response: %s", got.body)
}

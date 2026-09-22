package production

import (
	"net/http"
	"strings"
	"testing"
)

// The model catalogue is the cheapest possible check that the deployment is
// serving the config it was given, and the first thing to look at when a test
// below fails for a model that simply is not there.
func TestProdModelCatalogue(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-models-"+uniqueSuffix(), "", 5)

	r := caller.get(t, "/v1/models", nil)
	requireStatus(t, r, http.StatusOK, "catalogo modelli")

	parsed := r.json(t)
	data, _ := parsed["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("catalogo vuoto: %s", truncate(string(r.body), 200))
	}

	found := false
	for _, entry := range data {
		item, _ := entry.(map[string]any)
		if id, _ := item["id"].(string); id == model() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("il modello usato dalla suite (%s) non è nel catalogo di %d modelli", model(), len(data))
	}
}

// Streaming is its own path through the proxy: a body that never buffers, and
// a framing the client depends on. A non-streaming test cannot see it break.
func TestProdStreaming(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-stream-"+uniqueSuffix(), "", 5)

	r := caller.post(t, "/v1/chat/completions", map[string]any{
		"model":       model(),
		"messages":    []map[string]any{{"role": "user", "content": "Conta da 1 a 5, solo i numeri."}},
		"max_tokens":  600,
		"temperature": 0,
		"stream":      true,
	}, map[string]string{"x-ubiquum-guard": "none", "x-ubiquum-cache": "false"})
	requireStatus(t, r, http.StatusOK, "streaming")

	body := string(r.body)
	if !strings.Contains(body, "data:") {
		t.Fatalf("nessun frame SSE nella risposta: %s", truncate(body, 200))
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("lo stream non si chiude con [DONE]: %s", truncate(body, 200))
	}
	// A stream that only ever sends the terminator carries no content.
	if strings.Count(body, "data:") < 2 {
		t.Errorf("stream con un solo frame: %s", truncate(body, 300))
	}
}

// The Responses API keeps state the provider does not: a stored turn has to be
// retrievable afterwards, and has to be usable as the parent of the next one.
// Those two are the whole point of the stateful half.
func TestProdResponsesStateful(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-responses-"+uniqueSuffix(), "", 5)
	headers := map[string]string{"x-ubiquum-guard": "none", "x-ubiquum-cache": "false"}

	created := caller.post(t, "/v1/responses", map[string]any{
		"model":             model(),
		"input":             "Ricorda il numero 41. Rispondi solo: OK",
		"max_output_tokens": 600,
		"temperature":       0,
		"store":             true,
	}, headers)
	requireStatus(t, created, http.StatusOK, "creazione response")

	id, _ := created.json(t)["id"].(string)
	if id == "" {
		t.Fatalf("la response creata non ha id: %s", truncate(string(created.body), 300))
	}

	t.Run("a stored turn can be retrieved", func(t *testing.T) {
		fetched := caller.get(t, "/v1/responses/"+id, headers)
		requireStatus(t, fetched, http.StatusOK, "retrieve")
		if got, _ := fetched.json(t)["id"].(string); got != id {
			t.Errorf("retrieve ha restituito un altro turno: %q invece di %q", got, id)
		}
	})

	t.Run("it can be the parent of the next turn", func(t *testing.T) {
		chained := caller.post(t, "/v1/responses", map[string]any{
			"model":                model(),
			"input":                "Quale numero ti ho chiesto di ricordare? Rispondi col solo numero.",
			"previous_response_id": id,
			"max_output_tokens":    600,
			"temperature":          0,
		}, headers)
		requireStatus(t, chained, http.StatusOK, "previous_response_id")
	})

	t.Run("another key cannot read it", func(t *testing.T) {
		// Stored turns are per caller: the id is not a bearer token.
		stranger := ephemeralKey(t, admin, "e2e-responses-other-"+uniqueSuffix(), "", 5)
		fetched := stranger.get(t, "/v1/responses/"+id, headers)
		if fetched.status == http.StatusOK {
			t.Errorf("una chiave estranea ha letto il turno %s di un'altra", id)
		}
	})
}

// Embeddings run through the same auth, budget and routing path as chat but a
// different handler, and nothing else in this suite exercises it.
func TestProdEmbeddings(t *testing.T) {
	admin := masterClient(t)
	caller := ephemeralKey(t, admin, "e2e-embeddings-"+uniqueSuffix(), "", 5)

	embeddingModel := "text-embedding-3-small"
	r := caller.post(t, "/v1/embeddings", map[string]any{
		"model": embeddingModel,
		"input": []string{"il gatto dorme", "il cane corre"},
	}, map[string]string{"x-ubiquum-guard": "none"})
	if r.status == http.StatusNotFound || r.status == http.StatusBadRequest {
		t.Skipf("%s non disponibile in questo deployment (HTTP %d)", embeddingModel, r.status)
	}
	requireStatus(t, r, http.StatusOK, "embeddings")

	data, _ := r.json(t)["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("attesi due vettori, ricevuti %d: %s", len(data), truncate(string(r.body), 200))
	}

	dims := make([]int, 0, 2)
	for _, entry := range data {
		item, _ := entry.(map[string]any)
		vector, _ := item["embedding"].([]any)
		if len(vector) == 0 {
			t.Fatalf("vettore vuoto nella risposta: %s", truncate(string(r.body), 200))
		}
		dims = append(dims, len(vector))
	}
	if dims[0] != dims[1] {
		t.Errorf("due input hanno prodotto vettori di dimensione diversa: %d e %d", dims[0], dims[1])
	}
}

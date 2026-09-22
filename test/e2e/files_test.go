package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uploadFile posts a small multipart file for the given model.
func (g *gateway) uploadFile(key, model, name, content string) response {
	g.t.Helper()
	ct, body := multipartFile(g.t, name, content)
	req, err := http.NewRequest("POST", g.url+"/v1/files", stringsReader(body))
	require.NoError(g.t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", ct)
	if model != "" {
		req.Header.Set("X-Ubiquum-Model", model)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(g.t, err)
	defer resp.Body.Close()
	raw, _ := readAll(resp)
	return response{code: resp.StatusCode, body: raw, hdr: resp.Header}
}

// The gateway keeps the metadata and hands back an id of its own, while the
// bytes go to the provider and stay there.
func TestFiles_UploadListRetrieveDelete(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	up := g.uploadFile(key, "m", "doc.txt", "contenuto vero")
	require.Equal(t, http.StatusOK, up.code, up.body)

	var file struct {
		ID     string
		Object string
	}
	decode(t, up.body, &file)
	require.NotEmpty(t, file.ID)
	assert.Equal(t, "file", file.Object)

	// The bytes reached the provider.
	reqs := g.scripts.fileRequests()
	require.NotEmpty(t, reqs, "the upload never reached the provider")
	assert.Equal(t, http.MethodPost, reqs[0].Method)

	list := g.do("GET", "/v1/files", key, "", "")
	require.Equal(t, http.StatusOK, list.code, list.body)
	assert.Contains(t, list.body, file.ID)

	got := g.do("GET", "/v1/files/"+file.ID, key, "", "")
	require.Equal(t, http.StatusOK, got.code, got.body)

	content := g.do("GET", "/v1/files/"+file.ID+"/content", key, "", "")
	require.Equal(t, http.StatusOK, content.code, content.body)
	assert.Contains(t, content.body, "contenuto vero")

	del := g.do("DELETE", "/v1/files/"+file.ID, key, "", "")
	require.Equal(t, http.StatusOK, del.code, del.body)

	gone := g.do("GET", "/v1/files/"+file.ID, key, "", "")
	assert.Equal(t, http.StatusNotFound, gone.code, "a deleted file was still served: %s", gone.body)
}

// A file belongs to the key that uploaded it.
func TestFiles_AreScopedToTheirOwner(t *testing.T) {
	g := newGateway(t, []string{"m"})
	mine := g.mintKey(100, "m")
	theirs := g.mintKey(100, "m")

	up := g.uploadFile(mine, "m", "doc.txt", "segreto")
	require.Equal(t, http.StatusOK, up.code, up.body)
	var file struct{ ID string }
	decode(t, up.body, &file)

	got := g.do("GET", "/v1/files/"+file.ID, theirs, "", "")
	assert.Equal(t, http.StatusNotFound, got.code, "another key read the file: %s", got.body)

	content := g.do("GET", "/v1/files/"+file.ID+"/content", theirs, "", "")
	assert.Equal(t, http.StatusNotFound, content.code, "another key read the contents: %s", content.body)
}

// The upload must say which model it is for: that is how a provider is chosen.
func TestFiles_UploadRequiresTheModelHeader(t *testing.T) {
	g := newGateway(t, []string{"m"})
	key := g.mintKey(100, "m")

	up := g.uploadFile(key, "", "doc.txt", "x")
	assert.Equal(t, http.StatusBadRequest, up.code, up.body)
	assert.Contains(t, up.body, "X-Ubiquum-Model")
}

// Uploading costs money, so it is gated like any other paid route.
func TestFiles_UploadIsGatedOnBudget(t *testing.T) {
	g := newGateway(t, []string{"paid", "oauth"}, "oauth")
	unfunded := g.seedUnfundedKey("paid", "oauth")

	paid := g.uploadFile(unfunded, "paid", "doc.txt", "x")
	assert.Equal(t, http.StatusPaymentRequired, paid.code, paid.body)
	assert.Contains(t, paid.body, "no budget")

	free := g.uploadFile(unfunded, "oauth", "doc.txt", "x")
	assert.Equal(t, http.StatusOK, free.code,
		"a model the gateway does not pay for needs no budget: %s", free.body)
}

// A model the key may not use cannot be used to upload either.
func TestFiles_UploadHonoursTheModelWhitelist(t *testing.T) {
	g := newGateway(t, []string{"allowed", "forbidden"})
	key := g.mintKey(100, "allowed")

	up := g.uploadFile(key, "forbidden", "doc.txt", "x")
	assert.Equal(t, http.StatusForbidden, up.code, up.body)
}

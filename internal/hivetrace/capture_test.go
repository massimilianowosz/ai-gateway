package hivetrace

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushRecorder counts flushes so the passthrough guarantee can be asserted:
// capture must not swallow or delay a chunk the handler pushed out.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() { f.flushes++ }

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func TestTraceWriter_PassesResponseThroughUnchanged(t *testing.T) {
	rec := newFlushRecorder()
	tw := newTraceWriter(rec, 1024)
	defer tw.release()

	tw.WriteHeader(http.StatusCreated)
	for _, chunk := range []string{"data: one\n\n", "data: two\n\n", "data: [DONE]\n\n"} {
		_, err := tw.Write([]byte(chunk))
		require.NoError(t, err)
		tw.Flush()
	}

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "data: one\n\ndata: two\n\ndata: [DONE]\n\n", rec.Body.String())
	assert.Equal(t, 3, rec.flushes, "every flush must reach the client")
	assert.Equal(t, rec.Body.String(), string(tw.body()))
	assert.False(t, tw.truncated)
}

// Truncation bounds what is retained, never what the client receives.
func TestTraceWriter_TruncatesTheCopyNotTheStream(t *testing.T) {
	rec := newFlushRecorder()
	tw := newTraceWriter(rec, 8)
	defer tw.release()

	_, err := tw.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)

	assert.Equal(t, "0123456789abcdef", rec.Body.String(), "the client gets the whole stream")
	assert.Equal(t, "01234567", string(tw.body()))
	assert.True(t, tw.truncated)
}

func TestTraceWriter_TruncatesAcrossChunkBoundary(t *testing.T) {
	rec := newFlushRecorder()
	tw := newTraceWriter(rec, 5)
	defer tw.release()

	_, _ = tw.Write([]byte("abc"))
	_, _ = tw.Write([]byte("def"))
	_, _ = tw.Write([]byte("ghi"))

	assert.Equal(t, "abcdefghi", rec.Body.String())
	assert.Equal(t, "abcde", string(tw.body()))
	assert.True(t, tw.truncated)
}

func TestTraceWriter_DefaultsToOKAndRecordsFirstByte(t *testing.T) {
	rec := newFlushRecorder()
	tw := newTraceWriter(rec, 64)
	defer tw.release()

	assert.True(t, tw.firstByte.IsZero())
	_, _ = tw.Write([]byte("hi"))

	assert.Equal(t, http.StatusOK, tw.status)
	assert.False(t, tw.firstByte.IsZero(), "time to first byte comes from the first Write")
}

// Only the first WriteHeader counts, matching net/http, so a late header from
// a wrapper does not rewrite the status the client actually saw.
func TestTraceWriter_KeepsFirstStatus(t *testing.T) {
	rec := newFlushRecorder()
	tw := newTraceWriter(rec, 64)
	defer tw.release()

	tw.WriteHeader(http.StatusTooManyRequests)
	tw.WriteHeader(http.StatusOK)

	assert.Equal(t, http.StatusTooManyRequests, tw.status)
}

func TestTraceWriter_UnwrapExposesTheUnderlyingWriter(t *testing.T) {
	rec := newFlushRecorder()
	tw := newTraceWriter(rec, 64)
	defer tw.release()

	assert.Same(t, rec, tw.Unwrap())
}

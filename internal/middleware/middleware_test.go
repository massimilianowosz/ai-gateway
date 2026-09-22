package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newNopLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestChain_Order(t *testing.T) {
	var order []string

	m1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m1-before")
			next.ServeHTTP(w, r)
			order = append(order, "m1-after")
		})
	}

	m2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m2-before")
			next.ServeHTTP(w, r)
			order = append(order, "m2-after")
		})
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})

	chain := Chain(handler, m1, m2)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	// m1 is outermost, m2 is inner
	assert.Equal(t, []string{"m1-before", "m2-before", "handler", "m2-after", "m1-after"}, order)
}

func TestRequestID_Generated(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		assert.NotEmpty(t, id)
		assert.Len(t, id, 16) // 8 bytes = 16 hex chars
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
}

func TestRequestID_Passthrough(t *testing.T) {
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		assert.Equal(t, "custom-id-123", id)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "custom-id-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "custom-id-123", rec.Header().Get("X-Request-ID"))
}

func TestRecovery_PanicCaught(t *testing.T) {
	logger := newNopLogger()
	handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "internal server error")
}

func TestMaxBody_Enforced(t *testing.T) {
	handler := MaxBody(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Small body — should pass
	req := httptest.NewRequest("POST", "/", bytes.NewBufferString("small"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Large body — should fail
	req = httptest.NewRequest("POST", "/", bytes.NewBufferString("this is way too large for the limit"))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

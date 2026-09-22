package proxy

import (
	"errors"
	"fmt"
	"net/http"
)

// bodyTooLarge reports whether a decode failed because the body exceeded the
// configured limit, and how large that limit was.
//
// MaxBody wraps the request in an http.MaxBytesReader, so the limit surfaces as
// a decode error. Reporting that as a malformed body sends the caller looking
// for a syntax mistake that is not there, and hides the one setting that would
// fix it.
func bodyTooLarge(err error) (int64, bool) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return tooLarge.Limit, true
	}
	return 0, false
}

// writeDecodeError reports a failed request decode, distinguishing an oversized
// body from a malformed one.
func writeDecodeError(w http.ResponseWriter, err error) {
	if limit, ok := bodyTooLarge(err); ok {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("request body exceeds the %d byte limit", limit))
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
}

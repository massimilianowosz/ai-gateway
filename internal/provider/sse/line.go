package sse

import (
	"bufio"
	"errors"
	"io"
)

// MaxLineBytes caps a single SSE line. Generous for any real event — a large
// tool-argument fragment is orders of magnitude smaller — and small enough that
// one upstream cannot grow a slice until the process dies.
const MaxLineBytes = 8 << 20

// ErrLineTooLong is returned when an upstream sends a line past MaxLineBytes.
var ErrLineTooLong = errors.New("sse: line exceeds the maximum length")

// ReadLine reads one line, bounded.
//
// bufio.Reader has a fixed buffer, but ReadBytes accumulates into a slice that
// grows until it finds a newline: an upstream that never sends one — by fault
// or by design — allocates without limit. This returns ErrLineTooLong instead,
// which a caller treats as a broken stream.
func ReadLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > MaxLineBytes {
			return nil, ErrLineTooLong
		}
		if err == nil {
			return out, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // the line spans more than one buffer; keep going
		}
		if errors.Is(err, io.EOF) && len(out) > 0 {
			return out, nil // a final line with no trailing newline
		}
		return nil, err
	}
}

package perf

import (
	"bytes"
	"sync"
)

// BufferPool provides reusable byte buffers to reduce GC pressure.
// At 10k+ concurrent requests, avoiding allocations is critical.
var BufferPool = &sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

// GetBuffer returns a buffer from the pool.
func GetBuffer() *bytes.Buffer {
	buf := BufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// PutBuffer returns a buffer to the pool.
func PutBuffer(buf *bytes.Buffer) {
	if buf.Cap() > 1<<20 { // Don't pool buffers > 1MB (unusual large responses)
		return
	}
	BufferPool.Put(buf)
}

// SmallBufferPool for SSE chunks (typically < 1KB).
var SmallBufferPool = &sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 512)
		return &b
	},
}

// GetSmallBuffer returns a small byte slice from the pool.
func GetSmallBuffer() *[]byte {
	return SmallBufferPool.Get().(*[]byte)
}

// PutSmallBuffer returns a small byte slice to the pool.
func PutSmallBuffer(b *[]byte) {
	*b = (*b)[:0]
	SmallBufferPool.Put(b)
}

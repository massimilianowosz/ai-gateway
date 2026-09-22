package perf

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetBuffer_ReturnsResetBuffer(t *testing.T) {
	buf := GetBuffer()
	assert.Equal(t, 0, buf.Len())
	buf.WriteString("hello")
	assert.Equal(t, 5, buf.Len())
	PutBuffer(buf)
}

func TestGetBuffer_ReusesPutBuffer(t *testing.T) {
	buf1 := GetBuffer()
	buf1.WriteString("some data")
	PutBuffer(buf1)

	// Not guaranteed to be the exact same instance (sync.Pool may allocate a
	// new one), but if reused it must come back reset regardless.
	buf2 := GetBuffer()
	assert.Equal(t, 0, buf2.Len(), "buffer retrieved from the pool must always be reset")
}

func TestPutBuffer_DoesNotPoolOversizedBuffers(t *testing.T) {
	huge := GetBuffer()
	huge.Grow(2 << 20) // > 1MB capacity
	assert.Greater(t, huge.Cap(), 1<<20, "test setup: buffer must exceed 1MB to exercise the oversized path")

	// Should not panic and should simply be dropped instead of pooled.
	PutBuffer(huge)
}

func TestGetSmallBuffer_LengthResetOnPut(t *testing.T) {
	b := GetSmallBuffer()
	*b = append(*b, []byte("chunk")...)
	assert.Equal(t, 5, len(*b))

	PutSmallBuffer(b)
	assert.Equal(t, 0, len(*b), "PutSmallBuffer must reset length to 0")
}

func TestBufferPool_ConcurrentGetPut(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := GetBuffer()
			buf.WriteString("x")
			PutBuffer(buf)

			sb := GetSmallBuffer()
			*sb = append(*sb, 'y')
			PutSmallBuffer(sb)
		}()
	}
	wg.Wait()
}

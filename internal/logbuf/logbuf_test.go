package logbuf

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuffer_WriteAndLines_Basic(t *testing.T) {
	b := New(5)
	n, err := b.Write([]byte("line1\nline2\n"))
	require.NoError(t, err)
	assert.Equal(t, len("line1\nline2\n"), n)

	assert.Equal(t, []string{"line1", "line2"}, b.Lines(0))
}

func TestBuffer_Write_SkipsEmptyLines(t *testing.T) {
	b := New(5)
	_, err := b.Write([]byte("line1\n\nline2\n\n\n"))
	require.NoError(t, err)

	assert.Equal(t, []string{"line1", "line2"}, b.Lines(0))
}

func TestBuffer_Write_NoTrailingNewline(t *testing.T) {
	b := New(5)
	_, err := b.Write([]byte("only-line"))
	require.NoError(t, err)

	assert.Equal(t, []string{"only-line"}, b.Lines(0))
}

func TestBuffer_Lines_EmptyBufferReturnsEmptySlice(t *testing.T) {
	b := New(5)
	assert.Empty(t, b.Lines(0))
}

func TestBuffer_Lines_NLimitsToMostRecent(t *testing.T) {
	b := New(10)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(b, "line%d\n", i)
	}

	assert.Equal(t, []string{"line4", "line5"}, b.Lines(2))
	assert.Equal(t, []string{"line1", "line2", "line3", "line4", "line5"}, b.Lines(0))
	// n greater than available lines should just return everything.
	assert.Equal(t, []string{"line1", "line2", "line3", "line4", "line5"}, b.Lines(100))
}

func TestBuffer_RingWraparound_OverwritesOldestLines(t *testing.T) {
	b := New(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(b, "line%d\n", i)
	}

	// Capacity 3, wrote 5 lines: only the last 3 should remain, oldest-first.
	assert.Equal(t, []string{"line3", "line4", "line5"}, b.Lines(0))
}

func TestBuffer_RingWraparound_ExactlyFillsCapacity(t *testing.T) {
	b := New(3)
	_, _ = b.Write([]byte("a\nb\nc\n"))

	assert.Equal(t, []string{"a", "b", "c"}, b.Lines(0))
}

func TestBuffer_MultipleWritesAccumulate(t *testing.T) {
	b := New(10)
	_, _ = b.Write([]byte("a\n"))
	_, _ = b.Write([]byte("b\n"))
	_, _ = b.Write([]byte("c\n"))

	assert.Equal(t, []string{"a", "b", "c"}, b.Lines(0))
}

func TestBuffer_ConcurrentWritesAndReads(t *testing.T) {
	b := New(100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fmt.Fprintf(b, "line%d\n", i)
			b.Lines(10)
		}(i)
	}
	wg.Wait()

	// After 50 writes into a 100-capacity buffer, all 50 lines must be present
	// (order among concurrent writers is not deterministic, so just check count).
	assert.Len(t, b.Lines(0), 50)
}

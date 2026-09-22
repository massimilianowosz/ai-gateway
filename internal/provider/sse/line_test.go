package sse

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// bufio.Reader has a fixed buffer, but ReadBytes grows a slice until it finds a
// newline: an upstream that never sends one allocates without limit.
func TestReadLine_RefusesAnUnboundedLine(t *testing.T) {
	endless := strings.NewReader(strings.Repeat("x", MaxLineBytes+1024))
	_, err := ReadLine(bufio.NewReader(endless))
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

// A line longer than one buffer but within the cap is still returned whole.
func TestReadLine_SpansBuffersAndKeepsTheLine(t *testing.T) {
	long := strings.Repeat("y", 200_000)
	r := bufio.NewReader(strings.NewReader("data: " + long + "\ndata: second\n"))

	first, err := ReadLine(r)
	if err != nil {
		t.Fatalf("first line: %v", err)
	}
	if got := strings.TrimSuffix(string(first), "\n"); got != "data: "+long {
		t.Fatalf("first line was truncated at %d bytes", len(got))
	}

	second, err := ReadLine(r)
	if err != nil {
		t.Fatalf("second line: %v", err)
	}
	if strings.TrimSuffix(string(second), "\n") != "data: second" {
		t.Fatalf("second line = %q", second)
	}
}

// A final line with no trailing newline is still delivered.
func TestReadLine_UnterminatedFinalLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("data: last"))
	got, err := ReadLine(r)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(got) != "data: last" {
		t.Fatalf("got %q", got)
	}
}

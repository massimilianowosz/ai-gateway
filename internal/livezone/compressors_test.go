package livezone

import (
	"fmt"
	"strings"
	"testing"
)

// npm, cargo and docker redraw a progress line hundreds of times with \r. A
// terminal shows only the last frame; the transcript carries them all.
func TestTerminalRedrawsCollapseToWhatTheUserSaw(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("npm warn deprecated some-package@1.0.0: no longer maintained\n")
	for i := 0; i <= 100; i++ {
		fmt.Fprintf(&sb, "downloading dependencies [%d%%] ................................\r", i)
	}
	sb.WriteString("downloading dependencies [100%] done\n")
	for i := 0; i < 12; i++ {
		sb.WriteString("added package number seven hundred and something to the tree\n")
	}
	sb.WriteString("npm error ERESOLVE could not resolve dependency\n")
	in := sb.String()

	res := Transform(in, DefaultOptions())
	if !res.Applied {
		t.Fatalf("not applied: %s (kind %v)", res.Reason, res.Kind)
	}
	if strings.Count(res.Content, "downloading dependencies") > 1 {
		t.Error("intermediate progress frames survived")
	}
	if !strings.Contains(res.Content, "npm error ERESOLVE could not resolve dependency") {
		t.Error("the line the agent needed was dropped")
	}
	if res.BytesAfter >= res.BytesBefore {
		t.Errorf("bytes %d -> %d, expected a reduction", res.BytesBefore, res.BytesAfter)
	}
}

// CRLF is a line ending, not an overwrite. Treating it as one would empty
// every line of a Windows transcript.
func TestTerminalCRLFIsNotARedraw(t *testing.T) {
	in := "first line\r\nsecond line\r\nthird line"
	got := collapseRedraws(in)
	if got != "first line\nsecond line\nthird line" {
		t.Errorf("collapseRedraws(CRLF) = %q", got)
	}
}

const dockerPS = `CONTAINER ID   IMAGE                      COMMAND                  CREATED         STATUS         PORTS                    NAMES
9f2b1c4d5e6a   ubiquum-ai-gateway          "/ubiquum serve -c…"   3 hours ago     Up 3 hours     0.0.0.0:4000->4000/tcp   ubiquum-ai-gateway-1
1a2b3c4d5e6f   postgres:15-alpine         "docker-entrypoint.s…"   8 hours ago     Up 8 hours     0.0.0.0:5433->5432/tcp   ubiquum-portal-db-1
7g8h9i0j1k2l   redis/redis-stack:latest   "/entrypoint.sh"         8 hours ago     Up 8 hours     0.0.0.0:8001->8001/tcp   ubiquum-redis-1
3m4n5o6p7q8r   nats:2.10-alpine           "/nats-server -js"       8 hours ago     Up 8 hours     0.0.0.0:4222->4222/tcp   ubiquum-nats-1`

// A shell tool pads every cell to the width of its widest value. The padding
// is most of a wide line and carries nothing.
func TestTableUnpadsColumnsWithoutLosingCells(t *testing.T) {
	if got := Detect(dockerPS); got != KindTable {
		t.Fatalf("Detect = %v, want table", got)
	}

	res := Transform(dockerPS, DefaultOptions()) // AllowLossy is false
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Reason)
	}
	if res.Lossy {
		t.Error("removing padding discards no data and must not be gated as lossy")
	}
	for _, cell := range []string{"ubiquum-ai-gateway-1", "0.0.0.0:5433->5432/tcp", "redis/redis-stack:latest", "CONTAINER ID"} {
		if !strings.Contains(res.Content, cell) {
			t.Errorf("cell %q was lost", cell)
		}
	}
	if got, want := len(strings.Split(res.Content, "\n")), len(strings.Split(dockerPS, "\n")); got != want {
		t.Errorf("rows %d, want %d", got, want)
	}
	if strings.Contains(res.Content, "   ") {
		t.Error("padding runs of three or more spaces survived")
	}
}

// Leading indentation is meaning, not padding: a stack trace and a YAML block
// both depend on it.
func TestTableKeepsLeadingIndentation(t *testing.T) {
	in := "root   value   here\n" +
		"    nested   value   here\n" +
		"    nested   other   here\n" +
		"        deeper   value   here\n"

	out, ok := compactTable(in)
	if !ok {
		t.Fatal("no change")
	}
	for _, prefix := range []string{"\n    nested", "\n        deeper"} {
		if !strings.Contains(out, prefix) {
			t.Errorf("indentation %q was not preserved:\n%s", prefix, out)
		}
	}
}

func pemBlock(lines int) string {
	var sb strings.Builder
	sb.WriteString("-----BEGIN CERTIFICATE-----\n")
	for i := 0; i < lines; i++ {
		sb.WriteString("MIIFazCCA1OgAwIBAgIRAIIQz7DSQONZRGPgu2OCiwAwDQYJKoZIhvcNAQELBQAw\n")
	}
	sb.WriteString("-----END CERTIFICATE-----")
	return sb.String()
}

// Encoded binary is the one payload an agent cannot act on, and it is often
// the largest thing in the transcript.
func TestBlobElisionNeedsOptIn(t *testing.T) {
	in := pemBlock(40)
	if got := Detect(in); got != KindBlob {
		t.Fatalf("Detect = %v, want blob", got)
	}

	if res := Transform(in, DefaultOptions()); res.Applied {
		t.Errorf("elided content without AllowLossy: %s", res.Reason)
	}

	opts := DefaultOptions()
	opts.AllowLossy = true
	res := Transform(in, opts)
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Reason)
	}
	if !res.Lossy {
		t.Error("eliding content must be reported as lossy")
	}
	if !strings.Contains(res.Content, "-----BEGIN CERTIFICATE-----") {
		t.Error("the header that identifies the payload was lost")
	}
	if !strings.Contains(res.Content, "-----END CERTIFICATE-----") {
		t.Error("the trailer that identifies the payload was lost")
	}
	if !strings.Contains(res.Content, "elided") {
		t.Error("no note of what was removed")
	}
	if res.BytesAfter*4 > res.BytesBefore {
		t.Errorf("bytes %d -> %d, expected far more from a certificate", res.BytesBefore, res.BytesAfter)
	}
}

// Ordinary prose is not a blob, however long it runs.
func TestBlobIgnoresProse(t *testing.T) {
	in := strings.Repeat("This is an ordinary sentence of explanatory text in the output.\n", 30)
	if got := Detect(in); got == KindBlob {
		t.Error("classified prose as encoded data")
	}
}

func TestXMLIsTreatedAsMarkup(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<project>\n")
	sb.WriteString("    <groupId>ai.ubiquum</groupId>\n    <artifactId>gateway</artifactId>\n")
	sb.WriteString("    <dependencies>\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&sb, "        <dependency>\n            <groupId>org.example.group%d</groupId>\n            <artifactId>library-%d</artifactId>\n        </dependency>\n", i, i)
	}
	sb.WriteString("    </dependencies>\n</project>")
	in := sb.String()

	if got := Detect(in); got != KindHTML {
		t.Fatalf("Detect = %v, want html/xml", got)
	}
	res := Transform(in, DefaultOptions())
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Reason)
	}
	if !strings.Contains(res.Content, "<artifactId>gateway</artifactId>") {
		t.Error("element content did not survive")
	}
}

// Inside CDATA the whitespace is content.
func TestXMLLeavesCDATAAlone(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<?xml version=\"1.0\"?>\n<config>\n    <script><![CDATA[\n        if (a) {\n            run();\n        }\n    ]]></script>\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&sb, "    <setting name=\"option-number-%d\">a value long enough to matter</setting>\n", i)
	}
	sb.WriteString("</config>")

	res := Transform(sb.String(), DefaultOptions())
	if res.Applied {
		t.Errorf("rewrote a document containing CDATA:\n%s", res.Content)
	}
}

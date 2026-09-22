package livezone

import (
	"fmt"
	"strings"
	"testing"
)

func grepOutput(files, hitsPerFile int) string {
	var sb strings.Builder
	for f := 0; f < files; f++ {
		path := fmt.Sprintf("internal/provider/anthropic/very/deep/package_%d.go", f)
		for h := 0; h < hitsPerFile; h++ {
			fmt.Fprintf(&sb, "%s:%d:\tif err := doSomething(ctx); err != nil {\n", path, h*7+3)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// A grep across a few files repeats the path once per hit, and the path is
// usually longer than the matched line. Stating it once per file is the whole
// saving, and it costs nothing: no hit is dropped and none is reordered.
func TestSearchGroupsHitsUnderOneHeadingPerFile(t *testing.T) {
	in := grepOutput(4, 12)
	if got := Detect(in); got != KindSearch {
		t.Fatalf("Detect = %v, want search", got)
	}

	res := Transform(in, DefaultOptions()) // AllowLossy is false
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Reason)
	}
	if res.Lossy {
		t.Error("grouping discards nothing and must not be gated as lossy")
	}
	if res.BytesAfter >= res.BytesBefore {
		t.Errorf("bytes %d -> %d, expected a reduction", res.BytesBefore, res.BytesAfter)
	}

	// Every hit survives, in the order the search produced it.
	var lineNumbers []string
	for _, l := range strings.Split(res.Content, "\n") {
		if strings.HasPrefix(l, "  ") {
			num, _, _ := strings.Cut(strings.TrimSpace(l), ":")
			lineNumbers = append(lineNumbers, num)
		}
	}
	if len(lineNumbers) != 48 {
		t.Errorf("kept %d hits, want 48", len(lineNumbers))
	}
	if n := strings.Count(res.Content, "package_0.go"); n != 1 {
		t.Errorf("path repeated %d times, want 1", n)
	}
}

// One hit per file has no repeated path to factor out, and a heading per hit
// would make the block longer.
func TestSearchIgnoresOneHitPerFile(t *testing.T) {
	in := grepOutput(8, 1)
	if got := Detect(in); got == KindSearch {
		t.Error("classified as search with nothing to group")
	}
}

// ripgrep writes context lines with '-', and a path may contain one too.
// Anything that is not uniformly "path:line:content" is left alone rather than
// guessed at.
func TestSearchRefusesMixedBlocks(t *testing.T) {
	in := grepOutput(2, 6) + "\n--\ninternal/other-file-2-thing.go-40-context line"
	if got := Detect(in); got == KindSearch {
		t.Error("classified a block it cannot split unambiguously as search")
	}
}

func TestSearchParseLine(t *testing.T) {
	tests := []struct {
		in         string
		path, rest string
		ok         bool
	}{
		{"a/b.go:12:hello", "a/b.go", "12:hello", true},
		{"C:\\src\\a.go:12:hello", "C:\\src\\a.go", "12:hello", true},
		{"no line number here", "", "", false},
		{":12:leading colon", "", "", false},
		{"a/b.go:12", "", "", false},
	}
	for _, tt := range tests {
		path, rest, ok := parseSearchLine(tt.in)
		if ok != tt.ok || path != tt.path || rest != tt.rest {
			t.Errorf("parseSearchLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, path, rest, ok, tt.path, tt.rest, tt.ok)
		}
	}
}

const fetchedPage = `<!DOCTYPE html>
<html>
    <head>
        <style>
            body { margin: 0; padding: 0; font-family: sans-serif; }
        </style>
        <script src="https://example.com/analytics.js"></script>
        <script>
            window.dataLayer = window.dataLayer || [];
            function gtag(){ dataLayer.push(arguments); }
        </script>
    </head>
    <body>
        <!-- navigation, regenerated on every deploy -->
        <div class="content">
            <p>The answer the agent came for.</p>
        </div>
    </body>
</html>`

// A fetched page is mostly indentation. Collapsing it is what a renderer would
// have done anyway, so it needs no policy decision and runs by default.
func TestHTMLCollapsesIndentationLosslessly(t *testing.T) {
	if got := Detect(fetchedPage); got != KindHTML {
		t.Fatalf("Detect = %v, want html", got)
	}

	res := Transform(fetchedPage, DefaultOptions()) // AllowLossy is false
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Reason)
	}
	if res.Lossy {
		t.Error("whitespace collapsing must not be reported as lossy")
	}
	if !strings.Contains(res.Content, "The answer the agent came for.") {
		t.Error("the page's actual content did not survive")
	}
	if !strings.Contains(res.Content, "dataLayer") {
		t.Error("script content was dropped without AllowLossy")
	}
}

func TestHTMLDropsBrowserOnlyBlocksWhenAllowed(t *testing.T) {
	opts := DefaultOptions()
	opts.AllowLossy = true

	res := Transform(fetchedPage, opts)
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Reason)
	}
	if !res.Lossy {
		t.Error("dropping script and style discards content and must say so")
	}
	for _, gone := range []string{"dataLayer", "font-family", "regenerated on every deploy"} {
		if strings.Contains(res.Content, gone) {
			t.Errorf("%q survived", gone)
		}
	}
	if !strings.Contains(res.Content, "The answer the agent came for.") {
		t.Error("the page's actual content did not survive")
	}
}

// Inside <pre> the whitespace is the content.
func TestHTMLLeavesPreformattedAlone(t *testing.T) {
	in := `<html><body><pre>
  col1    col2
  a       b
</pre>` + strings.Repeat(`<div class="x">text</div>`, 6) + `</body></html>`

	res := Transform(in, DefaultOptions())
	if res.Applied {
		t.Errorf("rewrote a document containing <pre>: %s", res.Content)
	}
}

// Prose that opens with a tag is not a document.
func TestHTMLIgnoresProse(t *testing.T) {
	in := "<b>Note</b> the following, which is a sentence and not a page. " +
		strings.Repeat("It continues for a while so the block is large enough to look at. ", 12)
	if got := Detect(in); got == KindHTML {
		t.Error("classified prose as html")
	}
}

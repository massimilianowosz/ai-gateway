package livezone

import (
	"regexp"
	"strings"
)

var (
	htmlScriptRe  = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	htmlStyleRe   = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`)
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	// htmlIndentRe matches the whitespace a pretty-printer left between tags.
	htmlIndentRe = regexp.MustCompile(`>\s{2,}<`)
)

// looksLikeHTML recognises a fetched page or an XML document. It is
// reluctant: prose that happens to open with a tag is not a document, so a
// fragment has to carry enough closing tags to prove it is markup.
func looksLikeHTML(t string) bool {
	if t == "" || t[0] != '<' {
		return false
	}
	lower := strings.ToLower(t)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<?xml") {
		return true
	}
	return strings.Count(lower, "</") >= 5
}

// compactHTML removes what a document carries for a parser rather than for a
// reader.
//
// Collapsing the whitespace between tags is lossless: a run becomes a single
// space, which is what a renderer would have made of it anyway — skipped
// entirely when the document has a <pre> or a CDATA section, where whitespace
// is content. Dropping script, style and comment blocks does discard text, so
// it waits for AllowLossy: those blocks are never the answer to the question
// the agent asked, but that is a policy call rather than a free one.
func compactHTML(s string, allowLossy bool) (string, string, bool) {
	lower := strings.ToLower(s)
	out := s
	if !strings.Contains(lower, "<pre") && !strings.Contains(s, "<![CDATA[") {
		out = htmlIndentRe.ReplaceAllString(out, "> <")
	}
	collapsed := out != s

	if allowLossy {
		before := out
		out = htmlScriptRe.ReplaceAllString(out, "")
		out = htmlStyleRe.ReplaceAllString(out, "")
		out = htmlCommentRe.ReplaceAllString(out, "")
		if out != before {
			return out, "html_clean", true
		}
	}

	if !collapsed {
		return "", "", false
	}
	return out, "html_indent", false
}

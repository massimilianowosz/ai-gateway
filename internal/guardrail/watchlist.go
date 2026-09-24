package guardrail

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// WatchlistScanner matches terms an organisation named itself.
//
// It exists because the generic detectors answer a different question. A PERSON
// detector reports that somebody was mentioned, which nobody acts on; a
// watchlist reports that *this* name, this address, this number appeared,
// which is the alert an organisation actually wants. It has no false-positive
// problem by construction: the terms are declared, not inferred.
//
// Patterns are literal except for *, which matches any run of characters:
//
//	*assimilia*            anywhere in a word or a sentence
//	*.pippo@relatech.com   any local-part prefix on that address
//	Mario Rossi            the whole phrase, on word boundaries
//
// A term with no wildcard is bounded, so Rossi does not match Rossini. A term
// with one is not, because that is what asking for a wildcard means.
type WatchlistScanner struct {
	terms []watchTerm
}

type watchTerm struct {
	label string
	re    *regexp.Regexp
}

// WatchTerm is one configured entry. Label is what the finding is called, so
// an operator reads "CEO mobile" rather than the number itself.
type WatchTerm struct {
	Label   string
	Pattern string
}

func NewWatchlistScanner(terms []WatchTerm) *WatchlistScanner {
	s := &WatchlistScanner{}
	for _, t := range terms {
		re, ok := compileWatchTerm(t.Pattern)
		if !ok {
			continue
		}
		label := strings.TrimSpace(t.Label)
		if label == "" {
			label = "WATCHLIST"
		}
		s.terms = append(s.terms, watchTerm{label: label, re: re})
	}
	return s
}

// ValidWatchPattern reports whether a pattern is usable, so the console can
// refuse a term at the point somebody types it rather than silently storing
// one that never matches.
func ValidWatchPattern(pattern string) bool {
	_, ok := compileWatchTerm(pattern)
	return ok
}

// compileWatchTerm turns a wildcard pattern into a regexp. Everything but *
// is quoted, so a term containing a dot or a plus means those characters and
// not their regexp meaning — an operator writing an email address or a phone
// number is not writing a pattern language.
//
// * expands to a run of non-space characters rather than to anything at all.
// Greedy `.*` swallowed the rest of the line, so a term matched once per line
// and the reported value was the line: useless for judging an alert. Within a
// token it gives the answer wanted — massimilian* reports massimiliano and
// massimiliana separately. The cost is that a * cannot span a space, so
// "Mario*Rossi" will not match "Mario Rossi"; write the phrase instead.
//
// Word boundaries are added only where they mean something. \b between two
// non-word characters does not exist, so anchoring a term that starts with +
// or a digit-and-symbol phone number made it unmatchable.
func compileWatchTerm(pattern string) (*regexp.Regexp, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, false
	}

	parts := strings.Split(pattern, "*")
	leading := parts[0] == ""
	trailing := len(parts) > 1 && parts[len(parts)-1] == ""

	// The literal parts, with the empty edges left by an outer * removed.
	core := parts
	if leading {
		core = core[1:]
	}
	if trailing && len(core) > 0 {
		core = core[:len(core)-1]
	}
	if len(core) == 0 || strings.Join(core, "") == "" {
		return nil, false // nothing but wildcards
	}

	var b strings.Builder
	b.WriteString(`(?i)`)
	if !leading && isWordEdge(rune(pattern[0])) {
		b.WriteString(`\b`)
	}
	if leading {
		b.WriteString(`\S*`)
	}
	// Group 1 spans what the operator actually typed. Without it a term with
	// an outer * reports the whole token it landed in — a file path matched on
	// one word of it is shown as the path, with nothing saying which word.
	b.WriteString(`(`)
	for i, part := range core {
		b.WriteString(regexp.QuoteMeta(part))
		if i < len(core)-1 {
			b.WriteString(`\S*`)
		}
	}
	b.WriteString(`)`)
	if trailing {
		b.WriteString(`\S*`)
	}
	if !trailing && isWordEdge(rune(pattern[len(pattern)-1])) {
		b.WriteString(`\b`)
	}

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false
	}
	return re, true
}

// isWordEdge reports whether \b next to this character means anything.
func isWordEdge(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// Findings returns every term that appeared, with occurrence counts.
func (s *WatchlistScanner) Findings(text string) []DetectorMatch {
	return s.findings(text, 0)
}

// FindingsWithSamples also returns up to max distinct matched values per term.
func (s *WatchlistScanner) FindingsWithSamples(text string, max int) []DetectorMatch {
	return s.findings(text, max)
}

func (s *WatchlistScanner) findings(text string, max int) []DetectorMatch {
	if s == nil || text == "" || len(s.terms) == 0 {
		return nil
	}
	var out []DetectorMatch
	for _, t := range s.terms {
		spans := mentions(text, t.re)
		if len(spans) == 0 {
			continue
		}
		m := DetectorMatch{Name: t.label, Count: len(spans)}
		if max > 0 {
			seen := make(map[string]struct{}, max)
			for _, sp := range spans {
				if len(m.Samples) >= max {
					break
				}
				v := watchSample(text, sp[0], sp[1], sp[2], sp[3])
				if _, dup := seen[v]; dup || v == "" {
					continue
				}
				seen[v] = struct{}{}
				m.Samples = append(m.Samples, v)
			}
		}
		out = append(out, m)
	}
	return out
}

// Match reports whether any term matches the candidate as a whole — an MCP
// server name, a bare tool name or a file path — rather than searching for a
// mention inside free text. mentions' path exclusion below does not apply
// here: a firewall rule written for a file pattern exists specifically to
// match paths, not to ignore them.
func (s *WatchlistScanner) Match(candidate string) (label string, ok bool) {
	if s == nil || candidate == "" {
		return "", false
	}
	for _, t := range s.terms {
		if t.re.MatchString(candidate) {
			return t.label, true
		}
	}
	return "", false
}

// mentions returns the matches that are not part of a file path. A name in
// /Users/<name>/ is the machine's account, not a mention of the person, and on
// an agent's traffic it would outnumber every real hit.
func mentions(text string, re *regexp.Regexp) [][]int {
	var out [][]int
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		if !inPath(text, m[2], m[3]) {
			out = append(out, m)
		}
	}
	return out
}

const tokenDelims = " \t\r\n\"'`()<>[]{},;=|"

// inPath reports whether text[start:end] names a directory in a filesystem
// path. The file name itself still counts — CV_Mario_Rossi.pdf is about the
// person — and so do URLs and addresses.
func inPath(text string, start, end int) bool {
	from := strings.LastIndexAny(text[:start], tokenDelims) + 1
	to := len(text)
	if i := strings.IndexAny(text[end:], tokenDelims); i >= 0 {
		to = end + i
	}
	tok := text[from:to]
	if strings.Contains(tok, "://") || strings.Contains(tok, "@") || !strings.ContainsAny(tok, `/\`) {
		return false
	}
	if strings.ContainsAny(text[end:to], `/\`) {
		return true
	}
	// A home directory at the end of the path: cd /Users/name
	parent := strings.ToLower(text[from:start])
	if i := strings.LastIndexAny(parent, `/\`); i >= 0 {
		parent = parent[:i+1]
	}
	for _, home := range []string{"/users/", "/home/", `\users\`} {
		if strings.HasSuffix(parent, home) {
			return true
		}
	}
	return false
}

// watchSample keeps the matched term inside the value that gets reported.
//
// A term with an outer wildcard matches the whole surrounding token, and a
// token can be a file path. Truncating that from the left, as sample() does,
// cuts off the very word that caused the finding and leaves the operator
// looking at a path with no idea why it is there. The window is centred on
// the match instead.
func watchSample(text string, fullStart, fullEnd, coreStart, coreEnd int) string {
	if fullEnd-fullStart <= sampleLimit {
		return sample(text[fullStart:fullEnd])
	}

	pad := (sampleLimit - (coreEnd - coreStart)) / 2
	if pad < 0 {
		pad = 0
	}
	start := max(coreStart-pad, fullStart)
	end := start + sampleLimit
	if end > fullEnd {
		end = fullEnd
		start = max(end-sampleLimit, fullStart)
	}
	start, end = snapToRunes(text, start, end)

	out := text[start:end]
	if start > fullStart {
		out = "\u2026" + out
	}
	if end < fullEnd {
		out += "\u2026"
	}
	return out
}

// snapToRunes moves a byte window outwards onto rune boundaries, so slicing a
// path with an accent in it cannot produce a replacement character.
func snapToRunes(text string, start, end int) (int, int) {
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return start, end
}

// Redact replaces every watched term with a [LABEL] placeholder.
func (s *WatchlistScanner) Redact(text string) (string, []string) {
	if s == nil || text == "" || len(s.terms) == 0 {
		return text, nil
	}
	out := text
	var replaced []string
	for _, t := range s.terms {
		spans := mentions(out, t.re)
		if len(spans) == 0 {
			continue
		}
		var b strings.Builder
		last := 0
		for _, sp := range spans {
			b.WriteString(out[last:sp[0]])
			b.WriteString("[" + t.label + "]")
			last = sp[1]
		}
		b.WriteString(out[last:])
		out = b.String()
		replaced = append(replaced, t.label)
	}
	return out, replaced
}

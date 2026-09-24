package guardrail

import (
	"context"
	"regexp"
	"strings"
	"unicode"
)

// SecretsScanner detects leaked credentials and API keys using regex patterns.
type SecretsScanner struct{}

// NewSecretsScanner creates a secrets scanner.
func NewSecretsScanner() *SecretsScanner { return &SecretsScanner{} }

func (s *SecretsScanner) Name() string { return "secrets" }

func (s *SecretsScanner) Scan(ctx context.Context, userText, fullText string) (ScanResult, error) {
	for _, d := range secretDetectors {
		if d.observeOnly {
			continue
		}
		if d.re.MatchString(fullText) {
			return ScanResult{
				Blocked:  true,
				Reason:   "Secret detected: " + d.name,
				Scanner:  "secrets",
				Category: d.name,
			}, nil
		}
	}
	return ScanResult{Scanner: "secrets"}, nil
}

// DetectorMatch counts how often one detector fired.
//
// Samples is empty unless the caller asked for it through a WithSamples
// method. The default carries no matched text on purpose: a findings log that
// held the values would be the easiest place in the appliance to harvest
// exactly what it was built to detect. The opt-in exists for diagnosing a
// detector — deciding whether a count is real data or noise — and whatever
// enables it is responsible for how long the values then live.
type DetectorMatch struct {
	Name    string
	Count   int
	Samples []string
}

// sampleLimit bounds one sample. Long enough to recognise what matched,
// short enough that a key or a document does not land in the store whole.
const sampleLimit = 80

func sample(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= sampleLimit {
		return s
	}
	return s[:sampleLimit] + "\u2026"
}

// Findings returns every detector that matched, with occurrence counts.
//
// Scan stops at the first hit because it only has to decide whether to block.
// Traffic analysis needs the whole picture instead: one response can leak
// several distinct credentials, and "which kinds, how many" is the answer an
// operator acts on.
func (s *SecretsScanner) Findings(text string) []DetectorMatch {
	return s.findings(text, 0)
}

// FindingsWithSamples also returns up to max distinct matched values per
// detector. See DetectorMatch on what keeping them costs.
func (s *SecretsScanner) FindingsWithSamples(text string, max int) []DetectorMatch {
	return s.findings(text, max)
}

func (s *SecretsScanner) findings(text string, max int) []DetectorMatch {
	if text == "" {
		return nil
	}
	var out []DetectorMatch
	for _, d := range secretDetectors {
		spans := d.spans(text)
		if len(spans) == 0 {
			continue
		}
		m := DetectorMatch{Name: d.name, Count: len(spans)}
		if max > 0 {
			seen := make(map[string]struct{}, max)
			for _, sp := range spans {
				if len(m.Samples) >= max {
					break
				}
				v := sample(text[sp[0]:sp[1]])
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

// Redact replaces every detected secret with a [NAME] placeholder and returns
// the rewritten text plus the detector names it replaced. It mirrors
// PIIScanner.Redact, which the request-side middleware already relies on.
func (s *SecretsScanner) Redact(text string) (string, []string) {
	out := text
	var redacted []string

	for _, d := range secretDetectors {
		var b strings.Builder
		last := 0
		for _, m := range d.spans(out) {
			start, end := m[0], m[1]
			if start < last { // overlaps a span already rewritten
				continue
			}
			b.WriteString(out[last:start])
			b.WriteByte('[')
			b.WriteString(d.name)
			b.WriteByte(']')
			last = end
		}
		if last == 0 {
			continue
		}
		b.WriteString(out[last:])
		out = b.String()
		redacted = append(redacted, d.name)
	}

	return out, redacted
}

type secretDetector struct {
	name string
	re   *regexp.Regexp
	// valid, when set, receives the first capture group and rejects matches
	// that only have the shape of a secret.
	valid func(string) bool
	// observeOnly detectors report and redact but never block: their shapes
	// also turn up in config files and code an agent legitimately reads.
	observeOnly bool
}

func (d secretDetector) spans(text string) [][]int {
	var out [][]int
	for _, m := range d.re.FindAllStringSubmatchIndex(text, -1) {
		if d.valid != nil && (len(m) < 4 || m[2] < 0 || !d.valid(text[m[2]:m[3]])) {
			continue
		}
		start := m[0]
		for start < m[1] && unicode.IsSpace(rune(text[start])) {
			start++
		}
		out = append(out, []int{start, m[1]})
	}
	return out
}

// realPassword rejects the stand-ins documentation and templates use in place
// of a password, and single words, which in prose are sentences, not secrets.
func realPassword(p string) bool {
	p = strings.Trim(p, `"'`+"`")
	if len(p) < 6 || strings.ContainsAny(p[:1], "$<{%[") {
		return false
	}
	lower := strings.ToLower(p)
	for _, stub := range []string{"password", "passwd", "your", "example", "changeme", "secret", "redacted", "placeholder"} {
		if strings.Contains(lower, stub) {
			return false
		}
	}
	if strings.Count(p, p[:1]) == len(p) {
		return false // xxxxxx, ******
	}
	var letter, other bool
	for _, r := range p {
		if unicode.IsLetter(r) {
			letter = true
		} else {
			other = true
		}
	}
	return letter && other
}

var secretDetectors = []secretDetector{
	{name: "AWS_ACCESS_KEY", re: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{name: "AWS_SECRET_KEY", re: regexp.MustCompile(`(?i)(?:aws_secret_access_key|secret_key)\s*[=:]\s*[A-Za-z0-9/+=]{40}`)},
	{name: "GITHUB_TOKEN", re: regexp.MustCompile(`\b(?:ghp|gho|ghs|ghr|github_pat)_[A-Za-z0-9_]{30,255}\b`)},
	{name: "GITLAB_TOKEN", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9\-_]{20,}\b`)},
	{name: "SLACK_TOKEN", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{24,}\b`)},
	{name: "SLACK_WEBHOOK", re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`)},
	{name: "STRIPE_KEY", re: regexp.MustCompile(`\b(?:sk|pk)_(?:live|test)_[A-Za-z0-9]{24,}\b`)},
	{name: "OPENAI_KEY", re: regexp.MustCompile(`\bsk-(?:proj-|live-|ant-)?[A-Za-z0-9_-]{20,}\b`)},
	{name: "GOOGLE_API_KEY", re: regexp.MustCompile(`\bAIza[A-Za-z0-9_\-]{35}\b`)},
	{name: "PRIVATE_KEY", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`)},
	{name: "GENERIC_SECRET", re: regexp.MustCompile(`(?i)(?:password|passwd|secret|token|api_key|apikey)\s*[=:]\s*["']?[A-Za-z0-9/+=_\-]{16,}["']?`)},
	// Header and payload both open with eyJ, base64 for `{"`. Unsigned tokens
	// (alg none, empty signature) grant nothing and are left out.
	{name: "JWT", observeOnly: true, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}`)},
	{name: "CREDENTIAL_URL", observeOnly: true, valid: realPassword,
		re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:([^\s/@]+)@[^\s/?#]+`)},
	{name: "BASIC_AUTH", observeOnly: true, valid: realPassword,
		re: regexp.MustCompile(`(?:^|\s)(?:-u|--user)[ =]['"]?[^\s:'"]+:([^\s'"]+)`)},
	{name: "AUTH_HEADER", observeOnly: true,
		re: regexp.MustCompile(`(?i)\bauthorization:\s*(?:basic|bearer)\s+[A-Za-z0-9._~+/-]{16,}={0,2}`)},
	{name: "PASSWORD", observeOnly: true, valid: realPassword,
		re: regexp.MustCompile(`(?i)\b(?:password|passwd|passcode|pwd)\s+(?:is|was|è|e')\s+([^\s,;]*[^\s,;.!?:)])`)},
}

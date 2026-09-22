package guardrail

import (
	"context"
	"net"
	"regexp"
	"strings"
	"unicode"
)

// PIIScanner detects personally identifiable information using compiled regex
// patterns. Every pattern carries the entity id the portal shows in its PII
// entity list (EMAIL_ADDRESS, PHONE_NUMBER, PERSON, …), so one tenant selection
// drives both policies built on this scanner: PII Redaction replaces matches
// with [ENTITY] and forwards, PII Blocking refuses the request.
type PIIScanner struct{}

// NewPIIScanner creates a PII scanner.
func NewPIIScanner() *PIIScanner { return &PIIScanner{} }

func (s *PIIScanner) Name() string { return "pii" }

// Scan runs every detector: the behaviour for a tenant that has never
// configured entities.
func (s *PIIScanner) Scan(ctx context.Context, userText, fullText string) (ScanResult, error) {
	return s.scan(fullText, nil), nil
}

// ScanWithEntities limits detection to the entity identifiers selected in the
// portal. An empty list intentionally runs no portal-managed detector: it means
// the tenant unticked every box, not that it configured nothing.
func (s *PIIScanner) ScanWithEntities(ctx context.Context, userText, fullText string, enabledEntities []string) (ScanResult, error) {
	return s.scan(fullText, allowedEntities(enabledEntities)), nil
}

// Redact replaces every match of the enabled entities with an [ENTITY]
// placeholder and returns the rewritten text plus the entities it replaced.
// A nil selection means "not configured": every detector applies.
func (s *PIIScanner) Redact(text string, enabledEntities *[]string) (string, []string) {
	var allowed map[string]bool
	if enabledEntities != nil {
		allowed = allowedEntities(*enabledEntities)
	}

	out := text
	var redacted []string

	for _, d := range piiDetectors {
		if !d.enabled(allowed) {
			continue
		}

		var b strings.Builder
		last := 0
		for _, m := range d.re.FindAllStringSubmatchIndex(out, -1) {
			start, end := detectorSpan(m, d.group)
			if start < last { // group absent, or overlapping a span already rewritten
				continue
			}
			if d.validate != nil && !d.validate(out[start:end]) {
				continue
			}
			b.WriteString(out[last:start])
			b.WriteString("[" + d.entity + "]")
			last = end
		}
		if last == 0 {
			continue
		}
		b.WriteString(out[last:])
		out = b.String()
		redacted = append(redacted, d.entity)
	}

	return out, redacted
}

// scan reports the first enabled entity found. A nil allowed map means no
// tenant selection exists, so every detector runs.
func (s *PIIScanner) scan(text string, allowed map[string]bool) ScanResult {
	for _, d := range piiDetectors {
		if !d.enabled(allowed) {
			continue
		}
		for _, m := range d.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := detectorSpan(m, d.group)
			if start < 0 {
				continue
			}
			if d.validate != nil && !d.validate(text[start:end]) {
				continue
			}
			return ScanResult{
				Blocked:  true,
				Reason:   "PII detected: " + d.entity,
				Scanner:  "pii",
				Category: d.entity,
			}
		}
	}
	return ScanResult{Scanner: "pii"}
}

// allowedEntities normalises a portal selection into a lookup set.
func allowedEntities(entities []string) map[string]bool {
	allowed := make(map[string]bool, len(entities))
	for _, e := range entities {
		allowed[strings.ToUpper(strings.TrimSpace(e))] = true
	}
	return allowed
}

// detectorSpan returns the byte range a detector considers sensitive: the whole
// match, or the capture group when the pattern only uses context to find it.
// A group that did not participate in the match yields (-1, -1).
func detectorSpan(match []int, group int) (int, int) {
	if group <= 0 || 2*group+1 >= len(match) {
		return match[0], match[1]
	}
	return match[2*group], match[2*group+1]
}

// piiDetector is a compiled pattern with optional validation.
type piiDetector struct {
	entity   string         // portal entity id, also the redaction placeholder
	re       *regexp.Regexp //
	group    int            // capture group holding the sensitive span (0 = whole match)
	alwaysOn bool           // no portal checkbox controls this one
	validate func(string) bool
}

// enabled reports whether the detector runs for a given tenant selection.
// A nil map means the tenant has no configuration and everything applies.
func (d piiDetector) enabled(allowed map[string]bool) bool {
	if d.alwaysOn || allowed == nil {
		return true
	}
	return allowed[d.entity]
}

// piiDetectors are compiled once at package init. Order matters for redaction:
// the structured identifiers come first so that a card number or an IBAN is
// replaced as a whole before the looser phone and IP patterns see its digits.
var piiDetectors = []piiDetector{
	// === Italian national identifiers ===
	// No portal checkbox represents these, so they stay on whatever the tenant
	// selects: unticking every box must not quietly drop coverage the scanner
	// had before the entity list existed.
	{
		entity:   "CODICE_FISCALE",
		re:       regexp.MustCompile(`(?i)\b[A-Z]{6}[0-9]{2}[A-Z][0-9]{2}[A-Z][0-9]{3}[A-Z]\b`),
		alwaysOn: true,
		validate: validateCodiceFiscale,
	},
	{
		entity:   "PARTITA_IVA",
		re:       regexp.MustCompile(`(?i)(?:P\.?\s?IVA|partita\s+iva)\b.{0,30}?\b(?:IT\s?)?([0-9]{11})\b`),
		group:    1,
		alwaysOn: true,
	},
	// === Portal entity list ===
	{
		entity: "IBAN_CODE",
		re:     regexp.MustCompile(`(?i)\bIT\s?[0-9]{2}\s?[A-Z]\s?(?:[0-9]{4}\s?){5}[0-9]{2}\b`),
	},
	{
		entity: "IBAN_CODE",
		re:     regexp.MustCompile(`(?i)\b[A-Z]{2}[0-9]{2}\s?[A-Z0-9]{4}\s?(?:[A-Z0-9]{4}\s?){2,7}[A-Z0-9]{1,4}\b`),
	},
	{
		entity:   "CREDIT_CARD",
		re:       regexp.MustCompile(`\b(?:[0-9]{4}[\s-]?){3}[0-9]{4}\b`),
		validate: validateLuhn,
	},
	{
		entity: "CRYPTO",
		re:     regexp.MustCompile(`\b(?:0x[a-fA-F0-9]{40}|bc1[ac-hj-np-z02-9]{11,71}|[13][a-km-zA-HJ-NP-Z1-9]{25,34})\b`),
	},
	{
		entity: "US_SSN",
		re:     regexp.MustCompile(`\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`),
	},
	{
		entity: "US_BANK_NUMBER",
		re:     regexp.MustCompile(`(?i)\b(?:bank\s+account|account\s+(?:number|no\.?|#)|routing\s+number)\b\D{0,12}?\b([0-9]{8,17})\b`),
		group:  1,
	},
	{
		entity: "EMAIL_ADDRESS",
		re:     regexp.MustCompile(`(?i)\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),
	},
	{
		// The number must start where a number can start. Without the leading
		// context guard the international "00" prefix matches inside any long
		// digit run — the middle of an IBAN or of a crypto address — which
		// mangles those entities whenever a tenant selects phones but not them.
		entity: "PHONE_NUMBER",
		re: regexp.MustCompile(`(?:^|[^0-9A-Za-z+])` +
			`((?:\+|00)[1-9][0-9]{0,2}[\s.\-]?[0-9](?:[\s.\-]?[0-9]){6,13}|3[0-9]{2}[\s.\-]?[0-9]{6,7})\b`),
		group: 1,
	},
	{
		entity: "IP_ADDRESS",
		re: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b` +
			`|(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}`),
		validate: validateIPAddress,
	},
	// PERSON and LOCATION have no self-contained shape: they are recognised by
	// the phrase that introduces them, and only the name itself is replaced.
	// The cue is case-insensitive, the name is not.
	{
		entity: "PERSON",
		re: regexp.MustCompile(`(?i:mi\s+chiamo|il\s+mio\s+nome\s+è|my\s+name\s+is|sig\.ra|sig\.|signora|signor|dott\.ssa|dott\.|dr\.|mr\.|mrs\.|ms\.)\s+` +
			`(\p{Lu}[\p{L}']+(?:\s+\p{Lu}[\p{L}']+){0,2})`),
		group: 1,
	},
	{
		entity: "LOCATION",
		re: regexp.MustCompile(`(?i:residente\s+(?:a|in)|abito\s+(?:a|in)|città\s+di|vivo\s+(?:a|in)|lives?\s+in|living\s+in|located\s+in|based\s+in)\s+` +
			`(\p{Lu}[\p{L}']+(?:[\s'-]\p{Lu}[\p{L}']+){0,2})`),
		group: 1,
	},
}

func validateIPAddress(value string) bool {
	return net.ParseIP(value) != nil
}

// --- Codice Fiscale checksum validation ---

// validateCodiceFiscale verifies the check digit (last character) of an Italian tax code.
func validateCodiceFiscale(raw string) bool {
	cf := strings.ToUpper(strings.TrimSpace(raw))
	if len(cf) != 16 {
		return false
	}
	for _, r := range cf {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}

	sum := 0
	for i := 0; i < 15; i++ {
		c := cf[i]
		if i%2 == 0 {
			// odd positions (1-indexed) → use odd table
			sum += cfOddValue(c)
		} else {
			// even positions → use even table
			sum += cfEvenValue(c)
		}
	}

	expected := byte('A' + sum%26)
	return cf[15] == expected
}

func cfEvenValue(c byte) int {
	if c >= '0' && c <= '9' {
		return int(c - '0')
	}
	return int(c - 'A')
}

func cfOddValue(c byte) int {
	// Italian CF odd-position value table
	table := map[byte]int{
		'0': 1, '1': 0, '2': 5, '3': 7, '4': 9,
		'5': 13, '6': 15, '7': 17, '8': 19, '9': 21,
		'A': 1, 'B': 0, 'C': 5, 'D': 7, 'E': 9,
		'F': 13, 'G': 15, 'H': 17, 'I': 19, 'J': 21,
		'K': 2, 'L': 4, 'M': 18, 'N': 20, 'O': 11,
		'P': 3, 'Q': 6, 'R': 8, 'S': 12, 'T': 14,
		'U': 16, 'V': 10, 'W': 22, 'X': 25, 'Y': 24,
		'Z': 23,
	}
	return table[c]
}

// --- Credit Card Luhn validation ---

func validateLuhn(raw string) bool {
	// Strip spaces and dashes
	var digits []int
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}

	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

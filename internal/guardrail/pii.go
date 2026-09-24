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

// PIITypes lists every entity the scanner can report, in detector order.
func PIITypes() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range piiDetectors {
		if !seen[d.entity] {
			seen[d.entity] = true
			out = append(out, d.entity)
		}
	}
	return out
}

// PIIReportedByDefault reports whether traffic analysis shows an entity until
// an operator says otherwise. PERSON is off: a name being mentioned is not
// something anyone acts on, and the watchlist covers the names that matter.
func PIIReportedByDefault(entity string) bool { return entity != "PERSON" }

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

// Findings returns every enabled entity that matched, with occurrence counts.
//
// scan stops at the first hit because blocking only needs one reason. Traffic
// analysis needs the full inventory instead: an operator asking what personal
// data went through a session is asking which entities and how many, not which
// one happened to sort first.
func (s *PIIScanner) Findings(text string, enabledEntities *[]string) []DetectorMatch {
	return s.findings(text, enabledEntities, 0)
}

// FindingsWithSamples also returns up to max distinct matched values per
// entity. See DetectorMatch on what keeping them costs.
func (s *PIIScanner) FindingsWithSamples(text string, enabledEntities *[]string, max int) []DetectorMatch {
	return s.findings(text, enabledEntities, max)
}

func (s *PIIScanner) findings(text string, enabledEntities *[]string, max int) []DetectorMatch {
	if text == "" {
		return nil
	}
	var allowed map[string]bool
	if enabledEntities != nil {
		allowed = allowedEntities(*enabledEntities)
	}

	var out []DetectorMatch
	for _, d := range piiDetectors {
		if !d.enabled(allowed) {
			continue
		}
		count := 0
		var samples []string
		seen := map[string]struct{}{}
		for _, m := range d.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := detectorSpan(m, d.group)
			if start < 0 {
				continue
			}
			if d.validate != nil && !d.validate(text[start:end]) {
				continue
			}
			count++
			if max > 0 && len(samples) < max {
				v := sample(text[start:end])
				if _, dup := seen[v]; !dup && v != "" {
					seen[v] = struct{}{}
					samples = append(samples, v)
				}
			}
		}
		if count > 0 {
			out = append(out, DetectorMatch{Name: d.entity, Count: count, Samples: samples})
		}
	}
	return out
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
		entity:   "IBAN_CODE",
		re:       regexp.MustCompile(`(?i)\bIT\s?[0-9]{2}\s?[A-Z]\s?(?:[0-9]{4}\s?){5}[0-9]{2}\b`),
		validate: validateIBAN,
	},
	// Written either compact or in groups of four; a free mix of the two is what
	// let ordinary words ("RY506AX9 Orologio Solare") pass for an IBAN.
	{
		entity:   "IBAN_CODE",
		re:       regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}(?:[A-Z0-9]{11,30}|(?: [A-Z0-9]{4}){2,7}(?: [A-Z0-9]{1,4})?)\b`),
		validate: validateIBAN,
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
		entity:   "IP_ADDRESS",
		re:       regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b`),
		validate: validateIPAddress,
	},
	{
		// IPv6 separately, because \b cannot delimit it: a colon is already a
		// non-word character, so ::c inside std::cout looked like a bounded
		// address — and net.ParseIP agrees that ::c is valid, so validation did
		// not catch it either. Every C++ scope operator in a session was being
		// counted as somebody's IP. RE2 has no lookaround, so the delimiters are
		// matched explicitly and the address is taken from the group.
		entity:   "IP_ADDRESS",
		re:       regexp.MustCompile(`(?:^|[^0-9A-Za-z:])((?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4})(?:[^0-9A-Za-z:]|$)`),
		group:    1,
		validate: validateIPAddress,
	},
	// PERSON and LOCATION have no self-contained shape: they are recognised by
	// the phrase that introduces them, and only the name itself is replaced.
	// The cue is case-insensitive, the name is not.
	//
	// The leading \b keeps the cue honest. Without it a phrase boundary inside
	// a longer word matched, and the capitalised word that opened the next
	// sentence was reported as a name or a place.
	//
	// A cue only catches someone who introduces themselves, so this misses
	// every name mentioned in passing. The watchlist covers the names an
	// organisation actually cares about; this stays for the rest.
	{
		entity: "PERSON",
		re: regexp.MustCompile(`\b(?i:mi\s+chiamo|il\s+mio\s+nome\s+è|my\s+name\s+is|sig\.ra|sig\.|signora|signor|dott\.ssa|dott\.|dr\.|mr\.|mrs\.|ms\.)\s+` +
			`(\p{Lu}[\p{L}']+(?:\s+\p{Lu}[\p{L}']+){0,2})`),
		group: 1,
	},
	{
		entity: "LOCATION",
		re: regexp.MustCompile(`\b(?i:residente\s+(?:a|in)|abito\s+(?:a|in)|città\s+di|vivo\s+(?:a|in)|lives?\s+in|living\s+in|located\s+in|based\s+in)\s+` +
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

// --- IBAN validation (ISO 13616) ---

// ibanLengths is the SWIFT registry: a country code the registry does not list,
// or the wrong length for it, is not an IBAN whatever its digits say.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22, "BH": 22, "BR": 29,
	"BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22, "DK": 18, "DO": 28, "EE": 20, "EG": 29,
	"ES": 24, "FI": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28,
	"HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "MC": 27, "MD": 24, "ME": 22, "MK": 19,
	"MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15, "PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29,
	"RO": 24, "RS": 22, "SA": 24, "SC": 31, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "ST": 25, "SV": 28,
	"TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
}

func validateIBAN(raw string) bool {
	iban := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	if len(iban) < 4 || ibanLengths[iban[:2]] != len(iban) {
		return false
	}
	// Check digits: the first four characters moved to the end, letters as
	// 10–35, must leave 1 modulo 97.
	rem := 0
	for _, r := range iban[4:] + iban[:4] {
		switch {
		case r >= '0' && r <= '9':
			rem = (rem*10 + int(r-'0')) % 97
		case r >= 'A' && r <= 'Z':
			rem = (rem*100 + int(r-'A') + 10) % 97
		default:
			return false
		}
	}
	return rem == 1
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

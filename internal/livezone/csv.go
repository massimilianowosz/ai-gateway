package livezone

import (
	"encoding/csv"
	"fmt"
	"strings"
)

// looksLikeCSV recognises delimiter-separated rows: many lines, a stable field
// count, and a delimiter that actually separates something.
//
// The stable-field-count requirement is what keeps prose out. A paragraph
// containing commas has a wildly varying "field count" per line; a CSV does
// not.
func looksLikeCSV(t string) (rune, bool) {
	lines := strings.Split(strings.TrimRight(t, "\n"), "\n")
	if len(lines) < 10 {
		return 0, false
	}
	// Bound the probe: classification must not cost more than the transform.
	sample := t
	if len(lines) > 500 {
		sample = strings.Join(lines[:500], "\n")
	}

	for _, delim := range []rune{',', '\t', ';', '|'} {
		if !strings.ContainsRune(lines[0], delim) {
			continue
		}
		// Field counting goes through the CSV parser, not a character count:
		// a quoted field may legitimately contain the delimiter, and counting
		// raw separators would reject exactly the tables that need quoting.
		r := csv.NewReader(strings.NewReader(sample))
		r.Comma = delim
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		rows, err := r.ReadAll()
		if err != nil || len(rows) < 10 {
			continue
		}
		want := len(rows[0])
		if want < 2 {
			continue
		}
		consistent := 0
		for _, row := range rows {
			if len(row) == want {
				consistent++
			}
		}
		if consistent < len(rows)*9/10 || consistent < 10 {
			continue
		}
		// Consistency alone is not enough: prose repeating the same sentence
		// has a perfectly consistent field count. A real table's header names
		// columns, so its fields are short and mostly single words.
		if !headerLooksTabular(rows[0]) {
			continue
		}
		return delim, true
	}
	return 0, false
}

// headerLooksTabular checks that the first row names columns rather than
// being a sentence that happens to contain the delimiter.
func headerLooksTabular(fields []string) bool {
	if len(fields) < 2 {
		return false
	}
	withSpaces, total := 0, 0
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		total++
		if len(f) > 40 {
			return false // a sentence, not a column name
		}
		if strings.Contains(f, " ") {
			withSpaces++
		}
	}
	if total < 2 {
		return false
	}
	// Column names like "First Name" exist, but a majority of spaced fields
	// means prose.
	return withSpaces*3 <= total
}

// CSVConfig bounds tabular truncation.
type CSVConfig struct {
	// MinRows is the row count below which nothing is dropped.
	MinRows int
	// KeepHead and KeepTail are the data rows kept at each end, excluding the
	// header row, which is always kept.
	KeepHead, KeepTail int
}

// DefaultCSVConfig returns conservative limits.
func DefaultCSVConfig() CSVConfig { return CSVConfig{MinRows: 30, KeepHead: 10, KeepTail: 5} }

// compactCSV shortens a delimited table.
//
// It is the tabular twin of JSON array truncation and follows the same rule:
// keep the shape and the exceptions. The header row always survives, so the
// agent still knows what the columns mean; the first and last data rows
// survive; and any row mentioning an error or failure survives wherever it
// sits. A trailing line records how many rows were dropped.
//
// Quoting is preserved by re-encoding through encoding/csv rather than
// slicing text, so a field containing the delimiter or a newline stays intact.
func compactCSV(s string, delim rune, cfg CSVConfig) (string, bool) {
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = delim
	r.FieldsPerRecord = -1 // tolerate ragged rows rather than failing
	r.LazyQuotes = true

	rows, err := r.ReadAll()
	if err != nil || len(rows) < cfg.MinRows {
		return "", false
	}

	header, data := rows[0], rows[1:]
	if len(data) <= cfg.KeepHead+cfg.KeepTail {
		return "", false
	}

	keep := make([]bool, len(data))
	for i := 0; i < cfg.KeepHead && i < len(data); i++ {
		keep[i] = true
	}
	for i := 0; i < cfg.KeepTail && i < len(data); i++ {
		keep[len(data)-1-i] = true
	}
	for i, row := range data {
		if keep[i] {
			continue
		}
		if notableRe.MatchString(strings.Join(row, " ")) {
			keep[i] = true
		}
	}

	out := make([][]string, 0, len(data)+2)
	out = append(out, header)
	omitted := 0
	for i, row := range data {
		if keep[i] {
			out = append(out, row)
			continue
		}
		omitted++
	}
	if omitted == 0 {
		return "", false
	}

	var sb strings.Builder
	w := csv.NewWriter(&sb)
	w.Comma = delim
	if err := w.WriteAll(out); err != nil {
		return "", false
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", false
	}

	fmt.Fprintf(&sb, "# %d of %d data rows omitted by ubiquum live compression; "+
		"header, first %d, last %d and all rows reporting errors were kept\n",
		omitted, len(data), cfg.KeepHead, cfg.KeepTail)
	return sb.String(), true
}

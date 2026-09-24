package guardrail

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchlist_WildcardsMatchWhatWasAskedFor(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{
		{Label: "CEO alias", Pattern: "*assimilia*"},
		{Label: "CEO mailbox", Pattern: "*.pippo@relatech.com"},
	})

	cases := []struct {
		text string
		want string
	}{
		{"scrivi a massimiliano oggi", "CEO alias"},
		{"MASSIMILIANO in maiuscolo", "CEO alias"},
		{"manda a mario.pippo@relatech.com il report", "CEO mailbox"},
		{"manda a anna.pippo@relatech.com il report", "CEO mailbox"},
	}
	for _, c := range cases {
		found := s.Findings(c.text)
		require.Len(t, found, 1, c.text)
		assert.Equal(t, c.want, found[0].Name)
	}
}

// A term without a wildcard is a whole phrase, or every surname would drag in
// every longer name that starts the same way.
func TestWatchlist_PlainTermIsWordBounded(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CFO", Pattern: "Rossi"}})

	assert.Len(t, s.Findings("ha firmato Rossi ieri"), 1)
	assert.Empty(t, s.Findings("il concerto di Rossini"))
}

// An operator typing an address or a number is not writing a pattern: the
// characters must mean themselves.
func TestWatchlist_PatternCharactersAreLiteral(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO mobile", Pattern: "+39 333 1234567"}})

	assert.Len(t, s.Findings("chiama il +39 333 1234567 subito"), 1)
	assert.Empty(t, s.Findings("chiama il 39 333 1234567 subito"),
		"the plus is a character, not a quantifier")
}

func TestWatchlist_SamplesAreDistinct(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO alias", Pattern: "massimilian*"}})
	found := s.FindingsWithSamples("massimiliano e massimiliana e massimiliano", 5)

	require.Len(t, found, 1)
	assert.Equal(t, 3, found[0].Count)
	assert.Len(t, found[0].Samples, 2, "a repeated value must not fill the list")
}

func TestWatchlist_IgnoresEmptyAndUnusable(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{
		{Label: "blank", Pattern: "   "},
		{Label: "", Pattern: "ok"},
	})
	found := s.Findings("ok")
	require.Len(t, found, 1)
	assert.Equal(t, "WATCHLIST", found[0].Name, "an unlabelled term still needs a name")
}

func TestWatchlist_RedactsByLabel(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO mobile", Pattern: "+39 333 1234567"}})
	out, replaced := s.Redact("chiama il +39 333 1234567 subito")

	assert.Equal(t, "chiama il [CEO mobile] subito", out)
	assert.Equal(t, []string{"CEO mobile"}, replaced)
}

func TestWatchlist_EmptyScannerIsInert(t *testing.T) {
	s := NewWatchlistScanner(nil)
	assert.Empty(t, s.Findings("qualsiasi testo"))
	out, replaced := s.Redact("qualsiasi testo")
	assert.Equal(t, "qualsiasi testo", out)
	assert.Empty(t, replaced)
}

func TestWatchlist_RejectsPatternOfOnlyWildcards(t *testing.T) {
	assert.False(t, ValidWatchPattern("*"))
	assert.False(t, ValidWatchPattern("***"))
	assert.True(t, ValidWatchPattern("*a*"))
}

// A long path truncated from the left hides the word that caused the finding,
// which is the one thing the operator opened the panel to see.
func TestWatchlist_SampleKeepsTheMatchVisible(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO", Pattern: "*assimilia*"}})
	path := "/Users/deeply/nested/directory/that/goes/on/and/on/for/a/very/long/while/massimiliano-notes.txt"
	require.Greater(t, len(path), sampleLimit, "the case only exists past the limit")

	found := s.FindingsWithSamples("ho aperto "+path, 5)

	require.Len(t, found, 1)
	require.Len(t, found[0].Samples, 1)
	assert.Contains(t, found[0].Samples[0], "assimilia")
	assert.LessOrEqual(t, len(found[0].Samples[0]), sampleLimit+8, "window plus ellipses")
}

func TestWatchlist_ShortMatchIsReportedWhole(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO", Pattern: "*assimilia*"}})
	found := s.FindingsWithSamples("scrivi a massimiliano oggi", 5)

	require.Len(t, found, 1)
	assert.Equal(t, []string{"massimiliano"}, found[0].Samples)
}

// The account a machine runs under is not a mention of its owner.
func TestWatchlist_DirectoryInAPathIsNotAMention(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO", Pattern: "*assimilia*"}})
	for _, text := range []string{
		`{"file_path":"/Users/massimilianowosz/code/app/main.go"}`,
		"cd /Users/massimilianowosz && ls",
		`C:\Users\massimiliano\Desktop\notes.txt`,
		"~/massimiliano/src/x.go",
		"/home/massimiliano",
	} {
		assert.Empty(t, s.Findings(text), text)
		out, _ := s.Redact(text)
		assert.Equal(t, text, out, "a path must survive redaction")
	}
}

func TestWatchlist_FileNamesURLsAndAddressesStillCount(t *testing.T) {
	s := NewWatchlistScanner([]WatchTerm{{Label: "CEO", Pattern: "*assimilia*"}})
	for _, text := range []string{
		"/tmp/cv-massimiliano.pdf",
		"https://linkedin.com/in/massimilianowosz",
		"massimiliano@relatech.com",
		"ho parlato con massimiliano ieri /Users/massimilianowosz/x",
	} {
		found := s.Findings(text)
		require.Len(t, found, 1, text)
		assert.Equal(t, 1, found[0].Count, text)
	}
}

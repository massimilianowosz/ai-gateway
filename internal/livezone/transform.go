// Package livezone compresses the newest content entering a prompt — the
// output of the tool call the agent just made — before it reaches the provider.
//
// It deliberately touches only the *live zone*: everything from the newest
// real user turn onward, which in an agentic loop is that turn plus the tool
// results it produced. System messages, the tools array and earlier turns are
// never rewritten. Those sit inside the region a provider prompt cache covers,
// so editing them re-bills every surviving token at full input price. See
// internal/hivestate/prefix_cache.go for the economics.
//
// The live zone's own bytes may well be cached by the time the next request
// arrives, which is why every transform is deterministic: re-compressing an
// already-compressed block yields the same bytes, so the prefix stays stable
// turn to turn and the saving is taken once rather than paid for repeatedly.
//
// Every transform is deterministic, bounded, and fails open: an error, a panic
// or an output that did not actually get smaller all yield the original bytes.
package livezone

import (
	"fmt"
	"strings"
)

// Kind identifies what a block of content looks like, which selects the
// transformer. Detection is heuristic and conservative — Unknown means "leave
// it alone".
type Kind int

const (
	KindUnknown Kind = iota
	KindJSON
	KindLogs
	KindDiff
	KindCSV
	KindSearch
	KindHTML
	KindTable
	KindBlob
)

func (k Kind) String() string {
	switch k {
	case KindJSON:
		return "json"
	case KindLogs:
		return "logs"
	case KindDiff:
		return "diff"
	case KindCSV:
		return "csv"
	case KindSearch:
		return "search"
	case KindHTML:
		return "html"
	case KindTable:
		return "table"
	case KindBlob:
		return "blob"
	default:
		return "unknown"
	}
}

// Result describes one transform attempt.
type Result struct {
	// Content is the text to forward. On any refusal it is the input unchanged.
	Content string
	// Applied is true when Content differs from the input.
	Applied bool
	// Kind is the detected content type.
	Kind Kind
	// Transformer names what ran, for metrics and the diagnostic header.
	Transformer string
	// Reason explains a refusal, empty when Applied.
	Reason string
	// BytesBefore and BytesAfter measure the change.
	BytesBefore, BytesAfter int
	// Lossy is true when information was discarded. Lossless transforms only
	// remove encoding overhead, so they are safe to apply unconditionally.
	Lossy bool
}

// Options bound what a transform may do.
type Options struct {
	// MinBytes is the size below which content is left alone. Compressing a
	// small block cannot repay the risk or the CPU.
	MinBytes int
	// MaxBytes caps the input a transform will look at, so a pathological
	// payload cannot stall the request path. Larger content passes through.
	MaxBytes int
	// AllowLossy enables transforms that discard information. Off by default:
	// lossless compaction is free, discarding content is a policy decision.
	AllowLossy bool
	// MinGainRatio is the fraction of bytes a transform must remove to be
	// worth applying (0.05 = must save at least 5%). A marginal win is not
	// worth changing the bytes the model sees.
	MinGainRatio float64
	// JSONTruncate bounds array truncation. Only consulted when AllowLossy.
	JSONTruncate JSONTruncateConfig
	// Diff bounds diff context trimming. Only consulted when AllowLossy.
	Diff DiffConfig
	// CSV bounds tabular truncation. Only consulted when AllowLossy.
	CSV CSVConfig
}

// DefaultOptions returns conservative settings.
func DefaultOptions() Options {
	return Options{
		MinBytes:     512,
		MaxBytes:     4 << 20, // 4 MiB
		AllowLossy:   false,
		MinGainRatio: 0.05,
		JSONTruncate: DefaultJSONTruncate(),
		Diff:         DefaultDiffConfig(),
		CSV:          DefaultCSVConfig(),
	}
}

// Transform detects the content type of s and applies the matching
// transformer.
//
// It never returns content larger than the input, never returns an error, and
// never panics: a transformer that misbehaves yields the original text with a
// reason recorded.
func Transform(s string, opts Options) (res Result) {
	res = Result{Content: s, BytesBefore: len(s), BytesAfter: len(s), Kind: KindUnknown}

	if opts.MinBytes > 0 && len(s) < opts.MinBytes {
		res.Reason = "below_min_bytes"
		return res
	}
	if opts.MaxBytes > 0 && len(s) > opts.MaxBytes {
		res.Reason = "above_max_bytes"
		return res
	}

	// A transformer is ordinary code operating on untrusted input; a panic
	// must degrade to passthrough rather than fail the request.
	defer func() {
		if r := recover(); r != nil {
			res = Result{
				Content: s, BytesBefore: len(s), BytesAfter: len(s),
				Reason: fmt.Sprintf("panic:%v", r),
			}
		}
	}()

	// Terminal noise is not a content type. Escape sequences and progress
	// redraws turn up in every kind of output, and a terminal would have
	// resolved both before a human saw any of it — so they come off first,
	// which also lets the detector classify the text rather than the control
	// codes wrapped around it.
	norm := ansiRe.ReplaceAllString(s, "")
	norm = collapseRedraws(norm)

	kind := Detect(norm)
	res.Kind = kind

	var out string
	var name string
	var lossy bool
	// gated records that a transformer was withheld by policy rather than
	// finding nothing to do, which is a different thing for an operator reading
	// the reason back.
	var gated string
	switch kind {
	case KindJSON:
		out, name, lossy = compactJSON(norm, opts)
	case KindLogs:
		out, name, lossy = compactLogs(norm, opts)
	case KindHTML:
		// Not gated here: the whitespace pass is lossless and runs regardless,
		// while compactHTML itself withholds the parts that discard content.
		out, name, lossy = compactHTML(norm, opts.AllowLossy)
	case KindSearch:
		if o, ok := compactSearch(norm); ok {
			out, name, lossy = o, "search_group", false
		}
	case KindTable:
		if o, ok := compactTable(norm); ok {
			out, name, lossy = o, "table_unpad", false
		}
	case KindBlob:
		if !opts.AllowLossy {
			gated = "lossy_not_allowed"
			break
		}
		if o, ok := compactBlob(norm); ok {
			out, name, lossy = o, "blob_elide", true
		}
	case KindDiff:
		// Gate before the work, not after: these two transformers are
		// unconditionally lossy, so with AllowLossy off a full parse-and-rewrite
		// would be done on the request path only to be discarded below.
		if !opts.AllowLossy {
			gated = "lossy_not_allowed"
			break
		}
		if o, ok := compactDiff(norm, opts.Diff); ok {
			out, name, lossy = o, "diff_compact", true
		}
	case KindCSV:
		if !opts.AllowLossy {
			gated = "lossy_not_allowed"
			break
		}
		// Detect classified the trimmed text, so the transform must see the
		// same string: looksLikeCSV requires the delimiter in the first line,
		// and leading whitespace would otherwise make every delimiter fail
		// here after Detect had already committed to KindCSV.
		t := strings.TrimSpace(norm)
		if delim, ok := looksLikeCSV(t); ok {
			if o, ok := compactCSV(t, delim, opts.CSV); ok {
				out, name, lossy = o, "csv_compact", true
			}
		}
	}

	// A lossy transformer the policy will not allow is, from here, the same as
	// no transformer having run at all — and the normalisation below is still
	// lossless and still free. Returning early on it would throw that away
	// too, which is a strictly worse answer than the one the caller would have
	// got had the detector never claimed the text.
	if lossy && !opts.AllowLossy {
		out, name, lossy = "", "", false
		gated = "lossy_not_allowed"
	}

	// No content transformer ran, but the terminal noise came off anyway. That
	// is a real saving on any output a build tool produced, whatever its shape,
	// and it discards nothing a terminal would have shown.
	if (out == "" || name == "") && norm != s {
		out, name, lossy = norm, "terminal_normalize", false
	}

	if out == "" || name == "" {
		switch {
		case gated != "":
			res.Reason = gated
		case kind == KindUnknown:
			res.Reason = "no_transformer"
		default:
			res.Reason = "no_change"
		}
		return res
	}

	// The output must actually be smaller. This is the backstop that makes
	// every transformer safe to add: a bug that inflates content can only
	// cost the CPU that produced it.
	if len(out) >= len(s) {
		res.Reason = "no_gain"
		return res
	}
	if opts.MinGainRatio > 0 {
		gain := float64(len(s)-len(out)) / float64(len(s))
		if gain < opts.MinGainRatio {
			res.Reason = "gain_below_threshold"
			return res
		}
	}

	res.Content = out
	res.Applied = true
	res.Transformer = name
	res.Lossy = lossy
	res.BytesAfter = len(out)
	return res
}

// Detect classifies content. It is intentionally reluctant: anything it cannot
// confidently place is Unknown and passes through untouched.
func Detect(s string) Kind {
	t := strings.TrimSpace(s)
	if t == "" {
		return KindUnknown
	}
	if (t[0] == '{' || t[0] == '[') && looksLikeJSON(t) {
		return KindJSON
	}
	if looksLikeDiff(t) {
		return KindDiff
	}
	// HTML before the line-oriented heuristics: a document opens with a tag,
	// which is a far more specific signal than any of them test for.
	if looksLikeHTML(t) {
		return KindHTML
	}
	// Search before CSV: a grep result set has a stable colon count per line
	// and would read as a two-column table, which would then be truncated —
	// dropping hits. The "path:digits:" prefix is the more specific signal.
	if looksLikeSearch(t) {
		return KindSearch
	}
	// Blob before the line-oriented heuristics: encoded data has no delimiters
	// and no timestamps, but a long enough run of it would eventually satisfy
	// one of them by accident.
	if looksLikeBlob(t) {
		return KindBlob
	}
	// CSV before logs: a table whose rows carry timestamps or a "status"
	// column reads as log-shaped to the weaker log heuristic, and routing a
	// table through run-collapsing would lose rows. The CSV signal — a stable
	// delimiter count across nearly every row plus a header naming columns —
	// is the more specific of the two.
	if _, ok := looksLikeCSV(t); ok {
		return KindCSV
	}
	// Table before logs, and it is the stricter of the two: `go test` and
	// `docker ps` satisfy both, and unpadding their columns keeps every row
	// where routing them through run-collapsing would start dropping lines.
	if looksLikeTable(t) {
		return KindTable
	}
	if looksLikeLogs(t) {
		return KindLogs
	}
	// Last, and the weakest signal of all: output that repeats itself enough
	// for run-collapsing to pay, whatever it turns out to be.
	if looksLikeRepetitive(t) {
		return KindLogs
	}
	return KindUnknown
}

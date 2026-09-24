// Command scanshow runs the gateway's own detectors over text and shows what
// they matched, line by line, so a finding count can be judged instead of
// trusted.
//
//	go run ./test/benchmarks/scanshow < some-prompt.txt
//	pbpaste | go run ./test/benchmarks/scanshow
//
// It exists because the trace store deliberately keeps no matched values: a
// findings log that held them would be the easiest place in the appliance to
// harvest exactly what it was built to protect. The question "is this detector
// finding real data or noise" is a question about the detector, so it is
// answered here, against text the operator already has, and nothing is
// written anywhere.
package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
)

func main() {
	secrets := guardrail.NewSecretsScanner()
	pii := guardrail.NewPIIScanner()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	counts := map[string]int{}
	flagged, total := 0, 0

	for line := 1; in.Scan(); line++ {
		text := in.Text()
		total++
		if strings.TrimSpace(text) == "" {
			continue
		}

		marked, replaced := secrets.Redact(text)
		marked, piiReplaced := pii.Redact(marked, nil)
		replaced = append(replaced, piiReplaced...)
		if len(replaced) == 0 {
			continue
		}

		flagged++
		for _, name := range replaced {
			counts[name]++
		}
		// Both lines, one above the other: the placeholders say what fired and
		// where, the original says on what. Reading them together is the whole
		// point — a count alone cannot tell a real address from a sample one.
		fmt.Printf("line %d\n", line)
		fmt.Printf("  found : %s\n", strings.Join(dedupe(replaced), ", "))
		fmt.Printf("  text  : %s\n", text)
		fmt.Printf("  marked: %s\n\n", marked)
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	fmt.Printf("%d of %d lines matched\n", flagged, total)
	if len(counts) == 0 {
		return
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return counts[names[i]] > counts[names[j]] })
	for _, n := range names {
		fmt.Printf("  %-22s %d\n", n, counts[n])
	}
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

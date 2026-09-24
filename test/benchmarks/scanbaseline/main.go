// Command scanbaseline runs the gateway's own detectors over the sensitive
// corpus and prints what they found, so a classifier can be compared against
// the thing it would have to beat rather than against nothing.
//
//	go run ./test/benchmarks/scanbaseline < test/benchmarks/sensitive_corpus.json
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/guardrail"
)

type corpus struct {
	Items []struct {
		ID     string `json:"id"`
		Bucket string `json:"bucket"`
		Leak   bool   `json:"leak"`
		PII    bool   `json:"pii"`
		Text   string `json:"text"`
	} `json:"items"`
}

type result struct {
	ID       string   `json:"id"`
	Bucket   string   `json:"bucket"`
	Leak     bool     `json:"leak"`
	PII      bool     `json:"pii"`
	Secrets  []string `json:"secrets"`
	PIITypes []string `json:"pii_types"`
	// Fired is what the console would show: any detector at all.
	Fired bool `json:"fired"`
}

func main() {
	var c corpus
	if err := json.NewDecoder(os.Stdin).Decode(&c); err != nil {
		fmt.Fprintln(os.Stderr, "read corpus:", err)
		os.Exit(1)
	}

	secrets := guardrail.NewSecretsScanner()
	pii := guardrail.NewPIIScanner()

	out := make([]result, 0, len(c.Items))
	for _, item := range c.Items {
		r := result{ID: item.ID, Bucket: item.Bucket, Leak: item.Leak, PII: item.PII}
		for _, m := range secrets.Findings(item.Text) {
			r.Secrets = append(r.Secrets, m.Name)
		}
		for _, m := range pii.Findings(item.Text, nil) {
			r.PIITypes = append(r.PIITypes, m.Name)
		}
		r.Fired = len(r.Secrets) > 0 || len(r.PIITypes) > 0
		out = append(out, r)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "write results:", err)
		os.Exit(1)
	}
}

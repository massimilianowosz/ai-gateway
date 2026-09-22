package guardrail

import (
	"context"
	"regexp"
)

// SecretsScanner detects leaked credentials and API keys using regex patterns.
type SecretsScanner struct{}

// NewSecretsScanner creates a secrets scanner.
func NewSecretsScanner() *SecretsScanner { return &SecretsScanner{} }

func (s *SecretsScanner) Name() string { return "secrets" }

func (s *SecretsScanner) Scan(ctx context.Context, userText, fullText string) (ScanResult, error) {
	for _, d := range secretDetectors {
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

type secretDetector struct {
	name string
	re   *regexp.Regexp
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
}

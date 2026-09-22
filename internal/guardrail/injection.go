package guardrail

import (
	"context"
	"regexp"
	"strings"
)

// InjectionScanner detects prompt injection and jailbreak attempts
// using weighted regex heuristics applied only to user-role messages.
type InjectionScanner struct{}

// NewInjectionScanner creates an injection scanner.
func NewInjectionScanner() *InjectionScanner { return &InjectionScanner{} }

func (s *InjectionScanner) Name() string { return "injection" }

func (s *InjectionScanner) Scan(ctx context.Context, userText, fullText string) (ScanResult, error) {
	// Only scan user-role messages to avoid false positives from system prompts
	if userText == "" {
		return ScanResult{Scanner: "injection"}, nil
	}

	lower := strings.ToLower(userText)

	totalWeight := 0
	var matched []string

	for _, p := range injectionPatterns {
		if p.re.MatchString(lower) {
			totalWeight += p.weight
			matched = append(matched, p.name)
		}
	}

	// Also check for delimiter injection in original case
	for _, p := range delimiterPatterns {
		if p.re.MatchString(userText) {
			totalWeight += p.weight
			matched = append(matched, p.name)
		}
	}

	// Block if weighted score exceeds threshold
	if totalWeight >= injectionThreshold {
		return ScanResult{
			Blocked:  true,
			Reason:   "Prompt injection detected: " + strings.Join(matched, ", "),
			Scanner:  "injection",
			Category: "prompt_injection",
			Score:    float64(totalWeight) / float64(injectionMaxScore),
		}, nil
	}

	return ScanResult{Scanner: "injection"}, nil
}

const (
	injectionThreshold = 3 // minimum weight to trigger block
	injectionMaxScore  = 15
)

type injectionPattern struct {
	name   string
	re     *regexp.Regexp
	weight int
}

// injectionPatterns are matched against lowercased user text.
var injectionPatterns = []injectionPattern{
	// Direct instruction override (high severity)
	{
		name:   "ignore_instructions",
		re:     regexp.MustCompile(`(?:ignore|disregard|forget|override|bypass|skip)\s+(?:all\s+)?(?:previous|prior|above|earlier|original|system|my)\s+(?:instructions?|prompts?|rules?|guidelines?|context|directives?)`),
		weight: 5,
	},
	{
		name:   "new_instructions",
		re:     regexp.MustCompile(`(?:new|updated|revised|real|actual|true)\s+(?:instructions?|system\s*prompt|directives?)\s*[:：]`),
		weight: 5,
	},
	// Role manipulation
	{
		name:   "role_override",
		re:     regexp.MustCompile(`you\s+are\s+(?:now\s+)?(?:a\s+|an\s+)?(?:different|new|unrestricted|unfiltered|evil|jailbroken)|you\s+are\s+no\s+longer\s+(?:a\s+|an\s+)?(?:ai|assistant|chatbot|helpful|restricted|bound)`),
		weight: 5,
	},
	{
		name:   "no_restrictions",
		re:     regexp.MustCompile(`(?:act|behave|respond)\s+(?:as\s+if|like)\s+(?:you\s+(?:have|had)\s+)?no\s+(?:rules|restrictions|limits|constraints|guidelines|filters|boundaries)`),
		weight: 4,
	},
	{
		name:   "pretend_to_be",
		re:     regexp.MustCompile(`(?:pretend|act|behave|imagine)\s+(?:that\s+|as\s+if\s+)?(?:you\s+(?:are|have|can|don'?t)|there\s+are\s+no\s+(?:rules|restrictions|limits))`),
		weight: 4,
	},
	// System prompt extraction
	{
		name:   "reveal_system_prompt",
		re:     regexp.MustCompile(`(?:show|reveal|display|print|output|repeat|tell me|what (?:is|are))\s+(?:your\s+)?(?:system\s*prompt|initial\s*(?:instructions?|prompt)|hidden\s*(?:instructions?|prompt)|original\s*(?:instructions?|prompt))`),
		weight: 4,
	},
	// DAN-style jailbreaks
	{
		name:   "dan_jailbreak",
		re:     regexp.MustCompile(`(?:do\s+anything\s+now|d\.?a\.?n\.?\s+mode|jailbreak(?:ed)?|developer\s+mode|god\s+mode|unrestricted\s+mode)`),
		weight: 5,
	},
	// Output manipulation
	{
		name:   "output_manipulation",
		re:     regexp.MustCompile(`(?:respond|reply|answer)\s+(?:only\s+)?(?:with|in)\s+(?:your\s+)?(?:system\s*prompt|hidden|internal|raw)`),
		weight: 4,
	},
	// Multilingual injection (Italian)
	{
		name:   "ignore_instructions_it",
		re:     regexp.MustCompile(`(?:ignora|dimentica|tralascia|scorda)\s+(?:tutte?\s+)?(?:le\s+)?(?:istruzioni|regole|direttive|prompt)\s+(?:precedenti|di sistema|iniziali|originali)`),
		weight: 5,
	},
	// Encoding tricks
	{
		name:   "base64_instruction",
		re:     regexp.MustCompile(`(?:decode|decodifica|base64|rot13|hex)\s+(?:this|the following|questo|il seguente)\s*[:：]`),
		weight: 3,
	},
	// Continuation / completion attacks
	{
		name:   "completion_attack",
		re:     regexp.MustCompile(`(?:continue|complete)\s+(?:the\s+)?(?:following|text|conversation)\s*[:：]\s*(?:system|assistant)\s*[:：]`),
		weight: 4,
	},
}

// delimiterPatterns are matched against original-case text (for special tokens).
var delimiterPatterns = []injectionPattern{
	{
		name:   "token_delimiter",
		re:     regexp.MustCompile(`<\|(?:im_start|im_end|system|endoftext|pad)\|>`),
		weight: 5,
	},
	{
		name:   "markdown_system_block",
		re:     regexp.MustCompile(`(?m)^#{1,3}\s*(?:SYSTEM|System Prompt|INSTRUCTIONS)\s*$|#{2,}\s*SYSTEM\s*#{2,}|<<<\s*(?:END|STOP|RESET)\s*>>>`),
		weight: 3,
	},
}

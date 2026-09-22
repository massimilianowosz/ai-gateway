package store

import "context"

// UsageCapture holds a pointer that downstream handlers fill with real usage data
// from the provider response. Used by HiveState middleware to get actual token counts.
type UsageCapture struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedPromptTokens and CacheCreationTokens are subsets of PromptTokens,
	// carried so HiveState can see whether the provider actually served the
	// prompt from cache — the signal its prefix-cache guard reasons about.
	CachedPromptTokens  int
	CacheCreationTokens int
	Cost                float64
	Filled              bool
}

type usageCaptureKeyType struct{}

var usageCaptureKey = usageCaptureKeyType{}

// WithUsageCapture stores a UsageCapture pointer in the request context.
func WithUsageCapture(ctx context.Context, uc *UsageCapture) context.Context {
	return context.WithValue(ctx, usageCaptureKey, uc)
}

// GetUsageCapture retrieves the UsageCapture pointer from context, or nil.
func GetUsageCapture(ctx context.Context) *UsageCapture {
	uc, _ := ctx.Value(usageCaptureKey).(*UsageCapture)
	return uc
}

package hivestate

// TokenCounter estimates token counts for messages.
type TokenCounter interface {
	Count(text string) int
	CountMessages(messages []Message) int
}

// charRatioCounter estimates tokens as chars/4.
type charRatioCounter struct{}

func (c *charRatioCounter) Count(text string) int {
	n := len(text) / 4
	if n == 0 && len(text) > 0 {
		return 1
	}
	return n
}

func (c *charRatioCounter) CountMessages(messages []Message) int {
	total := 0
	for _, m := range messages {
		// ~4 tokens overhead per message (role, formatting)
		total += 4 + c.Count(m.Content)
	}
	return total
}

// NewTokenCounter returns the default token counter (chars/4 estimation).
func NewTokenCounter() TokenCounter {
	return &charRatioCounter{}
}

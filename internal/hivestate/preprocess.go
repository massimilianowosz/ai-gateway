package hivestate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PreProcessHistory reduces the token count of History messages before passing them
// to the state extractor. This makes extraction faster, cheaper, and more focused.
//
// Strategies:
// - JSON arrays: sample first/last N items
// - Code blocks: keep only signatures/imports
// - Long text: truncate to first N lines
// - Tool results with file content: keep first/last sections
func PreProcessHistory(messages []Message, maxContentTokens int) []Message {
	result := make([]Message, len(messages))
	for i, m := range messages {
		result[i] = Message{
			Role:       m.Role,
			ToolCallID: m.ToolCallID,
			Content:    preprocessContent(m.Content, maxContentTokens),
		}
	}
	return result
}

func preprocessContent(content string, maxTokens int) string {
	// Short content: pass through
	if len(content) < 500 {
		return content
	}

	trimmed := strings.TrimSpace(content)

	// JSON array: sample items
	if strings.HasPrefix(trimmed, "[") {
		if compressed := compressJSONArray(trimmed); compressed != "" {
			return compressed
		}
	}

	// JSON object: trim long string values
	if strings.HasPrefix(trimmed, "{") {
		if compressed := compressJSONObject(trimmed); compressed != "" {
			return compressed
		}
	}

	// Code-like content: keep structure
	if looksLikeCode(trimmed) {
		return compressCode(trimmed)
	}

	// Long text: keep first and last portions
	return truncateWithEnds(trimmed, maxTokens)
}

// compressJSONArray samples items from a JSON array.
func compressJSONArray(content string) string {
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(content), &arr); err != nil {
		return ""
	}

	if len(arr) <= 5 {
		return content // Already small
	}

	// Keep first 3 + last 2 items
	var sampled []json.RawMessage
	sampled = append(sampled, arr[:3]...)
	sampled = append(sampled, arr[len(arr)-2:]...)

	result, err := json.Marshal(sampled)
	if err != nil {
		return ""
	}

	return string(result) + "\n[" + strings.Repeat(".", 3) + " " +
		string(rune('0'+len(arr)/100%10)) + string(rune('0'+len(arr)/10%10)) + string(rune('0'+len(arr)%10)) +
		" total items, showing 5]"
}

// compressJSONObject trims long string values in a JSON object.
func compressJSONObject(content string) string {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return ""
	}

	modified := false
	for k, v := range obj {
		if s, ok := v.(string); ok && len(s) > 200 {
			// Truncate long string values, keep first and last
			obj[k] = s[:100] + "..." + s[len(s)-50:]
			modified = true
		}
	}

	if !modified {
		return ""
	}

	result, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(result)
}

// looksLikeCode detects if content is source code.
func looksLikeCode(content string) bool {
	codeIndicators := []string{
		"func ", "def ", "class ", "import ",
		"package ", "module ", "const ", "var ",
		"public ", "private ", "protected ",
		"interface ", "struct ", "enum ",
		"if (", "for (", "while (",
		"#include", "#define", "#pragma",
	}

	lines := strings.Split(content, "\n")
	if len(lines) < 5 {
		return false
	}

	indicators := 0
	for _, line := range lines[:min(20, len(lines))] {
		trimLine := strings.TrimSpace(line)
		for _, ind := range codeIndicators {
			if strings.HasPrefix(trimLine, ind) || strings.Contains(trimLine, ind) {
				indicators++
				break
			}
		}
	}

	return indicators >= 3
}

// compressCode keeps imports, signatures, and structure markers.
func compressCode(content string) string {
	lines := strings.Split(content, "\n")
	var kept []string
	inBody := false
	braceDepth := 0

	for _, line := range lines {
		trimLine := strings.TrimSpace(line)

		// Always keep: imports, package declarations, type definitions, function signatures
		isStructural := false
		structuralPrefixes := []string{
			"import", "from ", "package ", "module ",
			"func ", "def ", "class ", "type ",
			"interface ", "struct ", "enum ",
			"pub fn ", "fn ", "pub struct ",
			"export ", "const ", "var ", "let ",
			"#", "//", "/*", "*/",
		}
		for _, prefix := range structuralPrefixes {
			if strings.HasPrefix(trimLine, prefix) {
				isStructural = true
				break
			}
		}

		// Track brace depth for body detection
		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
		if braceDepth > 1 && !isStructural {
			if !inBody {
				kept = append(kept, "    // ... body ...")
				inBody = true
			}
			continue
		}
		inBody = false

		if isStructural || braceDepth <= 1 || trimLine == "}" || trimLine == "" {
			kept = append(kept, line)
		}
	}

	// If we didn't reduce much, return truncated original
	if len(kept) > len(lines)*3/4 {
		return truncateWithEnds(content, 500)
	}

	return strings.Join(kept, "\n")
}

// truncateWithEnds keeps the first and last portions of long text.
func truncateWithEnds(content string, maxWords int) string {
	lines := strings.Split(content, "\n")
	if len(lines) <= 30 {
		return content
	}

	// Keep first 15 + last 10 lines
	headLines := 15
	tailLines := 10
	if len(lines) < headLines+tailLines+5 {
		return content
	}

	var result strings.Builder
	for _, l := range lines[:headLines] {
		result.WriteString(l)
		result.WriteByte('\n')
	}
	fmt.Fprintf(&result, "\n[... %d lines omitted ...]\n\n", len(lines)-headLines-tailLines)
	for _, l := range lines[len(lines)-tailLines:] {
		result.WriteString(l)
		result.WriteByte('\n')
	}

	return result.String()
}

// TruncateToolResponses truncates long tool/assistant messages that contain JSON responses.
// Keeps the structure (keys, IDs) but trims long nested values. Preserves short messages intact.
// Used for tool-agent profiles where full PreProcessHistory would remove critical tool call structure.
func TruncateToolResponses(messages []Message, maxChars int) []Message {
	result := make([]Message, len(messages))
	for i, m := range messages {
		result[i] = Message{
			Role:       m.Role,
			ToolCallID: m.ToolCallID,
			Content:    truncateToolContent(m.Content, maxChars),
		}
	}
	return result
}

func truncateToolContent(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}

	trimmed := strings.TrimSpace(content)

	// If it's a JSON object, compress it preserving keys and short values
	if strings.HasPrefix(trimmed, "{") {
		if compressed := compressToolJSON(trimmed, maxChars); compressed != "" {
			return compressed
		}
	}

	// If it's a JSON array, sample it
	if strings.HasPrefix(trimmed, "[") {
		if compressed := compressJSONArray(trimmed); compressed != "" && len(compressed) <= maxChars {
			return compressed
		}
	}

	// Fallback: keep first portion + last portion
	head := maxChars * 2 / 3
	tail := maxChars / 3
	return content[:head] + "\n[...truncated...]\n" + content[len(content)-tail:]
}

// compressToolJSON flattens a JSON object, keeping keys and short values,
// truncating long string values (which are often nested JSON responses).
func compressToolJSON(content string, maxChars int) string {
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return ""
	}

	trimJSONValues(obj, 100)

	out, err := json.Marshal(obj)
	if err != nil {
		return ""
	}

	s := string(out)
	if len(s) > maxChars {
		// Still too long — trim more aggressively
		trimJSONValues(obj, 50)
		out, err = json.Marshal(obj)
		if err != nil {
			return ""
		}
		s = string(out)
	}

	return s
}

// trimJSONValues recursively truncates long string values in a JSON structure.
func trimJSONValues(obj map[string]interface{}, maxLen int) {
	for k, v := range obj {
		switch val := v.(type) {
		case string:
			if len(val) > maxLen {
				obj[k] = val[:maxLen] + "..."
			}
		case map[string]interface{}:
			trimJSONValues(val, maxLen)
		case []interface{}:
			// Keep at most 3 items
			if len(val) > 3 {
				obj[k] = val[:3]
			}
			for _, item := range val {
				if m, ok := item.(map[string]interface{}); ok {
					trimJSONValues(m, maxLen)
				}
			}
		}
	}
}

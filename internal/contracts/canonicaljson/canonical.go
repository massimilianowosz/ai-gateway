// Package canonicaljson implements Ubiquum canonical JSON v1 for signed
// governance contracts. It intentionally accepts only the interoperable JSON
// data model and rejects non-finite or non-exact numbers.
package canonicaljson

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maxSafeInteger int64 = 1<<53 - 1

func utf16Less(left, right string) bool {
	a := utf16.Encode([]rune(left))
	b := utf16.Encode([]rune(right))
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func canonicalFloat(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", errors.New("NaN and Infinity are not valid canonical JSON numbers")
	}
	if value == 0 {
		return "0", nil
	}
	abs := math.Abs(value)
	if abs >= 1e21 || abs < 1e-6 {
		rendered := strconv.FormatFloat(value, 'e', -1, 64)
		parts := strings.Split(rendered, "e")
		exponent, err := strconv.Atoi(parts[1])
		if err != nil {
			return "", err
		}
		sign := ""
		if exponent >= 0 {
			sign = "+"
		}
		return parts[0] + "e" + sign + strconv.Itoa(exponent), nil
	}
	return strconv.FormatFloat(value, 'f', -1, 64), nil
}

func number(value json.Number) (string, error) {
	if !strings.ContainsAny(value.String(), ".eE") {
		integer, err := strconv.ParseInt(value.String(), 10, 64)
		if err != nil {
			return "", err
		}
		if integer > maxSafeInteger || integer < -maxSafeInteger {
			return "", errors.New("integer exceeds IEEE-754 safe range")
		}
		return strconv.FormatInt(integer, 10), nil
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return "", err
	}
	return canonicalFloat(parsed)
}

func appendString(out *strings.Builder, value string) error {
	if !utf8.ValidString(value) {
		return errors.New("strings must contain valid Unicode")
	}
	out.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(character)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if character < 0x20 {
				out.WriteString(`\u00`)
				out.WriteString(hex.EncodeToString([]byte{byte(character)}))
			} else {
				out.WriteRune(character)
			}
		}
	}
	out.WriteByte('"')
	return nil
}

func appendValue(out *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case string:
		if err := appendString(out, typed); err != nil {
			return err
		}
	case json.Number:
		rendered, err := number(typed)
		if err != nil {
			return err
		}
		out.WriteString(rendered)
	case float64:
		rendered, err := canonicalFloat(typed)
		if err != nil {
			return err
		}
		out.WriteString(rendered)
	case []any:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendValue(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendString(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := appendValue(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return errors.New("value is outside the canonical JSON data model")
	}
	return nil
}

func Marshal(value any) ([]byte, error) {
	var out strings.Builder
	if err := appendValue(&out, value); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func SHA256(value any) (string, error) {
	encoded, err := Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

package callback

import (
	"strings"
	"unicode/utf16"
)

// UTF16Len mirrors Java String.length(), which Jakarta @Size uses for the
// backend callback contract.
func UTF16Len(value string) int {
	length := 0
	for _, character := range value {
		length += utf16.RuneLen(character)
	}
	return length
}

// TruncateUTF16 limits text without splitting a Unicode code point or its
// UTF-16 surrogate pair.
func TruncateUTF16(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if UTF16Len(value) <= limit {
		return value
	}
	var builder strings.Builder
	builder.Grow(min(len(value), limit))
	used := 0
	for _, character := range value {
		units := utf16.RuneLen(character)
		if used+units > limit {
			break
		}
		builder.WriteRune(character)
		used += units
	}
	return builder.String()
}

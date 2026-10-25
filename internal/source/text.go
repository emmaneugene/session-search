package source

import (
	"strings"
	"time"
	"unicode/utf8"
)

// TitleFrom makes a one-line title of at most 80 characters from text.
func TitleFrom(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(t) <= 80 {
		return t
	}
	r := []rune(t)
	return string(r[:80]) + "…"
}

// ParseTime parses an RFC 3339 timestamp, returning the zero time when it
// can't.
func ParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// MillisTime converts Unix milliseconds to a UTC time; 0 gives the zero time.
func MillisTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

package main

import (
	"strconv"
	"strings"
)

// humanizeInt renders an integer with thousands separators (1234567 ->
// "1,234,567"). Shared by the daemon status TUI and other CLI summaries.
func humanizeInt[T int | int32 | int64 | uint32 | uint64](v T) string {
	s := strconv.FormatInt(int64(v), 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

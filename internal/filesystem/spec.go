package filesystem

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseSize parses a human size such as "10MB", "1.5GiB" or "512".
// Both decimal (KB=1000) and binary (KiB=1024) prefixes are accepted;
// a bare "KB" is treated as 1024 because that is what developers mean,
// even though it is technically wrong.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("size %q: no leading number", s)
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	unit := strings.ToUpper(strings.TrimSpace(s[i:]))

	var mult float64 = 1
	switch unit {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("size %q: unknown unit %q", s, unit)
	}
	v := num * mult
	if v < 0 || v > float64(1<<62) {
		return 0, fmt.Errorf("size %q out of range", s)
	}
	return int64(v), nil
}

// ParseAge parses a duration that may use day and week units, which
// time.ParseDuration does not support.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	last := s[len(s)-1]
	if last == 'd' || last == 'w' || last == 'y' {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		day := 24 * float64(time.Hour)
		switch last {
		case 'd':
			return time.Duration(n * day), nil
		case 'w':
			return time.Duration(n * 7 * day), nil
		case 'y':
			// 365 days, not a calendar year. Documented, not clever.
			return time.Duration(n * 365 * day), nil
		}
	}
	return time.ParseDuration(s)
}

package ir

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var durationPartRe = regexp.MustCompile(`(\d+)\s*([a-zA-Z]*)`)

// ParseMinutes parses human durations such as "1h 30m", "3 hours",
// "90 minutes" or "45" (minutes) into minutes.
func ParseMinutes(s string) (int, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, false
	}
	total := 0.0
	found := false
	for _, m := range durationPartRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		found = true
		unit := m[2]
		switch {
		case unit == "" || strings.HasPrefix(unit, "m") && !strings.HasPrefix(unit, "mo"):
			total += float64(n)
		case strings.HasPrefix(unit, "h"):
			total += float64(n) * 60
		case strings.HasPrefix(unit, "d"):
			total += float64(n) * 60 * 24
		case strings.HasPrefix(unit, "w"):
			total += float64(n) * 60 * 24 * 7
		case strings.HasPrefix(unit, "s"):
			total += float64(n) / 60
		default:
			return 0, false
		}
	}
	if !found {
		return 0, false
	}
	minutes := int(total)
	if float64(minutes) < total {
		minutes++
	}
	return minutes, true
}

// DurationDays parses "30 days", "1 week", "2 months" (30 days each) or
// "1 year" into whole days, rounding up anything shorter than a day.
func DurationDays(s string) (int, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "never" {
		return 0, false
	}
	days := 0.0
	found := false
	for _, m := range durationPartRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		found = true
		unit := m[2]
		switch {
		case unit == "" || strings.HasPrefix(unit, "d"):
			days += float64(n)
		case strings.HasPrefix(unit, "w"):
			days += float64(n) * 7
		case strings.HasPrefix(unit, "mo"):
			days += float64(n) * 30
		case strings.HasPrefix(unit, "y"):
			days += float64(n) * 365
		case strings.HasPrefix(unit, "h"), strings.HasPrefix(unit, "m"), strings.HasPrefix(unit, "s"):
			days += 1.0 / 24
		default:
			return 0, false
		}
	}
	if !found {
		return 0, false
	}
	d := int(days)
	if float64(d) < days {
		d++
	}
	return d, true
}

// FormatMinutes renders minutes as a GitLab-style duration ("1h 30m").
func FormatMinutes(m int) string {
	h, rest := m/60, m%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", rest)
	case rest == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, rest)
}

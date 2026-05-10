package identity

import (
	"fmt"
	"strings"
	"time"
)

// FormatRelativeDate returns a human-readable date description given the
// number of days between today and the date in question. Mirrors
// Springdrift's identity.gleam:format_relative_date exactly:
//
//	0       → "today"
//	1       → "yesterday"
//	2..6    → "N days ago"
//	7..13   → "last week"
//	14..29  → "N days ago"
//	otherwise → "more than 30 days ago"
//
// Negative inputs are treated as 0 (defensive — a "future" date in
// recall context is almost certainly a clock-skew artefact, surface as
// "today" rather than crashing or producing nonsense).
func FormatRelativeDate(daysAgo int) string {
	switch {
	case daysAgo <= 0:
		return "today"
	case daysAgo == 1:
		return "yesterday"
	case daysAgo >= 2 && daysAgo <= 6:
		return fmt.Sprintf("%d days ago", daysAgo)
	case daysAgo >= 7 && daysAgo <= 13:
		return "last week"
	case daysAgo >= 14 && daysAgo <= 29:
		return fmt.Sprintf("%d days ago", daysAgo)
	default:
		return "more than 30 days ago"
	}
}

// FormatRelativeDateFromStrings parses two dates in YYYY-MM-DD form (or
// ISO-8601 with a `T` separator — only the date part is used) and
// returns the relative description for `date` measured against `today`.
// Falls back to the raw `date` string on parse failure or when the diff
// is negative or > 29.
//
// Springdrift's `format_relative_date_from_strings` uses a rough
// 365/30-day approximation; we use Go's calendar arithmetic for
// accuracy. Differences only matter at month/year boundaries which the
// approximation misreads anyway, so this is strictly a fix.
func FormatRelativeDateFromStrings(date, today string) string {
	d, ok := parseDateOnly(date)
	if !ok {
		return date
	}
	t, ok := parseDateOnly(today)
	if !ok {
		return date
	}
	days := int(t.Sub(d).Hours() / 24)
	if days < 0 || days > 29 {
		return date
	}
	return FormatRelativeDate(days)
}

// parseDateOnly accepts YYYY-MM-DD or any ISO-8601 timestamp; only the
// date portion (before any "T") is used. Returns (zeroValue, false) on
// any malformed input.
func parseDateOnly(s string) (time.Time, bool) {
	if i := strings.Index(s, "T"); i >= 0 {
		s = s[:i]
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

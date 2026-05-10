package identity

import (
	"testing"
)

func TestFormatRelativeDate_AllRanges(t *testing.T) {
	cases := []struct {
		days int
		want string
	}{
		{0, "today"},
		{-1, "today"}, // defensive — clock-skew "future" → today
		{1, "yesterday"},
		{2, "2 days ago"},
		{3, "3 days ago"},
		{6, "6 days ago"},
		{7, "last week"},
		{10, "last week"},
		{13, "last week"},
		{14, "14 days ago"},
		{20, "20 days ago"},
		{29, "29 days ago"},
		{30, "more than 30 days ago"},
		{100, "more than 30 days ago"},
	}
	for _, c := range cases {
		if got := FormatRelativeDate(c.days); got != c.want {
			t.Errorf("FormatRelativeDate(%d) = %q, want %q", c.days, got, c.want)
		}
	}
}

func TestFormatRelativeDateFromStrings_HappyPath(t *testing.T) {
	got := FormatRelativeDateFromStrings("2026-04-29", "2026-04-30")
	if got != "yesterday" {
		t.Errorf("got %q", got)
	}
}

func TestFormatRelativeDateFromStrings_AcceptsISOTimestamp(t *testing.T) {
	got := FormatRelativeDateFromStrings("2026-04-29T15:30:00Z", "2026-04-30")
	if got != "yesterday" {
		t.Errorf("got %q", got)
	}
}

func TestFormatRelativeDateFromStrings_FarPastReturnsRaw(t *testing.T) {
	got := FormatRelativeDateFromStrings("2026-01-01", "2026-04-30")
	if got != "2026-01-01" {
		t.Errorf("got %q, want raw date back", got)
	}
}

func TestFormatRelativeDateFromStrings_FutureReturnsRaw(t *testing.T) {
	got := FormatRelativeDateFromStrings("2027-01-01", "2026-04-30")
	if got != "2027-01-01" {
		t.Errorf("got %q, want raw date back", got)
	}
}

func TestFormatRelativeDateFromStrings_MalformedDateReturnsRaw(t *testing.T) {
	got := FormatRelativeDateFromStrings("not-a-date", "2026-04-30")
	if got != "not-a-date" {
		t.Errorf("got %q", got)
	}
}

func TestFormatRelativeDateFromStrings_MalformedTodayReturnsRaw(t *testing.T) {
	got := FormatRelativeDateFromStrings("2026-04-29", "blah")
	if got != "2026-04-29" {
		t.Errorf("got %q", got)
	}
}

func TestFormatRelativeDateFromStrings_SameDay(t *testing.T) {
	got := FormatRelativeDateFromStrings("2026-04-30", "2026-04-30")
	if got != "today" {
		t.Errorf("got %q", got)
	}
}

func TestFormatRelativeDateFromStrings_CrossesMonthBoundary(t *testing.T) {
	// 5 days ago crossing a month boundary — Go's calendar
	// arithmetic should be exact (Springdrift's approximation
	// misreads these).
	got := FormatRelativeDateFromStrings("2026-04-25", "2026-04-30")
	if got != "5 days ago" {
		t.Errorf("got %q", got)
	}
}

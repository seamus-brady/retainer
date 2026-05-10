package decay

import (
	"math"
	"testing"
	"time"
)

func nearly(t *testing.T, got, want, tol float64, name string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %g, want ~%g (tol %g)", name, got, want, tol)
	}
}

// ---- Confidence ----

func TestConfidence_HalfLifeAtAgeEqualToHalfLife(t *testing.T) {
	// At age == half-life, value should be exactly half the original.
	got := Confidence(1.0, 30, 30)
	nearly(t, got, 0.5, 1e-9, "1.0 at half-life")
}

func TestConfidence_DoubleHalfLife(t *testing.T) {
	got := Confidence(1.0, 60, 30)
	nearly(t, got, 0.25, 1e-9, "1.0 at 2x half-life")
}

func TestConfidence_QuarterHalfLife(t *testing.T) {
	got := Confidence(1.0, 7, 30)
	want := math.Pow(2.0, -7.0/30.0)
	nearly(t, got, want, 1e-9, "1.0 at fractional half-life")
}

func TestConfidence_PreservesUnderUnit(t *testing.T) {
	got := Confidence(0.7, 30, 30)
	nearly(t, got, 0.35, 1e-9, "0.7 at half-life")
}

func TestConfidence_ZeroAgeReturnsOriginal(t *testing.T) {
	if got := Confidence(0.85, 0, 30); got != 0.85 {
		t.Errorf("zero age should return original; got %g", got)
	}
}

func TestConfidence_NegativeAgeReturnsOriginal(t *testing.T) {
	// Future timestamps shouldn't amplify confidence.
	if got := Confidence(0.85, -5, 30); got != 0.85 {
		t.Errorf("negative age should return original; got %g", got)
	}
}

func TestConfidence_ZeroHalfLifeReturnsOriginal(t *testing.T) {
	if got := Confidence(0.85, 30, 0); got != 0.85 {
		t.Errorf("zero half-life should return original; got %g", got)
	}
}

func TestConfidence_NegativeHalfLifeReturnsOriginal(t *testing.T) {
	if got := Confidence(0.85, 30, -1); got != 0.85 {
		t.Errorf("negative half-life should return original; got %g", got)
	}
}

func TestConfidence_ClampsAboveOne(t *testing.T) {
	// An over-1 original at zero age would survive without clamping.
	// Confidence is always input * 2^x where x ≤ 0; result is ≤ original.
	// But an over-1 ORIGINAL with zero age short-circuits; with a real
	// age it decays into [0,1] eventually.
	got := Confidence(2.5, 0, 30)
	if got != 2.5 {
		t.Errorf("zero-age short-circuit should return original (even if >1); got %g", got)
	}
	// At half-life, 2.5 * 0.5 = 1.25 → clamped to 1.0
	got = Confidence(2.5, 30, 30)
	nearly(t, got, 1.0, 1e-9, "above-1 clamped at half-life")
}

func TestConfidence_ClampsBelowZero(t *testing.T) {
	// Negative original with valid decay → still clamped to [0, 1].
	if got := Confidence(-0.5, 30, 30); got != 0.0 {
		t.Errorf("negative original clamped to 0.0; got %g", got)
	}
}

// ---- ConfidenceAt ----

func TestConfidenceAt_ZeroRecordedReturnsOriginal(t *testing.T) {
	if got := ConfidenceAt(0.9, time.Time{}, time.Now(), 30); got != 0.9 {
		t.Errorf("zero recordedAt should return original; got %g", got)
	}
}

func TestConfidenceAt_FutureRecordedReturnsOriginal(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	if got := ConfidenceAt(0.9, future, now, 30); got != 0.9 {
		t.Errorf("future recordedAt should return original; got %g", got)
	}
}

func TestConfidenceAt_ComputesAgeInDays(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	thirtyDaysAgo := now.Add(-30 * 24 * time.Hour)
	got := ConfidenceAt(1.0, thirtyDaysAgo, now, 30)
	nearly(t, got, 0.5, 1e-9, "30 days = half-life")
}

func TestConfidenceAt_RoundsDown(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// 35 hours ago → 1 day old
	thirtyFiveHoursAgo := now.Add(-35 * time.Hour)
	got := ConfidenceAt(1.0, thirtyFiveHoursAgo, now, 30)
	want := math.Pow(2.0, -1.0/30.0)
	nearly(t, got, want, 1e-9, "35h treated as 1 day")
}

// ---- DefaultHalfLifeDays ----

func TestDefaultHalfLifeDays_Is30(t *testing.T) {
	if DefaultHalfLifeDays != 30 {
		t.Errorf("default half-life expected 30, got %d", DefaultHalfLifeDays)
	}
}

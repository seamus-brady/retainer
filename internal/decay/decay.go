// Package decay implements half-life confidence decay for facts and CBR
// cases. Stored confidence values diminish naturally as information ages
// without ever mutating the on-disk record — decay is applied at read /
// query time only. Append-only persistence is preserved.
//
// Formula (port of Springdrift's `dprime/decay.gleam`):
//
//	confidence_t = confidence_0 * 2 ^ (-age_days / half_life_days)
//
// Result clamped to [0.0, 1.0]. When age <= 0 or half-life <= 0, the
// original confidence is returned unchanged — degenerate inputs don't
// silently corrupt scores.
//
// Used by:
//   - Librarian fact reads (confidence floor for stale facts)
//   - CBR retrieval (case confidence weighted by age in the utility signal)
package decay

import (
	"math"
	"time"
)

// DefaultHalfLifeDays is the default half-life for confidence decay
// when callers don't specify one. Mirrors Springdrift's default and is
// roughly the timescale at which a piece of information stops being
// "freshly observed" and becomes "remembered" — beyond that, it
// progressively halves in influence.
const DefaultHalfLifeDays = 30

// Confidence applies half-life decay to an original confidence value.
// `ageDays` is how many days have passed since the value was recorded;
// `halfLifeDays` is the half-life in days. Returns the decayed value
// clamped to [0.0, 1.0].
//
// When either age or half-life is non-positive (degenerate inputs that
// can't produce a meaningful exponent) the original confidence is
// returned unchanged. Callers that want decay applied should pass
// strictly positive values.
func Confidence(original float64, ageDays, halfLifeDays int) float64 {
	if ageDays <= 0 || halfLifeDays <= 0 {
		return original
	}
	exponent := -float64(ageDays) / float64(halfLifeDays)
	decayed := original * math.Pow(2.0, exponent)
	if decayed < 0.0 {
		return 0.0
	}
	if decayed > 1.0 {
		return 1.0
	}
	return decayed
}

// ConfidenceAt is the convenience wrapper that takes timestamps instead
// of pre-computed day deltas. Uses 24-hour calendar days (UTC), rounded
// down, so a value recorded 35h ago is "1 day old" not "2".
//
// `recordedAt` is when the original confidence was assigned;
// `now` is the time the read is happening. When `recordedAt` is zero
// or in the future, age is treated as zero (no decay).
func ConfidenceAt(original float64, recordedAt, now time.Time, halfLifeDays int) float64 {
	if recordedAt.IsZero() || recordedAt.After(now) {
		return original
	}
	ageDays := int(now.Sub(recordedAt).Hours() / 24.0)
	return Confidence(original, ageDays, halfLifeDays)
}

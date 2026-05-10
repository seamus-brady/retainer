package cbr

import (
	"sort"
	"time"
)

// DefaultPruneMaxFailureAge is the cutoff above which a failure
// case with no recorded lesson becomes prunable. 30 days is long
// enough for a partial fix or follow-up cycle to either annotate
// the failure with a pitfall (rescuing it) or correct the case
// (rescuing it differently). Anything past that is dead weight in
// the corpus — it failed, nobody learned anything, and retrieval
// would only surface noise.
const DefaultPruneMaxFailureAge = 30 * 24 * time.Hour

// IsPrunable reports whether a case satisfies the
// "failure with no lesson, past max age" rule. Pure helper so the
// housekeeper's policy can be unit-tested without I/O.
//
// Rules (all required for true):
//
//   - Outcome.Status == StatusFailure. We never prune successes
//     (those are positive examples) or partials (mixed signal still
//     has retrieval value).
//   - len(Outcome.Pitfalls) == 0. A pitfall IS the lesson — once
//     attached, the failure is reusable knowledge.
//   - UsageStats == nil OR HelpfulCount == 0. An operator-validated
//     case stays even when the algorithm wants to prune.
//   - now - Timestamp >= maxAge. Recent failures might still get
//     a follow-up pitfall annotation; we only prune when the
//     window has closed.
//   - Not already redacted / superseded. No point re-marking.
//   - Not categoryless (Conversation cycles get a category=""
//     case for audit; pruning them is fine but we don't need to —
//     they're already excluded from retrieval by category).
//
// Returns false for any case that fails the gate. Symmetric:
// IsPrunable doesn't depend on call order.
func IsPrunable(c Case, now time.Time, maxAge time.Duration) bool {
	if c.Redacted || c.SupersededBy != "" {
		return false
	}
	if c.Outcome.Status != StatusFailure {
		return false
	}
	if len(c.Outcome.Pitfalls) > 0 {
		return false
	}
	if c.UsageStats != nil && c.UsageStats.HelpfulCount > 0 {
		return false
	}
	if c.Category == "" {
		// Conversation cycles get categoryless cases — already
		// hidden from retrieval. Pruning them adds no value and
		// would hide an audit-only record.
		return false
	}
	if maxAge <= 0 {
		// Defensive: a non-positive max age would prune
		// everything. The housekeeper should never call us this
		// way, but treat zero as "never prune" rather than crash.
		return false
	}
	if c.Timestamp.IsZero() {
		// No timestamp → can't compute age. Be conservative.
		return false
	}
	return now.Sub(c.Timestamp) >= maxAge
}

// FindPrunable scans cases and returns those that satisfy IsPrunable.
// Returned slice is sorted by ID for deterministic output.
func FindPrunable(cases []Case, now time.Time, maxAge time.Duration) []Case {
	var out []Case
	for _, c := range cases {
		if IsPrunable(c, now, maxAge) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

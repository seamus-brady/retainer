package policy

import (
	"fmt"
	"strings"
)

// evaluateDeterministic runs the regex prefilter against text. Returns
// Allow if no rules trigger or the normalised score stays below threshold,
// Block otherwise. Interactive demotion (Block → Escalate) happens at the
// caller in EvaluateInput so it doesn't apply to output/tool gates.
func evaluateDeterministic(text string, rules []Rule, threshold float64) Result {
	var triggered []Trigger
	var raw float64
	for _, rule := range rules {
		if rule.Pattern.MatchString(text) {
			triggered = append(triggered, Trigger{
				Source:     "deterministic",
				RuleName:   rule.Name,
				Domain:     rule.Domain,
				Importance: rule.Importance,
				Magnitude:  rule.Magnitude,
			})
			raw += rule.Importance * rule.Magnitude
		}
	}

	score := normalise(raw)

	if len(triggered) == 0 {
		return Result{
			Verdict: Allow,
			Score:   0,
			Trail:   "deterministic: no rules matched",
		}
	}

	var verdict Verdict
	var verdictReason string
	if score >= threshold {
		verdict = Block
		verdictReason = fmt.Sprintf("score %.2f ≥ threshold %.2f", score, threshold)
	} else {
		verdict = Allow
		verdictReason = fmt.Sprintf("score %.2f < threshold %.2f", score, threshold)
	}

	names := make([]string, 0, len(triggered))
	for _, t := range triggered {
		names = append(names, t.RuleName)
	}
	trail := fmt.Sprintf("deterministic matched [%s]; %s", strings.Join(names, ", "), verdictReason)

	return Result{
		Verdict:   verdict,
		Score:     score,
		Triggered: triggered,
		Trail:     trail,
	}
}

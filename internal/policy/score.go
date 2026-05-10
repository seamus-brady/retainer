package policy

// normalise clamps a raw weighted-sum score to [0, 1]. Per arch notes
// (BF-03 fix): never compare raw weighted sums to thresholds; always
// normalise first.
func normalise(rawScore float64) float64 {
	if rawScore > 1.0 {
		return 1.0
	}
	if rawScore < 0 {
		return 0
	}
	return rawScore
}

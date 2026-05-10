package ambient

import (
	"testing"
	"time"
)

func TestSignal_FieldsRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := Signal{
		Source:    "forecaster",
		Kind:      "plan_health_degraded",
		Detail:    "endeavour 'auth-refactor' drift score 0.68",
		Timestamp: now,
	}
	if s.Source != "forecaster" {
		t.Errorf("Source: got %q want %q", s.Source, "forecaster")
	}
	if s.Kind != "plan_health_degraded" {
		t.Errorf("Kind: got %q", s.Kind)
	}
	if s.Detail == "" {
		t.Errorf("Detail unset")
	}
	if !s.Timestamp.Equal(now) {
		t.Errorf("Timestamp: got %v want %v", s.Timestamp, now)
	}
}

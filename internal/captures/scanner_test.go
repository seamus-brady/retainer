package captures

import (
	"strings"
	"testing"
	"time"
)

func now() time.Time {
	return time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
}

func TestScanReply_EmptyText(t *testing.T) {
	if got := ScanReply("c1", "", now()); len(got) != 0 {
		t.Errorf("empty text → expected no captures, got %d", len(got))
	}
	if got := ScanReply("c1", "    \n\n", now()); len(got) != 0 {
		t.Errorf("whitespace-only → expected no captures, got %d", len(got))
	}
}

func TestScanReply_EmptyCycleID(t *testing.T) {
	got := ScanReply("", "I'll check the status tomorrow.", now())
	if len(got) != 0 {
		t.Errorf("empty cycle id → expected no captures, got %d", len(got))
	}
}

func TestScanReply_DetectsCommitment(t *testing.T) {
	text := "Sure, I'll send the ICOM report by tomorrow. Anything else?"
	got := ScanReply("c1", text, now())
	if len(got) < 1 {
		t.Fatalf("expected at least one capture; got 0")
	}
	// "I'll send" and "by tomorrow" both fire; both should yield distinct IDs.
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ID] = true
		if c.Status != StatusPending {
			t.Errorf("status = %q, want %q", c.Status, StatusPending)
		}
		if c.Source != SourceAgentSelf {
			t.Errorf("source = %q, want %q", c.Source, SourceAgentSelf)
		}
		if c.SchemaVersion != SchemaVersion {
			t.Errorf("schema version = %d, want %d", c.SchemaVersion, SchemaVersion)
		}
		if c.SourceCycleID != "c1" {
			t.Errorf("cycle id = %q", c.SourceCycleID)
		}
		if c.Text == "" {
			t.Error("text excerpt should not be empty")
		}
	}
	if len(ids) != len(got) {
		t.Errorf("expected unique IDs across captures; got %d ids for %d captures",
			len(ids), len(got))
	}
}

func TestScanReply_Idempotent(t *testing.T) {
	text := "I'll send the report and let me check the inbox."
	first := ScanReply("c1", text, now())
	later := now().Add(time.Hour)
	second := ScanReply("c1", text, later)
	if len(first) != len(second) {
		t.Fatalf("scan twice → counts differ %d vs %d", len(first), len(second))
	}
	firstIDs := map[string]bool{}
	for _, c := range first {
		firstIDs[c.ID] = true
	}
	for _, c := range second {
		if !firstIDs[c.ID] {
			t.Errorf("re-scan produced new ID %q (expected idempotent on text+cycle)", c.ID)
		}
	}
}

func TestScanReply_SkipsNonCommitments(t *testing.T) {
	// Hedged + speech-act + greeting shapes — should NOT fire.
	cases := []string{
		"I'll be honest, this is hard.",
		"I might check the inbox later.",
		"Yes, that's right.",
		"Hello! How can I help?",
		"The report is in the library at doc:abc §2.",
	}
	for _, text := range cases {
		got := ScanReply("c1", text, now())
		if len(got) != 0 {
			t.Errorf("expected no captures for %q; got %d (first text: %q)",
				text, len(got), got[0].Text)
		}
	}
}

func TestScanReply_DetectsMultiplePhrasesInOneText(t *testing.T) {
	text := "I'll send it tomorrow and I'll check the queue first thing tomorrow."
	got := ScanReply("c1", text, now())
	if len(got) < 2 {
		t.Errorf("expected multiple phrases to fire; got %d (%v)", len(got), got)
	}
}

func TestScanReply_CaseInsensitive(t *testing.T) {
	got := ScanReply("c1", "BY TOMORROW i will send it.", now())
	if len(got) < 1 {
		t.Errorf("uppercase phrase should match; got %d", len(got))
	}
}

func TestExcerptAround_CapturesContext(t *testing.T) {
	text := "Sure, I'll send the ICOM R8600 report by tomorrow. Anything else?"
	at := strings.Index(text, "I'll send")
	got := excerptAround(text, at, 80)
	if !strings.Contains(strings.ToLower(got), "i'll send") {
		t.Errorf("excerpt missing the matched phrase: %q", got)
	}
	if len(got) > 90 {
		t.Errorf("excerpt too long: %d chars", len(got))
	}
}

func TestMakeID_DeterministicAndUnique(t *testing.T) {
	a := MakeID("c1", "I'll send", 10)
	b := MakeID("c1", "I'll send", 10)
	if a != b {
		t.Errorf("same inputs → different ids: %q vs %q", a, b)
	}
	c := MakeID("c1", "I'll send", 11)
	if a == c {
		t.Errorf("different offsets → same id: %q", a)
	}
	d := MakeID("c1", "I'll check", 10)
	if a == d {
		t.Errorf("different phrases → same id: %q", a)
	}
	e := MakeID("c2", "I'll send", 10)
	if a == e {
		t.Errorf("different cycle ids → same id: %q", a)
	}
}

package librarian

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- Schema migration backward compatibility ----

func TestNarrativeEntry_LegacyRecordDecodes(t *testing.T) {
	// A pre-migration JSONL record (4-field shape). New decoder
	// must accept it without error; new fields default to zero.
	raw := `{"cycle_id":"abc","timestamp":"2026-04-01T12:00:00Z","status":"complete","summary":"did a thing"}`
	var e NarrativeEntry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("legacy record should decode: %v", err)
	}
	if e.CycleID != "abc" {
		t.Errorf("CycleID = %q", e.CycleID)
	}
	if e.Status != NarrativeStatusComplete {
		t.Errorf("Status = %q", e.Status)
	}
	if e.Summary != "did a thing" {
		t.Errorf("Summary = %q", e.Summary)
	}
	// New fields should be zero.
	if e.SchemaVersion != 0 {
		t.Errorf("legacy SchemaVersion should be 0; got %d", e.SchemaVersion)
	}
	if !e.Intent.IsZero() {
		t.Errorf("legacy Intent should be zero; got %+v", e.Intent)
	}
	if !e.Outcome.IsZero() {
		t.Errorf("legacy Outcome should be zero; got %+v", e.Outcome)
	}
}

func TestNarrativeEntry_RichRecordRoundTrips(t *testing.T) {
	// Full new-shape record encodes + decodes to the same shape.
	original := NarrativeEntry{
		SchemaVersion: NarrativeSchemaVersion,
		CycleID:       "cyc-1",
		ParentCycleID: "cyc-0",
		EntryType:     EntryTypeNarrative,
		Summary:       "investigated auth",
		Intent: Intent{
			Classification: IntentExploration,
			Description:    "investigate the auth flow",
			Domain:         "auth",
		},
		Outcome: Outcome{
			Status:     OutcomeSuccess,
			Confidence: 0.85,
			Assessment: "found the timeout cause",
		},
		DelegationChain: []DelegationStep{
			{
				Agent:        "researcher",
				AgentCycleID: "agent-cyc-1",
				ToolsUsed:    []string{"brave_web_search"},
				InputTokens:  100,
				OutputTokens: 50,
				DurationMs:   2500,
			},
		},
		Decisions: []Decision{
			{Point: "approach", Choice: "delegate", Rationale: "external lookup needed"},
		},
		Topics: []string{"auth", "timeout"},
		Entities: Entities{
			Locations:     []string{"login service"},
			Organisations: []string{"Acme Corp"},
		},
		Sources: []Source{
			{SourceType: "web", URL: "https://example.com/auth", Name: "auth docs"},
		},
		Metrics: Metrics{
			InputTokens:  500,
			OutputTokens: 200,
			ToolCalls:    3,
			ModelUsed:    "mistral-small-2603",
		},
		Observations: []Observation{
			{ObservationType: "anomaly", Severity: ObservationWarning, Detail: "high latency"},
		},
		Status:   NarrativeStatusComplete,
		Keywords: []string{"auth", "timeout", "investigate"},
		Domain:   "auth",
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got NarrativeEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != NarrativeSchemaVersion {
		t.Errorf("SchemaVersion drift: %d → %d", original.SchemaVersion, got.SchemaVersion)
	}
	if got.Intent != original.Intent {
		t.Errorf("Intent drift: %+v → %+v", original.Intent, got.Intent)
	}
	if got.Outcome != original.Outcome {
		t.Errorf("Outcome drift: %+v → %+v", original.Outcome, got.Outcome)
	}
	if len(got.DelegationChain) != 1 || got.DelegationChain[0].Agent != "researcher" {
		t.Errorf("DelegationChain drift: %+v", got.DelegationChain)
	}
	if got.Metrics.ModelUsed != "mistral-small-2603" {
		t.Errorf("Metrics drift: %+v", got.Metrics)
	}
}

// ---- MirrorLegacyFields ----

func TestMirrorLegacyFields_OutcomeSuccessToStatusComplete(t *testing.T) {
	e := NarrativeEntry{
		Outcome: Outcome{Status: OutcomeSuccess},
	}
	e.MirrorLegacyFields()
	if e.Status != NarrativeStatusComplete {
		t.Errorf("Status = %q, want complete", e.Status)
	}
}

func TestMirrorLegacyFields_OutcomeFailureToStatusError(t *testing.T) {
	e := NarrativeEntry{
		Outcome: Outcome{Status: OutcomeFailure},
	}
	e.MirrorLegacyFields()
	if e.Status != NarrativeStatusError {
		t.Errorf("Status = %q, want error", e.Status)
	}
}

func TestMirrorLegacyFields_OutcomePartialToStatusComplete(t *testing.T) {
	// Partial maps to Complete for the legacy index — the cycle
	// delivered something. The structured Outcome carries the
	// nuance.
	e := NarrativeEntry{
		Outcome: Outcome{Status: OutcomePartial},
	}
	e.MirrorLegacyFields()
	if e.Status != NarrativeStatusComplete {
		t.Errorf("Status = %q, want complete", e.Status)
	}
}

func TestMirrorLegacyFields_DomainMirroredFromIntent(t *testing.T) {
	e := NarrativeEntry{
		Intent: Intent{Domain: "auth"},
	}
	e.MirrorLegacyFields()
	if e.Domain != "auth" {
		t.Errorf("Domain not mirrored: %q", e.Domain)
	}
}

func TestMirrorLegacyFields_DoesNotOverwriteExistingTopLevel(t *testing.T) {
	// When producer already set the legacy fields explicitly,
	// MirrorLegacyFields shouldn't clobber them.
	e := NarrativeEntry{
		Status: NarrativeStatusBlocked,
		Outcome: Outcome{Status: OutcomeFailure},
	}
	e.MirrorLegacyFields()
	if e.Status != NarrativeStatusBlocked {
		t.Errorf("MirrorLegacyFields overwrote explicit Status: %q", e.Status)
	}
}

func TestMirrorLegacyFields_StampsSchemaVersion(t *testing.T) {
	e := NarrativeEntry{}
	e.MirrorLegacyFields()
	if e.SchemaVersion != NarrativeSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", e.SchemaVersion, NarrativeSchemaVersion)
	}
}

func TestMirrorLegacyFields_Idempotent(t *testing.T) {
	e := NarrativeEntry{
		Outcome: Outcome{Status: OutcomeSuccess},
		Intent:  Intent{Domain: "x"},
	}
	e.MirrorLegacyFields()
	gotStatus, gotDomain, gotVersion := e.Status, e.Domain, e.SchemaVersion
	e.MirrorLegacyFields()
	if e.Status != gotStatus || e.Domain != gotDomain || e.SchemaVersion != gotVersion {
		t.Errorf("MirrorLegacyFields not idempotent: status=%q domain=%q ver=%d",
			e.Status, e.Domain, e.SchemaVersion)
	}
}

// ---- Type sanity ----

func TestEntryTypeStringValues(t *testing.T) {
	cases := map[EntryType]string{
		EntryTypeNarrative:   "narrative",
		EntryTypeAmendment:   "amendment",
		EntryTypeSummary:     "summary",
		EntryTypeObservation: "observation",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("EntryType %q != %q", string(got), want)
		}
	}
}

func TestOutcomeStatusStringValues(t *testing.T) {
	cases := map[OutcomeStatus]string{
		OutcomeSuccess: "success",
		OutcomePartial: "partial",
		OutcomeFailure: "failure",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("OutcomeStatus %q != %q", string(got), want)
		}
	}
}

func TestIntentClassificationMatchesSDVocabulary(t *testing.T) {
	// SD's IntentClassification has exactly these 9 values.
	// Drift here means the curator's controlled vocabulary
	// disagrees with SD — caught early.
	want := []IntentClassification{
		IntentDataReport, IntentDataQuery, IntentComparison,
		IntentTrendAnalysis, IntentMonitoringCheck, IntentExploration,
		IntentClarification, IntentSystemCommand, IntentConversation,
	}
	if len(want) != 9 {
		t.Errorf("classification count drift: %d, want 9", len(want))
	}
	// Pin the wire-format strings.
	pinned := map[IntentClassification]string{
		IntentDataReport:      "data_report",
		IntentDataQuery:       "data_query",
		IntentComparison:      "comparison",
		IntentTrendAnalysis:   "trend_analysis",
		IntentMonitoringCheck: "monitoring_check",
		IntentExploration:     "exploration",
		IntentClarification:   "clarification",
		IntentSystemCommand:   "system_command",
		IntentConversation:    "conversation",
	}
	for k, v := range pinned {
		if string(k) != v {
			t.Errorf("classification wire form %q != %q", string(k), v)
		}
	}
}

// ---- IsZero helpers ----

func TestIntentIsZero(t *testing.T) {
	if !(Intent{}).IsZero() {
		t.Error("empty Intent should be zero")
	}
	if (Intent{Classification: IntentExploration}).IsZero() {
		t.Error("populated Intent should not be zero")
	}
}

func TestOutcomeIsZero(t *testing.T) {
	if !(Outcome{}).IsZero() {
		t.Error("empty Outcome should be zero")
	}
	if (Outcome{Status: OutcomeSuccess}).IsZero() {
		t.Error("populated Outcome should not be zero")
	}
}

// ---- omitempty wire shape ----

func TestNarrativeEntry_EmptySliceFieldsOmitted(t *testing.T) {
	// Slice fields with omitempty drop cleanly when empty. Nested
	// struct fields don't (Go's omitempty doesn't recurse) — those
	// emit `{}` on the wire. The cost is ~30 bytes per record;
	// not worth a custom marshaller. Decoders read `{}` back as
	// zero structs cleanly via the IsZero helpers when needed.
	e := NarrativeEntry{
		CycleID: "x",
		Status:  NarrativeStatusComplete,
		Summary: "hello",
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(body)
	for _, key := range []string{
		"delegation_chain", "decisions", "topics", "sources",
		"observations", "keywords", "schema_version", "type",
		"strategy_used", "redacted",
	} {
		if strings.Contains(wire, `"`+key+`"`) {
			t.Errorf("empty %s should be omitted from wire; got: %s", key, wire)
		}
	}
}

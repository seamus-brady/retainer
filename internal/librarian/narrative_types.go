package librarian

import "time"

// EntryType discriminates the shape of a narrative record. SD's
// `narrative/types.gleam` uses this so the Archivist can emit
// amendments + summaries + observation records as their own
// types alongside the per-cycle Narrative entries. Today
// Retainer only writes Narrative; the others are reserved
// for when their producing subsystems land (Amendments need a
// curator-correction tool; Summaries need the periodic
// summary writer; ObservationEntry needs the observer's
// proactive emission).
type EntryType string

const (
	// EntryTypeNarrative — one cog cycle's record. The default
	// and the only type the current archivist emits.
	EntryTypeNarrative EntryType = "narrative"
	// EntryTypeAmendment — operator/agent correction to a prior
	// entry. Append-only; the original is never mutated.
	EntryTypeAmendment EntryType = "amendment"
	// EntryTypeSummary — periodic compressed summary across a
	// span of entries (weekly / monthly).
	EntryTypeSummary EntryType = "summary"
	// EntryTypeObservation — proactive entry from the observer
	// agent (anomaly noted, drift detected, etc.). Today
	// the observer is read-only; this lands when the full
	// observer per project_observer_eventual_shape ships.
	EntryTypeObservation EntryType = "observation"
)

// IntentClassification mirrors `cbr.IntentClassification` —
// duplicated here (rather than imported) because the librarian
// shouldn't depend on the cbr package. Same nine values; same
// semantics. The two enums must stay in sync — see the test
// `TestNarrativeIntentClassificationMatchesCbr` which pins
// them.
type IntentClassification string

const (
	IntentDataReport      IntentClassification = "data_report"
	IntentDataQuery       IntentClassification = "data_query"
	IntentComparison      IntentClassification = "comparison"
	IntentTrendAnalysis   IntentClassification = "trend_analysis"
	IntentMonitoringCheck IntentClassification = "monitoring_check"
	IntentExploration     IntentClassification = "exploration"
	IntentClarification   IntentClassification = "clarification"
	IntentSystemCommand   IntentClassification = "system_command"
	IntentConversation    IntentClassification = "conversation"
)

// OutcomeStatus captures cycle disposition. Three-state matches
// SD; binary forces conversational acks into success/failure
// awkwardly.
type OutcomeStatus string

const (
	OutcomeSuccess OutcomeStatus = "success"
	OutcomePartial OutcomeStatus = "partial"
	OutcomeFailure OutcomeStatus = "failure"
)

// Intent is the structured intent of a cycle. Curator-derived;
// not the verbatim user text.
type Intent struct {
	Classification IntentClassification `json:"classification,omitempty"`
	Description    string               `json:"description,omitempty"`
	Domain         string               `json:"domain,omitempty"`
}

// Outcome mirrors SD's narrative/types.gleam:Outcome. Lives on
// the entry separately from the legacy top-level Status field
// (kept for backward compat + SQL-index continuity).
type Outcome struct {
	Status     OutcomeStatus `json:"status,omitempty"`
	Confidence float64       `json:"confidence,omitempty"`
	Assessment string        `json:"assessment,omitempty"`
}

// DelegationStep is one agent dispatch from this cycle's
// perspective. Populated from the agent.CompletionRecord stream.
type DelegationStep struct {
	Agent          string `json:"agent,omitempty"`
	AgentCycleID   string `json:"agent_cycle_id,omitempty"`
	Instruction    string `json:"instruction,omitempty"`
	OutcomeText    string `json:"outcome,omitempty"`
	Contribution   string `json:"contribution,omitempty"`
	ToolsUsed      []string `json:"tools_used,omitempty"`
	InputTokens    int    `json:"input_tokens,omitempty"`
	OutputTokens   int    `json:"output_tokens,omitempty"`
	DurationMs     int64  `json:"duration_ms,omitempty"`
}

// Decision is a (point, choice, rationale, score) tuple that
// the curator can emit to capture the cog's reasoning anchors.
// Score is optional — SD uses it for D'-graded decisions; we
// don't have D' parity so it stays optional here.
type Decision struct {
	Point     string   `json:"point,omitempty"`
	Choice    string   `json:"choice,omitempty"`
	Rationale string   `json:"rationale,omitempty"`
	Score     *float64 `json:"score,omitempty"`
}

// DataPoint is a structured data observation extracted by the
// curator (e.g. a numeric measurement with units). Optional —
// most cycles produce zero data points.
type DataPoint struct {
	Label  string `json:"label,omitempty"`
	Value  string `json:"value,omitempty"`
	Unit   string `json:"unit,omitempty"`
	Period string `json:"period,omitempty"`
	Source string `json:"source,omitempty"`
}

// Entities groups named things mentioned in the cycle. Drives
// thread assignment (overlap scoring) and CBR retrieval. SD's
// shape exactly.
type Entities struct {
	Locations          []string    `json:"locations,omitempty"`
	Organisations      []string    `json:"organisations,omitempty"`
	DataPoints         []DataPoint `json:"data_points,omitempty"`
	TemporalReferences []string    `json:"temporal_references,omitempty"`
}

// Source is one external/internal reference the cycle drew on
// (URL, file, named source, etc.).
type Source struct {
	SourceType string `json:"source_type,omitempty"`
	URL        string `json:"url,omitempty"`
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	AccessedAt string `json:"accessed_at,omitempty"`
	DataDate   string `json:"data_date,omitempty"`
}

// Thread is a derived grouping of related cycles. Optional on
// the entry — populated by the threading subsystem when it
// lands (Phase deferred from the current spec).
type Thread struct {
	ThreadID         string `json:"thread_id,omitempty"`
	ThreadName       string `json:"thread_name,omitempty"`
	Position         int    `json:"position,omitempty"`
	PreviousCycleID  string `json:"previous_cycle_id,omitempty"`
	ContinuityNote   string `json:"continuity_note,omitempty"`
}

// Metrics is the per-cycle resource accounting. Populated from
// the cog's cycle log; the curator doesn't compute these.
type Metrics struct {
	TotalDurationMs   int64  `json:"total_duration_ms,omitempty"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
	ThinkingTokens    int    `json:"thinking_tokens,omitempty"`
	ToolCalls         int    `json:"tool_calls,omitempty"`
	AgentDelegations  int    `json:"agent_delegations,omitempty"`
	PolicyEvaluations int    `json:"policy_evaluations,omitempty"`
	ModelUsed         string `json:"model_used,omitempty"`
}

// ObservationSeverity is the severity level of an observation.
type ObservationSeverity string

const (
	ObservationInfo    ObservationSeverity = "info"
	ObservationWarning ObservationSeverity = "warning"
	ObservationError   ObservationSeverity = "error"
)

// Observation is a per-cycle anomaly or noteworthy event the
// curator captured. Distinct from the cycle log's mechanical
// telemetry — observations are interpreted, not logged.
type Observation struct {
	ObservationType string              `json:"observation_type,omitempty"`
	Severity        ObservationSeverity `json:"severity,omitempty"`
	Detail          string              `json:"detail,omitempty"`
}

// NarrativeSchemaVersion is the on-disk shape version. Bumped
// on breaking changes; new fields with omitempty don't require
// a bump.
const NarrativeSchemaVersion = 2

// IsZero reports whether an Intent has no meaningful content.
// Used by the writer to decide whether to encode the field.
func (i Intent) IsZero() bool {
	return i.Classification == "" && i.Description == "" && i.Domain == ""
}

// IsZero reports whether an Outcome is empty.
func (o Outcome) IsZero() bool {
	return o.Status == "" && o.Confidence == 0 && o.Assessment == ""
}

// IsZero reports whether Entities has any populated field.
func (e Entities) IsZero() bool {
	return len(e.Locations) == 0 &&
		len(e.Organisations) == 0 &&
		len(e.DataPoints) == 0 &&
		len(e.TemporalReferences) == 0
}

// IsZero reports whether Metrics has any populated field.
func (m Metrics) IsZero() bool {
	return m.TotalDurationMs == 0 &&
		m.InputTokens == 0 &&
		m.OutputTokens == 0 &&
		m.ThinkingTokens == 0 &&
		m.ToolCalls == 0 &&
		m.AgentDelegations == 0 &&
		m.PolicyEvaluations == 0 &&
		m.ModelUsed == ""
}

// _ time.Time anchor — keeps the import linkage stable while
// no public function uses time directly here. Moves into the
// signature when DurationMs starts being computed from times
// instead of passed in raw.
var _ = time.Now

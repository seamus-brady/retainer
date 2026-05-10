// Package cbr is the case-based reasoning memory layer. Each Case
// captures a (problem, solution, outcome) record persisted append-only
// as JSONL with a librarian-owned in-memory index for retrieval.
//
// Cases are derived from completed cycles by the archivist: it takes
// the cycle's NarrativeEntry plus outcome data (success/failure, agents
// involved, tools called) and emits a Case. Retrieval at runtime is
// 6-signal weighted-fusion (field score + inverted index + recency +
// domain match + utility + embedding cosine) — see CaseBase.
//
// Selective port of Springdrift's `cbr/`: same schema, same retrieval
// scoring, same K=4 cap. Cuts: no `mine_patterns` (Remembrancer is a
// separate deferred subsystem).
package cbr

import "time"

// Category is the kind of knowledge a Case captures. Assigned
// deterministically by the archivist based on outcome shape; not
// LLM-derived.
type Category string

const (
	// CategoryStrategy — high-level approach that worked.
	CategoryStrategy Category = "strategy"
	// CategoryCodePattern — reusable code snippet or template.
	CategoryCodePattern Category = "code_pattern"
	// CategoryTroubleshooting — how to diagnose / fix a specific
	// problem.
	CategoryTroubleshooting Category = "troubleshooting"
	// CategoryPitfall — what NOT to do, learned from failure.
	CategoryPitfall Category = "pitfall"
	// CategoryDomainKnowledge — factual knowledge about a domain.
	CategoryDomainKnowledge Category = "domain_knowledge"
)

// SchemaVersion tracks the on-disk shape so a future schema bump can
// rewrite or migrate older JSONL records. Bump only when the layout
// breaks compatibility with a JSON decode of the prior version.
const SchemaVersion = 1

// Status describes how a cycle ended for case-outcome purposes.
// Mirrors the narrative status taxonomy but lives here so the case
// shape doesn't depend on librarian internals. Three-state matches
// SD's `OutcomeStatus`: `partial` is the load-bearing middle ground
// for cycles where some work was done but claims exceed what the
// tool log shows, OR expected tools didn't fire. Without `partial`,
// conversational acks and edge cases get forced into success or
// failure and pollute CBR retrieval — see
// `doc/specs/memory-and-logging-audit.md`.
type Status string

const (
	StatusSuccess Status = "success"
	// StatusPartial — some work was done, but claims exceed what
	// the tools show or expected tools did not fire. Drives
	// CategoryDomainKnowledge in the deterministic Classify
	// cascade.
	StatusPartial Status = "partial"
	StatusFailure Status = "failure"
)

// IntentClassification is the controlled vocabulary the Curator
// LLM uses to label what kind of work a cycle was. Carried on the
// case's Problem so retrieval signals + category assignment can
// distinguish a research query from a pleasantry.
//
// Mirrors SD's `narrative/types.gleam:IntentClassification` exactly.
// Operators or the Remembrancer can extend this set; callers should
// treat unknown values as a no-op (no category, no special handling).
type IntentClassification string

const (
	// IntentDataReport — emitting a structured/tabular result.
	IntentDataReport IntentClassification = "data_report"
	// IntentDataQuery — fetching information from an external source.
	IntentDataQuery IntentClassification = "data_query"
	// IntentComparison — comparing two or more things.
	IntentComparison IntentClassification = "comparison"
	// IntentTrendAnalysis — reasoning about change over time.
	IntentTrendAnalysis IntentClassification = "trend_analysis"
	// IntentMonitoringCheck — a routine health/status check.
	IntentMonitoringCheck IntentClassification = "monitoring_check"
	// IntentExploration — open-ended investigation.
	IntentExploration IntentClassification = "exploration"
	// IntentClarification — refining a prior question.
	IntentClarification IntentClassification = "clarification"
	// IntentSystemCommand — operator-driven control (init, list,
	// inspect, configure).
	IntentSystemCommand IntentClassification = "system_command"
	// IntentConversation — pleasantry, ack, banter, follow-up
	// chat. Cases produced from Conversation cycles deliberately
	// get an empty Category — stored for audit but not surfaced
	// as patterns by recall_cases.
	IntentConversation IntentClassification = "conversation"
)

// IsKnown reports whether v is a recognised classification value.
// Curators that produce unknown strings get treated as "no
// classification" for category-assignment purposes.
func (c IntentClassification) IsKnown() bool {
	switch c {
	case IntentDataReport, IntentDataQuery, IntentComparison,
		IntentTrendAnalysis, IntentMonitoringCheck, IntentExploration,
		IntentClarification, IntentSystemCommand, IntentConversation:
		return true
	}
	return false
}

// Problem is the query-shaped descriptor of what the case dealt with.
// All fields are lowercased on retrieval-side comparisons; producers
// (archivist) should write in their natural casing.
type Problem struct {
	// UserInput is the operator's original input (truncated). Kept for
	// post-hoc inspection / curation tools; not used in retrieval
	// scoring directly.
	UserInput string `json:"user_input,omitempty"`
	// Intent is a short LLM-derived description of what the user was
	// trying to do ("debug auth flow", "summarise the architecture
	// document"). Drives intent-match in weighted field scoring.
	//
	// Critical: must NOT be the verbatim user text. Old records
	// produced before the Curator pipeline landed copied the user's
	// raw input here, polluting retrieval — those are grandfathered
	// but new derivation must populate this from the curation phase.
	Intent string `json:"intent"`
	// IntentClass is the controlled-vocabulary classification of the
	// cycle's intent. Drives Conversation→nil-category routing in
	// Classify. Empty on records produced before the Curator
	// pipeline landed — those records get default category-cascade
	// behaviour.
	IntentClass IntentClassification `json:"intent_class,omitempty"`
	// Domain is the subject area ("auth", "research", "scheduler").
	// Drives the domain-match signal (binary 0/1). LLM-derived by
	// the Curator phase; empty when the curator couldn't determine
	// one.
	Domain string `json:"domain"`
	// Entities are named things mentioned (people, repos, files,
	// services). Driven into the inverted index + Jaccard.
	Entities []string `json:"entities,omitempty"`
	// Keywords are non-entity salient terms. Driven into the inverted
	// index + Jaccard.
	Keywords []string `json:"keywords,omitempty"`
	// QueryComplexity is the classifier's tag ("simple" / "moderate"
	// / "complex"). Drives one inverted-index token; optional.
	QueryComplexity string `json:"query_complexity,omitempty"`
}

// Solution is what was done in response to the Problem. Used when
// surfacing a case to the agent — "last time you faced X, you tried Y
// using tools/agents Z".
type Solution struct {
	// Approach is a short prose description of what worked (or didn't).
	// Tokenised into the inverted index.
	Approach string `json:"approach"`
	// AgentsUsed lists the specialist agents called. Tokenised into
	// the inverted index.
	AgentsUsed []string `json:"agents_used,omitempty"`
	// ToolsUsed lists tools the cog or agents dispatched. Tokenised
	// into the inverted index.
	ToolsUsed []string `json:"tools_used,omitempty"`
	// Steps is an ordered list of what happened in order. Optional;
	// kept for inspection, not retrieval.
	Steps []string `json:"steps,omitempty"`
}

// Outcome describes how the cycle ended. confidence is decayed at read
// time via internal/decay (half-life-based) — never mutated on disk.
type Outcome struct {
	// Status captures the high-level success/failure.
	Status Status `json:"status"`
	// Confidence is the original [0, 1] confidence assigned at case
	// derivation. Decay applies at retrieval time; this field is
	// immutable.
	Confidence float64 `json:"confidence"`
	// Assessment is a short prose explanation of WHY it was a success
	// or failure. Surfaces in observer recall_cases output.
	Assessment string `json:"assessment,omitempty"`
	// Pitfalls are things to watch out for next time, populated for
	// failures and partial successes. Drives CategoryPitfall.
	Pitfalls []string `json:"pitfalls,omitempty"`
}

// UsageStats track how a case has performed in retrieval over time.
// Updated by the archivist when a cycle that retrieved this case
// completes — the cycle's outcome feeds back into the case's utility
// score. See utility() in this package.
type UsageStats struct {
	// RetrievalCount is the total number of times this case has been
	// returned by recall_cases.
	RetrievalCount int `json:"retrieval_count"`
	// RetrievalSuccessCount is how many of those retrievals were in
	// cycles that ultimately succeeded. The Laplace-smoothed ratio
	// drives the utility-score signal.
	RetrievalSuccessCount int `json:"retrieval_success_count"`
	// HelpfulCount is operator-curated: a case explicitly marked
	// helpful via the observer's annotate_case tool.
	HelpfulCount int `json:"helpful_count"`
	// HarmfulCount is operator-curated: a case explicitly marked
	// harmful — eligible for suppress_case follow-up.
	HarmfulCount int `json:"harmful_count"`
}

// Case is the on-disk record. Append-only via the cases JSONL writer.
// Updates (curation, usage stats) land as new records that supersede;
// the archive is immutable per project_archive_immutable.
type Case struct {
	// ID is the case's UUID. Set by the archivist at derivation;
	// never reused.
	ID string `json:"case_id"`
	// Timestamp is when the case was derived. RFC3339 on disk.
	Timestamp time.Time `json:"timestamp"`
	// SchemaVersion records the layout the record was written with
	// so future migrations can act selectively.
	SchemaVersion int `json:"schema_version"`
	// Problem describes what was being worked on.
	Problem Problem `json:"problem"`
	// Solution describes what was done.
	Solution Solution `json:"solution"`
	// Outcome describes how it ended.
	Outcome Outcome `json:"outcome"`
	// SourceNarrativeID is the cycle ID this case was derived from.
	// Lets the agent trace a recalled case back to its narrative.
	SourceNarrativeID string `json:"source_narrative_id"`
	// Profile is a multi-workspace tag (reserved). Today empty.
	Profile string `json:"profile,omitempty"`
	// Redacted is set by suppress_case to remove the case from
	// retrieval. The record stays in JSONL; the librarian's CaseBase
	// excludes it from retrieval results.
	Redacted bool `json:"redacted,omitempty"`
	// Category is the deterministic classification (Strategy /
	// CodePattern / Troubleshooting / Pitfall / DomainKnowledge).
	// Optional — the archivist sets it, but old records may not have
	// it.
	Category Category `json:"category,omitempty"`
	// UsageStats tracks retrieval feedback.
	UsageStats *UsageStats `json:"usage_stats,omitempty"`
	// SupersededBy carries the ID of the dominant case when this
	// record has been retired by the deduper. Distinct from Redacted
	// (which is operator-driven via suppress_case): SupersededBy is
	// the housekeeper's signal that another case covers the same
	// ground better. CaseBase retrieval skips superseded cases (same
	// effect as Redacted) but the audit trail preserves the link.
	// Empty on active cases.
	SupersededBy string `json:"superseded_by,omitempty"`
	// EmbedderID identifies the embedder + model the case's vector
	// was produced with. Stored on the Case (not just the CaseBase)
	// so a future model swap is detectable per-case.
	EmbedderID string `json:"embedder_id,omitempty"`
	// Embedding is the dense vector (when available). Stored on disk
	// so a librarian replay can hydrate the CaseBase without
	// re-embedding every case at startup. Optional — failed
	// embeddings produce a case with no vector; CBR retrieval
	// auto-renormalises weights when embeddings are absent.
	Embedding []float32 `json:"embedding,omitempty"`
}

// Query is what callers hand to the CaseBase to retrieve similar cases.
// Mirrors Problem field-for-field for the matched signals plus a
// MaxResults cap (default 4 per Memento paper finding that more
// causes context pollution).
type Query struct {
	Intent          string
	Domain          string
	Keywords        []string
	Entities        []string
	QueryComplexity string
	// MaxResults caps the returned case count. Zero defaults to
	// DefaultMaxResults (=4). Higher values are honored but the
	// caller carries the context-pollution risk.
	MaxResults int
}

// DefaultMaxResults is the K=4 cap from SD's CBR. Per Memento's
// finding (paper 2510.04618) more than four retrieved cases starts
// poisoning the model's context.
const DefaultMaxResults = 4

// Scored pairs a Case with the retrieval score that surfaced it.
// CaseBase.Retrieve returns these sorted by Score descending so
// callers can take the top-K.
type Scored struct {
	Score float64
	Case  Case
}

// Utility computes the Laplace-smoothed retrieval-success rate from a
// Case's usage stats. (RetrievalSuccessCount + 1) / (RetrievalCount +
// 2). Returns 0.5 (neutral) when stats are nil — a fresh case with no
// retrieval history shouldn't be penalised relative to one that's
// been retrieved a few times unsuccessfully.
//
// Used as the 6th signal in CaseBase retrieval weighting.
func Utility(s *UsageStats) float64 {
	if s == nil {
		return 0.5
	}
	num := float64(s.RetrievalSuccessCount + 1)
	denom := float64(s.RetrievalCount + 2)
	return num / denom
}

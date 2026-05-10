// Package captures is the commitment tracker. It scans the cog's
// recent reply text for phrases the agent uses to commit to
// deferred work ("I'll send the report tomorrow"), persists each
// match as a Capture, and surfaces the pending count in the
// sensorium so the agent perceives its own outstanding promises
// every cycle.
//
// Selective port from Springdrift's `captures/` subsystem:
//
//	Load-bearing here:
//	  - Capture struct + Status enum
//	  - JSONL persistence with daily rotation + replay
//	  - Status lifecycle: Pending → Expired (auto sweep)
//	  - Sensorium count (the consumer that closes the loop)
//
//	Deferred to v1.1:
//	  - LLM-based scanner (we use heuristic phrase matching)
//	  - OperatorAsk source (agent-self only for V1)
//	  - clarify_capture / dismiss_capture tools
//	  - Taskwarrior promotion via a `due:` modification
//	  - Satisfied state via auto-heuristic
//
// The minimum viable shape is "agent makes a promise → it lands
// here → sensorium says <captures pending="N"/> → agent walks the
// list and decides what to do." That's enough to keep the
// dev-manager pitch credible — the agent doesn't make promises and
// forget them.
package captures

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Status is the lifecycle state of one Capture. Stored in JSONL;
// the most-recent entry per ID wins on replay. Adding an Expired
// or Dismissed entry with the same ID supersedes the Pending
// entry without rewriting it (per project_archive_immutable).
type Status string

const (
	// StatusPending — captured but not yet acted on, dismissed,
	// or expired. The default state for newly-detected commitments.
	StatusPending Status = "pending"
	// StatusExpired — auto-aged past CapturesExpiryDays without
	// resolution. Written by the expiry sweep.
	StatusExpired Status = "expired"
	// StatusDismissed — operator or agent declared this commitment
	// no longer relevant. Reserved; v1.0 has no tool to set this.
	StatusDismissed Status = "dismissed"
	// StatusSatisfied — commitment was delivered on. Reserved;
	// v1.0 has no tool to set this.
	StatusSatisfied Status = "satisfied"
)

// Source records where a capture originated. v1.0 only emits
// AgentSelf; OperatorAsk + InboundComms are reserved for the
// v1.1 LLM-based scanner.
type Source string

const (
	// SourceAgentSelf — agent made the promise in its own reply.
	SourceAgentSelf Source = "agent_self"
	// SourceOperatorAsk — operator asked for deferred work in an
	// input message. Reserved; v1.0 doesn't scan inputs.
	SourceOperatorAsk Source = "operator_ask"
	// SourceInboundComms — derived from email or webhook content.
	// Reserved; needs the comms agent integration.
	SourceInboundComms Source = "inbound_comms"
)

// SchemaVersion is the wire-format version stamped on every
// capture record. Bump on any incompatible field change so old
// JSONL entries stay readable. Append-only forever.
const SchemaVersion = 1

// Capture is one detected commitment. Stable JSONL wire format —
// new fields go at the end with omitempty so older readers can
// skip them.
type Capture struct {
	// SchemaVersion is the wire format of this record. Bumped on
	// breaking changes only.
	SchemaVersion int `json:"schema_version"`
	// ID is content-addressed: deterministic hash of cycle id
	// plus the matched phrase plus its byte offset. Re-running
	// the scanner on the same cycle log is idempotent — same id
	// twice means we skip the duplicate write.
	ID string `json:"id"`
	// CreatedAt is the wall-clock time of the original detection.
	// On Status=Expired records this stays at original-detection
	// time; the new Status entry uses Timestamp for sweep ordering.
	CreatedAt time.Time `json:"created_at"`
	// Timestamp is when THIS record was written. Equal to
	// CreatedAt on the original Pending entry; later for
	// Expired/Dismissed/Satisfied supersession entries.
	Timestamp time.Time `json:"timestamp"`
	// SourceCycleID names the cog cycle whose reply text
	// generated the capture. The agent navigates back to context
	// via this field.
	SourceCycleID string `json:"source_cycle_id"`
	// Text is the matched phrase + a short surrounding window —
	// the actionable substance of the commitment. Capped to keep
	// the JSONL compact; full reply text lives in cycle log.
	Text string `json:"text"`
	// Source identifies who originated the capture (agent/operator/comms).
	Source Source `json:"source"`
	// Status is the current lifecycle state. Most-recent-by-ID
	// wins on replay.
	Status Status `json:"status"`
	// Reason is a free-form note attached to status transitions
	// (e.g. expiry sweep records "auto-expired after 7 days").
	// Empty on Pending entries.
	Reason string `json:"reason,omitempty"`
}

// MakeID builds the content-addressed capture ID from the cycle
// id, the matched phrase, and the byte offset of the match within
// the source text. The same scan against the same cycle text
// always produces the same ID, so re-runs are idempotent.
//
// Stable hash format: hex sha256, truncated to 12 chars (48 bits
// of entropy — collision-safe at the per-workspace scale; even
// 100k captures has effectively zero collision probability).
func MakeID(cycleID, phrase string, offset int) string {
	h := sha256.New()
	h.Write([]byte(cycleID))
	h.Write([]byte{'|'})
	h.Write([]byte(strings.ToLower(phrase)))
	h.Write([]byte{'|'})
	h.Write([]byte{byte(offset >> 8), byte(offset)})
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:12]
}

// Subdir is the directory under <dataDir> where capture JSONL
// files live. Lines up with `narrative/`, `facts/`, `cases/`.
const Subdir = "captures"

// FilenameSuffix is appended to the daily date to form the JSONL
// file name (e.g. `2026-05-09-captures.jsonl`). Matches the
// daily-rotation convention used by narrative + facts + comms.
const FilenameSuffix = "-captures.jsonl"

// DefaultExpiryDays is how long a Pending capture sits before the
// expiry sweep auto-marks it Expired. 7 days mirrors SD's default
// (`captures_expiry_days = 14` is the SD default; we tighten to
// 7 because Retainer workspaces are typically operator-driven
// rather than auto-running, so stale captures are noisier).
const DefaultExpiryDays = 7

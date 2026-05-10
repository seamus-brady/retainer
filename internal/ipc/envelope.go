// Package ipc defines the wire envelope between the cog (parent
// process) and a subprocess specialist agent (researcher today;
// comms / planner / writer when those slices land).
//
// Wire format: ndJSON over stdin/stdout. One JSON object per line,
// `\n`-terminated. Stderr is reserved for the subprocess's slog
// text output — the wrapper-actor reads it in a separate goroutine
// and forwards into the cog's logger.
//
// The envelope is intentionally tiny — header + payload + workspace
// contract. Artefacts live on the filesystem; the wire carries
// paths, never file content. See `doc/roadmap/shipped/subprocess-
// binaries.md` for the architecture rationale.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// timestampLayout is the wire format for Request / Response
// timestamps. RFC3339 matches every other JSONL we ship (cycle-log,
// narrative, facts) so log correlation across processes is
// trivial.
const timestampLayout = time.RFC3339

// Status enumerates the response disposition values. Defined as
// constants (not just strings) so encode/decode validation is
// exhaustive.
const (
	StatusProgress = "progress"
	StatusComplete = "complete"
	StatusError    = "error"
)

// Request is what the cog writes to the subprocess's stdin.
//
// One Request per dispatch. Subprocess reads it, runs its react
// loop, emits Response lines. Each field doc-comment explains its
// load-bearing role in the contract — operators reading the JSON on
// disk should be able to reconstruct the protocol from the comments
// alone.
type Request struct {
	// ID is the dispatch's unique identifier (UUID). Used as the
	// agent's cycle ID so cycle-log + DAG events from the
	// subprocess line up with this dispatch.
	ID string `json:"id"`

	// ParentCycleID is the cog cycle that triggered the dispatch.
	// Round-trips on every Response so the subprocess can stamp
	// its own cycle-log events with `parent_id` matching the cog
	// cycle — same provenance shape as the curator_assembled event.
	ParentCycleID string `json:"parent_cycle_id"`

	// InstanceID is the workspace's stable agent identity prefix
	// (8 chars of the agent UUID, see internal/agentid). The cog
	// stamps it; the subprocess copies it onto every cycle-log
	// event it emits so events from cog + subprocess share an
	// instance label. Optional — empty means the cog booted
	// without identity (telemetry degrades silently).
	InstanceID string `json:"instance_id,omitempty"`

	// Agent is the specialist's name ("researcher", "comms"). The
	// subprocess ignores this (it knows what it is); the wire
	// carries it for log clarity + multi-agent dispatch debugging.
	Agent string `json:"agent"`

	// Timestamp is when the cog created the request, RFC3339.
	Timestamp string `json:"timestamp"`

	// Instruction is the work description — typically the cog's
	// LLM-generated arg to the agent_<name> delegate tool. Becomes
	// the user message in the subprocess's agent react loop.
	Instruction string `json:"instruction"`

	// Context is optional supplementary text. Today empty; reserved
	// for future cog-side enrichment (recent narrative, retrieved
	// cases, etc.) without expanding the envelope schema.
	Context string `json:"context,omitempty"`

	// WorkspaceRoot is the absolute path of the workspace dir. The
	// subprocess reads <root>/config/config.toml for provider /
	// model and treats <root>/data/ as the canonical data dir.
	WorkspaceRoot string `json:"workspace_root"`

	// ArtefactDir is the workspace-relative dir the subprocess can
	// write artefacts into. Pre-created by the cog before spawn so
	// the subprocess doesn't need create permissions on the wider
	// data dir.
	ArtefactDir string `json:"artefact_dir"`

	// InputPaths are workspace-relative files the agent should
	// read. Today unused; reserved for future "here's a draft to
	// review" / "here's a paper to summarise" flows where the cog
	// stages content for the agent.
	InputPaths []string `json:"input_paths,omitempty"`
}

// Validate sanity-checks the request fields. Used both at the cog
// (before write) and the subprocess (after read) so a malformed
// dispatch fails loudly at the boundary that produced it.
//
// Returns nil when the envelope is well-formed. Errors point at the
// first field that's wrong — multiple problems aren't aggregated
// (callers fix one at a time during development; production paths
// can't legitimately produce malformed envelopes).
func (r Request) Validate() error {
	if r.ID == "" {
		return errors.New("ipc: request id is required")
	}
	if r.Agent == "" {
		return errors.New("ipc: request agent is required")
	}
	if r.Instruction == "" {
		return errors.New("ipc: request instruction is required")
	}
	if r.WorkspaceRoot == "" {
		return errors.New("ipc: request workspace_root is required")
	}
	if r.Timestamp != "" {
		if _, err := time.Parse(timestampLayout, r.Timestamp); err != nil {
			return fmt.Errorf("ipc: request timestamp %q must be RFC3339: %w", r.Timestamp, err)
		}
	}
	return nil
}

// Response is what the subprocess writes to stdout. Multiple
// responses per dispatch are allowed (zero or more `progress`,
// then exactly one terminal `complete` / `error`).
//
// Field grouping follows the user-facing protocol doc — header,
// disposition, complete-only fields, progress-only fields,
// error-only fields, telemetry. Fields default to omitempty so a
// progress envelope doesn't carry zero-valued complete fields.
type Response struct {
	// ID echoes the originating Request.ID. Mandatory on every
	// response so a wrapper-actor handling concurrent dispatches
	// (future) can route correctly.
	ID string `json:"id"`

	// ParentCycleID echoes the originating Request.ParentCycleID.
	ParentCycleID string `json:"parent_cycle_id"`

	// Agent identifies the responder.
	Agent string `json:"agent"`

	// Timestamp is when the response was emitted, RFC3339.
	Timestamp string `json:"timestamp"`

	// Status is the disposition: progress, complete, or error. The
	// terminal response is whichever of complete / error fires
	// first — wrapper-actor stops reading after seeing one.
	Status string `json:"status"`

	// --- Complete-only fields ---

	// Success is true when the agent finished its task as intended.
	// On a `complete` response with success=false, the agent ran
	// but didn't deliver — distinct from `error` (which indicates
	// the run itself failed). Cog surfaces success=false to its
	// react loop the same way it surfaces an in-process
	// Outcome.IsSuccess()=false today.
	Success bool `json:"success,omitempty"`

	// Result is the agent's final assistant text (the "tool result"
	// the cog gives back to its own LLM). Empty on error.
	Result string `json:"result,omitempty"`

	// ArtefactPaths are workspace-relative paths the subprocess
	// wrote during the dispatch. Empty when the agent didn't write
	// any (typical for the researcher today). The cog can read
	// them if downstream tools need to.
	ArtefactPaths []string `json:"artefact_paths,omitempty"`

	// --- Progress-only fields ---

	// Turn is the current react-loop turn (1-based) when the
	// progress was emitted.
	Turn int `json:"turn,omitempty"`

	// MaxTurns is the configured react-loop ceiling.
	MaxTurns int `json:"max_turns,omitempty"`

	// ToolName is the tool the agent just dispatched (one progress
	// envelope per tool call). Empty when the agent hasn't called
	// a tool yet.
	ToolName string `json:"tool_name,omitempty"`

	// --- Error-only fields ---

	// Error is the human-readable failure reason. Populated on
	// `status: error`; the cog surfaces it as the tool's IsError
	// content.
	Error string `json:"error,omitempty"`

	// --- Telemetry (complete OR error) ---

	// InputTokens / OutputTokens summed across every LLM call in
	// the react loop. Drive the cog's cycle-log telemetry events.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// DurationMs is the subprocess's wall-clock from receiving the
	// request to emitting this response.
	DurationMs int64 `json:"duration_ms,omitempty"`

	// ToolsUsed lists the tool names the agent dispatched, in
	// order, duplicates allowed.
	ToolsUsed []string `json:"tools_used,omitempty"`
}

// IsTerminal reports whether this response ends the dispatch.
// Wrapper-actors stop reading on the first terminal response.
func (r Response) IsTerminal() bool {
	return r.Status == StatusComplete || r.Status == StatusError
}

// Validate checks response fields against the disposition. Called
// by the wrapper-actor after every read so a malformed response
// doesn't get treated as a valid dispatch outcome.
func (r Response) Validate() error {
	if r.ID == "" {
		return errors.New("ipc: response id is required")
	}
	switch r.Status {
	case StatusProgress, StatusComplete, StatusError:
	default:
		return fmt.Errorf("ipc: response status %q must be progress / complete / error", r.Status)
	}
	if r.Status == StatusError && strings.TrimSpace(r.Error) == "" {
		return errors.New("ipc: error response missing error field")
	}
	if r.Timestamp != "" {
		if _, err := time.Parse(timestampLayout, r.Timestamp); err != nil {
			return fmt.Errorf("ipc: response timestamp %q must be RFC3339: %w", r.Timestamp, err)
		}
	}
	return nil
}

// WriteRequest serialises a Request and writes it as one JSON line
// to w. Stamps Timestamp to now if the caller left it empty (per
// `feedback_actor_timestamps` — let the boundary that physically
// emits the value assign it).
//
// Returns an error from json.Marshal or the underlying writer.
// Validate before write so the producer surfaces malformed
// envelopes loudly.
func WriteRequest(w io.Writer, req Request) error {
	if req.Timestamp == "" {
		req.Timestamp = time.Now().UTC().Format(timestampLayout)
	}
	if err := req.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("ipc: marshal request: %w", err)
	}
	body = append(body, '\n')
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("ipc: write request: %w", err)
	}
	return nil
}

// WriteResponse serialises a Response and writes it as one JSON
// line to w. Stamps Timestamp on empty.
func WriteResponse(w io.Writer, resp Response) error {
	if resp.Timestamp == "" {
		resp.Timestamp = time.Now().UTC().Format(timestampLayout)
	}
	if err := resp.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("ipc: marshal response: %w", err)
	}
	body = append(body, '\n')
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("ipc: write response: %w", err)
	}
	return nil
}

// ReadRequest reads exactly one Request from r. Used by the
// subprocess at startup — it expects one request, runs the task,
// exits. ReadRequest is one-shot; pipelined dispatches are not the
// V1 shape (one-shot subprocesses match the spec).
//
// Returns io.EOF if the stream is empty (cog spawned the
// subprocess and didn't write anything — process-level bug worth
// surfacing rather than blocking forever).
func ReadRequest(r io.Reader) (Request, error) {
	scanner := bufio.NewScanner(r)
	// Generous buffer: instruction text could be a paragraph; 1MB
	// is plenty without imposing artificial caps that bite later.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return Request{}, fmt.Errorf("ipc: read request: %w", err)
		}
		return Request{}, io.EOF
	}
	var req Request
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		return Request{}, fmt.Errorf("ipc: decode request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req, nil
}

// Reader streams Responses from r. Used by the cog-side wrapper-
// actor — it reads progress envelopes followed by a terminal
// complete / error. Constructed once per dispatch, discarded after
// the terminal response.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader wraps an io.Reader with the framing helpers. Buffer
// sized large enough for realistic result text (long research
// summaries, fetched URLs); 4MB headroom matches the JSONL
// readers elsewhere.
func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &Reader{scanner: s}
}

// Next reads the next Response. Returns io.EOF when the stream
// closes cleanly. Caller stops on the first response where
// IsTerminal() is true OR on EOF (which the wrapper-actor treats
// as a process-level failure).
func (r *Reader) Next() (Response, error) {
	if !r.scanner.Scan() {
		if err := r.scanner.Err(); err != nil {
			return Response{}, fmt.Errorf("ipc: read response: %w", err)
		}
		return Response{}, io.EOF
	}
	var resp Response
	if err := json.Unmarshal(r.scanner.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("ipc: decode response (line=%q): %w", r.scanner.Text(), err)
	}
	if err := resp.Validate(); err != nil {
		return Response{}, err
	}
	return resp, nil
}

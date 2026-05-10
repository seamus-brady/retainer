// Package transcript replays a day's cycle-log JSONL into the
// operator-readable chat transcript the webui's day-view
// surfaces.
//
// Read-only: never mutates the cycle log; never writes to disk.
// Cog emits `cycle_start.Text` (operator input) and
// `cycle_complete.Text` (assistant reply), so a day's
// conversation reconstructs from the JSONL alone — no need to
// cross-reference narrative or message-history state.
//
// Used by `cmd/retainer-webui/server.go`'s
// `/api/days/{date}` and `/api/days/{date}/export.md` handlers.
package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/cyclelog"
)

// Role discriminates a Turn's speaker. Operator-readable
// strings (lowercase) so the webui's JSON response can pass
// these straight through without translation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleError     Role = "error"
)

// Turn is one user message OR one assistant reply.  CycleID
// pairs the two halves of a conversation turn (user and
// assistant share the same id). Status carries the cycle's
// final state (complete / error / abandoned / blocked) so the
// UI can flag failed cycles distinctly.
type Turn struct {
	Role      Role      `json:"role"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
	CycleID   string    `json:"cycle_id"`
	Status    string    `json:"status,omitempty"`
}

// LoadDir lists all dates that have a cycle-log file under
// dir, sorted newest-first. Format: YYYY-MM-DD strings keyed to
// `<dir>/YYYY-MM-DD.jsonl`. Empty when the dir doesn't exist
// (fresh workspace) — distinguished from a true error so the
// UI can render an empty state cleanly.
func LoadDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("transcript: readdir %s: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		stem := strings.TrimSuffix(name, ".jsonl")
		if _, err := time.Parse("2006-01-02", stem); err != nil {
			continue
		}
		out = append(out, stem)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// LoadDay reads the cycle-log file for the given date and
// reduces it into ordered Turns. Pairs `cycle_start` (user
// turn) with the matching `cycle_complete` (assistant turn) by
// CycleID. Cycles whose start has no matching complete are
// surfaced with a placeholder assistant turn marking the
// status — useful when the cog crashed mid-cycle and the
// operator is reviewing what they last asked.
//
// Filters to top-level cycles only — agent_cycle_start /
// agent_cycle_complete are inner sub-cycles the operator
// already saw outcomes for; cluttering the transcript view
// with them isn't useful.
//
// Returns an empty slice + nil err when the date file doesn't
// exist (operator picking a date with no activity).
func LoadDay(dir, date string) ([]Turn, error) {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, fmt.Errorf("transcript: invalid date %q: %w", date, err)
	}
	path := filepath.Join(dir, date+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("transcript: open %s: %w", path, err)
	}
	defer f.Close()

	// First pass: collect by cycle id. Cycle start carries
	// the user input; cycle complete carries the assistant
	// reply + final status. Order in the output preserves the
	// cycle_start arrival order so the chat reads top-to-
	// bottom in chronological order.
	type pair struct {
		cycleID  string
		userText string
		userAt   time.Time
		replyText string
		replyAt   time.Time
		status   string
		hasReply bool
	}
	pairs := make([]*pair, 0, 32)
	byID := make(map[string]*pair, 32)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev cyclelog.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Don't fail the whole replay over one corrupt
			// line — surface a placeholder error turn for
			// the operator to investigate but keep going.
			continue
		}
		// Skip nested agent cycles — they're surfaced via
		// inspect_cycle, not the chat.
		if ev.NodeType == "agent" {
			continue
		}
		switch ev.Type {
		case cyclelog.EventCycleStart:
			if ev.Text == "" {
				// Pre-text-field cycles (legacy logs) skip
				// silently — nothing to render.
				continue
			}
			p := &pair{
				cycleID:  ev.CycleID,
				userText: ev.Text,
				userAt:   ev.Timestamp,
			}
			pairs = append(pairs, p)
			byID[ev.CycleID] = p
		case cyclelog.EventCycleComplete:
			p, ok := byID[ev.CycleID]
			if !ok {
				continue
			}
			p.replyText = ev.Text
			p.replyAt = ev.Timestamp
			p.status = string(ev.Status)
			p.hasReply = true
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("transcript: scan %s: %w", path, err)
	}

	out := make([]Turn, 0, len(pairs)*2)
	for _, p := range pairs {
		out = append(out, Turn{
			Role:      RoleUser,
			Text:      p.userText,
			Timestamp: p.userAt,
			CycleID:   p.cycleID,
		})
		// Assistant turn — always emitted so the UI has
		// something to render even when the cycle aborted.
		// Empty reply text + non-complete status renders as
		// a placeholder.
		role := RoleAssistant
		text := p.replyText
		if !p.hasReply {
			role = RoleError
			text = "[cycle did not complete; check cycle log]"
			p.status = "abandoned"
		} else if p.status == string(cyclelog.StatusError) {
			role = RoleError
			if text == "" {
				text = "[cycle errored]"
			}
		}
		ts := p.replyAt
		if ts.IsZero() {
			ts = p.userAt
		}
		out = append(out, Turn{
			Role:      role,
			Text:      text,
			Timestamp: ts,
			CycleID:   p.cycleID,
			Status:    p.status,
		})
	}
	return out, nil
}

// ExportMarkdown renders the day's turns as a single markdown
// document operators can archive or share. Headings include
// timestamps so a search for a specific turn lands quickly.
// Empty days produce a brief placeholder rather than empty
// bytes so the export download is never zero-length.
func ExportMarkdown(date, agentName string, turns []Turn) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", agentName, date)
	if len(turns) == 0 {
		b.WriteString("_No cycles on this date._\n")
		return []byte(b.String())
	}
	for _, t := range turns {
		ts := t.Timestamp.Format("15:04:05")
		switch t.Role {
		case RoleUser:
			fmt.Fprintf(&b, "## %s — operator\n\n%s\n\n", ts, t.Text)
		case RoleAssistant:
			fmt.Fprintf(&b, "## %s — %s\n\n%s\n\n", ts, agentName, t.Text)
		case RoleError:
			fmt.Fprintf(&b, "## %s — error\n\n> %s\n\n", ts, t.Text)
		}
	}
	return []byte(b.String())
}

// SafeDate validates a YYYY-MM-DD string. Returns the parsed
// time + the canonical string form. Used at HTTP boundaries
// before constructing the file path to prevent traversal.
func SafeDate(date string) (time.Time, string, error) {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid date %q: %w", date, err)
	}
	return t, t.Format("2006-01-02"), nil
}

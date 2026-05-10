package metalearning

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/cyclelog"
)

// FabricationAuditFactKey is the librarian fact the audit writes
// each run. Curator's <integrity> sensorium block reads this key
// and renders the count + examined attributes every cycle.
//
// Springdrift writes `integrity_suspect_facts_7d` because its
// failure mode is fact-laundering (synthesis-derivation facts
// that aren't grounded in tool output). Retainer's failure
// mode is reply-text fabrication (URLs and action claims in
// chat replies that no tool produced), so the key is named for
// the thing being audited — replies, not facts. Same pattern,
// different scan target.
const FabricationAuditFactKey = "integrity_suspect_replies_7d"

// fabricationAuditWindow is how far back the worker scans cycle
// logs on each run. 7 days matches the SD audit's 7-day window
// (the suffix of `_7d` on the fact key is the operator-visible
// reflection of this constant).
const fabricationAuditWindow = 7 * 24 * time.Hour

// fabricationAuditFactValue is the JSON shape the audit writes
// as the fact body. Curator decodes this. Stable wire format —
// every render-side field is a top-level attribute the curator
// surfaces in the sensorium element.
type fabricationAuditFactValue struct {
	// Count is the number of cycles in the window whose reply
	// contained unsupported claims. The headline integrity
	// signal — agent sees this in `<integrity suspect_replies_7d="N">`.
	Count int `json:"count"`
	// Examined is the total number of cog cycles scanned in the
	// window. Anchors `Count` against base rate — 0/0 is a
	// quiet workspace, 0/200 is a clean run, 5/200 is rare, 5/10
	// is concerning.
	Examined int `json:"examined"`
	// FromDate / ToDate bound the audit window in ISO format.
	// Operator-visible audit detail when they pull the fact via
	// memory_read; the sensorium itself shows only count/examined.
	FromDate string `json:"from_date"`
	ToDate   string `json:"to_date"`
	// SuspectCycleIDs lists the cycle ids that flagged. Capped
	// to keep the JSON value compact (full cycle log has the
	// detail). Operator can grep cycle log for any of these.
	SuspectCycleIDs []string `json:"suspect_cycle_ids,omitempty"`
}

// fabricationAuditMaxSuspectIDs caps how many cycle ids land in
// the fact value's SuspectCycleIDs slice. Past the cap the JSON
// gets noisy without adding signal — the curator only renders
// the count anyway, and operators wanting the full list grep the
// cycle log for the date range.
const fabricationAuditMaxSuspectIDs = 25

// FabricationAudit is the meta-learning worker that scans recent
// cycle logs for cog cycles whose reply text contains factual
// claims (URLs, action assertions) the cycle's own tool log
// doesn't ground.
//
// SD-faithful sensorium-loop pattern. Per SD's
// meta_learning/fabrication_audit.gleam +
// tools/remembrancer.gleam (audit_fabrication wrapper) +
// narrative/curator.gleam (render_sensorium_integrity):
//
//  1. Producer (this worker): scan window, count flagged cycles
//  2. Storage: ONE summary fact via FactSink — the librarian
//     keeps current state, sensorium queries it
//  3. Consumer: curator's renderSensoriumIntegrity reads the
//     fact, renders <integrity suspect_replies_7d="N"
//     replies_examined_7d="M"/> every cycle so the agent
//     perceives its own integrity number
//
// Without the consumer the audit is dead weight — that was the
// PR-105 mistake (audits.jsonl with no reader). The fact is the
// feedback loop.
//
// Why this, not the inline output gate? The inline gate's chat
// footer ("⚠ Verification: ...") was operator-burdening — every
// flagged reply forced the operator to read a disclaimer block.
// Catching fabrications matters; surfacing them in chat doesn't.
// The SD pattern surfaces them in the agent's own sensorium so
// the agent sees its own count and self-corrects.
//
// What it catches:
//   - URLs in replies that no brave_web_search / library_get /
//     read_message etc. returned in this cycle's tool log
//   - Replies with "verified" / "confirmed" / "sent" language
//     in cycles where no tools fired
//
// What it doesn't catch (out of scope, intentional):
//   - Hedged language ("the symptoms suggest...")
//   - Cross-cycle continuity ("as I mentioned before")
//   - General domain knowledge the agent already had
func FabricationAudit(ctx context.Context, deps Deps) error {
	if deps.DataDir == "" {
		return errors.New("fabrication audit: DataDir empty")
	}
	now := deps.NowFn()
	from := now.Add(-fabricationAuditWindow)

	cycleDir := filepath.Join(deps.DataDir, "cycle-log")
	cycles, err := scanCycleLogsForAudit(cycleDir, from)
	if err != nil {
		return fmt.Errorf("fabrication audit: scan cycle logs: %w", err)
	}

	suspectIDs := make([]string, 0)
	for _, c := range cycles {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		flagged := auditCycleClaims(c)
		if len(flagged) == 0 {
			continue
		}
		suspectIDs = append(suspectIDs, c.id)
	}

	// Cap the id list — the curator only reads count/examined; the
	// list is operator-eyes-only and balloons the JSONL otherwise.
	clipped := suspectIDs
	if len(clipped) > fabricationAuditMaxSuspectIDs {
		clipped = clipped[:fabricationAuditMaxSuspectIDs]
	}

	value := fabricationAuditFactValue{
		Count:           len(suspectIDs),
		Examined:        len(cycles),
		FromDate:        from.UTC().Format(time.RFC3339),
		ToDate:          now.UTC().Format(time.RFC3339),
		SuspectCycleIDs: clipped,
	}
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("fabrication audit: marshal fact value: %w", err)
	}

	if deps.FactSink != nil {
		deps.FactSink.RecordFact(FactRecord{
			Key:   FabricationAuditFactKey,
			Value: string(body),
		})
	} else {
		// No sink wired — log so operators see the audit ran but
		// the count went nowhere. Production always wires the
		// librarian; this branch is mostly for tests that exercise
		// the scan logic without the storage chain.
		deps.Logger.Warn("fabrication audit: no FactSink wired; skipping fact persist",
			"count", value.Count, "examined", value.Examined)
	}

	deps.Logger.Info("fabrication audit ran",
		"window_hours", int(fabricationAuditWindow.Hours()),
		"cycles_scanned", value.Examined,
		"suspect_count", value.Count,
	)
	return nil
}

// cycleSummary is one cog cycle the audit cares about — text
// reply, tool log so far. Built by replaying the cycle log JSONL
// for the audit window.
type cycleSummary struct {
	id        string
	startedAt time.Time
	replyText string
	toolCount int
	// toolOutputs is the concatenation of every tool_result body
	// the cycle saw. The audit tests claim strings against this
	// haystack — if a URL or identifier appears here, it's
	// grounded; if not, it's flagged.
	toolOutputs string
}

// scanCycleLogsForAudit reads recent JSONL files under
// `<cycleDir>/`, replays them per cycle_id, and returns one
// cycleSummary per completed cog cycle whose start time is
// within the window. Agent sub-cycles are filtered (the audit
// scopes to operator-visible cog timeline).
func scanCycleLogsForAudit(cycleDir string, since time.Time) ([]cycleSummary, error) {
	entries, err := os.ReadDir(cycleDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	// Sort newest-first so the per-day file containing `since`
	// is scanned (older files entirely outside the window get
	// skipped via filename check).
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		base := strings.TrimSuffix(n, ".jsonl")
		if len(base) < 10 || base[4] != '-' || base[7] != '-' {
			continue
		}
		names = append(names, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	cycles := map[string]*cycleSummary{}
	for _, n := range names {
		// Skip files entirely older than the window.
		datePart := strings.TrimSuffix(n, ".jsonl")
		fileDay, err := time.Parse("2006-01-02", datePart)
		if err == nil && fileDay.Add(24*time.Hour).Before(since) {
			break
		}
		path := filepath.Join(cycleDir, n)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			line := s.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev cyclelog.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			c, ok := cycles[ev.CycleID]
			if !ok {
				c = &cycleSummary{id: ev.CycleID}
				cycles[ev.CycleID] = c
			}
			switch ev.Type {
			case cyclelog.EventCycleStart:
				if ev.NodeType == "agent" {
					// Agent sub-cycle — drop from audit scope.
					delete(cycles, ev.CycleID)
					continue
				}
				c.startedAt = ev.Timestamp
			case cyclelog.EventCycleComplete:
				c.replyText = ev.Text
			case cyclelog.EventToolCall:
				c.toolCount++
			case cyclelog.EventToolResult:
				// Tool output content isn't in the cycle log
				// (would balloon JSONL size). Use the tool name
				// as a weak grounding signal — if the cycle
				// fired N searches, claims are at least
				// plausible. Audit's tightest signal is the
				// "claims with zero tools" case.
				c.toolOutputs += " " + ev.ToolName
			}
		}
		f.Close()
	}

	out := make([]cycleSummary, 0, len(cycles))
	for _, c := range cycles {
		if c.replyText == "" {
			continue
		}
		if c.startedAt.Before(since) {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].startedAt.Before(out[j].startedAt)
	})
	return out, nil
}

// auditCycleClaims runs the mechanical fabrication checks on one
// cycle's reply text. Returns flagged claim excerpts; empty
// means clean.
//
// The checks are deliberately conservative — false positives
// pollute the audit log + erode operator trust. We err on the
// side of NOT flagging hedged or general statements.
func auditCycleClaims(c cycleSummary) []string {
	var flagged []string

	// URL check: every URL in the reply must have appeared in
	// at least one tool output (we approximate by checking the
	// presence of fetch-style tools). When the cycle has zero
	// tools, every URL is unsupported.
	urls := extractURLs(c.replyText)
	hadFetchTool := strings.Contains(c.toolOutputs, "brave_web_search") ||
		strings.Contains(c.toolOutputs, "library_get") ||
		strings.Contains(c.toolOutputs, "library_read_section") ||
		strings.Contains(c.toolOutputs, "read_message") ||
		strings.Contains(c.toolOutputs, "library_search") ||
		strings.Contains(c.toolOutputs, "deep_search")
	if len(urls) > 0 && !hadFetchTool {
		for _, u := range urls {
			flagged = append(flagged, fmt.Sprintf(
				"URL %q in reply but no fetch-style tool fired this cycle", u))
		}
	}

	// Action-claim check: replies that say "sent" / "verified"
	// / "confirmed" / "delivered" in cycles with ZERO tool
	// calls. Strongest fabrication signal — agent claims a side
	// effect with no underlying action.
	actionPhrases := []string{
		"email has been sent",
		"email was sent",
		"have been sent",
		"have been verified",
		"have been confirmed",
		"the file is now",
		"i notified you",
		"i sent",
		"links have been verified",
		"all links are",
		"i confirm",
		"i can confirm",
		"successfully delivered",
	}
	if c.toolCount == 0 {
		lowReply := strings.ToLower(c.replyText)
		for _, p := range actionPhrases {
			if strings.Contains(lowReply, p) {
				flagged = append(flagged, fmt.Sprintf(
					"action claim %q in cycle with zero tool calls", p))
			}
		}
	}

	return flagged
}

// extractURLs pulls plausible http(s) URLs from text. Naive
// pattern — scans for "http://" / "https://" prefixes and reads
// to the next whitespace or sentence-terminator. Catches the
// common Mistral failure mode (URLs in email bodies); doesn't
// try to be a full URL grammar.
func extractURLs(text string) []string {
	var out []string
	for _, prefix := range []string{"http://", "https://"} {
		i := 0
		for {
			j := strings.Index(text[i:], prefix)
			if j < 0 {
				break
			}
			start := i + j
			end := start + len(prefix)
			for end < len(text) {
				r := text[end]
				if r == ' ' || r == '\n' || r == '\t' || r == ')' ||
					r == ']' || r == '"' || r == '\'' || r == '<' ||
					r == '>' || r == ',' {
					break
				}
				end++
			}
			candidate := strings.TrimRight(text[start:end], ".,;:!?")
			if _, err := url.Parse(candidate); err == nil {
				out = append(out, candidate)
			}
			i = end
		}
	}
	return out
}


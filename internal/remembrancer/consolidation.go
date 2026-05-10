package remembrancer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ConsolidationSubdir is the workspace data subdir holding the
// ConsolidationRun JSONL log. Date-rotated, append-only.
const ConsolidationSubdir = "consolidation"

// KnowledgeSubdir is the workspace data subdir holding markdown
// consolidation reports. Filenames are content-addressed by date +
// slug so re-running the same consolidation doesn't clobber.
const KnowledgeSubdir = "knowledge"

// ReportsSubdir is the consolidation-specific subdirectory under
// `knowledge/`. Splitting it now leaves room for other knowledge
// kinds (study notes, draft reports) when those subsystems land.
const ReportsSubdir = "consolidation"

// validPeriods are the allowed values for Run.Period. Constraining
// this at write time keeps the audit log queryable later.
var validPeriods = map[string]struct{}{
	"weekly":  {},
	"monthly": {},
	"ad-hoc":  {},
}

// validSlugPattern enforces a tight slug shape: lowercase
// alphanumerics + hyphens. Defensive against operator-injected
// path traversal — even though the report dir is fixed, a slug like
// "../oops" would still escape if we relied on filepath.Join alone.
var validSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Run is one consolidation pass — a synthesis the agent (or operator)
// performed over a date range. Persisted as one JSONL line per run
// in `<dataDir>/consolidation/YYYY-MM-DD.jsonl`.
//
// The Run record references the markdown report by relative path,
// not by content. Reports are immutable post-write; if the synthesis
// needs revising, write a new Run + report rather than editing the
// old one (matches `project_archive_immutable`).
type Run struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Period     string    `json:"period"`           // "weekly" | "monthly" | "ad-hoc"
	StartDate  string    `json:"start_date"`       // YYYY-MM-DD
	EndDate    string    `json:"end_date"`         // YYYY-MM-DD
	Topic      string    `json:"topic,omitempty"`  // optional free-form
	ReportPath string    `json:"report_path"`      // workspace-relative
	Stats      RunStats  `json:"stats"`
}

// RunStats is the count summary captured at write time. Mirrors the
// Stats type from query.go but without sample excerpts (we don't
// want per-run JSONL bloating with full case payloads).
type RunStats struct {
	NarrativeEntries int `json:"narrative_entries"`
	Facts            int `json:"facts"`
	Cases            int `json:"cases"`
}

// WriteRun appends a Run to the daily JSONL under
// `<dataDir>/consolidation/YYYY-MM-DD.jsonl`. Creates parent dirs
// on demand. Stamps Timestamp + ID when caller leaves them zero/
// empty (per `feedback_actor_timestamps`).
//
// Append-only: never edits an existing file. Date-rotation key is
// the Run's Timestamp UTC date.
func WriteRun(dataDir string, run Run) (Run, error) {
	if run.Timestamp.IsZero() {
		run.Timestamp = time.Now()
	}
	if run.ID == "" {
		run.ID = uuid.NewString()
	}
	if err := validateRun(run); err != nil {
		return Run{}, err
	}

	dir := filepath.Join(dataDir, ConsolidationSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Run{}, fmt.Errorf("remembrancer: mkdir consolidation: %w", err)
	}
	dayKey := run.Timestamp.UTC().Format("2006-01-02")
	path := filepath.Join(dir, dayKey+".jsonl")

	body, err := json.Marshal(run)
	if err != nil {
		return Run{}, fmt.Errorf("remembrancer: marshal run: %w", err)
	}
	body = append(body, '\n')
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return Run{}, fmt.Errorf("remembrancer: open run log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return Run{}, fmt.Errorf("remembrancer: write run: %w", err)
	}
	return run, nil
}

// WriteReport persists a markdown report at
// `<dataDir>/knowledge/consolidation/<date>-<slug>.md` and returns
// the workspace-relative path (suitable for storing on a Run record).
//
// Errors:
//   - empty body / slug / date.
//   - slug fails validSlugPattern (catches path traversal + funky
//     filenames before they hit disk).
//   - target file already exists (don't clobber prior synthesis;
//     operator must change slug or wait for the next day).
func WriteReport(dataDir, date, slug, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", errors.New("remembrancer: empty report body")
	}
	if strings.TrimSpace(slug) == "" {
		return "", errors.New("remembrancer: empty slug")
	}
	if !validSlugPattern.MatchString(slug) {
		return "", fmt.Errorf("remembrancer: slug %q must match [a-z0-9-] (lowercase alphanumerics + single hyphens)", slug)
	}
	if !validDatePattern.MatchString(date) {
		return "", fmt.Errorf("remembrancer: date %q must be YYYY-MM-DD", date)
	}

	dir := filepath.Join(dataDir, KnowledgeSubdir, ReportsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("remembrancer: mkdir reports: %w", err)
	}
	filename := date + "-" + slug + ".md"
	abs := filepath.Join(dir, filename)
	if _, err := os.Stat(abs); err == nil {
		return "", fmt.Errorf("remembrancer: report %s already exists; choose a different slug or wait for the next day", filename)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("remembrancer: write report: %w", err)
	}
	rel := filepath.Join(KnowledgeSubdir, ReportsSubdir, filename)
	return rel, nil
}

// validDatePattern matches a YYYY-MM-DD string. Used by both
// validateRun and WriteReport — the date format is the persistence
// schema's load-bearing key for daily-rotated logs.
var validDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// validateRun checks the fields a Run needs at write time. Catches
// malformed records before they pollute the audit log.
func validateRun(run Run) error {
	if _, ok := validPeriods[run.Period]; !ok {
		return fmt.Errorf("remembrancer: invalid period %q (allowed: weekly / monthly / ad-hoc)", run.Period)
	}
	if !validDatePattern.MatchString(run.StartDate) {
		return fmt.Errorf("remembrancer: start_date %q must be YYYY-MM-DD", run.StartDate)
	}
	if !validDatePattern.MatchString(run.EndDate) {
		return fmt.Errorf("remembrancer: end_date %q must be YYYY-MM-DD", run.EndDate)
	}
	if run.ReportPath == "" {
		return errors.New("remembrancer: report_path required (write the report first, then record the run)")
	}
	return nil
}

// SlugifyTitle is a small helper for tools that want to derive a
// slug from a free-form title. Lowercases, replaces non-alphanum
// runs with single hyphens, trims leading/trailing hyphens. Returns
// empty when the input has no alphanumerics.
//
// Tools should ALSO accept an explicit slug override — auto-slugging
// from titles is convenient but operator-supplied slugs win when
// provided.
func SlugifyTitle(title string) string {
	var b strings.Builder
	prevHyphen := true
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

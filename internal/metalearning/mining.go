package metalearning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/remembrancer"
)

// patternsDir is where the daily mining worker appends its output.
// Lives next to data/reports/ — both are durable knowledge artifacts
// the agent reaches via deep_search later. Per
// project_archive_immutable, entries are append-only; the file is
// never edited or pruned.
const patternsDir = "patterns"

// patternsFilename is the immutable-archive log of mined patterns.
// One JSONL line per cluster discovered on a given mining run.
const patternsFilename = "patterns.jsonl"

// dailyMiningMinCases is the cluster size threshold. Smaller
// clusters are noise; SD uses 3 for the same job. Below this, the
// cluster is dropped (not persisted) — keeps the patterns log
// signal-rich.
const dailyMiningMinCases = 3

// dailyMiningClusterThreshold is the structural-field similarity
// floor for cbr.FindClusters. Matches the threshold the prior
// mine_patterns tool used (cbr.DefaultClusterThreshold) so behaviour
// is unchanged — only the trigger moves from cog-tool-call to
// scheduled-tick.
var dailyMiningClusterThreshold = cbr.DefaultClusterThreshold

// MinedPattern is the JSONL record shape for one cluster discovered
// on a mining run. Stable wire format — append-only, never mutated
// in place. Field names use snake_case for consistency with the
// cog's other JSONL stores (narrative, cases, facts).
type MinedPattern struct {
	// MinedAt is the wall-clock time the mining run produced this
	// cluster. Same value for every cluster from one run.
	MinedAt time.Time `json:"mined_at"`
	// Domain is the cluster's primary domain (the modal
	// problem.domain across its members).
	Domain string `json:"domain,omitempty"`
	// Keywords are the shared keywords across the cluster's cases
	// (intersection, deduped).
	Keywords []string `json:"keywords,omitempty"`
	// CaseIDs are the case IDs that landed in this cluster, in the
	// order cbr.FindClusters returned them.
	CaseIDs []string `json:"case_ids"`
	// Size is len(CaseIDs) — denormalised for cheap downstream
	// filtering / counting without re-reading every entry.
	Size int `json:"size"`
}

// DailyMining is the worker function for the `daily_mining` worker.
// Reads the case archive, clusters via cbr.FindClusters, and
// appends one MinedPattern record per qualifying cluster to
// data/patterns/patterns.jsonl. No LLM calls; no state mutation
// beyond the append.
//
// Invariants:
//   - Patterns file is never edited or pruned (immutable archive).
//   - One run produces zero or more entries; entries from one run
//     share MinedAt timestamps for forensic grouping.
//   - When no cases qualify, the function returns nil (success) —
//     "nothing mined" is a valid outcome, not an error.
func DailyMining(ctx context.Context, deps Deps) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cases, err := remembrancer.ReadCases(deps.DataDir, deps.Logger)
	if err != nil {
		return fmt.Errorf("read cases: %w", err)
	}
	if len(cases) == 0 {
		deps.Logger.Debug("daily_mining: no cases on disk; nothing to mine")
		return nil
	}

	clusters := cbr.FindClusters(cases, dailyMiningMinCases, dailyMiningClusterThreshold)
	if len(clusters) == 0 {
		deps.Logger.Debug("daily_mining: no clusters above threshold",
			"cases", len(cases), "min_size", dailyMiningMinCases)
		return nil
	}

	now := deps.NowFn()
	patterns := make([]MinedPattern, 0, len(clusters))
	for _, c := range clusters {
		patterns = append(patterns, mapCluster(c, now))
	}

	if err := appendPatterns(deps.DataDir, patterns); err != nil {
		return fmt.Errorf("append patterns: %w", err)
	}
	deps.Logger.Info("daily_mining: appended clusters",
		"clusters", len(patterns),
		"cases_scanned", len(cases),
	)
	return nil
}

// mapCluster turns a cbr.Cluster into a MinedPattern. Pure helper so
// the JSONL shape is testable without the file-system round-trip.
func mapCluster(c cbr.Cluster, now time.Time) MinedPattern {
	return MinedPattern{
		MinedAt:  now,
		Domain:   c.CommonDomain,
		Keywords: append([]string(nil), c.CommonKeywords...),
		CaseIDs:  append([]string(nil), c.CaseIDs...),
		Size:     c.Size,
	}
}

// appendPatterns writes one JSONL line per pattern to the
// data/patterns/patterns.jsonl file. Creates the directory + file
// on first run. Never truncates — the file is the immutable archive.
func appendPatterns(dataDir string, patterns []MinedPattern) error {
	if len(patterns) == 0 {
		return nil
	}
	dir := filepath.Join(dataDir, patternsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, patternsFilename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var buf strings.Builder
	for _, p := range patterns {
		line, mErr := json.Marshal(p)
		if mErr != nil {
			return fmt.Errorf("marshal pattern: %w", mErr)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, err := f.WriteString(buf.String()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

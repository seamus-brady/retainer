package librarian

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/seamus-brady/retainer/internal/cbr"
	"github.com/seamus-brady/retainer/internal/embed"
)

// casesStore wraps the CBR JSONL store + in-memory CaseBase. The
// librarian goroutine is the single writer for both, mirroring the
// narrative + facts pattern.
//
// Cases are smaller in volume than narrative entries (one per cycle
// vs many per cycle) so we keep them in a single JSONL file rather
// than rotating by date. Replay reads the whole file at startup,
// taking the latest record per case_id — curation tools (suppress,
// boost, correct) append new records that supersede older ones.
//
// pendingRetrievals tracks per-cycle which cases were surfaced to the
// agent. Drained at end-of-cycle by the cog (via the archivist's
// CycleComplete) so usage stats can update — bumping each retrieved
// case's RetrievalCount, plus RetrievalSuccessCount when the cycle
// ended in success. Map is bounded by lifetime: at most one entry
// per in-flight cycle, cleared on drain.
type casesStore struct {
	store             *cbr.Store
	base              *cbr.CaseBase
	logger            *slog.Logger
	pendingRetrievals map[string][]string
}

// newCasesStore opens (or creates) the cases.jsonl file and builds an
// in-memory CaseBase, replaying any existing cases. Pass nil embedder
// to disable the embedding signal entirely (CBR retrieval falls back
// to the five other signals with renormalised weights).
func newCasesStore(dataDir string, embedder embed.Embedder, logger *slog.Logger) (*casesStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := cbr.NewStore(dataDir, logger)
	if err != nil {
		return nil, err
	}
	base := cbr.NewCaseBase(embedder)
	cases, err := store.LoadAll()
	if err != nil {
		return nil, fmt.Errorf("librarian: load cases: %w", err)
	}
	// Replay: latest record per case_id wins. cases.jsonl is appended
	// to over time, so curation overwrites land at the END of the file
	// — iterate in order and Retain (which upserts) gets us
	// last-write-wins automatically.
	for _, c := range cases {
		base.Retain(c)
	}
	logger.Info("librarian: cbr replay complete",
		"cases_loaded", len(cases),
		"active", base.Count(),
		"redacted", base.CountIncludingRedacted()-base.Count(),
	)
	return &casesStore{
		store:             store,
		base:              base,
		logger:            logger,
		pendingRetrievals: make(map[string][]string),
	}, nil
}

// recordCase appends to JSONL and upserts in the CaseBase. The case's
// embedding (when present) was computed by the caller — the librarian
// goroutine doesn't run the embedder itself, since embedding latency
// would block every other inbox message.
func (s *casesStore) recordCase(c cbr.Case) error {
	if err := s.store.Append(c); err != nil {
		return err
	}
	s.base.Retain(c)
	return nil
}

// retrieve scores cases via the CaseBase. The embedder runs inside
// CaseBase.Retrieve when the query needs it — query embedding is one
// short call vs the full retrieval scan, so the trade-off is
// acceptable. Embedder errors don't fail retrieval; CaseBase.Retrieve
// drops the embedding signal and renormalises.
func (s *casesStore) retrieve(ctx context.Context, q cbr.Query) []cbr.Scored {
	return s.base.Retrieve(ctx, q)
}

// registerRetrieval appends caseIDs to the per-cycle list. Empty
// cycleID or empty caseIDs are no-ops — registering against no cycle
// would leak; registering nothing has no effect. Called inside the
// librarian's retrieve handler when the request arrived with a
// cycleID in context.
func (s *casesStore) registerRetrieval(cycleID string, caseIDs []string) {
	if cycleID == "" || len(caseIDs) == 0 {
		return
	}
	s.pendingRetrievals[cycleID] = append(s.pendingRetrievals[cycleID], caseIDs...)
}

// drainRetrievedCaseIDs returns the deduplicated list of case IDs
// retrieved during cycleID and clears the entry. Called by the cog
// at end-of-cycle (success OR abandon paths so the map self-cleans).
//
// Dedup at drain time so repeated `recall_cases` calls within the
// same cycle that surface the same case only count once toward the
// usage-stats bump.
func (s *casesStore) drainRetrievedCaseIDs(cycleID string) []string {
	if cycleID == "" {
		return nil
	}
	raw, ok := s.pendingRetrievals[cycleID]
	if !ok {
		return nil
	}
	delete(s.pendingRetrievals, cycleID)
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// caseCount is the active (non-redacted) case count. Used by the
// curator's sensorium <memory cases="N"/> attr.
func (s *casesStore) caseCount() int {
	return s.base.Count()
}

// getCase returns a case by ID. Includes redacted cases — observer
// curation tools need to read them by ID even when retrieval excludes
// them.
func (s *casesStore) getCase(id string) (cbr.Case, bool) {
	return s.base.Get(id)
}

// allCases returns every case the base holds (including redacted +
// superseded). Snapshot only — callers can iterate freely without
// holding the librarian goroutine. Used by the housekeeper's CBR
// sweeps (dedup + prune) that need to scan the corpus.
func (s *casesStore) allCases() []cbr.Case {
	return s.base.All()
}

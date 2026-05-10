package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/seamus-brady/retainer/internal/policy"
)

// defaultTickInterval is how often the scheduler wakes up to
// check whether any job's NextFireAt has passed. 30 seconds
// gives sub-minute-grained scheduling without burning cycles
// (cron expressions never need finer granularity).
const defaultTickInterval = 30 * time.Second

// Submitter is the slice of *cog.Cog the scheduler uses.
// Decoupling lets tests substitute a fake without spinning up
// the cog. Always SourceAutonomous: scheduled inputs are
// non-interactive and the policy gate's strict path applies.
//
// Fire-and-forget: the scheduler doesn't observe the reply.
// Bootstrap wraps `*cog.Cog` with a tiny adapter that drains
// the cog's reply channel in a goroutine so cycles complete
// cleanly — see bootstrap.go.
type Submitter interface {
	SubmitWithSource(ctx context.Context, text string, source policy.Source)
}

// Config wires the scheduler's collaborators.
type Config struct {
	// DataDir is the workspace data directory. The scheduler's
	// JSONL log lands at <DataDir>/scheduler/jobs.jsonl.
	DataDir string
	// Submitter is the cog (or test fake). Required.
	Submitter Submitter
	// TickInterval is how often the actor polls for due jobs.
	// Zero defaults to 30 seconds.
	TickInterval time.Duration
	// NowFn returns the current time. Defaults to time.Now;
	// tests inject deterministic clocks.
	NowFn func() time.Time
	// Logger receives diagnostic logs. Defaults to slog.Default.
	Logger *slog.Logger
}

// Service is the scheduler actor. Construct with New, supervise
// via Run under actor.Permanent so a panic during a single fire
// restarts the loop without losing state (state lives on disk).
type Service struct {
	cfg    Config
	parser cron.Parser

	mu   sync.Mutex
	jobs map[string]*Job
}

// New constructs a Service from a Config, applying defaults +
// replaying the on-disk log. The Submitter can be left nil
// during construction and bound later via SetSubmitter — the
// cog and the scheduler agent have a circular dependency
// (scheduler needs the cog as submitter; cog's tool registry
// needs the scheduler agent), so bootstrap constructs the
// service first, builds the agent, builds cog tools, builds
// the cog, then binds the cog as the submitter.
func New(cfg Config) (*Service, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("scheduler: DataDir is required")
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	if cfg.NowFn == nil {
		cfg.NowFn = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	s := &Service{
		cfg:    cfg,
		parser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}

	ops, err := loadOps(cfg.dataSubdir())
	if err != nil {
		return nil, err
	}
	s.jobs = resolveJobs(ops)
	return s, nil
}

func (c Config) dataSubdir() string { return c.DataDir + "/scheduler" }

// Run is the actor loop. Block until ctx is cancelled. Wrap
// with actor.Run under actor.Permanent so a panic inside one
// fire still restarts the loop.
//
// On startup, performs one immediate due-check so jobs that
// were due during shutdown fire promptly on next boot.
func (s *Service) Run(ctx context.Context) error {
	s.cfg.Logger.Info("scheduler started",
		"tick_interval", s.cfg.TickInterval,
		"jobs_loaded", s.activeJobCount(),
	)
	defer s.cfg.Logger.Info("scheduler stopped")

	s.tick(ctx)

	t := time.NewTicker(s.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick runs one pass: every active job whose due time has
// passed since now fires. Per-job errors are logged but never
// bubble — a single bad cron expression shouldn't take out the
// loop.
func (s *Service) tick(ctx context.Context) {
	now := s.cfg.NowFn()
	s.mu.Lock()
	due := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if !j.Active {
			continue
		}
		if s.dueAt(j, now) {
			// Copy so the fire goroutine isn't racing the map.
			cp := *j
			due = append(due, &cp)
		}
	}
	s.mu.Unlock()

	for _, j := range due {
		if ctx.Err() != nil {
			return
		}
		s.fire(ctx, j, now)
	}
}

// dueAt reports whether job j should fire at time now. For
// one-shot jobs that's simply "FireAt has passed and we haven't
// fired yet"; for recurring it's "the next cron time after
// LastFiredAt (or CreatedAt) has passed."
func (s *Service) dueAt(j *Job, now time.Time) bool {
	switch j.Kind {
	case JobOneShot:
		if j.FiredCount > 0 {
			return false
		}
		return !j.FireAt.IsZero() && !now.Before(j.FireAt)
	case JobRecurring:
		schedule, err := s.parser.Parse(j.Cron)
		if err != nil {
			s.cfg.Logger.Warn("scheduler: bad cron expression; skipping job",
				"job_id", j.ID, "cron", j.Cron, "err", err)
			return false
		}
		anchor := j.LastFiredAt
		if anchor.IsZero() {
			anchor = j.CreatedAt
		}
		next := schedule.Next(anchor)
		return !next.After(now)
	}
	return false
}

// fire submits the job's prompt to the cog with
// SourceAutonomous and records the appropriate Op. One-shot
// jobs fire-then-complete (single Op for both transitions);
// recurring jobs append OpFired and stay Active.
//
// Errors writing the op are logged but don't roll back the
// dispatch — the operator already saw the cycle land in their
// log; not recording the fire is a cosmetic loss, not data.
func (s *Service) fire(ctx context.Context, j *Job, now time.Time) {
	s.cfg.Logger.Info("scheduler: firing job",
		"job_id", j.ID, "name", j.Name, "kind", j.Kind, "fire_count", j.FiredCount+1)

	// Best-effort dispatch. The cog's inbox might be full
	// (operator typing fast); we'd rather drop a tick than
	// block the scheduler loop. The cog gate handles policy.
	go s.submit(ctx, framedPrompt(j, now))

	op := Op{Timestamp: now, JobID: j.ID}
	if j.Kind == JobOneShot {
		op.Kind = OpCompleted
	} else {
		op.Kind = OpFired
	}

	s.mu.Lock()
	if live, ok := s.jobs[j.ID]; ok {
		live.LastFiredAt = now
		live.FiredCount++
		if j.Kind == JobOneShot {
			live.Active = false
		}
	}
	s.mu.Unlock()

	if err := appendOp(s.cfg.dataSubdir(), op); err != nil {
		s.cfg.Logger.Warn("scheduler: append op failed",
			"job_id", j.ID, "kind", op.Kind, "err", err)
	}
}

// SetSubmitter installs the cog (or test fake) as the
// dispatch target. Bootstrap calls this after the cog is
// constructed, breaking the scheduler ↔ cog circular
// dependency. Subsequent calls replace any previous submitter
// — supports test setups that swap in fakes mid-life.
func (s *Service) SetSubmitter(sub Submitter) {
	s.mu.Lock()
	s.cfg.Submitter = sub
	s.mu.Unlock()
}

// submit calls the cog's SubmitWithSource. Fire-and-forget — the
// adapter the bootstrap wires drains the cog's reply channel in
// its own goroutine so the cycle completes without us holding
// references. No-op when the submitter hasn't been bound yet
// (the early window between scheduler construction and the
// SetSubmitter call after the cog comes up).
func (s *Service) submit(ctx context.Context, prompt string) {
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Warn("scheduler: submit panicked", "panic", r)
		}
	}()
	s.mu.Lock()
	sub := s.cfg.Submitter
	s.mu.Unlock()
	if sub == nil {
		s.cfg.Logger.Warn("scheduler: submitter not bound; dropping fire", "prompt", truncatePrompt(prompt, 80))
		return
	}
	sub.SubmitWithSource(ctx, prompt, policy.SourceAutonomous)
}

// truncatePrompt is a tiny helper for log messages — keeps a
// long prompt from blowing up a single warn line.
func truncatePrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Public API — used by the agent's tools
// ---------------------------------------------------------------------------

// ScheduleRecurring registers a new recurring job. Returns the
// stored job (with assigned ID + CreatedAt). Validates the cron
// expression up front; bad expressions return an error so the
// agent surfaces it to the operator.
func (s *Service) ScheduleRecurring(name, cronExpr, prompt, description string) (Job, error) {
	if name == "" {
		return Job{}, fmt.Errorf("scheduler: name is required")
	}
	if cronExpr == "" {
		return Job{}, fmt.Errorf("scheduler: cron expression is required")
	}
	if prompt == "" {
		return Job{}, fmt.Errorf("scheduler: prompt is required")
	}
	if _, err := s.parser.Parse(cronExpr); err != nil {
		return Job{}, fmt.Errorf("scheduler: invalid cron %q: %w", cronExpr, err)
	}
	now := s.cfg.NowFn()
	j := Job{
		ID:          uuid.NewString(),
		Name:        name,
		Kind:        JobRecurring,
		Cron:        cronExpr,
		Prompt:      prompt,
		Description: description,
		CreatedAt:   now,
		Active:      true,
	}
	if err := s.persist(OpCreated, j.ID, &j, "", now); err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	cp := j
	s.jobs[j.ID] = &cp
	s.mu.Unlock()
	return j, nil
}

// ScheduleOneShot registers a job that fires once at fireAt.
// fireAt in the past is rejected — operator probably intended
// "now plus N", and we won't guess.
func (s *Service) ScheduleOneShot(name string, fireAt time.Time, prompt, description string) (Job, error) {
	if name == "" {
		return Job{}, fmt.Errorf("scheduler: name is required")
	}
	if fireAt.IsZero() {
		return Job{}, fmt.Errorf("scheduler: fire_at is required")
	}
	now := s.cfg.NowFn()
	if !fireAt.After(now) {
		return Job{}, fmt.Errorf("scheduler: fire_at %s is not in the future (now=%s)",
			fireAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if prompt == "" {
		return Job{}, fmt.Errorf("scheduler: prompt is required")
	}
	j := Job{
		ID:          uuid.NewString(),
		Name:        name,
		Kind:        JobOneShot,
		FireAt:      fireAt,
		Prompt:      prompt,
		Description: description,
		CreatedAt:   now,
		Active:      true,
	}
	if err := s.persist(OpCreated, j.ID, &j, "", now); err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	cp := j
	s.jobs[j.ID] = &cp
	s.mu.Unlock()
	return j, nil
}

// Cancel deactivates an active job. Returns an error when the
// job isn't found or is already cancelled. The cancellation
// reason is optional but logged for audit.
func (s *Service) Cancel(jobID, reason string) error {
	s.mu.Lock()
	j, ok := s.jobs[jobID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("scheduler: job %q not found", jobID)
	}
	if !j.Active {
		s.mu.Unlock()
		return fmt.Errorf("scheduler: job %q is not active", jobID)
	}
	j.Active = false
	s.mu.Unlock()

	now := s.cfg.NowFn()
	return s.persist(OpCancelled, jobID, nil, reason, now)
}

// List returns every active job sorted by NextFire (recurring)
// or FireAt (one-shot) ascending — soonest-due first. Inactive
// jobs are excluded; List+Inspect together give the full audit
// picture.
func (s *Service) List() []Job {
	s.mu.Lock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if !j.Active {
			continue
		}
		out = append(out, *j)
	}
	s.mu.Unlock()

	now := s.cfg.NowFn()
	sort.Slice(out, func(i, k int) bool {
		return s.nextFire(out[i], now).Before(s.nextFire(out[k], now))
	})
	return out
}

// Inspect returns one job by ID, including cancelled / completed
// records. Returns false when the ID is unknown.
func (s *Service) Inspect(jobID string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// NextFire computes the next time job j is expected to fire,
// relative to the supplied `now`. Used by the agent's `list_jobs`
// + `inspect_job` output formatters. Returns zero time when no
// future fire is scheduled (one-shot already fired, or the cron
// expression is malformed).
func (s *Service) NextFire(j Job, now time.Time) time.Time {
	return s.nextFire(j, now)
}

// nextFire is the internal counterpart used by List for sorting.
func (s *Service) nextFire(j Job, now time.Time) time.Time {
	switch j.Kind {
	case JobOneShot:
		if j.FiredCount > 0 || !j.Active {
			return time.Time{}
		}
		return j.FireAt
	case JobRecurring:
		schedule, err := s.parser.Parse(j.Cron)
		if err != nil {
			return time.Time{}
		}
		anchor := j.LastFiredAt
		if anchor.IsZero() {
			anchor = j.CreatedAt
		}
		// If the anchor is in the future relative to `now`,
		// schedule from now so List ordering matches reality.
		if anchor.After(now) {
			anchor = now
		}
		return schedule.Next(anchor)
	}
	return time.Time{}
}

func (s *Service) activeJobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range s.jobs {
		if j.Active {
			n++
		}
	}
	return n
}

// persist appends one op to the JSONL log. Marshalling errors
// surface to the caller; disk errors do too. The caller has
// already mutated in-memory state, so a persist failure leaves
// the on-disk record behind. On next replay the in-memory state
// regenerates from disk — the lost mutation reappears as the
// pre-mutation truth, which is acceptable for the rare disk-
// full case.
func (s *Service) persist(kind OpKind, jobID string, j *Job, reason string, now time.Time) error {
	op := Op{
		Kind:         kind,
		Timestamp:    now,
		JobID:        jobID,
		Job:          j,
		CancelReason: reason,
	}
	return appendOp(s.cfg.dataSubdir(), op)
}

// framedPrompt wraps a scheduled-job prompt with explicit framing
// so the cog's LLM treats each fire as an independent unit of work.
//
// Why: without framing, recurring fires share c.state.history with
// any prior interactive cycles. The model reads "I already sent the
// hello world email" in conversation history and fabricates a
// confirmation reply without firing the tool. Documented as a real
// 2026-05-09 incident (operator scheduled 5 hello-world fires; only
// 1 actually sent). The framing tells the model: this is a fresh
// fire, prior cycles in your context are independent jobs, execute
// the request below regardless.
//
// Format keeps the framing brief — long prefixes burn input tokens
// AND risk the model treating them as the request body. The
// brackets convention matches the operator-facing convention SD
// uses for system-injected context.
func framedPrompt(j *Job, now time.Time) string {
	fire := j.FiredCount + 1 // operator-readable; pre-increments before persist
	header := fmt.Sprintf(
		"[Scheduled job %q — fire #%d at %s. This is an autonomous fire; each fire is an independent unit of work. Execute the request below regardless of any apparent prior completion in your conversation history.]\n\n",
		j.Name, fire, now.UTC().Format(time.RFC3339),
	)
	return header + j.Prompt
}

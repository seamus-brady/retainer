package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"

	"github.com/seamus-brady/retainer/internal/actor"
	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/agentid"
	"github.com/seamus-brady/retainer/internal/agenttokens"
	observerAgentPkg "github.com/seamus-brady/retainer/internal/agents/observer"
	schedulerAgentPkg "github.com/seamus-brady/retainer/internal/agents/scheduler"
	"github.com/seamus-brady/retainer/internal/archivist"
	"github.com/seamus-brady/retainer/internal/captures"
	"github.com/seamus-brady/retainer/internal/cog"
	"github.com/seamus-brady/retainer/internal/cogsock"
	"github.com/seamus-brady/retainer/internal/config"
	"github.com/seamus-brady/retainer/internal/curator"
	"github.com/seamus-brady/retainer/internal/cyclelog"
	"github.com/seamus-brady/retainer/internal/dag"
	"github.com/seamus-brady/retainer/internal/dailyfile"
	"github.com/seamus-brady/retainer/internal/embed"
	"github.com/seamus-brady/retainer/internal/housekeeper"
	"github.com/seamus-brady/retainer/internal/identity"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/lockfile"
	"github.com/seamus-brady/retainer/internal/logging"
	"github.com/seamus-brady/retainer/internal/metalearning"
	"github.com/seamus-brady/retainer/internal/observer"
	"github.com/seamus-brady/retainer/internal/paths"
	"github.com/seamus-brady/retainer/internal/policy"
	"github.com/seamus-brady/retainer/internal/scheduler"
	"github.com/seamus-brady/retainer/internal/skills"
	"github.com/seamus-brady/retainer/internal/version"
)

// world bundles the runtime: actors, supervisor, and the cleanup
// closure. Both the TUI and the one-shot CLI build a world via
// bootstrap(); the TUI runs Bubble Tea on top, the CLI submits a
// single message and exits.
type world struct {
	dirs paths.Paths

	logger *slog.Logger

	cog        *cog.Cog
	librarian  *librarian.Librarian
	observer   *observer.Observer
	archivist  *archivist.Archivist
	curator    *curator.Curator
	dag        *dag.DAG
	supervisor *actor.Supervisor

	tuiAgents []*agent.Agent
	agentName string
	version   string
	cleanup   func()
}

// bootstrap constructs the full Retainer runtime from a workspace
// path + optional config override.
//
// The mockScriptPath, when non-empty, swaps the picked provider
// for a scripted mock loaded from that file — used by integration
// tests to drive deterministic tool-use sequences without hitting
// a real provider. Empty means "use the configured provider"
// (anthropic when ANTHROPIC_API_KEY is set, mock otherwise).
func bootstrap(workspace, configPath, mockScriptPath string) (*world, error) {
	_ = godotenv.Load()

	dirs, err := paths.Resolve(workspace)
	if err != nil {
		return nil, fmt.Errorf("paths: %w", err)
	}
	_ = godotenv.Load(filepath.Join(dirs.Workspace, ".env"))

	cfg, srcPath, err := loadConfig(configPath, dirs)
	if err != nil {
		return nil, err
	}

	logger, closeLog, err := logging.Setup(logging.Options{
		DataDir:       dirs.Data,
		Verbose:       config.BoolOr(cfg.Logging.Verbose, false),
		RetentionDays: config.IntOr(cfg.Logging.RetentionDays, defaultRetentionDays),
	})
	if err != nil {
		return nil, fmt.Errorf("logging: %w", err)
	}
	slog.SetDefault(logger)

	logger.Info("workspace resolved", "workspace", dirs.Workspace, "config", dirs.Config, "data", dirs.Data)
	if srcPath != "" {
		logger.Info("config loaded", "path", srcPath)
	}

	// Single-instance flock at <data>/cog.lock — second invocation
	// against the same workspace fails fast instead of racing.
	lockPath := filepath.Join(dirs.Data, "cog.lock")
	cogLock, err := lockfile.Acquire(lockPath, os.Getpid())
	if err != nil {
		closeLog()
		holder := lockfile.HolderPID(lockPath)
		if holder > 0 {
			return nil, fmt.Errorf("another retainer is already running for this workspace (pid %d holds %s)", holder, lockPath)
		}
		return nil, fmt.Errorf("lockfile: %w", err)
	}

	// Stable agent UUID — 8-char prefix becomes instance_id on
	// every cycle-log event for cross-restart correlation.
	selfID, err := agentid.LoadOrCreate(dirs.Data, logger.With("component", "agentid"))
	if err != nil {
		logger.Warn("agentid: load failed; running without stable identity", "err", err)
	}
	if !selfID.IsZero() {
		logger.Info("agentid: identity ready",
			"uuid", selfID.UUID,
			"instance_id", selfID.InstanceID(),
			"created_at", selfID.CreatedAt,
		)
	}

	cycleLogDir := filepath.Join(dirs.Data, "cycle-log")
	if err := os.MkdirAll(cycleLogDir, 0o755); err != nil {
		closeLog()
		return nil, fmt.Errorf("mkdir cycle-log: %w", err)
	}
	cycleFile := dailyfile.NewWriter(cycleLogDir, ".jsonl", time.Now)
	cycleSink := cyclelog.Sink(cyclelog.NewEmitter(cyclelog.NewWriter(cycleFile), selfID.InstanceID()))
	d := dag.New()

	// Embedder is optional — when Hugot can't load (offline,
	// missing model cache, unsupported platform) CBR retrieval
	// falls back to keyword-only and the rest of the system runs
	// normally.
	embedder, err := embed.NewHugot(context.Background(), embed.HugotConfig{
		ModelCacheDir: filepath.Join(dirs.Data, "models"),
		Logger:        logger.With("component", "embed"),
	})
	if err != nil {
		logger.Warn("embed: hugot unavailable; CBR runs without embeddings", "err", err)
		embedder = nil
	}

	libOpts := librarian.Options{
		DataDir:             dirs.Data,
		Logger:              logger.With("component", "librarian"),
		NarrativeWindowDays: config.IntOr(cfg.Memory.NarrativeWindowDays, defaultNarrativeWindowDays),
	}
	if embedder != nil {
		libOpts.Embedder = embedder
	}
	lib, err := librarian.New(libOpts)
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("librarian: %w", err)
	}

	// Captures store — commitment tracker. Curator reads pending
	// count for the sensorium; metalearning captures worker
	// appends + sweeps.
	capturesStore, err := captures.Open(dirs.Data, time.Now)
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("captures: %w", err)
	}

	rawProvider, model := pickProviderForBootstrap(cfg, mockScriptPath, logger)
	maxTokens := config.IntOr(cfg.MaxTokens, defaultMaxTokens)

	// Retry callback wires LLM backoff into the cog Activity hub
	// so the UI can render "rate limited, retry 2/5, 15s" during
	// a wait. cogRef is bound below once the cog exists.
	var cogRef *cog.Cog
	provider := llm.WithRetry(rawProvider, llm.RetryConfig{
		Logger: logger.With("component", "llm-retry"),
		OnBackoff: func(attempt, maxAttempts int, delay time.Duration, reason string) {
			if cogRef != nil {
				cogRef.NotifyRetry(attempt, maxAttempts, delay, reason)
			}
		},
	})

	policyRules, policySrc, err := policy.Load(filepath.Join(dirs.Config, "policy.json"))
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("policy: %w", err)
	}
	logger.Info("policy loaded", "source", policySrc)
	canaryLimit := config.IntOr(cfg.Policy.CanaryFailureLimit, defaultCanaryFailureLimit)
	llmScorerTimeout := time.Duration(config.IntOr(cfg.Policy.LLMScorerTimeoutMs, defaultLLMScorerTimeoutMs)) * time.Millisecond

	var fabricationCfg policy.FabricationScorerConfig
	if config.BoolOr(cfg.Policy.Fabrication.Enabled, true) {
		fabModel := config.StringOr(cfg.Policy.Fabrication.Model, model)
		fabTimeout := time.Duration(config.IntOr(cfg.Policy.Fabrication.TimeoutMs, 0)) * time.Millisecond
		fabricationCfg = policy.FabricationScorerConfig{
			Provider:      provider,
			Model:         fabModel,
			Timeout:       fabTimeout,
			MinConfidence: config.Float64Or(cfg.Policy.Fabrication.MinConfidence, 0),
		}
		logger.Info("policy: fabrication scorer enabled",
			"model", fabModel,
			"min_confidence", config.Float64Or(cfg.Policy.Fabrication.MinConfidence, 0.7),
		)
	}

	policyEngine := policy.New(policy.Config{
		Rules: policyRules,
		Canary: policy.CanaryConfig{
			Provider:     provider,
			Model:        model,
			FailureLimit: canaryLimit,
			OnDegraded: func() {
				logger.Warn("canary probes degraded", "consecutive_failures", canaryLimit)
			},
		},
		LLMScorer: policy.LLMScorerConfig{
			Provider: provider,
			Model:    model,
			Timeout:  llmScorerTimeout,
		},
		Fabrication: fabricationCfg,
	})

	gateTimeout := time.Duration(config.IntOr(cfg.Gate.TimeoutMs, defaultGateTimeoutMs)) * time.Millisecond
	inputQueueCap := config.IntOr(cfg.Gate.InputQueueCap, defaultInputQueueCap)
	inputRefusal := config.StringOr(cfg.Policy.InputRefusal, defaultInputRefusalText)
	outputRefusal := config.StringOr(cfg.Policy.OutputRefusal, defaultOutputRefusalText)
	agentName := config.StringOr(cfg.Agent.Name, defaultAgentName)

	ident, err := identity.Load(dirs.Config)
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("identity: %w", err)
	}
	logger.Info("identity loaded", "source", ident.Source)

	skillsDirs := skillsSearchDirs(dirs)
	discoveredSkills := skills.Discover(skillsDirs)
	logger.Info("skills discovered", "count", len(discoveredSkills), "dirs", skillsDirs)

	obs := observer.New(lib, d)

	tokenTracker := agenttokens.NewTracker(dirs.Data)

	var fabricationGate agent.AgentFabricationGate
	if policyEngine != nil && policyEngine.HasFabricationScorer() {
		fabricationGate = &fabricationGateAdapter{engine: policyEngine}
	}

	telemetry := agent.Telemetry{
		CycleLog:        cycleSink,
		DAG:             d,
		InstanceID:      selfID.InstanceID(),
		TokenSink:       tokenTracker,
		FabricationGate: fabricationGate,
	}

	// Closures bound after the cog exists. Every delegate tool's
	// OnDone forwards via recordCompletion → c.RecordAgentCompletion;
	// request_more_turns is a tool that calls into c.RequestMoreTurns.
	var recordCompletion = func(string, agent.CompletionRecord) {}
	var requestMoreTurns = func(string, int, string) (int, error) {
		return 0, fmt.Errorf("cog: request_more_turns called before cog bound")
	}

	observerAgent, err := observerAgentPkg.New(provider, model, buildObserverTools(logger, obs, dirs.Data, lib, skillsDirs), telemetry, logger.With("component", "observer"))
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("observer agent: %w", err)
	}

	// Scheduler — autonomous-cycle scheduling. Cog binds as
	// Submitter once it exists.
	schedSvc, err := scheduler.New(scheduler.Config{
		DataDir: dirs.Data,
		Logger:  logger.With("component", "scheduler"),
	})
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("scheduler service: %w", err)
	}

	schedulerAgent, err := schedulerAgentPkg.New(provider, model, schedulerAgentPkg.BuildTools(schedSvc), telemetry, logger.With("component", "scheduler-agent"))
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("scheduler agent: %w", err)
	}

	agentsActive := 2 // observer + scheduler

	agentDir := &workspaceAgentDirectory{
		tokens: tokenTracker,
	}
	agentDir.entries = append(agentDir.entries, agentDirEntry{
		name:        observerAgent.Name(),
		description: observerAgent.Description(),
	})
	agentDir.entries = append(agentDir.entries, agentDirEntry{
		name:        schedulerAgent.Name(),
		description: schedulerAgent.Description(),
	})

	cur, err := curator.New(curator.Config{
		Identity:          ident,
		Librarian:         lib,
		Workspace:         dirs.Workspace,
		AgentName:         agentName,
		AgentVersion:      version.Version,
		Logger:            logger.With("component", "curator"),
		CycleLog:          cycleSink,
		Skills:            discoveredSkills,
		BootstrapSkillIDs: skills.DefaultBootstrapSkillIDs,
		SessionSince:      time.Now(),
		AgentsActive:      agentsActive,
		Agents:            agentDir,
		Captures:          capturesStore,
	})
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("curator: %w", err)
	}
	systemPromptFn := func(ctx context.Context, snap cog.CycleSnapshot) llm.SystemPrompt {
		return cur.BuildSystemPrompt(ctx, curator.CycleContext{
			CycleID:        snap.CycleID,
			InputSource:    inputSourceLabel(snap.InputSource),
			QueueDepth:     snap.QueueDepth,
			MessageCount:   snap.MessageCount,
			AmbientSignals: snap.Ambient,
			UserInput:      snap.UserText,
		})
	}

	cogTools := buildCogTools(logger, lib, observerAgent, schedulerAgent, skillsDirs,
		func(parent string, rec agent.CompletionRecord) {
			recordCompletion(parent, rec)
		},
		func(parent string, additional int, reason string) (int, error) {
			return requestMoreTurns(parent, additional, reason)
		},
	)

	arcCfg := archivist.Config{
		Librarian: lib,
		Logger:    logger.With("component", "archivist"),
	}
	if embedder != nil {
		arcCfg.Embedder = embedder
	}
	arcCfg.Curator = &archivist.LLMCurator{
		Provider: provider,
		Model:    model,
		Logger:   logger.With("component", "curator"),
		CycleLog: cycleSink,
	}
	arc, err := archivist.New(arcCfg)
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("archivist: %w", err)
	}

	hk, err := housekeeper.New(housekeeper.Config{
		Librarian:           lib,
		NarrativeWindowDays: config.IntOr(cfg.Memory.NarrativeWindowDays, defaultNarrativeWindowDays),
		Logger:              logger.With("component", "housekeeper"),
		StateFile:           filepath.Join(dirs.Data, ".housekeeper-state.json"),
		CBRSweepEnabled:     true,
	})
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("housekeeper: %w", err)
	}

	mlPool, err := metalearning.New(metalearning.Config{
		Workers:       metalearning.DefaultWorkers(),
		DataDir:       dirs.Data,
		Logger:        logger.With("component", "metalearning"),
		FactSink:      librarianFactSink{lib: lib},
		CapturesStore: capturesStore,
	})
	if err != nil {
		abortBootstrap(cogLock, cycleFile, closeLog, embedder)
		return nil, fmt.Errorf("metalearning: %w", err)
	}

	c := cog.New(cog.Config{
		Provider:           provider,
		Model:              model,
		MaxTokens:          maxTokens,
		MaxToolTurns:       config.IntOr(cfg.Cog.MaxToolTurns, 0),
		MaxContextMessages: config.IntOr(cfg.Cog.MaxContextMessages, defaultMaxContextMessages),
		SystemPromptFn:     systemPromptFn,
		Logger:             logger.With("component", "cog"),
		InstanceID:         selfID.InstanceID(),
		CycleLog:           cycleSink,
		DAG:                d,
		Librarian:          lib,
		Archivist:          arc,
		Policy:             policyEngine,
		GateTimeout:        gateTimeout,
		InputQueueCap:      inputQueueCap,
		InputRefusalText:   inputRefusal,
		OutputRefusalText:  outputRefusal,
		Tools:              cogTools,
	})

	// Bind the cog into closures that referenced it before
	// construction.
	cogRef = c
	recordCompletion = c.RecordAgentCompletion
	schedSvc.SetSubmitter(cogSubmitter{cog: c})
	requestMoreTurns = c.RequestMoreTurns

	// Local-IPC server so retainer-webui can connect over a
	// Unix socket. TUI bypasses this (in-process channels).
	//
	// Default path is `<workspace>/data/cog.sock`. RETAINER_COG_SOCKET
	// overrides — Docker on macOS uses virtiofs for bind mounts and
	// virtiofs doesn't support `socket(AF_UNIX, ...)` bind, so the
	// container entrypoint sets this to a tmpfs path. Linux and
	// native runs are fine with the workspace default.
	socketPath := filepath.Join(dirs.Data, "cog.sock")
	if override := os.Getenv("RETAINER_COG_SOCKET"); override != "" {
		socketPath = override
		logger.Info("cogsock: socket path overridden via RETAINER_COG_SOCKET", "path", socketPath)
	}
	sockServer, sockErr := cogsock.New(cogsock.Config{
		SocketPath: socketPath,
		Cog:        c,
		AgentName:  agentName,
		InstanceID: selfID.InstanceID(),
		Logger:     logger.With("component", "cogsock"),
	})
	if sockErr != nil {
		logger.Warn("cogsock: server construction failed; webui clients will not connect", "err", sockErr)
	}

	specs := []actor.Spec{
		{Name: "cog", Run: c.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "dag", Run: d.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "librarian", Run: lib.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "curator", Run: cur.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "housekeeper", Run: hk.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "metalearning", Run: mlPool.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "scheduler", Run: schedSvc.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "archivist", Run: arc.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "observer-agent", Run: observerAgent.Run, Restart: actor.Transient, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
		{Name: "scheduler-agent", Run: schedulerAgent.Run, Restart: actor.Permanent, Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute}},
	}
	if sockServer != nil {
		specs = append(specs, actor.Spec{
			Name: "cogsock", Run: sockServer.Run, Restart: actor.Permanent,
			Intensity: actor.MaxRestartIntensity{Bursts: 5, Window: time.Minute},
		})
	}
	sup := actor.NewSupervisor(specs...)

	tuiAgents := []*agent.Agent{observerAgent, schedulerAgent}

	cleanup := func() {
		if embedder != nil {
			if err := embedder.Close(); err != nil {
				logger.Warn("embed: close failed", "err", err)
			}
		}
		if capturesStore != nil {
			if err := capturesStore.Close(); err != nil {
				logger.Warn("captures store: close failed", "err", err)
			}
		}
		_ = cycleFile.Close()
		_ = closeLog()
		if err := cogLock.Release(); err != nil {
			logger.Warn("lockfile: release failed", "err", err)
		}
	}

	return &world{
		dirs:       dirs,
		logger:     logger,
		cog:        c,
		librarian:  lib,
		observer:   obs,
		archivist:  arc,
		curator:    cur,
		dag:        d,
		supervisor: sup,
		tuiAgents:  tuiAgents,
		agentName:  agentName,
		version:    version.Version,
		cleanup:    cleanup,
	}, nil
}

// closeCycleAndLog releases the cycle log writer + slog handler
// without touching the lockfile. Used by abortBootstrap on failure
// paths so a partially-constructed runtime doesn't strand handles.
func closeCycleAndLog(cycleFile interface{ Close() error }, closeLog func() error, embedder embed.Embedder) {
	if embedder != nil {
		_ = embedder.Close()
	}
	_ = cycleFile.Close()
	_ = closeLog()
}

// abortBootstrap releases all in-flight resources on a failed
// bootstrap. Lockfile last so the workspace stays locked until
// the rest is unwound.
func abortBootstrap(cogLock *lockfile.Lock, cycleFile interface{ Close() error }, closeLog func() error, embedder embed.Embedder) {
	closeCycleAndLog(cycleFile, closeLog, embedder)
	_ = cogLock.Release()
}

// pickProviderForBootstrap picks the LLM adapter based on env +
// config + mock-script flag. Reference-implementation defaults:
// Anthropic when ANTHROPIC_API_KEY is set, mock otherwise. Mock
// echoes input — useful for dev + integration tests.
func pickProviderForBootstrap(cfg *config.Config, mockScriptPath string, logger *slog.Logger) (provider llm.Provider, model string) {
	if mockScriptPath != "" {
		p, err := llm.LoadScriptedMock(mockScriptPath)
		if err != nil {
			logger.Warn("mock script load failed; falling back to plain mock", "path", mockScriptPath, "err", err)
			return llm.NewMock(), defaultAnthropicModel
		}
		logger.Info("provider: scripted mock", "script", mockScriptPath)
		return p, defaultAnthropicModel
	}

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		m := config.StringOr(cfg.TaskModel, defaultAnthropicModel)
		logger.Info("provider: anthropic", "model", m)
		return llm.NewAnthropic(key, m, config.IntOr(cfg.MaxTokens, defaultMaxTokens)), m
	}

	logger.Info("provider: mock (no ANTHROPIC_API_KEY in environment)")
	return llm.NewMock(), defaultAnthropicModel
}

// cogSubmitter adapts *cog.Cog to scheduler.Submitter. The
// scheduler fires autonomous cycles at job time; this adapter
// drains the reply channel in a goroutine so a long-running
// cycle doesn't wedge the scheduler tick.
type cogSubmitter struct {
	cog *cog.Cog
}

func (a cogSubmitter) SubmitWithSource(ctx context.Context, text string, source policy.Source) {
	if a.cog == nil {
		return
	}
	go func() {
		ch := a.cog.SubmitWithSource(ctx, text, source)
		<-ch
	}()
}

// fabricationGateAdapter bridges *policy.Engine to the narrower
// agent.AgentFabricationGate interface.
type fabricationGateAdapter struct {
	engine *policy.Engine
}

func (a *fabricationGateAdapter) ShouldGate(toolName string) bool {
	if a == nil || a.engine == nil {
		return false
	}
	return a.engine.IsHighRiskTool(toolName)
}

func (a *fabricationGateAdapter) ScoreToolInput(
	ctx context.Context,
	toolName, input string,
	toolLog []agent.FabricationToolEvent,
) agent.FabricationGateResult {
	if a == nil || a.engine == nil {
		return agent.FabricationGateResult{Verdict: "allow", Trail: "fabrication: no engine"}
	}
	policyLog := make([]policy.ToolEvent, len(toolLog))
	for i, ev := range toolLog {
		policyLog[i] = policy.ToolEvent{Name: ev.Name, Output: ev.Output}
	}
	r := a.engine.EvaluateToolFabrication(ctx, toolName, input, policyLog)
	return agent.FabricationGateResult{
		Verdict:       r.Verdict.String(),
		Score:         r.Score,
		Trail:         r.Trail,
		FlaggedClaims: policy.FlaggedClaims(r),
	}
}

var _ agent.AgentFabricationGate = (*fabricationGateAdapter)(nil)

// librarianFactSink adapts *librarian.Librarian to
// metalearning.FactSink. Workers (today: fabrication-audit) emit
// FactRecords; this wrapper fills in scope/operation/confidence
// defaults.
type librarianFactSink struct {
	lib *librarian.Librarian
}

func (s librarianFactSink) RecordFact(r metalearning.FactRecord) {
	if s.lib == nil {
		return
	}
	s.lib.RecordFact(librarian.Fact{
		Key:           r.Key,
		Value:         r.Value,
		Scope:         librarian.FactScopePersistent,
		Operation:     librarian.FactOperationWrite,
		Confidence:    1.0,
		SourceCycleID: r.SourceCycleID,
		HalfLifeDays:  r.HalfLifeDays,
	})
}

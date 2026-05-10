package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/seamus-brady/retainer/internal/agent"
	"github.com/seamus-brady/retainer/internal/cog"
	"github.com/seamus-brady/retainer/internal/config"
	"github.com/seamus-brady/retainer/internal/example"
	"github.com/seamus-brady/retainer/internal/identity"
	"github.com/seamus-brady/retainer/internal/librarian"
	"github.com/seamus-brady/retainer/internal/observer"
	"github.com/seamus-brady/retainer/internal/paths"
	"github.com/seamus-brady/retainer/internal/policy"
	"github.com/seamus-brady/retainer/internal/skills"
	"github.com/seamus-brady/retainer/internal/tools"
	"github.com/seamus-brady/retainer/internal/tui"
)

const (
	defaultAnthropicModel       = "claude-haiku-4-5-20251001"
	defaultMaxTokens            = 2048
	defaultRetentionDays        = 30
	// defaultMaxContextMessages caps the cog's running conversation
	// history to the last N messages on every cycle. 200 leaves
	// plenty of room for multi-turn work (an interactive session
	// rarely accumulates more inside a single conversation) while
	// putting a hard ceiling on multi-day sessions so token costs
	// stay bounded. Operators who want unlimited (SD's default)
	// set [cog].max_context_messages = 0 explicitly. Operators who
	// want tighter budgets set a smaller positive number.
	defaultMaxContextMessages = 200
	defaultAgentName            = "Nemo"
	defaultGateTimeoutMs        = 60000
	defaultInputQueueCap        = 10
	defaultLLMScorerTimeoutMs   = 8000
	defaultCanaryFailureLimit   = 3
	defaultInputRefusalText     = "I can't help with that."
	defaultOutputRefusalText    = "I started to answer but stopped — could you rephrase?"
	defaultNarrativeWindowDays  = 60
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			if err := runInit(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "retainer init: %v\n", err)
				os.Exit(1)
			}
			return
		case "send":
			if err := runSend(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "retainer send: %v\n", err)
				os.Exit(1)
			}
			return
		case "serve":
			if err := runServe(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "retainer serve: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}
	if err := runTUI(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "retainer: %v\n", err)
		os.Exit(1)
	}
}

func runTUI(args []string) error {
	fs := flag.NewFlagSet("retainer", flag.ContinueOnError)
	var (
		workspace  string
		configPath string
	)
	fs.StringVar(&workspace, "workspace", "", "workspace directory; overrides $RETAINER_WORKSPACE and the default $HOME/retainer")
	fs.StringVar(&workspace, "w", "", "alias for --workspace")
	fs.StringVar(&configPath, "config", "", "path to config TOML; overrides the workspace's config/config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	w, err := bootstrap(workspace, configPath, "")
	if err != nil {
		return err
	}
	defer w.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	supDone := make(chan error, 1)
	go func() { supDone <- w.supervisor.Run(ctx) }()

	p := tea.NewProgram(tui.New(w.agentName, w.version, w.cog, w.observer, w.tuiAgents), tea.WithAltScreen())
	_, runErr := p.Run()

	cancel()
	<-supDone
	return runErr
}

// runTUIOld is the pre-bootstrap-extraction implementation kept as a
// reference during the integration-test refactor. Removed in a
// follow-up once runSend has been exercised in production.

func loadConfig(explicit string, dirs paths.Paths) (*config.Config, string, error) {
	auto := filepath.Join(dirs.Config, "config.toml")
	return config.Load(config.LoadOpts{
		Explicit:   explicit,
		FromEnv:    os.Getenv("RETAINER_CONFIG"),
		AutoSearch: auto,
	})
}

func runInit(args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	var force bool
	var noGitignore bool
	fset.BoolVar(&force, "force", false, "overwrite existing config.toml")
	fset.BoolVar(&noGitignore, "no-gitignore", false, "skip dropping a .gitignore in the workspace")
	if err := fset.Parse(args); err != nil {
		return err
	}

	target := ""
	if fset.NArg() > 0 {
		target = fset.Arg(0)
	}
	dirs, err := paths.Resolve(target)
	if err != nil {
		return err
	}

	for _, dir := range []string{dirs.Workspace, dirs.Config, dirs.Data} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	configPath := filepath.Join(dirs.Config, "config.toml")
	if err := writeWorkspaceFile(configPath, example.ConfigTOML, force); err != nil {
		return err
	}
	policyPath := filepath.Join(dirs.Config, "policy.json")
	if err := writeWorkspaceFile(policyPath, policy.EmbeddedDefaults(), force); err != nil {
		return err
	}

	identityDir := filepath.Join(dirs.Config, identity.IdentitySubdir)
	if err := os.MkdirAll(identityDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", identityDir, err)
	}
	for name, content := range identity.EmbeddedFiles() {
		p := filepath.Join(identityDir, name)
		if err := writeWorkspaceFile(p, content, force); err != nil {
			return err
		}
	}

	// Seed starter skills into <config>/skills/<id>/SKILL.md so the
	// curator's Discover walks find them on first run.
	skillsRoot := filepath.Join(dirs.Config, skills.SeedDir)
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", skillsRoot, err)
	}
	for id, files := range skills.EmbeddedSkills() {
		skillDir := filepath.Join(skillsRoot, id)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", skillDir, err)
		}
		for filename, body := range files {
			p := filepath.Join(skillDir, filename)
			if err := writeWorkspaceFile(p, body, force); err != nil {
				return err
			}
		}
	}

	if !noGitignore {
		gitignorePath := filepath.Join(dirs.Workspace, ".gitignore")
		if _, err := os.Stat(gitignorePath); errors.Is(err, fs.ErrNotExist) {
			body := "# generated state — not committed\ndata/\n"
			if err := os.WriteFile(gitignorePath, []byte(body), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", gitignorePath, err)
			}
		}
	}

	fmt.Printf("retainer workspace ready at %s\n", dirs.Workspace)
	fmt.Printf("  config:   %s\n", configPath)
	fmt.Printf("  policy:   %s\n", policyPath)
	fmt.Printf("  identity: %s/\n", identityDir)
	fmt.Printf("  skills:   %s/\n", skillsRoot)
	fmt.Printf("  data:     %s\n", dirs.Data)
	fmt.Println()
	fmt.Println("To use this workspace by default:")
	fmt.Printf("  export RETAINER_WORKSPACE=%s\n", dirs.Workspace)
	return nil
}

// writeWorkspaceFile writes content to path, refusing to clobber an existing
// file unless force is true.
func writeWorkspaceFile(path string, content []byte, force bool) error {
	_, err := os.Stat(path)
	if err == nil {
		if !force {
			return fmt.Errorf("%s exists; use --force to overwrite", path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// cogTurnExtender adapts a closure to the
// tools.CogTurnExtender interface. The bootstrap binds the
// closure to c.RequestMoreTurns after the cog is constructed;
// this adapter lets the closure fit the interface the tool
// expects.
type cogTurnExtender func(parentCycleID string, additional int, reason string) (int, error)

func (f cogTurnExtender) RequestMoreTurns(parentCycleID string, additional int, reason string) (int, error) {
	return f(parentCycleID, additional, reason)
}

// buildCogTools constructs the cog's own tool registry. Memory
// tools always register (librarian is always present). The cog
// reaches its specialists (observer + scheduler) via
// `agent_<name>` delegate tools.
//
// Web tools (web_search, fetch_url) live on the cog directly —
// no separate researcher actor in the reference cut. The model
// picks fetch_url for known URLs, web_search for unknown ones,
// and synthesises from there.
func buildCogTools(logger *slog.Logger, lib *librarian.Librarian, observerAgent *agent.Agent, schedulerAgent *agent.Agent, skillsDirs []string, onAgentDone func(string, agent.CompletionRecord), requestMoreTurns func(string, int, string) (int, error)) cog.ToolDispatcher {
	registry := tools.NewRegistry()

	// Memory — librarian-backed direct operations.
	registry.MustRegister(tools.MemoryWrite{Lib: lib})
	registry.MustRegister(tools.MemoryRead{Lib: lib})
	registry.MustRegister(tools.MemoryClearKey{Lib: lib})
	registry.MustRegister(tools.MemoryQueryFacts{Lib: lib})

	// Web — fetch_url + web_search via DuckDuckGo. Reference cut
	// keeps both on the cog; no API keys, no third-party SaaS.
	registry.MustRegister(tools.FetchURL{})
	registry.MustRegister(tools.WebSearch{})

	// Skill discovery surface.
	registry.MustRegister(tools.ReadSkill{SkillsDirs: skillsDirs})

	// Safety valve: agent can extend its own tool-turn budget
	// mid-cycle up to a hard ceiling.
	registry.MustRegister(tools.RequestMoreTurns{Cog: cogTurnExtender(requestMoreTurns)})

	// Specialist delegation — one tool per registered agent.
	if observerAgent != nil {
		registry.MustRegister(tools.DelegateToAgent{Agent: observerAgent, OnDone: onAgentDone})
	}
	if schedulerAgent != nil {
		registry.MustRegister(tools.DelegateToAgent{Agent: schedulerAgent, OnDone: onAgentDone})
	}

	logger.Info("cog tools registered", "names", registry.Names())
	return registry
}

// buildObserverTools constructs the observer agent's tool
// registry. Observer is the cog's knowledge gateway covering
// three layers:
//
//   - Recent (hot index): inspect_cycle, recall_recent, get_fact
//   - CBR curation: recall_cases + suppress / unsuppress /
//     boost / annotate / correct
//   - Deep read (folded from remembrancer 2026-05-07; mining +
//     consolidation moved to internal/metalearning/ on 2026-05-04):
//     deep_search, find_connections
//
// The deep-archive tools call into the system-layer
// internal/remembrancer/ package (ReadCases, ReadNarrative,
// Search, Consolidate, etc.) which is unchanged. Only the
// agent-facing wrappers moved.
func buildObserverTools(logger *slog.Logger, obs *observer.Observer, dataDir string, lib *librarian.Librarian, skillsDirs []string) agent.ToolDispatcher {
	registry := tools.NewRegistry()

	// Recent (hot index) tools.
	registry.MustRegister(tools.ObserverInspectCycle{Observer: obs})
	registry.MustRegister(tools.ObserverRecallRecent{Observer: obs})
	registry.MustRegister(tools.ObserverGetFact{Observer: obs})

	// CBR curation tools — operator/agent surface for recall +
	// curation. case_curate consolidates suppress / unsuppress /
	// boost / annotate / correct under an action discriminator
	// (mirrors strategy_curate from PR #58).
	registry.MustRegister(tools.ObserverRecallCases{Lib: lib})
	registry.MustRegister(tools.ObserverCaseCurate{Lib: lib})

	// Deep-read tools (folded from remembrancer). Live introspection
	// only — keyword search across the full archive +
	// cross-reference. mine_patterns / consolidate_memory /
	// write_consolidation_report retired 2026-05-04; that work
	// runs on the metalearning pool's tick and lands as durable
	// markdown reports + a patterns log the agent reads via
	// deep_search.
	remDeps := &tools.RemembrancerDeps{
		DataDir: dataDir,
		Logger:  logger.With("component", "deep-archive-tools"),
	}
	registry.MustRegister(tools.DeepSearch{Deps: remDeps})
	registry.MustRegister(tools.FindConnections{Deps: remDeps})

	// read_skill is always available so the observer can consult
	// memory-management before answering recall questions.
	registry.MustRegister(tools.ReadSkill{SkillsDirs: skillsDirs})
	logger.Info("observer tools registered", "names", registry.Names())
	return registry
}

// skillsSearchDirs returns the ordered list of directories
// `skills.Discover` walks. Workspace skills come first so an
// operator override beats a global default. The optional
// `~/.config/retainer/skills` lets one user share skills across
// workspaces without seeding each one; missing-dir is fine —
// Discover skips it silently.
func skillsSearchDirs(dirs paths.Paths) []string {
	return []string{
		filepath.Join(dirs.Config, skills.SeedDir),
		"~/.config/retainer/skills",
	}
}

// inputSourceLabel maps the cog's policy.Source into the curator's
// string-typed CycleContext.InputSource. Mirrors the label set the
// sensorium <situation input=...> attribute uses today; new Source
// values land here as the input-channels plan ships them.
func inputSourceLabel(s policy.Source) string {
	switch s {
	case policy.SourceInteractive:
		return "user"
	case policy.SourceAutonomous:
		return "autonomous"
	}
	return "user"
}


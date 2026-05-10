// Package config loads Retainer's TOML configuration.
//
// All fields are pointer-typed so "not set" is distinguishable from "set to
// the zero value." Defaults are applied at usage sites, never inside the
// loader — this preserves the layering so an env var or CLI flag can detect
// "config left this unset" and override.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Provider       *string `toml:"provider"`
	TaskModel      *string `toml:"task_model"`
	ReasoningModel *string `toml:"reasoning_model"`
	MaxTokens      *int    `toml:"max_tokens"`
	MaxTurns       *int    `toml:"max_turns"`

	Agent   AgentConfig   `toml:"agent"`
	Cog     CogConfig     `toml:"cog"`
	Gate    GateConfig    `toml:"gate"`
	Policy  PolicyConfig  `toml:"policy"`
	Memory  MemoryConfig  `toml:"memory"`
	Logging LoggingConfig `toml:"logging"`
	Retry   RetryConfig   `toml:"retry"`
}

// CogConfig holds cog-loop-level knobs the operator can tune
// per workspace. Defaults live at the cog package's
// constructor; this struct only carries operator overrides.
type CogConfig struct {
	// MaxToolTurns bounds the cog's react loop within a single
	// cycle. Default 10. Hard ceiling 20 (enforced by the cog;
	// values above 20 are clamped). Higher values give the cog
	// more room for multi-step diagnostics + deep delegation
	// chains; lower values tighten the runaway-loop guard.
	MaxToolTurns *int `toml:"max_tool_turns"`
	// MaxContextMessages caps the running conversation history
	// fed to the LLM each cycle. Zero / unset = unbounded
	// (matches SD's default). Operators with token-cost or
	// rate-limit pressure set it to e.g. 50; the cog drops the
	// oldest messages while preserving role-alternation and
	// tool-call pair invariants.
	MaxContextMessages *int `toml:"max_context_messages"`
}

type AgentConfig struct {
	Name    *string `toml:"name"`
	Version *string `toml:"version"`
}

type GateConfig struct {
	TimeoutMs     *int `toml:"timeout_ms"`
	InputQueueCap *int `toml:"input_queue_cap"`
}

type PolicyConfig struct {
	InputRefusal       *string `toml:"input_refusal"`
	OutputRefusal      *string `toml:"output_refusal"`
	LLMScorerTimeoutMs *int    `toml:"llm_scorer_timeout_ms"`
	CanaryFailureLimit *int    `toml:"canary_failure_limit"`

	// Fabrication is the LLM-scored output gate that flags
	// confidently-stated identifiers / file:line references /
	// struct fields not grounded in the cycle's tool log.
	// Disabled by default — opt-in via [policy.fabrication]
	// because every output cycle costs an extra LLM call when
	// it's on. See doc/specs/document-management.md and
	// project_fabrication_policy memory.
	Fabrication FabricationConfig `toml:"fabrication"`
}

// FabricationConfig wires the fabrication output gate. Defaults
// applied at the bootstrap call site (per the config-package
// convention).
//
// Enabled gates the wiring entirely. When false (default), the
// cog's output gate runs deterministic-only and the operator
// pays no extra LLM cost. Model defaults to the cog's task
// model. MinConfidence is the threshold above which an
// "escalate" verdict actually appends a verification footer to
// the operator's reply (below threshold, the score lands in
// the cycle log as telemetry only).
type FabricationConfig struct {
	Enabled       *bool    `toml:"enabled"`
	Model         *string  `toml:"model"`
	TimeoutMs     *int     `toml:"timeout_ms"`
	MinConfidence *float64 `toml:"min_confidence"`
}

// MemoryConfig holds librarian / memory-store knobs. Per-store windows
// reflect that different stores have different "recent" semantics —
// narrative is rolling; facts have no window (current state).
type MemoryConfig struct {
	// NarrativeWindowDays bounds the SQLite hot index for narrative.
	// JSONL on disk is unaffected — older entries remain accessible to
	// the remembrancer.
	NarrativeWindowDays *int `toml:"narrative_window_days"`
}

type LoggingConfig struct {
	Verbose       *bool `toml:"verbose"`
	RetentionDays *int  `toml:"retention_days"`
}

type RetryConfig struct {
	MaxRetries       *int `toml:"max_retries"`
	InitialDelayMs   *int `toml:"initial_delay_ms"`
	RateLimitDelayMs *int `toml:"rate_limit_delay_ms"`
	OverloadDelayMs  *int `toml:"overload_delay_ms"`
	MaxDelayMs       *int `toml:"max_delay_ms"`
}

// Parse decodes a Config from r. Unknown keys are tolerated (Springdrift's
// "log-not-reject" stance) so a newer config file can be read by an older
// binary without failing. Tests can opt into strict mode separately.
func Parse(r io.Reader) (*Config, error) {
	var c Config
	if err := toml.NewDecoder(r).Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	return &c, nil
}

// LoadOpts describes where Load should look for a config file. Explicit and
// FromEnv are required-if-set: a non-empty path that doesn't exist is an
// error. AutoSearch is best-effort: missing means "no config file in use,"
// not an error.
type LoadOpts struct {
	Explicit   string // --config <path>; user explicitly asked for this file
	FromEnv    string // $RETAINER_CONFIG
	AutoSearch string // <config-dir>/config.toml; OK if absent
}

// Load returns the loaded config and the path it came from (empty if none).
// An empty *Config is returned when no file is found via AutoSearch.
//
// After TOML decoding, env vars in the `RETAINER_<SECTION>_<KEY>`
// namespace are applied as overrides — see applyEnvOverrides for the
// convention. This is what makes `RETAINER_COMMS_INBOX_ID=inb_…`
// in your shell or workspace `.env` work without editing TOML.
func Load(opts LoadOpts) (*Config, string, error) {
	cfg, src, err := loadFromOpts(opts)
	if err != nil {
		return cfg, src, err
	}
	if err := applyEnvOverrides(cfg); err != nil {
		return cfg, src, err
	}
	return cfg, src, nil
}

func loadFromOpts(opts LoadOpts) (*Config, string, error) {
	switch {
	case opts.Explicit != "":
		return loadFile(opts.Explicit)
	case opts.FromEnv != "":
		return loadFile(opts.FromEnv)
	case opts.AutoSearch != "":
		if _, err := os.Stat(opts.AutoSearch); errors.Is(err, fs.ErrNotExist) {
			return &Config{}, "", nil
		}
		return loadFile(opts.AutoSearch)
	}
	return &Config{}, "", nil
}

func loadFile(path string) (*Config, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, path, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()
	c, err := Parse(f)
	if err != nil {
		return nil, path, err
	}
	return c, path, nil
}

// IntOr returns *p if non-nil, otherwise d. Use at the call site to apply a
// default for a single field without mutating the loaded Config.
func IntOr(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}

// StringOr returns *p if non-nil, otherwise d.
func StringOr(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}

// BoolOr returns *p if non-nil, otherwise d.
func BoolOr(p *bool, d bool) bool {
	if p == nil {
		return d
	}
	return *p
}

// Float64Or returns *p if non-nil, otherwise d.
func Float64Or(p *float64, d float64) float64 {
	if p == nil {
		return d
	}
	return *p
}

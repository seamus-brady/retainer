# Policy

`internal/policy` is the safety gate engine. It is a port of Springdrift's D'/dprime scoring layer with several layers deferred or dropped.

## Gates

The cog calls four gates per cycle (in order):

1. **Input gate** — before any LLM call. Evaluates the user's raw text.
2. **Tool gate** — before each tool dispatch. Evaluates the model's tool input.
3. **Post-exec gate** — after each tool returns. Evaluates the tool's output before the model sees it.
4. **Output gate** — before the final reply leaves the cog.

Each gate produces a `Result`:

```
Verdict      Allow | Escalate | Block
Score        float64
Triggered    []Trigger
Trail        string   // human-readable, written to cycle log
Inconclusive bool     // canary couldn't decide (LLM error)
```

## Source-aware demotion

`policy.Source` distinguishes `SourceInteractive` (operator typing in the TUI/webui) from `SourceAutonomous` (scheduler fires, future webhook receivers, future inbound mail).

On the **input gate only**, interactive sources get `Block → Escalate` demotion: an operator pasting an adversarial prompt for testing gets a "this is suspicious, please confirm" instead of a hard reject. Autonomous inputs preserve the hard reject — there is no operator in the loop to confirm.

## Layered evaluation (input gate)

In priority order during input evaluation:

```
L1 deterministic regex prefilter   (always)
   └─→ candidates?
       └─→ L2 LLM scorer            (when configured)  ← deferred today
   ⫘ in parallel
   Canary probes                    (when configured)
       ├─→ canary.hijack             — fresh sentinel hijack test
       └─→ canary.leakage            — system-prompt leakage test
```

### L1: deterministic rules

`internal/policy/rules.go` loads regex-based rules from `<workspace>/config/policy.json`. The `init` command seeds it from `internal/policy/defaults.json`. Each rule has:

- `name`, `pattern` (regex), `domain`, `importance`, `magnitude`.
- A score is summed across firing rules; a threshold determines `Allow / Escalate / Block`.

This layer is fast (no LLM), deterministic, and operator-editable. It's the first line of defence and the only layer that's always on.

### L2: LLM scorer

Deferred. The scorer would disambiguate L1 candidates with a structured-output LLM call. The wiring is present (`policy.LLMScorerConfig`) so the field exists in the engine, but no production use today.

### Canary probes

Two parallel LLM calls per input:

- **Hijack probe** — embeds a fresh randomised sentinel string in the system prompt, asks the model to repeat it. If the user's text manages to suppress the sentinel, the model is being hijacked.
- **Leakage probe** — checks whether the user's text causes the model to leak parts of the system prompt.

Both are independent of L1; they fire in parallel with the deterministic prefilter. A failure (LLM unreachable, timeout) marks the result `Inconclusive` rather than blocking — fail-open by design, with the inconclusive flag landing in the cycle log so an operator can investigate.

`canary_failure_limit` (default 3) marks the canary subsystem as **degraded** after consecutive failures; it stops calling out and the cog logs that input gating is running deterministic-only.

## Output side: fabrication scorer

`policy.FabricationScorerConfig` wires a separate LLM round-trip after the cog produces its final reply. It checks every claim in the reply against the cycle's tool log; verdicts above `min_confidence` (default 0.7) append a verification footer to the user-facing reply naming the flagged claims. Below threshold, the score lands in the cycle log as telemetry only.

Default-on. Disable via `[policy.fabrication].enabled = false`.

## Tool gate and post-exec gate

The tool gate runs deterministic rules against the tool input JSON. If `Block`, the cog injects a `tool_result` with `IsError=true` and the literal text "tool blocked by policy" — the model sees a clean failure and can recover or refuse.

The post-exec gate runs deterministic rules against the tool's actual output. If `Block`, the cog injects "tool output redacted by policy" in place of the real output — the model never sees the redacted content, but its own reasoning can continue.

Agents (specialists) currently skip the tool gate for low-risk tools — there's a `shouldGate` allowlist by tool name. High-risk tools (delegated work, web fetches with arbitrary URLs) still gate.

## What's deferred / dropped

From Springdrift's full D'/dprime stack:

- **Layer 2 LLM scorer** — wired but not enabled.
- **Layer 3 meta observer** — not ported.
- **Normative calculus / FlourishingVerdict / Stoic axioms** — out entirely. Retainer does not run an ethics-philosophy layer.

These are deliberate omissions; the project doc-comments call them out where they would otherwise live.

## Cycle log integration

Every gate evaluation writes a `policy_*` event to `data/cycle-log/<date>.jsonl` containing the verdict, score, all triggered rules, and the trail string. This is the audit trail an operator reads when asking "why did the agent refuse that?"

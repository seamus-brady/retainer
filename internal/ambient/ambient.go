// Package ambient defines the Signal type — perception that shapes the
// next cycle's sensorium without triggering its own cycle.
//
// Distinct from input. An input wakes the cog up and runs a cycle in
// response (operator typing, scheduler firing, comms receiving). An
// ambient signal is noticed but doesn't run a cycle on its own — it
// surfaces in the next cycle's <ambient> sensorium block, whatever
// triggered that cycle.
//
// Producers (today empty; planned: forecaster, observer, in-process
// telemetry) call cog.Notice(Signal). The cog buffers the signals and
// drains them at the start of each cycle, handing the snapshot to the
// curator which renders the <ambient> section.
//
// Buffer is bounded and lossy by design: when a producer notices faster
// than the cog cycles, the oldest signals drop. Auditable producers
// log to cyclelog directly, so the buffer being lossy doesn't lose
// forensics — just freshness for the LLM's perception.
package ambient

import "time"

// Signal is one ambient observation, surfaced in the next cycle's
// <ambient> sensorium block.
//
// Fields:
//
//   - Source identifies the producer ("forecaster", "observer",
//     "scheduler", etc.) so the agent can weigh signals by origin.
//   - Kind is the category of signal ("plan_health_degraded",
//     "cycle_anomaly", "memory_pressure"). Kept open-string for V1;
//     producers document their own taxonomy.
//   - Detail is human-readable context the LLM can consume directly.
//   - Timestamp is when the signal was noticed (producer-set, not
//     queue-receive time).
type Signal struct {
	Source    string
	Kind      string
	Detail    string
	Timestamp time.Time
}

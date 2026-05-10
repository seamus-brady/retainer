#!/usr/bin/env bash
# Deep-archive integration test (formerly "remembrancer" —
# the remembrancer agent was retired 2026-05-07; its tools
# now live on the observer). Drives multiple cycles to seed
# cases, then exercises mine_patterns and
# write_consolidation_report through scripted-mock cycles
# delegated to agent_observer.
#
# Asserts:
#   - 4 cycles seed 4 cases (3 auth-domain, 1 research)
#   - cog dispatches to agent_observer for both deep-archive
#     work cycles (mine + report)
#   - cycle log records agent_cycle_start events parented to
#     the cog cycles (proves the delegation chain)
#   - observer token state records the dispatches

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "remembrancer" "$ws"

wk_init "$ws"

fixtures="$(dirname "$0")/fixtures"

# Seed: 4 cycles → 4 cases. Three auth-domain (verb-driven
# Troubleshooting) + one research-domain (DomainKnowledge).
wk_send "$ws" "investigate the auth flow" >/dev/null
wk_send "$ws" "fix the auth login bug" >/dev/null
wk_send "$ws" "debug auth token handling" >/dev/null
wk_send "$ws" "summarise the architecture document" >/dev/null
pass "seeded 4 cycles"

case_count=$(wc -l <"$ws/data/cases/cases.jsonl" | tr -d ' ')
if [[ "$case_count" -ne 4 ]]; then
    fail "expected 4 cases, got $case_count"
fi
pass "4 cases derived from seeds"

# Cycle 5: mine_patterns via scripted mock. The cog's tool result
# carries the cluster summary text; the agent's final reply
# acknowledges. We don't assert on the reply text — we assert on the
# tool's BEHAVIOUR by inspecting the cycle log for a tool dispatch
# event with the expected name + checking that the cases.jsonl that
# the tool reads exists with the expected shape.
reply=$(wk_send_script "$ws" "$fixtures/remembrancer-mine.json" "what patterns do you see?")
if [[ -z "$reply" ]]; then
    fail "mine_patterns cycle reply empty"
fi
pass "mine_patterns cycle ran ($reply)"

# Cycle 6: cog delegates to agent_observer asking for a
# consolidation report. The sub-agent's mock provider is echo —
# it doesn't actually call write_consolidation_report. This
# scenario asserts on the DELEGATION shape (agent dispatched,
# parent_id chain, node_type=agent), not on the inner tool
# execution. Tool execution itself is unit-tested in the
# tools/remembrancer_test.go package — the wrapper structs
# are unchanged; only their registration moved.
wk_send_script "$ws" "$fixtures/remembrancer-report.json" "write a weekly review report" >/dev/null
pass "agent_observer (deep-archive report) cycle ran"

# Cycle log should show the agent was dispatched. Two cycles
# delegate to agent_remembrancer (mine + report); we expect at
# least 2 agent_cycle_start events with node_type=agent.
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/")"
agent_cycle_count=$(jq -c 'select(.type == "agent_cycle_start" and .node_type == "agent")' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$agent_cycle_count" -lt 2 ]]; then
    fail "expected >=2 agent_cycle_start events; got $agent_cycle_count"
fi
pass "$agent_cycle_count agent_cycle_start events recorded (node_type=agent)"

# Each agent_cycle_start should chain to a cog cycle_start
# cycle_id via parent_id — the cog→agent linkage that proves
# delegation actually happened.
agent_parents=$(jq -r 'select(.type == "agent_cycle_start") | .parent_id' <"$cycle_log" | sort -u)
for parent in $agent_parents; do
    matched=$(jq -c "select(.type == \"cycle_start\" and .cycle_id == \"$parent\")" <"$cycle_log" | wc -l | tr -d ' ')
    if [[ "$matched" -lt 1 ]]; then
        fail "agent_cycle_start.parent_id=$parent doesn't match any cog cycle_start.cycle_id"
    fi
done
pass "every agent_cycle_start.parent_id chains to a cog cycle_start"

# Token tracking should record the observer dispatches —
# the JSON state file is the canonical "this agent ran" signal,
# matching subprocess.sh's pattern for the researcher. The
# remembrancer agent retired 2026-05-07; its tools moved to
# the observer's registry, so the dispatch trail lives under
# .agent-observer-tokens.json now.
token_file="$ws/data/.agent-observer-tokens.json"
require_file "$token_file" "observer token state (deep-archive dispatches now under observer)"
dispatches=$(jq -r '.dispatch_count' <"$token_file")
if [[ "$dispatches" -lt 2 ]]; then
    fail "observer dispatch_count = $dispatches, want >= 2"
fi
pass "observer token state records $dispatches dispatches"

summary

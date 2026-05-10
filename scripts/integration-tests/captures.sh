#!/usr/bin/env bash
# Captures (commitment tracker) end-to-end test.
#
# Asserts:
#   - First send with a commitment-phrase reply writes a cycle log
#   - Second send's bootstrap fires the metalearning captures worker
#     (immediate checkAll on startup), which scans the prior cycle
#     log and appends a Pending capture to the captures JSONL
#   - The capture is content-addressed (id non-empty), tagged
#     source=agent_self, status=pending, with the source_cycle_id
#     pointing at cycle 1
#
# This is the v1.0 commitment-tracker MVP wire-format guard. The
# scanner is heuristic-only; a phrase like "I'll send" in the
# reply triggers detection. Mock provider echoes the operator's
# input, so the operator-typed phrase ends up in the reply.

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "captures (commitment tracker)" "$ws"

wk_init "$ws"

# Cycle 1: drive a reply that contains a commitment phrase. The
# mock prefixes the reply with "mock: " — the prefix doesn't break
# the phrase match, the heuristic is case-insensitive substring.
reply1=$(RETAINER_WORKSPACE="$ws" RETAINER_PROVIDER=mock \
    "$(wk_bin)" send "I'll send the report by tomorrow")
if [[ "$reply1" != *"send the report"* ]]; then
    fail "reply 1 missing the phrase: '$reply1'"
fi
pass "drove cycle 1 with a commitment-shaped reply"

# Verify the cycle log was written.
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/" | head -1)"
require_file "$cycle_log" "cycle-log JSONL"
cycle1_id=$(jq -r 'select(.type == "cycle_start") | .cycle_id' <"$cycle_log" | head -1)
if [[ -z "$cycle1_id" ]]; then
    fail "no cycle_start event in cycle log"
fi
pass "captured cycle 1 id: ${cycle1_id:0:8}..."

# The metalearning pool gates worker firing by interval —
# captures worker is 10-min cadence, so a second `send` started
# seconds later won't re-fire it. Reset the pool's per-worker
# state so the next bootstrap's immediate checkAll treats every
# worker as never-run and fires it. This is a test-only hack;
# production runs naturally cross the interval boundary.
rm -f "$ws/data/.metalearning-state.json"
pass "reset metalearning state to force captures worker re-fire"

# Cycle 2: any input. Bootstrap fires the metalearning pool's
# immediate checkAll on startup, which runs the captures worker
# against the cycle log and appends the Pending capture.
RETAINER_WORKSPACE="$ws" RETAINER_PROVIDER=mock \
    "$(wk_bin)" send "anything else?" >/dev/null
pass "drove cycle 2 (triggers metalearning checkAll)"

# Assert captures JSONL exists and has a Pending entry.
captures_dir="$ws/data/captures"
if [[ ! -d "$captures_dir" ]]; then
    fail "captures dir missing at $captures_dir"
fi
captures_file="$(ls "$captures_dir"/*-captures.jsonl 2>/dev/null | head -1 || true)"
if [[ -z "$captures_file" ]]; then
    fail "no captures JSONL file in $captures_dir"
fi
pass "captures JSONL exists: $(basename "$captures_file")"

pending_count=$(jq -r 'select(.status == "pending") | .id' <"$captures_file" | wc -l | tr -d ' ')
if [[ "$pending_count" -lt 1 ]]; then
    fail "expected at least 1 pending capture; got $pending_count"
fi
pass "$pending_count pending capture(s) recorded"

# Validate one capture's shape.
first_capture=$(head -1 "$captures_file")
got_source=$(jq -r '.source' <<<"$first_capture")
if [[ "$got_source" != "agent_self" ]]; then
    fail "capture source = '$got_source', want 'agent_self'"
fi
pass "capture source is agent_self"

got_status=$(jq -r '.status' <<<"$first_capture")
if [[ "$got_status" != "pending" ]]; then
    fail "capture status = '$got_status', want 'pending'"
fi
pass "capture status is pending"

got_cycle=$(jq -r '.source_cycle_id' <<<"$first_capture")
if [[ "$got_cycle" != "$cycle1_id" ]]; then
    fail "capture source_cycle_id = '$got_cycle', want '$cycle1_id'"
fi
pass "capture source_cycle_id points at cycle 1"

got_text=$(jq -r '.text' <<<"$first_capture")
if [[ -z "$got_text" ]] || [[ "$got_text" == "null" ]]; then
    fail "capture text excerpt is empty: '$got_text'"
fi
pass "capture text excerpt is populated"

summary

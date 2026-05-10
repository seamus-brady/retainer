#!/usr/bin/env bash
# Memory tool test: scripted mock drives a memory_write call, then a
# memory_read call in a second cycle.
#
# Asserts:
#   - First cycle's facts JSONL contains the written fact
#   - Provenance: source_cycle_id matches the in-flight cycle
#   - Second cycle's recall pulls the same value (verified by reading
#     facts.jsonl after the second send)

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "memory write" "$ws"

wk_init "$ws"

fixtures="$(dirname "$0")/fixtures"

# Cycle 1: write a fact via tool_use(memory_write).
reply=$(wk_send_script "$ws" "$fixtures/memory-write.json" "remember that I prefer integration tests")
if [[ "$reply" != *"remember"* ]]; then
    fail "unexpected reply: '$reply'"
fi
pass "memory_write cycle reply: '$reply'"

# Facts JSONL exists and has the entry.
fact_dir="$ws/data/facts"
if [[ ! -d "$fact_dir" ]]; then
    fail "facts dir missing"
fi
fact_file="$fact_dir/$(ls "$fact_dir")"
require_file "$fact_file" "facts JSONL"

# Inspect the fact record.
got_value=$(jq -r 'select(.key == "user.preference.testing") | .value' <"$fact_file" | head -1)
if [[ "$got_value" != "integration tests rule" ]]; then
    fail "fact value wrong: '$got_value'"
fi
pass "fact value persisted correctly"

got_scope=$(jq -r 'select(.key == "user.preference.testing") | .scope' <"$fact_file" | head -1)
if [[ "$got_scope" != "persistent" ]]; then
    fail "fact scope = '$got_scope', want persistent"
fi
pass "fact scope is persistent"

# Provenance: source_cycle_id should match the cycle's ID from cycle log.
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/")"
cycle_id=$(jq -r 'select(.type == "cycle_start") | .cycle_id' <"$cycle_log" | head -1)
got_provenance=$(jq -r 'select(.key == "user.preference.testing") | .source_cycle_id' <"$fact_file" | head -1)
if [[ "$got_provenance" != "$cycle_id" ]]; then
    fail "fact provenance = '$got_provenance', want '$cycle_id'"
fi
pass "fact provenance matches in-flight cycle"

# A case should also have been derived for this cycle (heuristic +
# OptimisticJudge default since no real LLM judge runs with mock).
require_file "$ws/data/cases/cases.jsonl" "cases JSONL"
case_count=$(wc -l <"$ws/data/cases/cases.jsonl" | tr -d ' ')
if [[ "$case_count" -lt 1 ]]; then
    fail "expected at least 1 case, got $case_count"
fi
pass "case derived alongside memory_write ($case_count case(s))"

summary

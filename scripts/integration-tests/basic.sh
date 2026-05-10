#!/usr/bin/env bash
# Basic round-trip test: one cycle through `retainer send` with the
# default echo mock provider.
#
# Asserts:
#   - cycle-log JSONL has cycle_start + cycle_complete events
#   - narrative JSONL has one entry with the cycle_id from the cycle log
#   - cases JSONL has one derived case (heuristic + judge defaults
#     produce a success-status case for any non-empty user input)
#   - facts JSONL is absent or empty (no memory_write tool was called)

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "basic round-trip" "$ws"

wk_init "$ws"

reply=$(wk_send "$ws" "investigate the auth bug")
if [[ "$reply" != "mock: investigate the auth bug" ]]; then
    fail "unexpected reply: '$reply'"
fi
pass "echo reply matches input"

# Cycle log
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/")"
require_file "$cycle_log" "cycle log"
require_jsonl_event "$cycle_log" "cycle_start"
require_jsonl_event "$cycle_log" "cycle_complete"
require_jsonl_event "$cycle_log" "llm_request"
require_jsonl_event "$cycle_log" "llm_response"
require_jsonl_event "$cycle_log" "policy_decision"

# Cycle ID consistency between cycle-log and narrative.
cycle_id=$(jq -r 'select(.type == "cycle_start") | .cycle_id' <"$cycle_log" | head -1)
if [[ -z "$cycle_id" ]]; then
    fail "no cycle_id in cycle_start event"
fi
pass "cycle_id captured: ${cycle_id:0:8}..."

# Narrative
narrative_dir="$ws/data/narrative"
narrative_file="$narrative_dir/$(ls "$narrative_dir")"
require_file "$narrative_file" "narrative log"
require_jq "$narrative_file" '.cycle_id' "$cycle_id" "narrative entry has matching cycle_id"
require_jq "$narrative_file" '.status' "complete" "narrative status is complete"

# Cases — one entry derived (intent contains "investigate" → Troubleshooting)
require_file "$ws/data/cases/cases.jsonl" "cases JSONL"
# Heuristic curator: classifies "investigate the auth bug" as
# Exploration (not an ack-prefix, no LLM); intent description is
# the heuristic prefix "exploration: <input>" so retrieval signals
# don't end up identical to the verbatim user text. Category is
# Troubleshooting via the verb-detection rule.
require_jq "$ws/data/cases/cases.jsonl" '.problem.intent_class' "exploration" "case intent_class is exploration"
require_jq "$ws/data/cases/cases.jsonl" '.category' "troubleshooting" "case category is troubleshooting (verb-driven)"
require_jq "$ws/data/cases/cases.jsonl" '.outcome.status' "success" "case outcome is success"
intent_value=$(jq -r '.problem.intent' <"$ws/data/cases/cases.jsonl")
if [[ "$intent_value" == "investigate the auth bug" ]]; then
    fail "intent should be curator's framing, not verbatim user input; got '$intent_value'"
fi
pass "case intent is curator-framed, not verbatim user input ('$intent_value')"

# Facts: the agent didn't call memory_write this cycle, so no
# operator-attributable facts should land. The metalearning
# fabrication-audit worker fires on startup and writes one
# system fact (`integrity_suspect_replies_7d`) regardless —
# that's expected, not a signal of agent-side memory writes.
# We filter that key out and assert the remainder is empty.
#
# awk (not grep -v) because under `set -o pipefail` a grep -v
# that matches every line returns exit code 1 (no unmatched
# lines) and the whole pipeline dies — exactly what happens on
# a clean run where ONLY the integrity fact is present. awk's
# pattern action returns 0 even when it filters out every line.
if [[ -d "$ws/data/facts" ]]; then
    fact_lines=$(find "$ws/data/facts" -name '*.jsonl' -exec cat {} \; | awk 'NF && !/"key":"integrity_/' | wc -l | tr -d ' ')
    if [[ "$fact_lines" -gt 0 ]]; then
        fail "expected no operator-attributable facts; found $fact_lines lines (excluding integrity_*)"
    fi
fi
pass "no operator-attributable facts written (only the system integrity fact, if any)"

summary

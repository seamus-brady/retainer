#!/usr/bin/env bash
# CBR recall (Phase 3): drive a cycle that establishes a case, then
# drive a second cycle whose user input matches the prior case.
# Assert that the curator_assembled cycle-log event for the second
# cycle carries recalled_case_ids — that's the audit hook proving
# the case actually reached the agent's system prompt.
#
# What this verifies end-to-end:
#   1. A real prior case lands in cases.jsonl after cycle 1.
#   2. Cycle 2's curator_assembled event has recalled_case_ids
#      with the prior case's 8-char prefix in the list.
#   3. The IDs only appear when UserInput is non-empty (negative
#      assertion: a separate workspace running with no second
#      cycle never gets a populated recalled_case_ids field).
#
# Why the cycle-log is the surface, not the prompt body itself:
#   The mock provider doesn't echo the system prompt. The curator
#   already records the picked IDs to its assembled event so audit
#   tools can answer "did case X reach this cycle?" — which is the
#   exact question this scenario asks.

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "CBR recall (Phase 3 case injection)" "$ws"

wk_init "$ws"

# Cycle 1 — derive a case the next cycle can match. Pick a phrase
# the heuristic curator will route to a non-conversational
# category (so it ends up in cases.jsonl with category != "").
priming_input="investigate the auth flow timeouts"
wk_send "$ws" "$priming_input" >/dev/null
pass "primed cycle 1 with: $priming_input"

cases_file="$ws/data/cases/cases.jsonl"
require_file "$cases_file" "cases.jsonl"

case_count=$(wc -l <"$cases_file" | tr -d ' ')
if [[ "$case_count" -ne 1 ]]; then
    fail "expected 1 case after priming, got $case_count"
fi
pass "1 case derived from priming cycle"

# Capture the priming case's full ID and 8-char prefix.
prior_case_id=$(jq -r '.case_id' <"$cases_file")
if [[ -z "$prior_case_id" || "$prior_case_id" = "null" ]]; then
    fail "priming case missing case_id"
fi
prior_short="${prior_case_id:0:8}"
pass "captured prior case prefix: $prior_short"

# Sanity: the prior case must have a category (recall_cases skips
# categoryless entries by design). Conversation cases would slip
# through this gate, but "investigate the auth flow timeouts" is
# heuristic-classified as troubleshooting.
prior_category=$(jq -r '.category // ""' <"$cases_file")
if [[ -z "$prior_category" ]]; then
    fail "priming case has empty category — cannot test recall path"
fi
pass "prior case category: $prior_category"

# Cycle 2 — a query that semantically overlaps the prior case so
# CBR retrieval scores it.
recall_input="how should I debug the auth timeout?"
wk_send "$ws" "$recall_input" >/dev/null
pass "drove recall cycle 2 with: $recall_input"

# Find the cycle-log file. Cog writes one daily JSONL.
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/" | head -1)"
require_file "$cycle_log" "cycle-log JSONL"

# Find curator_assembled events. There should be one per cycle —
# we want the second cycle's (the recall cycle).
assembled_count=$(jq -c 'select(.type == "curator_assembled")' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$assembled_count" -lt 2 ]]; then
    fail "expected >= 2 curator_assembled events, got $assembled_count"
fi
pass "$assembled_count curator_assembled event(s) recorded"

# Grab the second one (chronologically later) — that's the recall
# cycle's prompt.
recall_assembled=$(jq -c 'select(.type == "curator_assembled")' <"$cycle_log" | sed -n '2p')
if [[ -z "$recall_assembled" ]]; then
    fail "could not locate recall cycle's curator_assembled event"
fi

# Assert recalled_case_ids contains the prior case's prefix.
recall_ids=$(echo "$recall_assembled" | jq -c '.recalled_case_ids // []')
match=$(echo "$recall_ids" | jq -r --arg p "$prior_short" 'any(. == $p)')
if [[ "$match" != "true" ]]; then
    echo "--- recall cycle's curator_assembled event ---" >&2
    echo "$recall_assembled" | jq . >&2
    fail "recalled_case_ids missing prior case prefix '$prior_short' — case did not reach the agent"
fi
pass "recalled_case_ids contains '$prior_short' (prior case reached the agent)"

# Negative assertion: the very first cycle could not have recalled
# anything (cases.jsonl was empty when its prompt was assembled).
# Its curator_assembled event must have an empty / absent
# recalled_case_ids.
priming_assembled=$(jq -c 'select(.type == "curator_assembled")' <"$cycle_log" | sed -n '1p')
priming_ids=$(echo "$priming_assembled" | jq -c '.recalled_case_ids // []')
priming_len=$(echo "$priming_ids" | jq 'length')
if [[ "$priming_len" -ne 0 ]]; then
    echo "--- priming cycle's curator_assembled event (unexpected IDs) ---" >&2
    echo "$priming_assembled" | jq . >&2
    fail "priming cycle should have zero recalled_case_ids; got $priming_len"
fi
pass "priming cycle correctly recalled zero cases (case base was empty)"

summary

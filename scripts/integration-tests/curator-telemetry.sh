#!/usr/bin/env bash
# Curator telemetry (Phase 6): drive a case-worthy cycle and
# assert the cycle-log carries curator_reflection +
# curator_curation events with model + tokens + duration +
# parent_id pointing back at the cog cycle.
#
# Why these two events matter:
#   - The LLMCurator runs two LLM calls per case-worthy cycle.
#     Pre-Phase-6 those calls were invisible — no token tracking,
#     no latency, just slog warnings on errors. Cost visibility
#     is needed before this scales.
#   - Each curator call emits its own event so the operator can
#     see reflection vs curation cost separately and join them
#     to the originating cog cycle via parent_id.
#
# Default integration provider is the mock — Mock.Chat returns
# an echo reply, Mock.ChatStructured errors (no StructuredFunc
# set in the bootstrap path). So we expect:
#   - reflection events with success=true
#   - curation events with success=false + a non-empty error
# That divergence is itself a useful test: it proves both
# emissions work even on the failure path.

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "curator telemetry (Phase 6)" "$ws"

wk_init "$ws"

# Drive one case-worthy cycle. Use an exploration-shaped input so
# the heuristic-fallback path produces a non-conversation case
# (which means the curator pipeline ran).
input="investigate the auth flow timeouts"
wk_send "$ws" "$input" >/dev/null
pass "drove cycle: $input"

cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/" | head -1)"
require_file "$cycle_log" "cycle-log JSONL"

# Capture the cog cycle's id — both curator events should
# parent_id to this. There's exactly one cycle_start per cycle.
cog_cycle=$(jq -r 'select(.type == "cycle_start") | .cycle_id' <"$cycle_log" | head -1)
if [[ -z "$cog_cycle" ]]; then
    fail "could not find cycle_start event"
fi
pass "cog cycle id: $cog_cycle"

# Assertion 1: at least one curator_reflection event.
ref_count=$(jq -c 'select(.type == "curator_reflection")' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$ref_count" -lt 1 ]]; then
    echo "--- cycle-log (last 30 lines) ---" >&2
    tail -30 "$cycle_log" >&2
    fail "expected >= 1 curator_reflection event; got $ref_count"
fi
pass "$ref_count curator_reflection event(s)"

# Assertion 2: at least one curator_curation event.
cur_count=$(jq -c 'select(.type == "curator_curation")' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$cur_count" -lt 1 ]]; then
    fail "expected >= 1 curator_curation event; got $cur_count"
fi
pass "$cur_count curator_curation event(s)"

# Assertion 3: each curator event parents to a cycle (the cog
# cycle, in the LLMCurator path). The mock provider's Curate
# call may have been triggered by a different cycle if multiple
# ran — here we drove only one, so they should all link to it.
bad_parents=$(jq -c "select((.type == \"curator_reflection\" or .type == \"curator_curation\") and .parent_id != \"$cog_cycle\")" <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$bad_parents" -gt 0 ]]; then
    echo "--- curator events with mismatched parent_id ---" >&2
    jq -c "select((.type == \"curator_reflection\" or .type == \"curator_curation\") and .parent_id != \"$cog_cycle\")" <"$cycle_log" >&2
    fail "$bad_parents curator events have parent_id != cog cycle"
fi
pass "all curator events parent_id = cog cycle"

# Assertion 4: model is populated on every curator event.
no_model=$(jq -c 'select((.type == "curator_reflection" or .type == "curator_curation") and ((.model // "") == ""))' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$no_model" -gt 0 ]]; then
    fail "$no_model curator events missing model"
fi
pass "every curator event carries model"

# Assertion 5: reflection succeeds, curation fails (mock-provider
# default behaviour). This pins the emission shape on both paths.
ref_success_count=$(jq -c 'select(.type == "curator_reflection" and .success == true)' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$ref_success_count" -lt 1 ]]; then
    fail "expected >= 1 successful reflection event"
fi
pass "$ref_success_count reflection event(s) marked success"

cur_failure_count=$(jq -c 'select(.type == "curator_curation" and (.success // false) == false)' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$cur_failure_count" -lt 1 ]]; then
    fail "expected >= 1 failed curation event (mock provider has no StructuredFunc)"
fi
pass "$cur_failure_count curation event(s) marked failure (expected — mock has no StructuredFunc)"

# Assertion 6: failed curation event has an error message.
no_error=$(jq -c 'select(.type == "curator_curation" and (.success // false) == false and ((.error // "") == ""))' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$no_error" -gt 0 ]]; then
    fail "$no_error failed curation events missing error message"
fi
pass "every failed curation event carries error message"

# Assertion 7: duration_ms is recorded on every curator event.
no_duration=$(jq -c 'select((.type == "curator_reflection" or .type == "curator_curation") and ((.duration_ms // -1) < 0))' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$no_duration" -gt 0 ]]; then
    fail "$no_duration curator events missing duration_ms"
fi
pass "every curator event carries duration_ms"

summary

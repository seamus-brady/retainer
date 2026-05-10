#!/usr/bin/env bash
# CBR eval: drive a representative slice of cycle types through
# the cog and audit the resulting cases.jsonl. Verifies the
# memory-and-logging slice of work — specifically:
#
#   1. Conversational cycles (greeting, ack) get
#      intent_class="conversation" and category="" (empty).
#      Stored for audit but NOT surfaced as patterns.
#   2. Non-conversational cycles get intent_class="exploration"
#      (or other non-conversation values) under the heuristic
#      curator AND a non-empty category.
#   3. Intent description is NOT the verbatim user text — that
#      was the rubbish-cases bug.
#   4. Status reflects the cog's actual termination, not just a
#      strict "did the reply address the request" judge prompt.
#
# Runs with the default mock provider (echo). The HeuristicCurator
# fallback is what's being verified — the LLMCurator path runs
# only when a real LLM is configured. The audit document
# `doc/specs/memory-and-logging-audit.md` covers the full design.

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "cbr eval (heuristic curator path)" "$ws"

wk_init "$ws"

# Drive six representative cycles. Mix of: conversational acks,
# non-conversational explorations, longer multi-sentence prompts.
wk_send "$ws" "hi there" >/dev/null
wk_send "$ws" "ok" >/dev/null
wk_send "$ws" "investigate the auth flow timeouts" >/dev/null
wk_send "$ws" "what time is it" >/dev/null
wk_send "$ws" "summarise the architecture document" >/dev/null
wk_send "$ws" "thanks for the help" >/dev/null
pass "drove 6 representative cycles"

# 6 cycles → 6 cases.
cases_file="$ws/data/cases/cases.jsonl"
require_file "$cases_file" "cases.jsonl"
total=$(wc -l <"$cases_file" | tr -d ' ')
if [[ "$total" -ne 6 ]]; then
    fail "expected 6 cases, got $total"
fi
pass "$total cases derived (one per cycle)"

# ---- Assertion 1: conversational cycles get intent_class=conversation ----
# "hi there", "ok", "thanks for the help" are conversational. The
# heuristic matches by ack-prefix; "thanks ..." starts with "thanks".
conv_count=$(jq -c 'select(.problem.intent_class == "conversation")' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$conv_count" -lt 3 ]]; then
    echo "--- intent_class breakdown ---" >&2
    jq -r '.problem.intent_class' <"$cases_file" | sort | uniq -c >&2
    fail "expected >= 3 conversational cases (hi/ok/thanks); got $conv_count"
fi
pass "$conv_count conversational cases tagged intent_class=conversation"

# ---- Assertion 2: conversational cases have EMPTY category ----
# This is the load-bearing fix from the audit: Conversation
# cycles are stored for audit but should NOT surface as
# patterns in CBR retrieval.
conv_with_category=$(jq -c 'select(.problem.intent_class == "conversation" and (.category // "") != "")' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$conv_with_category" -gt 0 ]]; then
    echo "--- conversational cases with non-empty category (BUG) ---" >&2
    jq -c 'select(.problem.intent_class == "conversation" and (.category // "") != "") | {intent_class: .problem.intent_class, category: .category, intent: .problem.intent}' <"$cases_file" >&2
    fail "$conv_with_category conversational cases have non-empty category — should be empty"
fi
pass "all conversational cases have empty category (no pollution)"

# ---- Assertion 3: intent description is NOT verbatim user text ----
# Audit-flagged: prior heuristic copied user_input into intent.
# New shape: intent_description is curator's framing, distinct
# from user_input.
verbatim_count=$(jq -c 'select(.problem.intent != null and .problem.user_input != null and (.problem.intent | ascii_downcase) == (.problem.user_input | ascii_downcase))' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$verbatim_count" -gt 0 ]]; then
    echo "--- cases where intent equals user_input (rubbish pattern) ---" >&2
    jq -c 'select(.problem.intent != null and .problem.user_input != null and (.problem.intent | ascii_downcase) == (.problem.user_input | ascii_downcase)) | {intent: .problem.intent, user_input: .problem.user_input}' <"$cases_file" >&2
    fail "$verbatim_count cases have intent verbatim from user_input — that's the rubbish-cases bug"
fi
pass "no cases have intent equal to verbatim user_input"

# ---- Assertion 4: non-conversational cycles get a category ----
# Exploration cycles with success status should classify
# (DomainKnowledge or similar) — NOT empty category.
nonconv_no_cat=$(jq -c 'select((.problem.intent_class // "") != "conversation" and (.category // "") == "")' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$nonconv_no_cat" -gt 0 ]]; then
    echo "--- non-conversational cases with empty category ---" >&2
    jq -c 'select((.problem.intent_class // "") != "conversation" and (.category // "") == "") | {intent_class: .problem.intent_class, intent: .problem.intent, status: .outcome.status}' <"$cases_file" >&2
    fail "$nonconv_no_cat non-conversational cases have empty category — only Conversation should drop category"
fi
pass "non-conversational cases all have a category"

# ---- Assertion 5: status three-state distribution ----
# All 6 cycles ran with the mock provider, no tool failures.
# Status should be success (cog terminated normally with a reply).
# Conversation cycles: success. Exploration cycles: success.
non_success=$(jq -c 'select(.outcome.status != "success")' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$non_success" -gt 0 ]]; then
    echo "--- non-success cases (heuristic over-failing) ---" >&2
    jq -c 'select(.outcome.status != "success") | {status: .outcome.status, intent: .problem.intent, user_input: .problem.user_input}' <"$cases_file" >&2
    fail "$non_success cases have non-success status; clean mock cycles should all be success"
fi
pass "all clean cycles classified as success (no over-failure)"

# ---- Assertion 6: no Pitfall classification on the audit set ----
# The user's original complaint: "almost all pitfalls". With the
# new pipeline, none of these cycles should be Pitfall —
# Conversation cycles drop category, Exploration cycles get
# DomainKnowledge or Strategy.
pitfall_count=$(jq -c 'select(.category == "pitfall")' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$pitfall_count" -gt 0 ]]; then
    echo "--- Pitfall cases (rubbish pattern) ---" >&2
    jq -c 'select(.category == "pitfall") | {intent_class: .problem.intent_class, intent: .problem.intent, status: .outcome.status}' <"$cases_file" >&2
    fail "$pitfall_count cases classified as Pitfall on a clean-cycle eval set"
fi
pass "no Pitfall cases (was the dominant category before the fix)"

# ---- Optional: human-friendly summary of what the eval saw ----
echo
echo "--- case summary ---"
jq -r '"\(.problem.intent_class // "?")\t\(.category // "(none)")\t\(.outcome.status)\t\(.problem.intent | .[0:60])"' <"$cases_file" | column -t -s $'\t'
echo

summary

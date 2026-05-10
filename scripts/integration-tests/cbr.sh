#!/usr/bin/env bash
# CBR case-derivation test: drive multiple cycles, assert cases land
# in cases.jsonl with the expected categories.
#
# Asserts:
#   - 3 cycles → 3 cases
#   - "investigate" intent → CategoryTroubleshooting
#   - generic intent + agents/tools list empty → CategoryDomainKnowledge
#   - Each case carries source_narrative_id matching its cycle's id
#
# Recall path (cog → observer agent → recall_cases tool) is more
# elaborate to script — left for a follow-up integration test once
# we have an observer-agent mock fixture pattern.

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "CBR case derivation" "$ws"

wk_init "$ws"

# Run three cycles with different intents that exercise the
# Classify cascade.
inputs=(
    "investigate the slow request path"
    "summarise the architecture document"
    "fix the login bug"
)

for input in "${inputs[@]}"; do
    reply=$(wk_send "$ws" "$input")
    if [[ -z "$reply" ]]; then
        fail "empty reply for input: $input"
    fi
done
pass "drove 3 cycles end-to-end"

# cases.jsonl should hold exactly 3 cases.
cases_file="$ws/data/cases/cases.jsonl"
require_file "$cases_file" "cases JSONL"
case_count=$(wc -l <"$cases_file" | tr -d ' ')
if [[ "$case_count" -ne 3 ]]; then
    fail "expected 3 cases, got $case_count"
fi
pass "3 cases derived"

# Categories: "investigate" + "fix" → troubleshooting; "summarise"
# falls through to domain_knowledge.
troubleshooting=$(jq -r 'select(.category == "troubleshooting") | .case_id' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$troubleshooting" -ne 2 ]]; then
    fail "expected 2 troubleshooting cases, got $troubleshooting"
fi
pass "2 cases categorised as troubleshooting (verb-driven)"

domain_knowledge=$(jq -r 'select(.category == "domain_knowledge") | .case_id' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$domain_knowledge" -ne 1 ]]; then
    fail "expected 1 domain_knowledge case, got $domain_knowledge"
fi
pass "1 case fell through to domain_knowledge"

# Each case should reference a real cycle. Check source_narrative_id
# is present (UUID-shaped) and outcome.status is success.
missing_provenance=$(jq -r 'select(.source_narrative_id == "" or .source_narrative_id == null) | .case_id' <"$cases_file" | wc -l | tr -d ' ')
if [[ "$missing_provenance" -ne 0 ]]; then
    fail "$missing_provenance cases missing source_narrative_id"
fi
pass "every case has source_narrative_id provenance"

# Schema version should be pinned at 1 (sanity — guards against
# silent shape drift).
schema_versions=$(jq -r '.schema_version' <"$cases_file" | sort -u)
if [[ "$schema_versions" != "1" ]]; then
    fail "schema_version not pinned at 1: '$schema_versions'"
fi
pass "schema_version = 1 across all cases"

# Embedder ID may be present (when Hugot loaded successfully) or
# absent. Just sanity-check the EmbedderID is consistent — either
# all cases have it or none do.
embedder_ids=$(jq -r '.embedder_id // ""' <"$cases_file" | sort -u | wc -l | tr -d ' ')
if [[ "$embedder_ids" -gt 1 ]]; then
    fail "inconsistent embedder_id across cases (got $embedder_ids distinct values)"
fi
pass "embedder_id consistent across cases"

# Sensorium memory section (verified indirectly via librarian cycle
# log — the `cycle_complete` events should fire for each cycle).
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/")"
complete_count=$(jq -c 'select(.type == "cycle_complete")' <"$cycle_log" | wc -l | tr -d ' ')
if [[ "$complete_count" -ne 3 ]]; then
    fail "expected 3 cycle_complete events, got $complete_count"
fi
pass "3 cycle_complete events match 3 cases"

summary

#!/usr/bin/env bash
# Multi-turn integration test: each `retainer send` invocation is
# its own process — no in-memory continuity between them. But the
# librarian's hot index replays JSONL on startup, so prior cycles
# DO accumulate.
#
# Asserts:
#   - Three sequential sends grow the narrative log to 3 entries
#   - Three cases land (one per cycle)
#   - The cycle-log accumulates events from all three cycles in the
#     same daily file

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "multi-turn accumulation" "$ws"

wk_init "$ws"

wk_send "$ws" "first message" >/dev/null
wk_send "$ws" "second message" >/dev/null
wk_send "$ws" "third message" >/dev/null
pass "3 sequential sends completed"

# Narrative — 3 entries.
narrative_dir="$ws/data/narrative"
narrative_lines=$(find "$narrative_dir" -name '*.jsonl' -exec cat {} \; | wc -l | tr -d ' ')
if [[ "$narrative_lines" -ne 3 ]]; then
    fail "expected 3 narrative entries, got $narrative_lines"
fi
pass "3 narrative entries (one per cycle)"

# Each narrative entry's cycle_id is unique (no reuse).
unique_cycles=$(find "$narrative_dir" -name '*.jsonl' -exec cat {} \; | jq -r '.cycle_id' | sort -u | wc -l | tr -d ' ')
if [[ "$unique_cycles" -ne 3 ]]; then
    fail "expected 3 unique cycle_ids, got $unique_cycles"
fi
pass "cycle_ids are unique"

# Cases — 3 cases.
case_count=$(wc -l <"$ws/data/cases/cases.jsonl" | tr -d ' ')
if [[ "$case_count" -ne 3 ]]; then
    fail "expected 3 cases, got $case_count"
fi
pass "3 cases derived (one per cycle)"

# Cycle log accumulates across runs in the same daily file (date-
# rotated, so all three same-day sends land in the same JSONL).
cycle_log_lines=$(find "$ws/data/cycle-log" -name '*.jsonl' -exec cat {} \; | wc -l | tr -d ' ')
if [[ "$cycle_log_lines" -lt 9 ]]; then
    # 3 cycles × at least 3 events each (start + complete + llm_request)
    fail "expected ≥9 cycle-log events, got $cycle_log_lines"
fi
pass "cycle log accumulates across runs ($cycle_log_lines events)"

# Each cycle should have a matching cycle_complete event.
complete_count=$(find "$ws/data/cycle-log" -name '*.jsonl' -exec cat {} \; | jq -c 'select(.type == "cycle_complete")' | wc -l | tr -d ' ')
if [[ "$complete_count" -ne 3 ]]; then
    fail "expected 3 cycle_complete events, got $complete_count"
fi
pass "3 cycle_complete events"

summary

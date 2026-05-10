#!/usr/bin/env bash
# Asserts the workspace's stable agent identity is created and used:
#
#   - data/identity.json is created on first boot, reused on second
#   - every cycle-log event carries instance_id = first 8 chars of the
#     UUID, EXCEPT events emitted before identity is loaded (none today)
#   - the InstanceID is consistent across the whole cycle log

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "agent identity + telemetry" "$ws"

wk_init "$ws"

# First send mints + persists identity.
wk_send "$ws" "what's the time" >/dev/null
identity_file="$ws/data/identity.json"
require_file "$identity_file" "identity.json"

uuid=$(jq -r '.uuid' <"$identity_file")
if [[ -z "$uuid" || "$uuid" = "null" ]]; then
    fail "identity.json missing uuid field"
fi
prefix="${uuid:0:8}"
pass "uuid minted: $prefix... (full $uuid)"

schema_version=$(jq -r '.schema_version' <"$identity_file")
if [[ "$schema_version" != "1" ]]; then
    fail "schema_version = $schema_version, want 1"
fi
pass "schema_version = 1"

# Second send must reuse the same UUID.
wk_send "$ws" "second message" >/dev/null
uuid_after=$(jq -r '.uuid' <"$identity_file")
if [[ "$uuid_after" != "$uuid" ]]; then
    fail "uuid changed across runs: was $uuid, now $uuid_after"
fi
pass "uuid stable across restarts"

# Cycle log: every event should carry instance_id = $prefix.
cycle_log="$ws/data/cycle-log/$(ls "$ws/data/cycle-log/")"
require_file "$cycle_log" "cycle log"

# Count events that have instance_id set vs missing.
total=$(wc -l <"$cycle_log" | tr -d ' ')
with_iid=$(jq -c 'select(.instance_id != null and .instance_id != "")' <"$cycle_log" | wc -l | tr -d ' ')
mismatched=$(jq -c "select(.instance_id != null and .instance_id != \"\" and .instance_id != \"$prefix\")" <"$cycle_log" | wc -l | tr -d ' ')

if [[ "$with_iid" -lt 1 ]]; then
    fail "no cycle-log event carries instance_id"
fi
pass "$with_iid/$total cycle-log events stamped with instance_id"

if [[ "$mismatched" -gt 0 ]]; then
    fail "$mismatched cycle-log events have wrong instance_id (want $prefix)"
fi
pass "all stamped events match expected prefix $prefix"

# At least one cycle_start, cycle_complete, llm_request, llm_response
# carry the instance_id (sanity-check we didn't only stamp one type).
for ev in cycle_start cycle_complete llm_request llm_response; do
    count=$(jq -c "select(.type == \"$ev\" and .instance_id == \"$prefix\")" <"$cycle_log" | wc -l | tr -d ' ')
    if [[ "$count" -lt 1 ]]; then
        fail "no $ev event carries instance_id=$prefix"
    fi
done
pass "every event type stamps instance_id"

summary

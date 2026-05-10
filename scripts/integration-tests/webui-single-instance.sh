#!/usr/bin/env bash
# Single-instance enforcement test:
#   1. boot `retainer serve` against a fresh workspace
#   2. wait for cog.lock to appear
#   3. attempt a second `retainer send` against the same workspace
#   4. assert exit non-zero with "already running" in stderr

set -euo pipefail

source "$(dirname "$0")/lib.sh"

ws="$(wk_sandbox)"
header "single-instance enforcement" "$ws"

wk_init "$ws"

RETAINER_PROVIDER=mock RETAINER_WORKSPACE="$ws" "$(wk_bin)" serve --workspace "$ws" >/tmp/wk-si-cog.log 2>&1 &
COG_PID=$!

cleanup() {
    # Fire-and-forget: signal but don't block on the child. A
    # bare `wait` here hangs the whole suite if the cog ignores
    # SIGINT for any reason. The kernel reparents + reaps the
    # child when this script exits, and a stalled cog is its
    # own bug to chase — not something an integration test
    # should turn into a hang.
    if [[ -n "${COG_PID:-}" ]]; then
        kill -INT "$COG_PID" 2>/dev/null || true
        # Give it 2s to exit cleanly; SIGKILL if not.
        for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
            kill -0 "$COG_PID" 2>/dev/null || break
            sleep 0.1
        done
        kill -KILL "$COG_PID" 2>/dev/null || true
    fi
    rm -f /tmp/wk-si-cog.log /tmp/wk-si-second.err 2>/dev/null || true
}
trap cleanup EXIT

# Wait for the lockfile to appear.
deadline=$((SECONDS + 15))
while [[ ! -e "$ws/data/cog.lock" ]]; do
    if ! kill -0 "$COG_PID" 2>/dev/null; then
        cat /tmp/wk-si-cog.log >&2
        fail "cog exited before lockfile appeared"
    fi
    [[ $SECONDS -ge $deadline ]] && fail "lockfile never appeared"
    sleep 0.1
done
pass "cog.lock present at $ws/data/cog.lock"

# Attempt a second instance — must fail.
set +e
RETAINER_PROVIDER=mock RETAINER_WORKSPACE="$ws" "$(wk_bin)" send "second" 2>/tmp/wk-si-second.err
second_exit=$?
set -e

if [[ "$second_exit" -eq 0 ]]; then
    fail "second instance should have failed; exited 0 instead"
fi
pass "second instance exited non-zero ($second_exit)"

if ! grep -q "already running" /tmp/wk-si-second.err; then
    echo "second-instance stderr:" >&2
    cat /tmp/wk-si-second.err >&2
    fail "stderr should mention 'already running'"
fi
pass "stderr mentions 'already running'"

# Lockfile body should record the running cog's PID — the body
# is diagnostics-only (flock is the source of truth) but the
# integration value is "operator can `cat cog.lock` to see who
# holds it". Cheap to verify here, not covered by unit tests.
holder_pid=$(cat "$ws/data/cog.lock" 2>/dev/null || echo "")
if [[ -z "$holder_pid" || "$holder_pid" -ne "$COG_PID" ]]; then
    fail "lockfile body = '$holder_pid', want cog pid $COG_PID"
fi
pass "lockfile body records cog pid ($COG_PID)"

# Cleanup-on-shutdown is NOT integration-tested here:
# - flock is FD-bound and dropped by the kernel when the process
#   exits regardless of how — even SIGKILL releases it. Re-Acquire
#   correctness is covered by internal/lockfile/lockfile_test.go.
# - Socket cleanup is covered by internal/cogsock/server_test.go.
# Bounding shutdown timing in bash adds no signal and forced the
# whole suite to hang when a child stalled. The trap teardown
# below handles best-effort cleanup; we don't gate test results
# on its timing.

summary

#!/usr/bin/env bash
# Shared helpers for the integration test scripts. Source from each
# test:
#
#   source "$(dirname "$0")/lib.sh"
#
# Provides:
#   wk_init <sandbox>       — initialise a fresh workspace
#   wk_send <ws> <args...>  — run `retainer send` against the workspace
#   wk_send_script <ws> <script> <message> — run with a mock-script
#   require_file <path> <description>     — assert a file exists + non-empty
#   require_jsonl_event <path> <event>    — assert a cycle-log event type exists
#   require_jq <path> <jq-expr> <expected> — assert a jq query returns expected
#   pass <message>          — log a green PASS line
#   fail <message>          — log a red FAIL line + exit 1
#
# All helpers are pure-bash + jq; no GNU coreutils-specific flags so
# scripts run on macOS + Linux.

set -euo pipefail

# Resolve the binary path. Honours WK_BIN if set (CI sets this to the
# pre-built artifact); otherwise builds into a temp file at first call.
WK_BIN="${WK_BIN:-}"

# Colour codes — disabled when stdout isn't a tty so CI logs stay
# readable in plain-text artifacts.
if [[ -t 1 ]]; then
    GREEN=$'\033[0;32m'
    RED=$'\033[0;31m'
    YELLOW=$'\033[0;33m'
    RESET=$'\033[0m'
else
    GREEN='' RED='' YELLOW='' RESET=''
fi

# Strip provider-side env so the mock provider is selected. Tests
# that want a real provider override these explicitly.
unset MISTRAL_API_KEY ANTHROPIC_API_KEY 2>/dev/null || true
export RETAINER_PROVIDER="mock"

# wk_bin echoes the binary path, building it on first call. The
# build sits in a process-tree temp dir so multiple test scripts in
# one run share it (faster) but distinct test runs don't conflict.
wk_bin() {
    if [[ -n "$WK_BIN" && -x "$WK_BIN" ]]; then
        printf '%s' "$WK_BIN"
        return
    fi
    if [[ -z "${WK_BIN:-}" ]]; then
        local tmp_bin
        tmp_bin="$(mktemp -t wk-itest-bin.XXXXXX)"
        # mktemp leaves an empty file; build over it.
        rm -f "$tmp_bin"
        ( cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)" && go build -o "$tmp_bin" ./cmd/retainer/ ) >&2
        WK_BIN="$tmp_bin"
    fi
    printf '%s' "$WK_BIN"
}

# wk_sandbox creates a fresh workspace directory and prints its path.
wk_sandbox() {
    mktemp -d -t wk-itest.XXXXXX
}

# wk_init runs `retainer init` against the workspace. Idempotent on
# the same sandbox (uses --force).
wk_init() {
    local ws="$1"
    RETAINER_WORKSPACE="$ws" "$(wk_bin)" init --force "$ws" >/dev/null
}

# wk_send runs `retainer send` with the given message. Stdout is
# the agent reply; stderr (logs) is suppressed unless WK_TEST_VERBOSE=1.
# Returns the binary's exit code.
wk_send() {
    local ws="$1"; shift
    if [[ "${WK_TEST_VERBOSE:-0}" = "1" ]]; then
        RETAINER_WORKSPACE="$ws" "$(wk_bin)" send "$@"
    else
        RETAINER_WORKSPACE="$ws" "$(wk_bin)" send "$@" 2>/dev/null
    fi
}

# wk_send_script runs `retainer send` with a scripted mock provider
# replacing the default echo.
wk_send_script() {
    local ws="$1"; local script="$2"; local message="$3"
    if [[ "${WK_TEST_VERBOSE:-0}" = "1" ]]; then
        RETAINER_WORKSPACE="$ws" "$(wk_bin)" send --mock-script "$script" "$message"
    else
        RETAINER_WORKSPACE="$ws" "$(wk_bin)" send --mock-script "$script" "$message" 2>/dev/null
    fi
}

# pass + fail log status lines + bump a global counter.
PASS_COUNT=0
FAIL_COUNT=0

pass() {
    PASS_COUNT=$((PASS_COUNT + 1))
    printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$1"
}

fail() {
    FAIL_COUNT=$((FAIL_COUNT + 1))
    printf '  %s✗%s %s\n' "$RED" "$RESET" "$1"
    exit 1
}

# require_file asserts the path exists and has non-zero size.
require_file() {
    local path="$1"; local desc="$2"
    if [[ -s "$path" ]]; then
        pass "$desc exists ($(wc -l <"$path" | tr -d ' ') lines)"
    else
        fail "$desc missing or empty at $path"
    fi
}

# require_jsonl_event asserts that a cycle-log JSONL contains at least
# one event with .type == $event.
require_jsonl_event() {
    local path="$1"; local event="$2"
    local count
    count=$(jq -c "select(.type == \"$event\")" <"$path" 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$count" -gt 0 ]]; then
        pass "cycle-log has $count $event event(s)"
    else
        fail "cycle-log missing $event events"
    fi
}

# require_jq runs a jq query against a JSONL file (slurp mode) and
# asserts the result equals the expected string.
require_jq() {
    local path="$1"; local expr="$2"; local expected="$3"; local desc="$4"
    local got
    got=$(jq -r "$expr" <"$path" 2>/dev/null || true)
    if [[ "$got" = "$expected" ]]; then
        pass "$desc"
    else
        fail "$desc — expected '$expected', got '$got' (path=$path expr=$expr)"
    fi
}

# section prints a banner separating test scenarios.
section() {
    printf '\n%s── %s ──%s\n' "$YELLOW" "$1" "$RESET"
}

# header prints the test file name + sandbox path so failures are
# easy to reproduce.
header() {
    local title="$1"; local ws="$2"
    printf '\n%s%s%s\n' "$YELLOW" "$title" "$RESET"
    printf '  workspace: %s\n' "$ws"
}

# summary prints the pass/fail counts at end of test.
summary() {
    if [[ "$FAIL_COUNT" -gt 0 ]]; then
        printf '\n%sFAIL%s — %d passed, %d failed\n' "$RED" "$RESET" "$PASS_COUNT" "$FAIL_COUNT"
        exit 1
    fi
    printf '\n%sOK%s — %d passed\n' "$GREEN" "$RESET" "$PASS_COUNT"
}

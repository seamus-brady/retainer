#!/usr/bin/env bash
# Orchestrates the Retainer integration test suite. Runs each
# test script in turn; on any failure, exits non-zero so CI
# catches it.
#
# Usage:
#   scripts/integration-tests/run-all.sh
#
# Env:
#   WK_BIN            — pre-built retainer binary (if unset, scripts
#                       build their own)
#   WK_TEST_VERBOSE   — set to 1 for full stderr (default suppresses)

set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || dirname "$0")"
script_dir="scripts/integration-tests"

# Build once, reuse across all tests — saves ~3s per scenario.
WK_BIN="$(mktemp -t wk-itest-bin.XXXXXX)"
rm -f "$WK_BIN"
echo "Building retainer…"
go build -o "$WK_BIN" ./cmd/retainer/
export WK_BIN

# Build the webui binary so the webui scenario doesn't have to
# rebuild on every standalone invocation.
WK_WEBUI_BIN="$(mktemp -t wk-itest-webui.XXXXXX)"
rm -f "$WK_WEBUI_BIN"
echo "Building retainer-webui…"
go build -o "$WK_WEBUI_BIN" ./cmd/retainer-webui/
export WK_WEBUI_BIN

trap 'rm -f "$WK_BIN" "$WK_WEBUI_BIN"' EXIT

tests=(
    "$script_dir/basic.sh"
    "$script_dir/memory.sh"
    "$script_dir/cbr.sh"
    "$script_dir/multi-turn.sh"
    "$script_dir/remembrancer.sh"
    "$script_dir/identity.sh"
    "$script_dir/webui-single-instance.sh"
    "$script_dir/webui.sh"
    "$script_dir/cbr-eval.sh"
    "$script_dir/cbr-recall.sh"
    "$script_dir/curator-telemetry.sh"
    "$script_dir/captures.sh"
)

failed=0
for t in "${tests[@]}"; do
    if ! bash "$t"; then
        failed=1
    fi
done

if [[ "$failed" -eq 1 ]]; then
    echo
    echo "INTEGRATION TESTS FAILED"
    exit 1
fi

echo
echo "All integration tests passed."

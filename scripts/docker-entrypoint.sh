#!/usr/bin/env sh
# Default entrypoint inside the Docker image.
#
# Boots `retainer serve` in the background, waits for the cog
# socket to appear, then exec's `retainer-webui` in the
# foreground so its lifecycle drives the container's lifecycle
# (Ctrl-C / docker stop tears the whole thing down cleanly).
#
# The naive `retainer serve & retainer-webui ...` form races —
# webui exits with "no cog socket at /workspace/data/cog.sock"
# before the cog has bound the socket. This script polls the
# socket path with a 30s timeout.

set -eu

WORKSPACE="${RETAINER_WORKSPACE:-/workspace}"
ADDR="${RETAINER_WEBUI_ADDR:-0.0.0.0:7878}"

# Pin the cog socket to a container-local path (NOT inside the
# bind-mounted workspace). Docker on macOS uses virtiofs for bind
# mounts and virtiofs doesn't allow Unix-socket binds — the
# socket has to live on a normal filesystem inside the container.
# Both retainer + retainer-webui pick this up via RETAINER_COG_SOCKET.
SOCK="${RETAINER_COG_SOCKET:-/tmp/retainer-cog.sock}"
export RETAINER_COG_SOCKET="$SOCK"

# Seed the workspace if it isn't initialised. `retainer init`
# is idempotent against existing files — without --force it
# refuses to clobber, so we check for config.toml and skip when
# the workspace already has it. This lets operators bind-mount
# an empty host dir on first run without an extra step.
if [ ! -f "$WORKSPACE/config/config.toml" ]; then
    echo "retainer-docker: workspace not initialised, running retainer init..."
    retainer init "$WORKSPACE"
fi

# Start the cog in the background.
echo "retainer-docker: starting cog (workspace=$WORKSPACE)..."
retainer serve --workspace "$WORKSPACE" &
COG_PID=$!

# Trap so SIGTERM / SIGINT propagates to the cog.
trap 'kill "$COG_PID" 2>/dev/null; wait "$COG_PID" 2>/dev/null; exit 0' INT TERM

# Poll for the socket. 30 second budget — cog usually binds in
# under a second, but the embedder cache + librarian replay can
# stretch it on first run.
deadline=$(( $(date +%s) + 30 ))
while [ ! -S "$SOCK" ]; do
    if ! kill -0 "$COG_PID" 2>/dev/null; then
        echo "retainer-docker: cog exited before socket appeared" >&2
        wait "$COG_PID" 2>/dev/null
        exit 1
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "retainer-docker: timed out waiting for cog socket at $SOCK" >&2
        kill "$COG_PID" 2>/dev/null
        exit 1
    fi
    sleep 0.2
done
echo "retainer-docker: cog socket ready, starting webui on $ADDR..."

# Foreground webui — tini reaps the cog when this exits.
exec retainer-webui --workspace "$WORKSPACE" --addr "$ADDR"

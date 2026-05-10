# Scripts

Every script lives in `scripts/`, is bash, runs `set -euo pipefail`, and `cd`s to the repo root automatically — invoke from anywhere.

## Build, lint, test

### `scripts/build`

Builds the cog binary at `bin/retainer`.

```sh
scripts/build
```

Equivalent to `go build -o bin/retainer ./cmd/retainer`. Does not build `retainer-webui` — `scripts/retainer-with-webui` and the integration-tests runner build that on demand.

### `scripts/clean`

Removes `bin/`. No other side effects.

### `scripts/vet`

Runs `go vet ./...`. Silent on success.

### `scripts/test`

Runs the unit-test suite: `go test ./...`. CI runs the same set with `-race -v` (see `.github/workflows/go.yml`). For a single package: `go test ./internal/<pkg>/`. For a single test: `go test ./internal/<pkg>/ -run TestName -v`.

### `scripts/integration-tests/run-all.sh`

End-to-end suite. Builds both binaries once into temp paths, then runs ~12 bash scenarios that drive the production cog through `retainer send` against the **mock provider**, asserting on the JSONL artifacts the workspace writes out.

```sh
scripts/integration-tests/run-all.sh
WK_TEST_VERBOSE=1 scripts/integration-tests/run-all.sh   # surface stderr
```

Per-scenario invocation: `bash scripts/integration-tests/<name>.sh`. Available scenarios: `basic`, `memory`, `cbr`, `multi-turn`, `remembrancer`, `identity`, `webui-single-instance`, `webui`, `cbr-eval`, `cbr-recall`, `curator-telemetry`, `captures`. Shared helpers in `scripts/integration-tests/lib.sh`.

The runner sets `WK_BIN` and `WK_WEBUI_BIN` to its temp builds and exports them so individual scenarios reuse the binaries instead of re-compiling. The lib unsets `ANTHROPIC_API_KEY` / `MISTRAL_API_KEY` and forces `RETAINER_PROVIDER=mock` so tests never make a network call.

## Run

### `scripts/run`

`go run ./cmd/retainer "$@"` — boots the TUI against `$HOME/retainer` (or `$RETAINER_WORKSPACE` if set). All `retainer` flags pass through.

```sh
scripts/run                              # TUI against the default workspace
scripts/run --workspace /tmp/ws          # TUI against a specific workspace
scripts/run send "what's the weather?"   # one-shot send
```

### `scripts/run-example`

Boots `retainer` against the embedded example config (`internal/example/config.example.toml`) in a **persistent** sandbox at `$TMPDIR/retainer-example`. Logs accumulate across runs so you can read them. Wipe by hand:

```sh
rm -rf "${TMPDIR:-/tmp}/retainer-example"
```

Use this when you want to iterate on the example config and watch a stable workspace evolve.

### `scripts/run-example-fresh`

Same as `run-example`, but mints a fresh `mktemp` sandbox per invocation. Each run gets a clean librarian, cycle log, and identity. Path is printed to stderr at startup; sandbox is left on disk after exit so you can inspect it.

```sh
scripts/run-example-fresh
# sandbox: /var/folders/.../retainer-example.Mmrfqr
```

Use this for first-cycle bootstrap testing or when comparing identical runs side-by-side.

### `scripts/retainer-with-webui`

Launcher that brings up `retainer serve` (the cog, headless) and `retainer-webui` together. Polls for the cog Unix socket, execs the webui once it appears, and tears both down on Ctrl-C.

```sh
scripts/retainer-with-webui                              # $HOME/retainer, 127.0.0.1:7878
scripts/retainer-with-webui --workspace ~/work
scripts/retainer-with-webui --addr 0.0.0.0:8080
scripts/retainer-with-webui --ephemeral --open          # throwaway workspace, open browser
```

Workspace selection (first match wins):
1. `--workspace PATH`
2. `$RETAINER_WORKSPACE`
3. `--ephemeral` — throwaway temp dir, **not** auto-deleted (so cycle logs are inspectable post-mortem)
4. `$HOME/retainer` (persistent default)

Failure modes the script handles:
- Cog exits before its socket appears → script exits 1 with the cog's stderr.
- Webui can't reach the socket → script exits 1.
- Either child dies later → tears down the survivor and exits with the dead child's exit code.

If `retainer` / `retainer-webui` aren't on `$PATH`, the script auto-builds them from the current checkout into a temp dir (cleaned up on exit). Override with `--cog-bin PATH` / `--webui-bin PATH` or `RETAINER_COG_BIN` / `RETAINER_WEBUI_BIN`.

### `scripts/docker-run`

Wraps `docker build` + `docker run`. Loads `.env` from the repo root for `ANTHROPIC_API_KEY`, bind-mounts `$HOME/retainer` as the workspace, exposes the webui at `http://localhost:7878/`.

```sh
scripts/docker-run                       # foreground, attached
scripts/docker-run -d                    # detached
scripts/docker-run --workspace ~/work    # custom workspace
scripts/docker-run --port 8080
scripts/docker-run --rebuild             # rebuild image first
scripts/docker-run --env-file ./other-env
```

Stop a detached container with `docker stop retainer`. The image is named `retainer:latest` by default (override with `--image NAME`); the container is `retainer` (override with `--name`).

If `.env` is missing, the script falls back to whatever `ANTHROPIC_API_KEY` is exported in the calling shell. Without either, the container still boots — provider falls back to mock.

## Container internals

### `scripts/docker-entrypoint.sh`

Default `ENTRYPOINT` inside the Retainer Docker image. Not invoked directly — Docker runs it. Behaviour:

1. Seeds the workspace via `retainer init` if `<workspace>/config/config.toml` is missing.
2. Pins the cog socket to `/tmp/retainer-cog.sock` (container-local — virtiofs can't host Unix sockets on macOS).
3. Starts `retainer serve` in the background.
4. Polls for the socket, 30s budget.
5. Execs `retainer-webui` in the foreground so its lifecycle drives the container's lifecycle.

Honours `RETAINER_WORKSPACE` (default `/workspace`) and `RETAINER_WEBUI_ADDR` (default `0.0.0.0:7878`).

## Notes for macOS

bsd-mktemp's `-t prefix` mode treats the prefix literally and does **not** substitute the X-template, so `mktemp -d -t foo.XXXXXX` produces `/var/folders/.../foo.XXXXXX.<rand>` on macOS but `foo.<rand>` on Linux. The portable shape — used by `scripts/retainer-with-webui` and `scripts/run-example-fresh` — is to hand `mktemp -d` a fully-resolved template under `$TMPDIR`:

```sh
tmp="${TMPDIR:-/tmp}"
mktemp -d "${tmp%/}/prefix.XXXXXX"
```

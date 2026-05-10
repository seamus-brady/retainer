# Retainer — multi-stage Go build → minimal runtime image.
#
# Stage 1 builds both binaries from source. Stage 2 ships them
# alongside CA certificates (for the Anthropic + DuckDuckGo HTTPS
# calls) and a writable workspace volume mount-point.
#
# Build:
#   docker build -t retainer:latest .
#
# Run (webui at http://localhost:7878 — default; the entrypoint
# starts the cog, waits for its socket, then exec's the webui):
#   docker run -d --rm \
#     -e ANTHROPIC_API_KEY \
#     -v ~/retainer:/workspace \
#     -p 7878:7878 \
#     retainer:latest
#
# Run (interactive TUI; rare for Docker but supported):
#   docker run -it --rm \
#     -e ANTHROPIC_API_KEY \
#     -v ~/retainer:/workspace \
#     --entrypoint retainer \
#     retainer:latest
#
# Without ANTHROPIC_API_KEY the binary still boots — the LLM
# provider falls back to the mock (echoes inputs). Useful for
# verifying the harness without spending tokens.

# ---- build stage ---------------------------------------------------

FROM golang:1.26-alpine AS build

WORKDIR /src
RUN apk add --no-cache git

# Cache modules in their own layer so source-only edits don't
# bust the dependency cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binaries — no glibc dependency, drop into Alpine clean.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/retainer       ./cmd/retainer && \
    go build -trimpath -ldflags="-s -w" -o /out/retainer-webui ./cmd/retainer-webui

# ---- runtime stage -------------------------------------------------

FROM alpine:3.20

# CA certs for HTTPS to Anthropic + DuckDuckGo. tini for clean
# signal handling so Ctrl-C / docker stop unwinds the cog properly.
RUN apk add --no-cache ca-certificates tini

# Non-root runtime — the workspace dir is bind-mounted; the binary
# never writes outside it.
RUN addgroup -S retainer && adduser -S -G retainer retainer

COPY --from=build /out/retainer       /usr/local/bin/retainer
COPY --from=build /out/retainer-webui /usr/local/bin/retainer-webui
COPY scripts/docker-entrypoint.sh    /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Default workspace location inside the container — operators bind-
# mount their host dir here. RETAINER_WORKSPACE picks it up.
ENV RETAINER_WORKSPACE=/workspace
RUN mkdir -p /workspace && chown -R retainer:retainer /workspace
VOLUME ["/workspace"]

# Webui port — the default entrypoint starts the cog, waits for
# its socket, then exec's `retainer-webui` so this port becomes
# the only public surface.
EXPOSE 7878

USER retainer
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
CMD []

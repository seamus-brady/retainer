// Command retainer-webui is a local browser chat client for the
// Retainer cog. It connects to the cog's Unix socket at
// `<workspace>/data/cog.sock`, serves a chat HTML page on
// 127.0.0.1:<port>, and bridges browser events ↔ socket frames.
//
// Architecture: webui is a CLIENT of the cog, not a parent. The
// cog runs as the main `retainer` binary (TUI + cog in one
// process). This binary requires a running cog at the workspace
// — if the socket isn't there, the binary exits with a clear
// "no cog detected, run `retainer` first" message.
//
// Wire on cog side: `internal/cogsock` ndJSON envelopes.
// Wire on browser side: SSE for cog→browser stream + POST for
// user input + GET / for the static page. No WebSocket dep.
//
// Local-only by default: bind defaults to 127.0.0.1:7878. Override
// with --addr if you really need to expose it; that's the only
// auth boundary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/seamus-brady/retainer/internal/paths"
)

const (
	defaultAddr = "127.0.0.1:7878"
)

func main() {
	addr := flag.String("addr", defaultAddr, "HTTP listen address (host:port)")
	workspace := flag.String("workspace", "", "Retainer workspace dir (default: $RETAINER_WORKSPACE or $HOME/retainer)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dirs, err := paths.Resolve(*workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retainer-webui: %v\n", err)
		os.Exit(1)
	}
	socketPath := filepath.Join(dirs.Data, "cog.sock")
	if override := os.Getenv("RETAINER_COG_SOCKET"); override != "" {
		socketPath = override
	}

	if _, err := os.Stat(socketPath); err != nil {
		fmt.Fprintf(os.Stderr, "retainer-webui: no cog socket at %s\n", socketPath)
		fmt.Fprintf(os.Stderr, "retainer-webui: start `retainer` against this workspace first.\n")
		os.Exit(1)
	}
	logger.Info("retainer-webui: cog socket detected", "path", socketPath)

	ctx, cancel := signalContext()
	defer cancel()

	client := newCogClient(socketPath, logger.With("component", "cogclient"))
	if err := client.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "retainer-webui: cog client start failed: %v\n", err)
		os.Exit(1)
	}
	defer client.Stop()

	srv := newServer(client, dirs.Data, logger.With("component", "http"))

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retainer-webui: listen %s: %v\n", *addr, err)
		os.Exit(1)
	}
	logger.Info("retainer-webui: serving", "addr", listener.Addr().String(), "url", "http://"+listener.Addr().String()+"/")
	// Copy-pastable path output — mirrors `retainer serve` so
	// operators don't have to grep slog to find their workspace.
	fmt.Fprintln(os.Stderr, "retainer-webui: ready")
	fmt.Fprintf(os.Stderr, "  workspace: %s\n", dirs.Workspace)
	fmt.Fprintf(os.Stderr, "  data:      %s\n", dirs.Data)
	fmt.Fprintf(os.Stderr, "  socket:    %s\n", socketPath)
	fmt.Fprintf(os.Stderr, "  url:       http://%s/\n", listener.Addr().String())

	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("retainer-webui: serve failed", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("retainer-webui: shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpServer.Shutdown(shutCtx)
}

// signalContext returns a context cancelled on SIGINT or SIGTERM.
// Used by the binary's main loop so Ctrl-C cleanly closes both
// the HTTP server and the cog socket connection.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

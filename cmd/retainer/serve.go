package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// runServe boots the cog headless — same world as the TUI, just
// without the Bubble Tea program. Used by:
//
//   - `retainer-webui` consumers: the launcher script starts
//     the cog in serve mode, then fires up the webui that connects
//     to the cog's Unix socket.
//   - integration tests that need a long-running cog without a
//     TTY-attached TUI.
//
// Blocks on SIGINT / SIGTERM. The supervisor handles all the
// actor restart logic; this is just a lifecycle shell that
// constructs the world and waits for shutdown.
func runServe(args []string) error {
	fs := flag.NewFlagSet("retainer serve", flag.ContinueOnError)
	var (
		workspace  string
		configPath string
	)
	fs.StringVar(&workspace, "workspace", "", "workspace directory; overrides $RETAINER_WORKSPACE and the default $HOME/retainer")
	fs.StringVar(&workspace, "w", "", "alias for --workspace")
	fs.StringVar(&configPath, "config", "", "path to config TOML; overrides the workspace's config/config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	w, err := bootstrap(workspace, configPath, "")
	if err != nil {
		return err
	}
	defer w.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for shutdown signals — Ctrl-C in a foreground
	// terminal, SIGTERM from systemd / launchd / a launcher
	// script.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	supDone := make(chan error, 1)
	go func() { supDone <- w.supervisor.Run(ctx) }()

	w.logger.Info("retainer serve: ready",
		"workspace", w.dirs.Workspace,
		"socket", w.dirs.Data+"/cog.sock",
	)
	// Print paths in a copy-pastable shape so operators (and
	// integration tests) can grep state files without spelunking
	// the slog output.
	fmt.Fprintln(os.Stderr, "retainer: cog ready")
	fmt.Fprintf(os.Stderr, "  workspace: %s\n", w.dirs.Workspace)
	fmt.Fprintf(os.Stderr, "  data:      %s\n", w.dirs.Data)
	fmt.Fprintf(os.Stderr, "  socket:    %s\n", filepath.Join(w.dirs.Data, "cog.sock"))

	select {
	case <-sig:
		w.logger.Info("retainer serve: signal received, shutting down")
	case err := <-supDone:
		// Supervisor exited on its own — usually means a
		// permanent actor blew its restart budget. Surface for
		// visibility.
		if err != nil {
			w.logger.Error("retainer serve: supervisor exited", "err", err)
			cancel()
			return err
		}
	}

	cancel()
	<-supDone
	return nil
}

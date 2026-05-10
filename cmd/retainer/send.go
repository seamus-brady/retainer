package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/seamus-brady/retainer/internal/cog"
)

// runSend is the one-shot CLI: take a message, run one cycle through
// the production cog setup, print the reply, drain the archivist,
// exit. Used by integration tests + ad-hoc operator queries.
//
// Usage:
//
//	retainer send [-w workspace] [--config path] [--mock-script path]
//	                [--drain ms] [-m message]    "message text"
//
// The message can come from the trailing positional arg or from
// `-m`. Empty input is rejected; use the TUI for interactive
// sessions.
//
// Exit codes:
//
//	0  — cycle completed normally; reply printed to stdout
//	1  — bootstrap or cycle error; details on stderr
//	2  — input was refused by the policy gate (reply still printed,
//	     but the operator/test driver may want to distinguish refusals
//	     from successes for assertions)
func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	var (
		workspace      string
		configPath     string
		mockScriptPath string
		drainMs        int
		message        string
	)
	fs.StringVar(&workspace, "workspace", "", "workspace directory; overrides $RETAINER_WORKSPACE and the default $HOME/retainer")
	fs.StringVar(&workspace, "w", "", "alias for --workspace")
	fs.StringVar(&configPath, "config", "", "path to config TOML; overrides the workspace's config/config.toml")
	fs.StringVar(&mockScriptPath, "mock-script", "", "path to JSON file scripting the mock provider's responses (integration tests)")
	fs.IntVar(&drainMs, "drain", 500, "milliseconds to wait after the reply for the archivist + librarian to finish writing JSONL")
	fs.StringVar(&message, "m", "", "message text (alternative to a positional argument)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if message == "" {
		// Fall back to the trailing positional args joined with
		// spaces — operator-friendly for `retainer send hello there`.
		message = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if message == "" {
		return fmt.Errorf("send: empty message; provide via -m or as positional arg")
	}

	w, err := bootstrap(workspace, configPath, mockScriptPath)
	if err != nil {
		return err
	}
	defer w.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	supDone := make(chan error, 1)
	go func() { supDone <- w.supervisor.Run(ctx) }()

	// Submit + wait for reply. The cycle log + narrative + case
	// derivation all hang off this — the post-cycle work happens
	// AFTER the reply lands on the channel.
	reply := <-w.cog.Submit(ctx, message)

	// Print the reply to stdout. The operator (or test driver) sees
	// it directly; stderr stays clean for log lines.
	switch {
	case reply.Err != nil:
		fmt.Fprintf(os.Stderr, "send: cycle error: %v\n", reply.Err)
		shutdown(cancel, supDone)
		return reply.Err
	case reply.Text != "":
		fmt.Println(reply.Text)
	default:
		fmt.Println("(empty reply)")
	}

	// Drain window — give the archivist + librarian time to flush
	// CycleComplete → narrative + case JSONL writes before we
	// shut down. The archivist runs case-derivation in its own
	// goroutine off cog.Record, so the sleep is the simplest sync
	// barrier without piping a "drained" signal back.
	if drainMs > 0 {
		time.Sleep(time.Duration(drainMs) * time.Millisecond)
	}
	// One synchronous query against the librarian as a final
	// barrier: serialises behind any pending writes the archivist
	// may have just enqueued.
	_ = w.librarian.CaseCount()

	shutdown(cancel, supDone)

	// Exit 2 on policy refusal so test scripts can distinguish.
	if reply.Kind == cog.ReplyKindRefusal {
		os.Exit(2)
	}
	return nil
}

// shutdown cancels the context and waits up to 5 seconds for the
// supervisor to drain. Bounded so a stuck actor doesn't hang the
// process indefinitely; an unbounded wait would defeat the purpose
// of a one-shot CLI.
func shutdown(cancel context.CancelFunc, supDone <-chan error) {
	cancel()
	select {
	case <-supDone:
	case <-time.After(5 * time.Second):
		fmt.Fprintln(os.Stderr, "send: shutdown timeout (5s); some actors didn't drain")
	}
}

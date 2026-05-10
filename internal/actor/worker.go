package actor

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Spawn runs work in a new goroutine and delivers the result via the
// callback. Panics in work are recovered and surfaced as errors so the
// parent's inbox sees a normal error message rather than a goroutine
// crash. The full panic stack trace is captured via runtime/debug.Stack
// and logged via slog at ERROR level — without this, "actor: worker
// panic: nil pointer dereference" gives the operator nothing to debug
// from. The stack also ships in the returned error's message so the
// error landing in the cycle log carries enough context to pinpoint
// the panic without correlating across log streams.
//
// The deliver callback must be safe to call from a different goroutine — it
// typically forwards to the parent actor via a channel send.
func Spawn[T any](ctx context.Context, work func(context.Context) (T, error), deliver func(T, error)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				slog.Error("actor: worker panic recovered",
					"recover", fmt.Sprintf("%v", r),
					"stack", string(stack),
				)
				var zero T
				// Carry a one-line summary plus the first ~30 lines of
				// stack into the error string so cycle-log consumers
				// see the panic origin without having to grep slog.
				deliver(zero, fmt.Errorf("actor: worker panic: %v\n%s", r, truncateStack(stack, 30)))
			}
		}()
		result, err := work(ctx)
		deliver(result, err)
	}()
}

// truncateStack keeps the first n stack-frame lines so cycle-log
// strings stay readable while still carrying the location of the
// panic. Full stack always lands in slog regardless.
func truncateStack(stack []byte, n int) string {
	if len(stack) == 0 {
		return ""
	}
	count := 0
	for i, b := range stack {
		if b == '\n' {
			count++
			if count >= n {
				return string(stack[:i]) + "\n…(truncated; full stack in slog)"
			}
		}
	}
	return string(stack)
}

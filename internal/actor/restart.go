package actor

import (
	"context"
	"fmt"
	"time"
)

// Strategy selects how an actor's exit is handled by Run.
type Strategy int

const (
	// Permanent always restarts the actor on exit (clean or crash).
	Permanent Strategy = iota
	// Transient restarts only on crash; clean exit terminates the actor.
	Transient
	// Temporary never restarts.
	Temporary
)

// MaxRestartIntensity caps how often an actor can restart before it is
// declared failed. Bursts == 0 disables the limit. When active, more than
// Bursts restarts within Window terminates Run with an error wrapping the
// most recent run-error.
type MaxRestartIntensity struct {
	Bursts int
	Window time.Duration
}

// Run executes work in a loop per the restart strategy. Returns when the
// strategy decides not to restart, when ctx is cancelled, or when
// MaxRestartIntensity is exceeded. Panics in work are recovered and treated
// as errors.
func Run(ctx context.Context, strategy Strategy, intensity MaxRestartIntensity, work func(context.Context) error) error {
	var restarts []time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		runErr := runOnce(ctx, work)

		if err := ctx.Err(); err != nil {
			return err
		}

		switch strategy {
		case Temporary:
			return runErr
		case Transient:
			if runErr == nil {
				return nil
			}
		case Permanent:
			// always restart; fall through
		}

		if intensity.Bursts > 0 {
			now := time.Now()
			cutoff := now.Add(-intensity.Window)
			i := 0
			for ; i < len(restarts); i++ {
				if restarts[i].After(cutoff) {
					break
				}
			}
			restarts = restarts[i:]
			restarts = append(restarts, now)
			if len(restarts) > intensity.Bursts {
				return fmt.Errorf("actor: restart intensity exceeded (%d in %s, last err: %w)",
					len(restarts), intensity.Window, runErr)
			}
		}
	}
}

func runOnce(ctx context.Context, work func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("actor: panic: %v", r)
		}
	}()
	return work(ctx)
}

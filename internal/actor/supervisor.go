package actor

import (
	"context"
	"errors"
	"fmt"
)

// Spec describes one supervised child.
type Spec struct {
	Name      string
	Run       func(context.Context) error
	Restart   Strategy
	Intensity MaxRestartIntensity
}

// Supervisor runs a fixed set of children under a one-for-one strategy:
// each child's restart policy is independent, and a child terminating per
// its policy does not affect siblings. The supervisor terminates when ctx
// is cancelled or every child has permanently terminated.
type Supervisor struct {
	specs []Spec
}

func NewSupervisor(specs ...Spec) *Supervisor {
	return &Supervisor{specs: specs}
}

// Run blocks until the supervisor terminates. Returns nil if all children
// exited cleanly (or were cancelled by ctx); otherwise wraps the first
// non-cancellation error from a terminated child.
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.specs) == 0 {
		return nil
	}
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(s.specs))
	for _, spec := range s.specs {
		spec := spec
		go func() {
			err := Run(ctx, spec.Restart, spec.Intensity, spec.Run)
			results <- result{name: spec.Name, err: err}
		}()
	}
	var firstErr error
	for i := 0; i < len(s.specs); i++ {
		r := <-results
		if firstErr == nil && r.err != nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) {
			firstErr = fmt.Errorf("actor: child %q terminated: %w", r.name, r.err)
		}
	}
	return firstErr
}

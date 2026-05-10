package actor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisor_Empty(t *testing.T) {
	if err := NewSupervisor().Run(context.Background()); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestSupervisor_TransientCleanExits(t *testing.T) {
	var a, b atomic.Int32
	sup := NewSupervisor(
		Spec{Name: "a", Restart: Transient, Run: func(ctx context.Context) error {
			a.Add(1)
			return nil
		}},
		Spec{Name: "b", Restart: Transient, Run: func(ctx context.Context) error {
			b.Add(1)
			return nil
		}},
	)
	if err := sup.Run(context.Background()); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("a=%d b=%d", a.Load(), b.Load())
	}
}

func TestSupervisor_PermanentChildRestartsOnCrash(t *testing.T) {
	var calls atomic.Int32
	sup := NewSupervisor(
		Spec{Name: "p", Restart: Permanent, Run: func(ctx context.Context) error {
			n := calls.Add(1)
			if n < 3 {
				return errStop
			}
			<-ctx.Done()
			return ctx.Err()
		}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v, want nil (cancellation should not surface)", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not return after cancel")
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("expected at least 3 calls, got %d", got)
	}
}

func TestSupervisor_TemporaryFailureSurfaces(t *testing.T) {
	sentinel := errors.New("permanent exit reason")
	sup := NewSupervisor(
		Spec{Name: "t", Restart: Temporary, Run: func(ctx context.Context) error {
			return sentinel
		}},
	)
	err := sup.Run(context.Background())
	if err == nil {
		t.Fatal("expected error from terminated Temporary child")
	}
	if !strings.Contains(err.Error(), "permanent exit reason") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), `child "t"`) {
		t.Fatalf("err did not name child: %v", err)
	}
}

func TestSupervisor_OneForOneIsolation(t *testing.T) {
	// One Temporary child fails immediately; another Permanent child must
	// keep running until ctx is cancelled.
	sentinel := errors.New("dies fast")
	var permanentCalls atomic.Int32
	sup := NewSupervisor(
		Spec{Name: "dies", Restart: Temporary, Run: func(ctx context.Context) error {
			return sentinel
		}},
		Spec{Name: "lives", Restart: Permanent, Run: func(ctx context.Context) error {
			permanentCalls.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Give "lives" time to start.
	time.Sleep(30 * time.Millisecond)
	if permanentCalls.Load() == 0 {
		t.Fatal("permanent child did not start")
	}
	cancel()

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "dies fast") {
		t.Fatalf("err = %v, want surfaced 'dies fast'", err)
	}
}

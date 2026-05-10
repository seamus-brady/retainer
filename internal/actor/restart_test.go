package actor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var errStop = errors.New("stop")

func TestRun_Permanent_RestartsUntilCancel(t *testing.T) {
	var calls atomic.Int32
	work := func(ctx context.Context) error {
		calls.Add(1)
		return errStop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := Run(ctx, Permanent, MaxRestartIntensity{}, work)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected multiple restarts, got %d calls", calls.Load())
	}
}

func TestRun_Transient_CleanExit(t *testing.T) {
	var calls atomic.Int32
	work := func(ctx context.Context) error {
		calls.Add(1)
		return nil
	}
	err := Run(context.Background(), Transient, MaxRestartIntensity{}, work)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
}

func TestRun_Transient_RestartsOnCrash(t *testing.T) {
	var calls atomic.Int32
	work := func(ctx context.Context) error {
		n := calls.Add(1)
		if n < 3 {
			return errStop
		}
		return nil
	}
	err := Run(context.Background(), Transient, MaxRestartIntensity{}, work)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 calls, got %d", got)
	}
}

func TestRun_Temporary_NeverRestarts(t *testing.T) {
	var calls atomic.Int32
	work := func(ctx context.Context) error {
		calls.Add(1)
		return errStop
	}
	err := Run(context.Background(), Temporary, MaxRestartIntensity{}, work)
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want errStop", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call, got %d", got)
	}
}

func TestRun_PanicRecovered(t *testing.T) {
	work := func(ctx context.Context) error {
		panic("kapow")
	}
	err := Run(context.Background(), Temporary, MaxRestartIntensity{}, work)
	if err == nil {
		t.Fatal("expected error from panic")
	}
	if !strings.Contains(err.Error(), "kapow") {
		t.Fatalf("err = %v, want containing 'kapow'", err)
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	work := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Permanent, MaxRestartIntensity{}, work) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_RateLimitExceeded(t *testing.T) {
	var calls atomic.Int32
	work := func(ctx context.Context) error {
		calls.Add(1)
		return errStop
	}
	intensity := MaxRestartIntensity{Bursts: 3, Window: time.Second}
	err := Run(context.Background(), Permanent, intensity, work)
	if err == nil || !strings.Contains(err.Error(), "restart intensity exceeded") {
		t.Fatalf("err = %v, want intensity-exceeded", err)
	}
	if got := calls.Load(); got < 4 {
		t.Fatalf("expected at least 4 calls before exhaust, got %d", got)
	}
}

func TestRun_RateLimitWindowSliding(t *testing.T) {
	// With a short window, restarts that age out shouldn't count toward the
	// burst limit.
	var calls atomic.Int32
	work := func(ctx context.Context) error {
		n := calls.Add(1)
		if n >= 5 {
			return nil // clean exit eventually under Transient
		}
		time.Sleep(15 * time.Millisecond)
		return errStop
	}
	intensity := MaxRestartIntensity{Bursts: 2, Window: 10 * time.Millisecond}
	err := Run(context.Background(), Transient, intensity, work)
	if err != nil {
		t.Fatalf("err = %v, want nil (window should slide)", err)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("expected 5 calls, got %d", got)
	}
}

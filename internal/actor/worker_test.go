package actor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSpawn_Success(t *testing.T) {
	type result struct {
		v   int
		err error
	}
	out := make(chan result, 1)
	Spawn(context.Background(),
		func(ctx context.Context) (int, error) { return 42, nil },
		func(v int, err error) { out <- result{v, err} },
	)
	select {
	case r := <-out:
		if r.v != 42 || r.err != nil {
			t.Fatalf("got (%d, %v), want (42, nil)", r.v, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Spawn did not deliver")
	}
}

func TestSpawn_Error(t *testing.T) {
	sentinel := errors.New("expected")
	type result struct {
		v   int
		err error
	}
	out := make(chan result, 1)
	Spawn(context.Background(),
		func(ctx context.Context) (int, error) { return 0, sentinel },
		func(v int, err error) { out <- result{v, err} },
	)
	r := <-out
	if !errors.Is(r.err, sentinel) {
		t.Fatalf("err = %v, want %v", r.err, sentinel)
	}
}

func TestSpawn_PanicRecovered(t *testing.T) {
	type result struct {
		v   int
		err error
	}
	out := make(chan result, 1)
	Spawn(context.Background(),
		func(ctx context.Context) (int, error) { panic("kapow") },
		func(v int, err error) { out <- result{v, err} },
	)
	r := <-out
	if r.err == nil {
		t.Fatal("expected error from panic, got nil")
	}
	if !strings.Contains(r.err.Error(), "kapow") {
		t.Fatalf("err did not contain 'kapow': %v", r.err)
	}
}

func TestSpawn_PanicErrorCarriesStackTrace(t *testing.T) {
	// Pin the diagnostic-improvement contract: a recovered panic
	// must carry enough stack info in the returned error that
	// the operator can pinpoint the file:line without grepping
	// slog. Without this, "actor: worker panic: nil pointer
	// dereference" gives nothing to debug from.
	type result struct {
		v   int
		err error
	}
	out := make(chan result, 1)
	Spawn(context.Background(),
		func(ctx context.Context) (int, error) { return panicViaNilDeref() },
		func(v int, err error) { out <- result{v, err} },
	)
	r := <-out
	if r.err == nil {
		t.Fatal("expected error from panic")
	}
	msg := r.err.Error()
	for _, want := range []string{
		"actor: worker panic",
		"panicViaNilDeref",     // function name appears in stack
		"goroutine",            // standard runtime.Stack format header
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
}

// panicViaNilDeref triggers a real nil-pointer dereference so the
// stack-capture test exercises the same code path the operator
// hit on 2026-05-09 (worker panic during cycle 114ca83a). The
// return statement is unreachable — `p.X` panics first — but
// the compiler needs it to satisfy the signature.
func panicViaNilDeref() (int, error) {
	var p *struct{ X int }
	v := p.X
	return v, nil
}

func TestTruncateStack_KeepsFirstNFrames(t *testing.T) {
	stack := []byte("frame1\nframe2\nframe3\nframe4\n")
	got := truncateStack(stack, 2)
	if !strings.Contains(got, "frame1") {
		t.Errorf("missing frame1: %s", got)
	}
	if !strings.Contains(got, "frame2") {
		t.Errorf("missing frame2: %s", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker: %s", got)
	}
	if strings.Contains(got, "frame4") {
		t.Errorf("frame4 should be truncated: %s", got)
	}
}

func TestTruncateStack_ShortStackReturnedAsIs(t *testing.T) {
	stack := []byte("frame1\nframe2\n")
	got := truncateStack(stack, 100)
	if got != string(stack) {
		t.Errorf("short stack should pass through, got: %q", got)
	}
}

func TestSpawn_ContextPassedThrough(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "hello")
	out := make(chan string, 1)
	Spawn(ctx,
		func(ctx context.Context) (string, error) {
			v, _ := ctx.Value(ctxKey{}).(string)
			return v, nil
		},
		func(v string, err error) { out <- v },
	)
	if got := <-out; got != "hello" {
		t.Fatalf("ctx value = %q, want hello", got)
	}
}

package actor

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchdog_Fires(t *testing.T) {
	w := New()
	fired := make(chan struct{}, 1)
	w.Arm(10*time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("watchdog did not fire")
	}
}

func TestWatchdog_DisarmPrevents(t *testing.T) {
	w := New()
	var count atomic.Int32
	gen := w.Arm(10*time.Millisecond, func() { count.Add(1) })
	if !w.Disarm(gen) {
		t.Fatal("Disarm returned false on a fresh arm")
	}
	time.Sleep(50 * time.Millisecond)
	if got := count.Load(); got != 0 {
		t.Fatalf("fire ran after disarm: count=%d", got)
	}
}

func TestWatchdog_RearmCancelsOld(t *testing.T) {
	w := New()
	var which atomic.Int32
	w.Arm(20*time.Millisecond, func() { which.Store(1) })
	w.Arm(20*time.Millisecond, func() { which.Store(2) })
	time.Sleep(120 * time.Millisecond)
	if got := which.Load(); got != 2 {
		t.Fatalf("expected second arm to win, got %d", got)
	}
}

func TestWatchdog_DisarmStaleReturnsFalse(t *testing.T) {
	w := New()
	var count atomic.Int32
	gen := w.Arm(5*time.Millisecond, func() { count.Add(1) })
	time.Sleep(80 * time.Millisecond) // let it fire
	if w.Disarm(gen) {
		t.Fatal("Disarm of fired gen returned true")
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

func TestWatchdog_DisarmZero(t *testing.T) {
	w := New()
	if w.Disarm(0) {
		t.Fatal("Disarm(0) returned true on never-armed watchdog")
	}
}

func TestWatchdog_ConcurrentArmDisarm(t *testing.T) {
	// Race detector catches data races; this also stresses the gen counter.
	w := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				gen := w.Arm(time.Millisecond, func() {})
				w.Disarm(gen)
			}
		}()
	}
	wg.Wait()
}

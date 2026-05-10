package cog

import (
	"sync"
	"testing"
	"time"
)

func TestActivityHub_FreshHasNoSubscribers(t *testing.T) {
	h := newActivityHub()
	if got := h.subscriberCount(); got != 0 {
		t.Errorf("fresh hub subs = %d, want 0", got)
	}
}

func TestActivityHub_PublishWithNoSubscribersIsNoOp(t *testing.T) {
	h := newActivityHub()
	// Should not panic, block, or otherwise misbehave.
	h.publish(Activity{Status: StatusThinking})
}

func TestActivityHub_SubscribeReceivesPublished(t *testing.T) {
	h := newActivityHub()
	ch, cancel := h.subscribe(4)
	defer cancel()
	h.publish(Activity{Status: StatusThinking, CycleID: "cycle-1"})
	select {
	case got := <-ch:
		if got.Status != StatusThinking || got.CycleID != "cycle-1" {
			t.Errorf("got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber didn't receive published event")
	}
}

func TestActivityHub_FanOutToMultipleSubscribers(t *testing.T) {
	h := newActivityHub()
	const n = 5
	chs := make([]<-chan Activity, n)
	cancels := make([]func(), n)
	for i := 0; i < n; i++ {
		chs[i], cancels[i] = h.subscribe(4)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	if h.subscriberCount() != n {
		t.Errorf("subs = %d, want %d", h.subscriberCount(), n)
	}
	h.publish(Activity{Status: StatusThinking})
	for i, ch := range chs {
		select {
		case got := <-ch:
			if got.Status != StatusThinking {
				t.Errorf("subscriber %d got status %v", i, got.Status)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d didn't receive event", i)
		}
	}
}

func TestActivityHub_DropOnFullDoesNotBlock(t *testing.T) {
	h := newActivityHub()
	// Buffer 1 — second publish must drop without blocking.
	_, cancel := h.subscribe(1)
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.publish(Activity{Status: StatusThinking})
		h.publish(Activity{Status: StatusUsingTools}) // drops
		h.publish(Activity{Status: StatusIdle})       // drops
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on full subscriber")
	}
}

func TestActivityHub_OneSlowSubscriberDoesntStarveOthers(t *testing.T) {
	h := newActivityHub()
	slow, cancelSlow := h.subscribe(0) // never read; drops every event
	fast, cancelFast := h.subscribe(8)
	defer cancelSlow()
	defer cancelFast()
	for i := 0; i < 4; i++ {
		h.publish(Activity{Status: StatusThinking})
	}
	// Fast subscriber should have all 4 events buffered.
	count := 0
loop:
	for {
		select {
		case <-fast:
			count++
			if count == 4 {
				break loop
			}
		case <-time.After(500 * time.Millisecond):
			break loop
		}
	}
	if count != 4 {
		t.Errorf("fast subscriber got %d events, want 4 (slow consumer should not have starved it)", count)
	}
	_ = slow
}

func TestActivityHub_CancelStopsDelivery(t *testing.T) {
	h := newActivityHub()
	ch, cancel := h.subscribe(4)
	if h.subscriberCount() != 1 {
		t.Fatalf("subs = %d, want 1", h.subscriberCount())
	}
	cancel()
	if h.subscriberCount() != 0 {
		t.Errorf("after cancel, subs = %d, want 0", h.subscriberCount())
	}
	// Channel must be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
	// Publish to no subscribers — must not panic.
	h.publish(Activity{Status: StatusIdle})
}

func TestActivityHub_CancelIsIdempotent(t *testing.T) {
	h := newActivityHub()
	_, cancel := h.subscribe(4)
	cancel()
	// Second cancel must not panic. The slot is gone — the closure
	// can't find itself, but that's fine: the closed-channel close
	// is gated by finding the slot.
	cancel()
}

func TestActivityHub_NegativeBufferTreatedAsZero(t *testing.T) {
	h := newActivityHub()
	_, cancel := h.subscribe(-5)
	defer cancel()
	if h.subscriberCount() != 1 {
		t.Errorf("negative buffer should still register subscriber")
	}
	// Publish must not block on the empty buffer.
	done := make(chan struct{})
	go func() {
		h.publish(Activity{Status: StatusIdle})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on zero-buffer subscriber")
	}
}

func TestActivityHub_ConcurrentSubscribeAndPublish(t *testing.T) {
	// Race-detector smoke test: subscribers come and go while the
	// publisher hammers events.
	h := newActivityHub()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.publish(Activity{Status: StatusThinking})
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_, cancel := h.subscribe(2)
		cancel()
	}
	close(stop)
	wg.Wait()
}

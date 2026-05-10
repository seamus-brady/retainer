package cog

import "sync"

// activityHub fans out Activity events to N subscribers. Each
// subscriber gets its own buffered channel; the hub publishes
// non-blocking (drop-on-full per subscriber) so a slow consumer
// can't wedge the cog goroutine.
//
// Concurrency model: subscribe / unsubscribe are safe to call from
// any goroutine; publish is called only from the cog's run loop
// (single-writer invariant matches `emitActivity`'s contract).
//
// The hub is intentionally tiny — no broadcasting goroutine, no
// inbox channel. Publish iterates the subscriber list under a
// short read-lock and drops on full. That keeps the cog loop's
// publish path predictable: O(N) non-blocking sends on a list
// that's effectively bounded by "number of UIs connected" (1 for
// TUI-only, +1 per webui tab).
type activityHub struct {
	mu   sync.RWMutex
	subs []*activitySub
}

// activitySub is one subscriber's slot. The channel is exposed
// read-only to the consumer; the hub holds the write side.
type activitySub struct {
	ch chan Activity
}

// newActivityHub returns an empty hub.
func newActivityHub() *activityHub {
	return &activityHub{}
}

// subscribe adds a subscriber with the given buffer size. Returns
// a receive-only channel and a cancel func that removes the
// subscription and closes the channel. Buffer=0 is allowed but
// effectively means "every event drops unless the consumer is
// blocked in receive when the publish happens" — set buffer to
// match the consumer's expected lag tolerance (the cog's TUI
// uses 16; sockets pick whatever fits their flush rate).
func (h *activityHub) subscribe(buffer int) (<-chan Activity, func()) {
	if buffer < 0 {
		buffer = 0
	}
	sub := &activitySub{ch: make(chan Activity, buffer)}
	h.mu.Lock()
	h.subs = append(h.subs, sub)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i, s := range h.subs {
			if s == sub {
				// Fast unordered remove — order doesn't matter,
				// publishes iterate the whole slice.
				h.subs[i] = h.subs[len(h.subs)-1]
				h.subs = h.subs[:len(h.subs)-1]
				close(sub.ch)
				return
			}
		}
	}
	return sub.ch, cancel
}

// publish sends one event to every current subscriber, non-
// blocking. Slow subscribers drop. Called only from the cog's
// run loop.
func (h *activityHub) publish(a Activity) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sub := range h.subs {
		select {
		case sub.ch <- a:
		default:
			// Subscriber not keeping up — drop this event for
			// this subscriber. Status is ambient; the next
			// event supersedes the dropped one within ms.
		}
	}
}

// subscriberCount reports the current number of attached
// subscribers. Used by tests + diagnostics; not part of the
// publish hot path.
func (h *activityHub) subscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

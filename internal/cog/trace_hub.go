package cog

import "sync"

// traceHub fans out Trace events to N subscribers — operator-
// facing TUI / webui SSE clients that want to render the cog's
// autonomous-cycle replies in the chat log.
//
// Mirrors activityHub's shape: per-subscriber buffered channel,
// non-blocking publish, drop-on-full per subscriber so a stuck
// consumer can't wedge the cog goroutine. Same locking pattern
// (RWMutex; subscribe/unsubscribe holds the write lock for the
// list mutation; publish holds a read lock and iterates).
//
// Concurrency: subscribe / unsubscribe safe from any goroutine;
// publish is called from the cog's run loop after an autonomous
// cycle's onLLMResponseFinal lands the reply.
type traceHub struct {
	mu   sync.RWMutex
	subs []*traceSub
}

type traceSub struct {
	ch chan Trace
}

// newTraceHub returns an empty hub.
func newTraceHub() *traceHub {
	return &traceHub{}
}

// subscribe adds a subscriber with the given buffer size. Returns
// a receive-only channel and a cancel func that removes the
// subscription and closes the channel.
func (h *traceHub) subscribe(buffer int) (<-chan Trace, func()) {
	if buffer < 0 {
		buffer = 0
	}
	sub := &traceSub{ch: make(chan Trace, buffer)}
	h.mu.Lock()
	h.subs = append(h.subs, sub)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for i, s := range h.subs {
			if s == sub {
				h.subs[i] = h.subs[len(h.subs)-1]
				h.subs = h.subs[:len(h.subs)-1]
				close(sub.ch)
				return
			}
		}
	}
	return sub.ch, cancel
}

// publish fans one Trace out to every current subscriber, non-
// blocking. Slow subscribers drop.
func (h *traceHub) publish(t Trace) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sub := range h.subs {
		select {
		case sub.ch <- t:
		default:
		}
	}
}

// subscriberCount reports current subscribers. Test/diagnostic
// only; not part of the publish hot path.
func (h *traceHub) subscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

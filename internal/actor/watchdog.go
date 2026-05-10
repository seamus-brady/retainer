// Package actor provides goroutine-based primitives that fill the role
// OTP / Ergo would fill in Springdrift. Intentionally not a generic
// Actor[S, M] abstraction — each actor (cog, librarian, scheduler, ...) is a
// hand-rolled goroutine that composes these primitives.
//
// See doc/roadmap/shipped/actor-framework.md for the porting plan.
package actor

import (
	"sync"
	"time"
)

// Watchdog is a single-shot timer with a generation counter so Arm/Disarm
// races resolve safely: a stale timer fire after Disarm or after Arm-rearm
// becomes a no-op rather than running the previous fire callback.
type Watchdog struct {
	mu    sync.Mutex
	gen   uint64
	timer *time.Timer
}

func New() *Watchdog { return &Watchdog{} }

// Arm cancels any pending fire and schedules fire to run after d. Returns
// the generation token, which Disarm uses to identify this arming.
func (w *Watchdog) Arm(d time.Duration, fire func()) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.gen++
	armed := w.gen
	w.timer = time.AfterFunc(d, func() {
		w.mu.Lock()
		if w.gen != armed {
			w.mu.Unlock()
			return
		}
		w.gen++
		w.mu.Unlock()
		fire()
	})
	return armed
}

// Disarm cancels the arming with the given generation token. Returns true
// if this generation was current (and is now cancelled), false if it had
// already fired or been replaced by a subsequent Arm.
func (w *Watchdog) Disarm(gen uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen == 0 || w.gen != gen {
		return false
	}
	w.gen++
	if w.timer != nil {
		w.timer.Stop()
	}
	return true
}

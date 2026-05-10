package cog

import (
	"testing"
	"time"
)

// TestNotifyRetry_PublishesActivityWithRetryFields confirms that
// NotifyRetry produces an Activity event with StatusRetrying + the
// retry metadata fields populated, and that subscribers receive it.
//
// The test bypasses cog.New() because NotifyRetry only touches the
// hub + the atomic cycle-ID pointer — the rest of the cog's
// machinery isn't needed and constructing it would drag in every
// dependency the bootstrap wires up. A bare *Cog with those two
// fields initialised exercises the contract end-to-end.
func TestNotifyRetry_PublishesActivityWithRetryFields(t *testing.T) {
	c := &Cog{hub: newActivityHub()}
	cid := "cycle-123"
	c.currentCycleID.Store(&cid)

	ch, cancel := c.Subscribe(4)
	defer cancel()

	c.NotifyRetry(2, 5, 15*time.Second, "rate_limited")

	select {
	case got := <-ch:
		if got.Status != StatusRetrying {
			t.Errorf("Status = %v, want StatusRetrying", got.Status)
		}
		if got.CycleID != "cycle-123" {
			t.Errorf("CycleID = %q, want cycle-123", got.CycleID)
		}
		if got.RetryAttempt != 2 {
			t.Errorf("RetryAttempt = %d, want 2", got.RetryAttempt)
		}
		if got.RetryMaxAttempts != 5 {
			t.Errorf("RetryMaxAttempts = %d, want 5", got.RetryMaxAttempts)
		}
		if got.RetryDelayMs != 15000 {
			t.Errorf("RetryDelayMs = %d, want 15000", got.RetryDelayMs)
		}
		if got.RetryReason != "rate_limited" {
			t.Errorf("RetryReason = %q, want rate_limited", got.RetryReason)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber didn't receive the retry Activity event")
	}
}

func TestNotifyRetry_EmptyCycleIDWhenNoCycleInFlight(t *testing.T) {
	// currentCycleID may legitimately be empty (or never stored)
	// when NotifyRetry races against discardPending. The Activity
	// event should still publish — just without a CycleID stamp.
	c := &Cog{hub: newActivityHub()}

	ch, cancel := c.Subscribe(4)
	defer cancel()

	c.NotifyRetry(1, 3, time.Second, "transient")

	select {
	case got := <-ch:
		if got.CycleID != "" {
			t.Errorf("CycleID = %q, want empty", got.CycleID)
		}
		if got.Status != StatusRetrying {
			t.Errorf("Status = %v, want StatusRetrying", got.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber didn't receive the retry Activity event")
	}
}

func TestStatusRetrying_String(t *testing.T) {
	if got := StatusRetrying.String(); got != "retrying" {
		t.Errorf("StatusRetrying.String() = %q, want retrying", got)
	}
}

package proc_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
)

func TestWatchdogFiresOnSilence(t *testing.T) {
	var got atomic.Int32
	reasonc := make(chan proc.ExpiryReason, 1)
	wd := proc.NewWatchdog(0, 50*time.Millisecond, func(r proc.ExpiryReason) {
		got.Add(1)
		reasonc <- r
	})
	defer wd.Stop()
	select {
	case r := <-reasonc:
		if r != proc.ReasonSilence {
			t.Fatalf("reason = %v, want silence", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire on silence")
	}
}

func TestWatchdogSilenceResetByFrames(t *testing.T) {
	var mu sync.Mutex
	fired := false
	wd := proc.NewWatchdog(0, 100*time.Millisecond, func(proc.ExpiryReason) {
		mu.Lock()
		defer mu.Unlock()
		fired = true
	})
	defer wd.Stop()
	// Reset three times, staying under the silence interval each time.
	for range 3 {
		time.Sleep(40 * time.Millisecond)
		wd.FrameSeen()
	}
	mu.Lock()
	defer mu.Unlock()
	if fired {
		t.Fatal("watchdog fired despite frame resets")
	}
}

func TestWatchdogDeadlineFires(t *testing.T) {
	reasonc := make(chan proc.ExpiryReason, 1)
	wd := proc.NewWatchdog(50*time.Millisecond, time.Hour, func(r proc.ExpiryReason) {
		reasonc <- r
	})
	defer wd.Stop()
	select {
	case r := <-reasonc:
		if r != proc.ReasonDeadline {
			t.Fatalf("reason = %v, want deadline", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadline timer did not fire")
	}
	if !wd.Expired() {
		t.Fatal("Expired = false after firing")
	}
}

func TestWatchdogStopPreventsExpiry(t *testing.T) {
	fired := make(chan proc.ExpiryReason, 1)
	wd := proc.NewWatchdog(0, 50*time.Millisecond, func(r proc.ExpiryReason) { fired <- r })
	wd.Stop()
	select {
	case r := <-fired:
		t.Fatalf("watchdog fired after Stop: %v", r)
	case <-time.After(200 * time.Millisecond):
	}
}

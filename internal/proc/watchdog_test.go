// SPDX-License-Identifier: GPL-3.0-or-later

package proc_test

import (
	"bytes"
	"runtime/pprof"
	"strings"
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

// TestWatchdogLoserTimerDoesNotParkBehindCallback pins that when both timers
// fire together, only one goroutine runs the (possibly long) expiry callback:
// the other returns immediately instead of waiting out the kill ladder.
func TestWatchdogLoserTimerDoesNotParkBehindCallback(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	wd := proc.NewWatchdog(50*time.Millisecond, 50*time.Millisecond, func(proc.ExpiryReason) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
	})
	defer wd.Stop()
	defer close(release)
	<-entered
	time.Sleep(300 * time.Millisecond) // the second timer has fired by now

	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		t.Fatal(err)
	}
	inExpire := 0
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "(*Watchdog).expire") {
			inExpire++
		}
	}
	if inExpire != 1 || calls.Load() != 1 {
		t.Fatalf("goroutines in expire = %d, callbacks = %d; want 1 and 1", inExpire, calls.Load())
	}
}

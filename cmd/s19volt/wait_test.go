package main

import (
	"testing"
	"time"
)

// a stop during a long wait ends it within about a second; a stale stop flag outside a setup run does not
func TestWaitForStops(t *testing.T) {
	defer func() { setupActive.Store(false); stopFlag.Store(false) }()
	setupActive.Store(true)
	stopFlag.Store(false)
	go func() { time.Sleep(300 * time.Millisecond); requestStop() }()
	t0 := time.Now()
	if waitFor(time.Minute, 30*time.Second, func() bool { return false }) {
		t.Fatal("waitFor returned true")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("stop took %s", d)
	}
	// waitUp ignores the stop (bosminer back up before leaving)
	n := 0
	if !waitUp(time.Minute, 10*time.Millisecond, func() bool { n++; return n > 3 }) {
		t.Fatal("waitUp must not stop")
	}
	// stale flag, no setup running: the core's own waits are not cut short
	setupActive.Store(false)
	n = 0
	if !waitFor(time.Minute, 10*time.Millisecond, func() bool { n++; return n > 3 }) {
		t.Fatal("a stale stop flag cut a core wait short")
	}
	// timeout still works
	if waitLoop(50*time.Millisecond, 20*time.Millisecond, func() bool { return false }, func() bool { return false }) {
		t.Fatal("timeout")
	}
}

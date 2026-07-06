package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
)

func TestBuilderIdleMonitorFiresOnSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 4)
	stalled := make(chan struct{}, 1)

	go startBuilderIdleMonitor(ctx, 20*time.Millisecond, progress, nil, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire after silence")
	}
}

func TestBuilderIdleMonitorDoesNotFireUnderSteadyProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 32)
	stalled := make(chan struct{}, 1)

	go startBuilderIdleMonitor(ctx, 25*time.Millisecond, progress, nil, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	for i := 0; i < 24; i++ {
		time.Sleep(5 * time.Millisecond)
		progress <- "turn_done"
	}

	select {
	case <-stalled:
		t.Fatal("idle monitor fired while progress arrived continuously")
	default:
	}
}

func TestBuilderIdleMonitorDoesNotResetOnErrorBursts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 16)
	stalled := make(chan struct{}, 1)
	hook := progressObservabilityHook{progress: newBuilderProgressReporter(progress)}

	go startBuilderIdleMonitor(ctx, 30*time.Millisecond, progress, nil, func() { stalled <- struct{}{} }, builderIdleTestLogger())
	for i := 0; i < 5; i++ {
		hook.OnBridleEvent(bridle.ToolCallResult{ID: "tool", Err: "boom"})
		hook.OnBridleEvent(bridle.TurnError{Stage: "provider"})
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire during error-only activity")
	}
}

func TestProgressObservabilityHookResetsOnSuccessSignals(t *testing.T) {
	progress := make(chan string, 4)
	hook := progressObservabilityHook{progress: newBuilderProgressReporter(progress)}

	hook.OnBridleEvent(bridle.ToolCallResult{ID: "tool"})
	hook.OnBridleEvent(bridle.TurnDone{})

	got := []string{<-progress, <-progress}
	if got[0] != "tool_call" || got[1] != "turn_done" {
		t.Fatalf("progress reasons = %v, want [tool_call turn_done]", got)
	}
}

// TestBuilderIdleMonitorSuspendedWhileToolInFlight covers case 1 from the
// fix spec: an in-flight tool call spanning many multiples of the idle
// timeout must never trigger onStall — the tool executing IS progress.
func TestBuilderIdleMonitorSuspendedWhileToolInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 4)
	stalled := make(chan struct{}, 1)
	inFlight := &builderInFlightTracker{}
	inFlight.inc() // a tool call starts and never completes in this test

	go startBuilderIdleMonitor(ctx, 15*time.Millisecond, progress, inFlight, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	// Let many multiples of the timeout elapse with the tool still
	// marked in flight — the monitor must never fire.
	time.Sleep(150 * time.Millisecond)

	select {
	case <-stalled:
		t.Fatal("idle monitor fired while a tool call was still in flight")
	default:
	}
}

// TestBuilderIdleMonitorFiresAfterToolCompletesAndGoesQuiet covers case 2:
// a tool completes (crossing the in-flight count back to zero), and then
// a subsequent quiet gap longer than the timeout must still fire onStall
// exactly once.
func TestBuilderIdleMonitorFiresAfterToolCompletesAndGoesQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 4)
	stalled := make(chan struct{}, 2)
	inFlight := &builderInFlightTracker{}
	hook := progressObservabilityHook{progress: newBuilderProgressReporter(progress), inFlight: inFlight}

	go startBuilderIdleMonitor(ctx, 20*time.Millisecond, progress, inFlight, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	hook.OnBridleEvent(bridle.ToolCallStart{ID: "t1"})
	time.Sleep(5 * time.Millisecond)
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "t1"})

	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire after the tool completed and activity stopped")
	}

	// startBuilderIdleMonitor returns immediately after calling onStall
	// once, so a second signal must never arrive.
	select {
	case <-stalled:
		t.Fatal("onStall fired more than once")
	default:
	}
}

// TestBuilderIdleMonitorOverlappingToolCallsStaySuspendedUntilLastResult
// covers case 3: two overlapping tool calls keep the monitor suspended
// until the SECOND (last) result crosses the in-flight count back to
// zero, at which point the idle window restarts from that instant.
func TestBuilderIdleMonitorOverlappingToolCallsStaySuspendedUntilLastResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 8)
	stalled := make(chan struct{}, 1)
	inFlight := &builderInFlightTracker{}
	hook := progressObservabilityHook{progress: newBuilderProgressReporter(progress), inFlight: inFlight}

	go startBuilderIdleMonitor(ctx, 20*time.Millisecond, progress, inFlight, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	hook.OnBridleEvent(bridle.ToolCallStart{ID: "a"})
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "b"})
	time.Sleep(60 * time.Millisecond) // several timeouts, both tools still in flight
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "a"})

	select {
	case <-stalled:
		t.Fatal("idle monitor fired while a second tool call ('b') was still in flight")
	default:
	}
	if !inFlight.inFlight() {
		t.Fatal("expected tool 'b' to still be counted in flight")
	}

	// Resolving the last outstanding tool call crosses the count back to
	// zero — this is the moment the idle window restarts from.
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "b"})
	if inFlight.inFlight() {
		t.Fatal("expected in-flight count to reach zero after the last result")
	}

	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire after the last tool call completed and activity stopped")
	}
}

// TestBuilderInFlightTrackerUnderflowGuard covers case 4: a
// ToolCallResult with no matching ToolCallStart must not panic or drive
// the counter negative, and must log a warning.
func TestBuilderInFlightTrackerUnderflowGuard(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	tracker := &builderInFlightTracker{}

	if crossed := tracker.dec(log); crossed {
		t.Fatal("dec() reported a zero-crossing on an already-empty tracker")
	}
	if tracker.inFlight() {
		t.Fatal("tracker should report not-in-flight after an underflowing dec()")
	}
	if !strings.Contains(buf.String(), "underflow") {
		t.Fatalf("expected a warning to be logged on underflow, got: %q", buf.String())
	}

	// The tracker must still behave correctly for subsequent legitimate
	// start/result pairs after an underflow.
	tracker.inc()
	if !tracker.inFlight() {
		t.Fatal("expected tracker to be in flight after inc()")
	}
	if crossed := tracker.dec(log); !crossed {
		t.Fatal("expected dec() to report a zero-crossing for a balanced inc/dec")
	}
	if tracker.inFlight() {
		t.Fatal("expected tracker to be empty after the balanced dec()")
	}
}

func TestBuilderIdleTimeoutDefaultFromEnv(t *testing.T) {
	if got := builderIdleTimeoutDefaultFromEnv(func(string) string { return "" }); got != defaultBuilderIdleTimeout {
		t.Fatalf("empty env default = %v, want %v", got, defaultBuilderIdleTimeout)
	}
	if got := builderIdleTimeoutDefaultFromEnv(func(string) string { return "90s" }); got != 90*time.Second {
		t.Fatalf("env default = %v, want 90s", got)
	}
	if got := builderIdleTimeoutDefaultFromEnv(func(string) string { return "bad" }); got != defaultBuilderIdleTimeout {
		t.Fatalf("bad env default = %v, want %v", got, defaultBuilderIdleTimeout)
	}
}

func builderIdleTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

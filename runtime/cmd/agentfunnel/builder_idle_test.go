package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
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

// TestBuilderIdleMonitorSuppressesOnInFlightTool verifies the core NET-49
// invariant: when a ToolCallStart has been observed but its matching
// ToolCallResult has not yet arrived, the idle timer MUST reset rather than
// fire. The model is working on the tool's output — that wall-clock time
// should not count against liveness.
func TestBuilderIdleMonitorSuppressesOnInFlightTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 16)
	flight := &bridleToolFlightCounter{}
	stalled := make(chan struct{}, 1)
	hook := progressObservabilityHook{
		progress:      newBuilderProgressReporter(progress),
		flightCounter: flight,
	}

	go startBuilderIdleMonitor(ctx, 40*time.Millisecond, progress, flight, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	// Start a tool. Counter is now 1. Timer is running at 40ms.
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "tc1", Name: "Write"})

	// Wait longer than the idle timeout. With the in-flight guard working,
	// the timer should reset each time it would fire and NOT stall.
	time.Sleep(200 * time.Millisecond)

	select {
	case <-stalled:
		t.Fatal("idle monitor fired while tool was in-flight (counter=1)")
	default:
	}

	// Tool finishes — counter drops to 0. From now on, silence should let
	// the timer fire again (no more in-flight protection).
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "tc1"})

	// Wait past the timeout. Should stall now.
	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire after tool completed + silence")
	}
}

// TestBuilderIdleMonitorSuppressesOnNestedTools verifies that when multiple
// tools are in-flight simultaneously (a model chaining tool calls), the
// counter tracks them all and only releases when the last one completes.
func TestBuilderIdleMonitorSuppressesOnNestedTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 32)
	flight := &bridleToolFlightCounter{}
	stalled := make(chan struct{}, 1)
	hook := progressObservabilityHook{
		progress:      newBuilderProgressReporter(progress),
		flightCounter: flight,
	}

	go startBuilderIdleMonitor(ctx, 30*time.Millisecond, progress, flight, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	// Start three tools (counter = 3).
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "a", Name: "Read"})
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "b", Name: "Grep"})
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "c", Name: "Edit"})

	if got := flight.count.Load(); got != 3 {
		t.Fatalf("after 3 ToolCallStart: counter = %d, want 3", got)
	}

	// Wait past timeout — should still be suppressed.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-stalled:
		t.Fatal("idle monitor fired while 3 tools in-flight")
	default:
	}

	// Two complete (counter = 1). Still suppressed.
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "a"})
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "b"})
	if got := flight.count.Load(); got != 1 {
		t.Fatalf("after 2 ToolCallResult: counter = %d, want 1", got)
	}

	// Wait past timeout — still suppressed (1 in-flight).
	time.Sleep(200 * time.Millisecond)
	select {
	case <-stalled:
		t.Fatal("idle monitor fired while 1 tool still in-flight")
	default:
	}

	// Last one completes (counter = 0). Silence should now let stall fire.
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "c"})
	if got := flight.count.Load(); got != 0 {
		t.Fatalf("after 3 ToolCallResult: counter = %d, want 0", got)
	}

	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire after all tools completed + silence")
	}
}

// TestProgressObservabilityHookTracksFlightCounter verifies that
// progressObservabilityHook correctly maps bridle events to counter state:
// ToolCallStart increments, ToolCallResult decrements (success or error),
// other events leave the counter alone.
func TestProgressObservabilityHookTracksFlightCounter(t *testing.T) {
	progress := make(chan string, 8)
	flight := &bridleToolFlightCounter{}
	hook := progressObservabilityHook{
		progress:      newBuilderProgressReporter(progress),
		flightCounter: flight,
	}

	// 1. Start + success: counter 0 → 1 → 0, progress gets "tool_call".
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "t1", Name: "Read"})
	if got := flight.count.Load(); got != 1 {
		t.Fatalf("after ToolCallStart: counter = %d, want 1", got)
	}
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "t1"})
	if got := flight.count.Load(); got != 0 {
		t.Fatalf("after ToolCallResult (ok): counter = %d, want 0", got)
	}
	got := <-progress
	if got != "tool_call" {
		t.Fatalf("progress on success: got %q, want tool_call", got)
	}

	// 2. Start + error: counter 0 → 1 → 0, NO progress signal (error).
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "t2", Name: "Bash"})
	if got := flight.count.Load(); got != 1 {
		t.Fatalf("after ToolCallStart (err): counter = %d, want 1", got)
	}
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "t2", Err: "exit 1"})
	if got := flight.count.Load(); got != 0 {
		t.Fatalf("after ToolCallResult (err): counter = %d, want 0", got)
	}
	select {
	case got := <-progress:
		t.Fatalf("no progress on error result, got %q", got)
	default:
	}

	// 3. TurnDone does NOT touch the counter.
	hook.OnBridleEvent(bridle.TurnDone{})
	if got := flight.count.Load(); got != 0 {
		t.Fatalf("after TurnDone: counter = %d, want 0", got)
	}

	// 4. TurnError does NOT touch the counter.
	hook.OnBridleEvent(bridle.TurnError{Stage: "provider"})
	if got := flight.count.Load(); got != 0 {
		t.Fatalf("after TurnError: counter = %d, want 0", got)
	}
}

// TestProgressObservabilityHookNilCounterIsNoop verifies that a hook with no
// flightCounter attached (the non-builder mode or a hook constructed before
// the counter was wired in) does not panic and behaves like a pass-through.
// With a nil counter the success-path progress signal on ToolCallResult{Err:""}
// still fires (that is the pre-NET-49 behavior preserved for the progress
// channel; the in-flight guard lives in the idle monitor, not here).
func TestProgressObservabilityHookNilCounterIsNoop(t *testing.T) {
	progress := make(chan string, 8)
	hook := progressObservabilityHook{
		progress: newBuilderProgressReporter(progress),
	}

	// These must not panic with a nil counter.
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "t1", Name: "Read"})
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "t1"}) // success → progress "tool_call"
	hook.OnBridleEvent(bridle.TurnDone{})

	got := <-progress
	if got != "tool_call" {
		t.Fatalf("first progress (nil counter, success ToolCallResult): got %q, want tool_call", got)
	}
	got = <-progress
	if got != "turn_done" {
		t.Fatalf("second progress (nil counter, TurnDone): got %q, want turn_done", got)
	}
}

// TestBridleToolFlightCounterBasics exercises the atomic counter directly —
// zero value is valid, Increment/Decrement pair to zero, Active semantics.
func TestBridleToolFlightCounterBasics(t *testing.T) {
	var c bridleToolFlightCounter
	// Zero value: Active() is false, Load() is 0.
	if c.Active() {
		t.Fatal("zero-value counter should report Active() == false")
	}
	if c.count.Load() != 0 {
		t.Fatalf("zero-value counter: Load() = %d, want 0", c.count.Load())
	}

	c.Increment()
	if !c.Active() {
		t.Fatal("after Increment: Active() should be true")
	}
	if c.count.Load() != 1 {
		t.Fatalf("after Increment: Load() = %d, want 1", c.count.Load())
	}

	c.Increment()
	if c.count.Load() != 2 {
		t.Fatalf("after 2nd Increment: Load() = %d, want 2", c.count.Load())
	}

	c.Decrement()
	if c.count.Load() != 1 {
		t.Fatalf("after Decrement: Load() = %d, want 1", c.count.Load())
	}

	c.Decrement()
	if c.Active() {
		t.Fatal("after full pair: Active() should be false")
	}
	if c.count.Load() != 0 {
		t.Fatalf("after full pair: Load() = %d, want 0", c.count.Load())
	}
}

// TestBridleToolFlightCounterConcurrent verifies race-freedom: many
// goroutines Incrementing and Decrementing concurrently should end with a
// deterministic final count equal to starts - completes.
func TestBridleToolFlightCounterConcurrent(t *testing.T) {
	var c bridleToolFlightCounter
	const N = 100
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			c.Increment()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			c.Decrement()
		}
	}()
	wg.Wait()

	if got := c.count.Load(); got != 0 {
		t.Fatalf("after N Increments + N Decrementss: counter = %d, want 0", got)
	}
}

// TestBuilderIdleMonitorToolStartResetsSilenceTimer is the integration check:
// a tool starts after the timer has already been running, and the start
// should reset the timer so the stall does not fire during the in-flight
// window. Distinct from TestBuilderIdleMonitorSuppressesOnInFlightTool in
// that the timer is already mid-count when the ToolCallStart arrives (not
// fresh).
func TestBuilderIdleMonitorToolStartResetsSilenceTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 16)
	flight := &bridleToolFlightCounter{}
	stalled := make(chan struct{}, 1)
	hook := progressObservabilityHook{
		progress:      newBuilderProgressReporter(progress),
		flightCounter: flight,
	}

	go startBuilderIdleMonitor(ctx, 30*time.Millisecond, progress, flight, func() { stalled <- struct{}{} }, builderIdleTestLogger())

	// Let the timer almost fire.
	time.Sleep(25 * time.Millisecond)

	// Tool starts — should reset the timer.
	hook.OnBridleEvent(bridle.ToolCallStart{ID: "t1", Name: "Write"})

	// Wait well past the timeout. Should be suppressed.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-stalled:
		t.Fatal("idle monitor fired after ToolCallStart reset")
	default:
	}

	// Finish tool + silence → should fire.
	hook.OnBridleEvent(bridle.ToolCallResult{ID: "t1"})
	select {
	case <-stalled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle monitor did not fire after tool result + silence")
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

func builderIdleTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
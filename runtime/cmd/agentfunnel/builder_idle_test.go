package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
)

func TestBuilderIdleMonitorFiresOnSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := make(chan string, 4)
	stalled := make(chan struct{}, 1)

	go startBuilderIdleMonitor(ctx, 20*time.Millisecond, progress, func() { stalled <- struct{}{} }, builderIdleTestLogger())

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

	go startBuilderIdleMonitor(ctx, 25*time.Millisecond, progress, func() { stalled <- struct{}{} }, builderIdleTestLogger())

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

	go startBuilderIdleMonitor(ctx, 30*time.Millisecond, progress, func() { stalled <- struct{}{} }, builderIdleTestLogger())
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

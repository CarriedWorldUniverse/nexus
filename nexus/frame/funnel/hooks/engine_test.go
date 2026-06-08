package hooks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type handlerFunc func(context.Context, map[string]any) (Decision, error)

func (f handlerFunc) Run(ctx context.Context, payload map[string]any) (Decision, error) {
	return f(ctx, payload)
}

func TestDispatchMatcherResolution(t *testing.T) {
	engine := New()
	engine.Register("PreToolUse", "Bash", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "exact"}, nil
	}))
	engine.Register("PreToolUse", "Edit|Write", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "regex"}, nil
	}))
	engine.Register("PreToolUse", "*", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "all"}, nil
	}))
	engine.Register("Stop", "", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "stop"}, nil
	}))

	got, err := engine.Dispatch(context.Background(), "PreToolUse", map[string]any{"tool_name": "Edit"})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if got.AdditionalContext != "regex\nall" {
		t.Fatalf("AdditionalContext = %q, want regex and all", got.AdditionalContext)
	}

	got, err = engine.Dispatch(context.Background(), "PreToolUse", map[string]any{"tool_name": "Bashful"})
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if got.AdditionalContext != "all" {
		t.Fatalf("AdditionalContext = %q, want only all", got.AdditionalContext)
	}
}

func TestDispatchTimeoutAndPanicIsolation(t *testing.T) {
	engine := New()
	engine.Register("Stop", "", 10*time.Millisecond, handlerFunc(func(ctx context.Context, _ map[string]any) (Decision, error) {
		<-ctx.Done()
		return Decision{}, ctx.Err()
	}))
	engine.Register("Stop", "", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		panic("boom")
	}))
	engine.Register("Stop", "", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "survived"}, nil
	}))

	got, err := engine.Dispatch(context.Background(), "Stop", nil)
	if err == nil {
		t.Fatal("Dispatch error = nil, want collected handler errors")
	}
	if got.AdditionalContext != "survived" {
		t.Fatalf("AdditionalContext = %q, want survived", got.AdditionalContext)
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("Dispatch error %q does not mention timeout/deadline", err.Error())
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Dispatch error %q does not mention panic", err.Error())
	}
}

func TestDispatchReturnsNonBlockingHandlerErrors(t *testing.T) {
	engine := New()
	engine.Register("Stop", "", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{}, errors.New("handler failed")
	}))
	engine.Register("Stop", "", 0, handlerFunc(func(context.Context, map[string]any) (Decision, error) {
		return Decision{AdditionalContext: "ok"}, nil
	}))

	got, err := engine.Dispatch(context.Background(), "Stop", nil)
	if err == nil || !strings.Contains(err.Error(), "handler failed") {
		t.Fatalf("Dispatch error = %v, want handler failure", err)
	}
	if got.AdditionalContext != "ok" {
		t.Fatalf("AdditionalContext = %q, want ok", got.AdditionalContext)
	}
}

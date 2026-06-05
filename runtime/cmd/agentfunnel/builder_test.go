package main

import (
	"context"
	"log/slog"
	"testing"
)

func TestBuilderOnTaskDoneCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	onDone := builderOnTaskDone(cancel, slog.Default(), "anvil")

	onDone("PR opened")

	select {
	case <-ctx.Done():
	default:
		t.Fatal("OnTaskDone did not cancel the context")
	}
}

func TestBuilderReplyTopicOnlyAppliesInBuilderMode(t *testing.T) {
	if got := builderReplyTopic(true, "NEX-443"); got != "NEX-443" {
		t.Errorf("builderReplyTopic(true): got %q, want NEX-443", got)
	}
	if got := builderReplyTopic(false, "NEX-443"); got != "" {
		t.Errorf("builderReplyTopic(false): got %q, want empty", got)
	}
}

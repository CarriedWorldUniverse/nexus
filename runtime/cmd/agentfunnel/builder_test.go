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

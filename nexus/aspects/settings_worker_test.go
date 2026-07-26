package aspects

import (
	"context"
	"testing"
)

// CentralFor is the whole NEX-826 decision, so it is tested as a truth
// table rather than by example. The fallback row is the one that matters
// most: an unset worker variant must reproduce pre-NEX-826 behaviour
// exactly, or shipping the migration changes what every identity boots
// with before anyone has written a word of worker policy.
func TestCentralFor(t *testing.T) {
	const (
		interactive = "## interactive policy"
		headless    = "## headless policy"
	)
	cases := []struct {
		name     string
		identity string
		aspect   string
		worker   string
		want     string
		why      string
	}{
		{
			name: "base aspect gets interactive", identity: "shadow",
			aspect: interactive, worker: headless, want: interactive,
			why: "a long-lived aspect must never receive worker policy",
		},
		{
			name: "dotted hand gets worker", identity: "shadow.umbra",
			aspect: interactive, worker: headless, want: headless,
			why: "a derived hand is headless",
		},
		{
			name: "pool worker gets worker", identity: "anvil-builder",
			aspect: interactive, worker: headless, want: headless,
			why: "<personality>-<role> is the other headless shape",
		},
		{
			name: "hand falls back when variant unset", identity: "shadow.umbra",
			aspect: interactive, worker: "", want: interactive,
			why: "empty variant must reproduce the single-prompt behaviour",
		},
		{
			name: "base aspect unaffected by variant", identity: "shadow",
			aspect: interactive, worker: "", want: interactive,
		},
		{
			name: "hand with only worker set", identity: "shadow.umbra",
			aspect: "", worker: headless, want: headless,
		},
		{
			name: "aspect with only worker set gets empty", identity: "shadow",
			aspect: "", worker: headless, want: "",
			why: "there is no fallback from worker policy to an aspect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := &NexusSettings{NexusMD: tc.aspect, NexusMDWorker: tc.worker}
			if got := ns.CentralFor(tc.identity); got != tc.want {
				t.Errorf("CentralFor(%q) = %q, want %q\n  %s", tc.identity, got, tc.want, tc.why)
			}
		})
	}
}

// A nil receiver must not panic: Get's error path leaves callers holding
// nil, and central content is explicitly non-fatal (validate.go degrades
// gracefully rather than rejecting a verified handshake).
func TestCentralFor_NilReceiver(t *testing.T) {
	var ns *NexusSettings
	if got := ns.CentralFor("shadow.umbra"); got != "" {
		t.Errorf("nil receiver returned %q, want empty", got)
	}
}

// Round-trip through the real store: the variant persists, is readable,
// and does not disturb the interactive column.
func TestSetNexusMDWorker_RoundTrip(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	ctx := context.Background()

	if _, err := ss.SetNexusMD(ctx, "## interactive"); err != nil {
		t.Fatalf("SetNexusMD: %v", err)
	}
	vWorker, err := ss.SetNexusMDWorker(ctx, "## headless")
	if err != nil {
		t.Fatalf("SetNexusMDWorker: %v", err)
	}

	ns, err := ss.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ns.NexusMD != "## interactive" {
		t.Errorf("worker write clobbered the interactive column: %q", ns.NexusMD)
	}
	if ns.NexusMDWorker != "## headless" {
		t.Errorf("NexusMDWorker = %q, want %q", ns.NexusMDWorker, "## headless")
	}
	if ns.Version != vWorker {
		t.Errorf("Get version %d != returned version %d", ns.Version, vWorker)
	}

	// One row, one counter: a worker write is a central change.
	if _, err := ss.SetNexusMDWorker(ctx, "## headless v2"); err != nil {
		t.Fatalf("second SetNexusMDWorker: %v", err)
	}
	ns2, err := ss.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ns2.Version <= ns.Version {
		t.Errorf("version did not bump on worker write: %d -> %d", ns.Version, ns2.Version)
	}
}

// Clearing the variant is the rollback, so it must be a supported write
// and must restore the single-prompt shape for hands.
func TestSetNexusMDWorker_ClearRestoresFallback(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	ctx := context.Background()

	if _, err := ss.SetNexusMD(ctx, "## interactive"); err != nil {
		t.Fatalf("SetNexusMD: %v", err)
	}
	if _, err := ss.SetNexusMDWorker(ctx, "## headless"); err != nil {
		t.Fatalf("SetNexusMDWorker: %v", err)
	}
	if _, err := ss.SetNexusMDWorker(ctx, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}

	ns, err := ss.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := ns.CentralFor("shadow.umbra"); got != "## interactive" {
		t.Errorf("after clearing the variant a hand got %q, want the interactive text", got)
	}
}

// Writing the variant into a bare table must not fabricate interactive
// content — the INSERT arm leaves nexus_md at its default.
func TestSetNexusMDWorker_ColdInsertLeavesInteractiveEmpty(t *testing.T) {
	ss, _ := freshSettingsRig(t)
	ctx := context.Background()

	if _, err := ss.SetNexusMDWorker(ctx, "## headless"); err != nil {
		t.Fatalf("SetNexusMDWorker: %v", err)
	}
	ns, err := ss.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ns.NexusMD != "" {
		t.Errorf("cold worker write invented interactive content: %q", ns.NexusMD)
	}
	if got := ns.CentralFor("shadow"); got != "" {
		t.Errorf("aspect got %q from a worker-only row, want empty", got)
	}
}

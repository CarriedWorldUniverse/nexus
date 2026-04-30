package handqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nexus-cw/nexus/nexus/frames"
	"github.com/nexus-cw/nexus/runtime/handexec"
)

// TestMain lets the test binary double as a fake harness for
// SpawnExecutor tests. When HANDQUEUE_FAKE_HARNESS env is set, we
// act like a dispatch-mode harness: read a handexec.Request from
// stdin, write a canned DispatchResultPayload to stdout, exit.
func TestMain(m *testing.M) {
	if os.Getenv("HANDQUEUE_FAKE_HARNESS") != "" {
		runFakeHarness()
		return
	}
	os.Exit(m.Run())
}

// runFakeHarness reads stdin, produces a canned response.
func runFakeHarness() {
	var req handexec.Request
	dec := json.NewDecoder(os.Stdin)
	if err := dec.Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "fake harness decode:", err)
		os.Exit(2)
	}
	resp := frames.DispatchResultPayload{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: req.DispatchID,
		Output: map[string]any{
			"echoed_payload": req.Payload,
		},
	}
	// Mimic a harness that logs to stdout before the JSON envelope.
	fmt.Println("fake harness starting up")
	raw, _ := json.Marshal(resp)
	fmt.Println(string(raw))
}

// TestSpawnExecutorRoundTrip runs the test binary as the harness,
// checks that stdin → subprocess → stdout parse works end-to-end.
func TestSpawnExecutorRoundTrip(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	ex := &SpawnExecutor{
		HarnessPath: self,
		HomeResolver: AspectHomeResolverFunc(func(name string) (string, bool) {
			return "/tmp/" + name, true
		}),
		ExtraEnv: []string{"HANDQUEUE_FAKE_HARNESS=1"},
	}

	ctx := context.Background()
	res, err := ex.Execute(ctx, frames.DispatchPayload{
		Aspect:     "wren",
		Thread:     "t-1",
		DispatchID: "d-1",
		Payload:    map[string]any{"text": "a passage"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Aspect != "wren" {
		t.Errorf("Aspect = %q", res.Aspect)
	}
	if res.DispatchID != "d-1" {
		t.Errorf("DispatchID = %q", res.DispatchID)
	}
	// The fake prints "fake harness starting up" before the JSON —
	// SpawnExecutor must still find the JSON line.
	echoedRaw, ok := res.Output["echoed_payload"]
	if !ok {
		t.Errorf("Output.echoed_payload missing — stdout parse failed")
	}
	echoedMap, ok := echoedRaw.(map[string]any)
	if !ok {
		t.Fatalf("echoed_payload type = %T, want map", echoedRaw)
	}
	if echoedMap["text"] != "a passage" {
		t.Errorf("echoed text = %v", echoedMap["text"])
	}
}

func TestSpawnExecutorRejectsUnknownAspect(t *testing.T) {
	ex := &SpawnExecutor{
		HarnessPath: "/bin/true",
		HomeResolver: AspectHomeResolverFunc(func(string) (string, bool) {
			return "", false
		}),
	}
	_, err := ex.Execute(context.Background(), frames.DispatchPayload{
		Aspect: "unknown",
	})
	if err == nil || !strings.Contains(err.Error(), "not locally resolvable") {
		t.Errorf("err = %v, want not-resolvable", err)
	}
}

func TestLastJSONLine(t *testing.T) {
	cases := map[string]string{
		"single line": `{"ok":true}`,
		"trailing newline": `{"ok":true}
`,
		"log + json": `starting up
configuring
{"ok":true}`,
		"multiple json — takes last": `{"first":1}
{"second":2}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			line := lastJSONLine([]byte(input))
			if len(line) == 0 {
				t.Errorf("no line returned for %q", input)
			}
			if line[0] != '{' {
				t.Errorf("line doesn't start with {, got %q", line)
			}
		})
	}
}

func TestLastJSONLineEmpty(t *testing.T) {
	if line := lastJSONLine([]byte("")); line != nil {
		t.Errorf("empty input should return nil, got %q", line)
	}
	if line := lastJSONLine([]byte("no json here\nat all\n")); line != nil {
		t.Errorf("no-json input should return nil, got %q", line)
	}
}

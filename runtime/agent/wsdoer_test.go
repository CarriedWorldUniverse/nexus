package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/cwb-client/herald"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

func TestWSDoerRelaysAndDecodes(t *testing.T) {
	var gotPillar, gotPath, gotMethod string
	nx := newFakeNexus(t, "tok")
	nx.onCWB = func(p frames.CWBRequestPayload) frames.CWBResponsePayload {
		gotPillar, gotPath, gotMethod = p.Pillar, p.Path, p.Method
		return frames.CWBResponsePayload{Status: 200, Body: []byte(`{"id":"a1","fingerprint":"fp1"}`)}
	}
	a := newAgent(t, nx.URL(), "tok", &mockProvider{reply: "ok"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	// wait for registration (mirror the existing poll pattern in this file)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.registers.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.registers.Load() == 0 {
		t.Fatal("agent never registered")
	}

	d := a.CWBDoer()
	ag, err := herald.GetAgentByFingerprint(context.Background(), d, "fp1")
	if err != nil {
		t.Fatalf("over wsDoer: %v", err)
	}
	if ag.ID != "a1" || gotPillar != "herald" || gotPath != "/api/agents/by-fingerprint/fp1" || gotMethod != http.MethodGet {
		t.Fatalf("ag=%+v pillar=%q path=%q method=%q", ag, gotPillar, gotPath, gotMethod)
	}
}

func TestWSDoerSurfacesRelayError(t *testing.T) {
	nx := newFakeNexus(t, "tok")
	nx.cwbErr = "herald exploded"
	a := newAgent(t, nx.URL(), "tok", &mockProvider{reply: "ok"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && nx.registers.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if nx.registers.Load() == 0 {
		t.Fatal("agent never registered")
	}
	_, err := herald.GetAgentByFingerprint(context.Background(), a.CWBDoer(), "fp1")
	if err == nil {
		t.Fatal("expected error from relayed cwb.request.error")
	}
	if !strings.Contains(err.Error(), "herald exploded") {
		t.Fatalf("err = %v, want it to contain the relayed message", err)
	}
}

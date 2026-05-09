package frame

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

func newFrameAspect(t *testing.T, name string) *FrameAspect {
	t.Helper()
	return &FrameAspect{
		Path: t.TempDir(),
		Name: name,
		Config: schemas.AspectConfig{
			Name:           name,
			Role:           schemas.RoleFrame,
			ContextMode:    schemas.ContextGlobal,
			Provider:       "claude-api",
			ProviderConfig: map[string]any{"model": "claude-opus-4-7"},
			Capabilities:   []string{"bootstrap", "broadcast", "admin"},
		},
	}
}

func TestEmbed_HappyPath(t *testing.T) {
	r := roster.New()
	ts := broker.NewTokenStore()

	e, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "frame"),
		Roster:     r,
		TokenStore: ts,
		DB:         nil, // in-memory token mode
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if e.Aspect.Name != "frame" {
		t.Errorf("Aspect.Name = %q want frame", e.Aspect.Name)
	}
	if e.AdminToken == "" {
		t.Error("AdminToken empty")
	}
	if e.SessionID == "" {
		t.Error("SessionID empty")
	}
	if e.State == nil {
		t.Fatal("State nil")
	}

	// Roster should now contain the Frame.
	got, ok := r.Get("frame")
	if !ok {
		t.Fatal("Frame not in roster after Embed")
	}
	if got.SessionID != e.SessionID {
		t.Errorf("roster session = %q want %q", got.SessionID, e.SessionID)
	}
	if got.PID != os.Getpid() {
		t.Errorf("roster PID = %d want %d", got.PID, os.Getpid())
	}
	if got.ContextMode != schemas.ContextGlobal {
		t.Errorf("roster ContextMode = %q want global", got.ContextMode)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("roster Model = %q want claude-opus-4-7", got.Model)
	}

	// Token resolves with admin=true.
	info, ok := ts.ResolveToken(e.AdminToken)
	if !ok {
		t.Fatal("AdminToken does not resolve")
	}
	if info.AgentID != "frame" {
		t.Errorf("AdminToken AgentID = %q want frame", info.AgentID)
	}
	if !info.Admin {
		t.Error("AdminToken Admin = false; should be true")
	}
}

func TestEmbed_NilDetected(t *testing.T) {
	_, err := Embed(context.Background(), EmbedConfig{
		Roster:     roster.New(),
		TokenStore: broker.NewTokenStore(),
	})
	if !errors.Is(err, ErrEmbedRequiresFrame) {
		t.Fatalf("expected ErrEmbedRequiresFrame, got %v", err)
	}
}

func TestEmbed_MissingDeps(t *testing.T) {
	cases := []struct {
		name string
		cfg  EmbedConfig
	}{
		{"no roster", EmbedConfig{Detected: newFrameAspect(t, "frame"), TokenStore: broker.NewTokenStore()}},
		{"no token store", EmbedConfig{Detected: newFrameAspect(t, "frame"), Roster: roster.New()}},
	}
	for _, tc := range cases {
		_, err := Embed(context.Background(), tc.cfg)
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestEmbed_OperatorChosenName(t *testing.T) {
	// The Frame's identity comes from aspect.json, not a hardcoded
	// "frame" constant. Embed with a non-default name should produce a
	// roster entry + admin token under that name.
	r := roster.New()
	ts := broker.NewTokenStore()

	e, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "anchor"),
		Roster:     r,
		TokenStore: ts,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if e.Aspect.Name != "anchor" {
		t.Errorf("Aspect.Name = %q", e.Aspect.Name)
	}
	if _, ok := r.Get("anchor"); !ok {
		t.Error("anchor not in roster")
	}
	info, ok := ts.ResolveToken(e.AdminToken)
	if !ok || info.AgentID != "anchor" || !info.Admin {
		t.Errorf("token resolve = %+v ok=%v want anchor+admin", info, ok)
	}
}

func TestEmbed_Heartbeat(t *testing.T) {
	r := roster.New()
	ts := broker.NewTokenStore()
	e, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "frame"),
		Roster:     r,
		TokenStore: ts,
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	before, _ := r.Get("frame")
	time.Sleep(10 * time.Millisecond)
	if err := e.Heartbeat(r, time.Now().UTC()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	after, _ := r.Get("frame")
	if !after.LastHeartbeat.After(before.LastHeartbeat) {
		t.Errorf("heartbeat did not advance LastHeartbeat: before=%v after=%v",
			before.LastHeartbeat, after.LastHeartbeat)
	}
}

func TestEmbed_EmptyNameRejected(t *testing.T) {
	bad := newFrameAspect(t, "")
	bad.Name = "" // ensure both struct fields blank
	bad.Config.Name = ""

	_, err := Embed(context.Background(), EmbedConfig{
		Detected:   bad,
		Roster:     roster.New(),
		TokenStore: broker.NewTokenStore(),
	})
	if err == nil {
		t.Fatal("expected error on empty name")
	}
}

func TestEmbed_RegistersAdminCapability(t *testing.T) {
	r := roster.New()
	ts := broker.NewTokenStore()
	_, err := Embed(context.Background(), EmbedConfig{
		Detected:   newFrameAspect(t, "frame"),
		Roster:     r,
		TokenStore: ts,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("frame")
	hasAdmin := false
	for _, c := range got.Capabilities {
		if c == "admin" {
			hasAdmin = true
		}
	}
	if !hasAdmin {
		t.Errorf("Frame roster entry missing admin capability: %v", got.Capabilities)
	}
}

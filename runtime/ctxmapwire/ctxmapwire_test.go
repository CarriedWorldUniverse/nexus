package ctxmapwire

import (
	"path/filepath"
	"testing"
)

func TestResolveDisabledByDefault(t *testing.T) {
	t.Setenv("CTXMAP_ENABLED", "")
	if Resolve("/home/asp", "anvil").Enabled {
		t.Fatal("ctxmap must be OFF unless CTXMAP_ENABLED is set")
	}
	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Setenv("CTXMAP_ENABLED", v)
		if !Resolve("/home/asp", "anvil").Enabled {
			t.Fatalf("CTXMAP_ENABLED=%q should enable", v)
		}
	}
	t.Setenv("CTXMAP_ENABLED", "0")
	if Resolve("/home/asp", "anvil").Enabled {
		t.Fatal("CTXMAP_ENABLED=0 must stay off")
	}
}

func TestResolveDefaultsAndOverrides(t *testing.T) {
	t.Setenv("CTXMAP_ENABLED", "1")
	t.Setenv("CTXMAP_MODELS", "")
	t.Setenv("CTXMAP_EXTRACT_MODEL", "")
	t.Setenv("CTXMAP_STORE", "")
	t.Setenv("CTXMAP_THREADS", "")
	c := Resolve("/home/asp", "anvil")
	if c.Extract != filepath.Join("/models", "Qwen3-1.7B-Q8_0.gguf") {
		t.Fatalf("extract default from /models PVC mount, got %q", c.Extract)
	}
	if c.StorePath != filepath.Join("/home/asp", "ctxmap.db") {
		t.Fatalf("store defaults under aspect home, got %q", c.StorePath)
	}
	if c.Session != "anvil" || c.Threads != 8 {
		t.Fatalf("session/threads defaults wrong: %q %d", c.Session, c.Threads)
	}
	// overrides win
	t.Setenv("CTXMAP_MODELS", "/pvc/m")
	t.Setenv("CTXMAP_EXTRACT_MODEL", "/custom/x.gguf")
	t.Setenv("CTXMAP_THREADS", "4")
	c = Resolve("/home/asp", "anvil")
	if c.Extract != "/custom/x.gguf" {
		t.Fatalf("explicit extract override ignored, got %q", c.Extract)
	}
	if c.Kind != filepath.Join("/pvc/m", "Qwen3-4B-Q8_0.gguf") {
		t.Fatalf("kind should derive from CTXMAP_MODELS, got %q", c.Kind)
	}
	if c.Threads != 4 {
		t.Fatalf("threads override ignored, got %d", c.Threads)
	}
}

func TestNopHandleIsSafe(t *testing.T) {
	h := Nop()
	h.AttachTo(nil) // must not panic
	if err := h.Close(); err != nil {
		t.Fatalf("nop close: %v", err)
	}
}

package aspects

import (
	"testing"
	"time"
)

func TestMintSessionFor_ReusesAspectIdentity(t *testing.T) {
	cfg := RefreshConfig{
		NexusID:              "nx-test",
		SessionSigningSecret: []byte("test-secret-must-be-32-bytes-min"),
		NewSessionID:         func() string { return "ses-new" },
		Now:                  func() time.Time { return time.Unix(1_700_000_000, 0) },
		JWTTTL:               24 * time.Hour,
	}
	aspect := &Aspect{Name: "alpha", CurrentKeyfileVersion: 3}

	out, err := MintSessionFor(cfg, aspect)
	if err != nil {
		t.Fatalf("MintSessionFor: %v", err)
	}
	if out.SessionJWT == "" {
		t.Fatal("empty JWT")
	}
	if out.Claims.Sub != "alpha" {
		t.Fatalf("sub = %q, want alpha", out.Claims.Sub)
	}
	if out.Claims.Kfv != 3 {
		t.Fatalf("kfv = %d, want 3", out.Claims.Kfv)
	}
	if got := out.ExpiresAt.Unix(); got != 1_700_000_000+86400 {
		t.Fatalf("expiry = %d, want %d", got, 1_700_000_000+86400)
	}
}

func TestMintSessionFor_RejectsInvalidConfig(t *testing.T) {
	_, err := MintSessionFor(RefreshConfig{}, &Aspect{Name: "x"})
	if err == nil {
		t.Fatal("expected error on empty config")
	}
}

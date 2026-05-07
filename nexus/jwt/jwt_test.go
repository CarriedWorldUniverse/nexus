package jwt

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("test-secret-32-bytes-long-padding")

func sampleClaims(now time.Time) Claims {
	return Claims{
		Iss: "nexus://550e8400-e29b-41d4-a716-446655440000",
		Sub: "plumb",
		Iat: now.Unix(),
		Exp: now.Add(1 * time.Hour).Unix(),
		Kfv: 7,
		Ses: "ses-uuid",
	}
}

// TestSign_VerifyRoundTrip — primary contract: a token Sign produces
// must Verify back to the exact claims.
func TestSign_VerifyRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	c := sampleClaims(now)

	tok, err := Sign(testSecret, c)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts; want 3", len(parts))
	}

	// Verify at the same instant — exp is 1h ahead so the token is live.
	got, err := Verify(testSecret, tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Sub != c.Sub || got.Iss != c.Iss || got.Kfv != c.Kfv ||
		got.Iat != c.Iat || got.Exp != c.Exp || got.Ses != c.Ses {
		t.Errorf("claims mismatch: got %+v want %+v", got, c)
	}
}

// TestVerify_RejectsExpired — exp <= now must fail. This is the primary
// session-rotation safety; without it a 1h JWT lives forever.
func TestVerify_RejectsExpired(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	c := Claims{Iss: "x", Sub: "y", Iat: now.Unix(), Exp: now.Unix() - 1, Kfv: 1, Ses: "s"}

	tok, err := Sign(testSecret, c)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	_, err = Verify(testSecret, tok, now)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify expired = %v; want ErrInvalidToken", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error %q does not say 'expired'", err)
	}
}

// TestVerify_RejectsBadSignature — a tampered signature must fail.
// hmac.Equal is the constant-time compare; this confirms verification
// reaches it (rather than e.g. skipping when claims look reasonable).
func TestVerify_RejectsBadSignature(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	tok, err := Sign(testSecret, sampleClaims(now))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Flip a bit in the signature segment.
	parts := strings.Split(tok, ".")
	tamperedSig := parts[2]
	if tamperedSig[0] == 'A' {
		tamperedSig = "B" + tamperedSig[1:]
	} else {
		tamperedSig = "A" + tamperedSig[1:]
	}
	tampered := parts[0] + "." + parts[1] + "." + tamperedSig

	_, err = Verify(testSecret, tampered, now)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify tampered = %v; want ErrInvalidToken", err)
	}
}

// TestVerify_RejectsTamperedClaims — flip a bit in the claims segment.
// Must fail (signature won't match recomputed input).
func TestVerify_RejectsTamperedClaims(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	tok, err := Sign(testSecret, sampleClaims(now))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + parts[1] + "X" + "." + parts[2]
	_, err = Verify(testSecret, tampered, now)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify tampered claims = %v; want ErrInvalidToken", err)
	}
}

// TestVerify_RejectsAlgNone — the canonical JWT exploit. Header
// alg="none" must NEVER be accepted, regardless of how convincing the
// rest of the token looks.
func TestVerify_RejectsAlgNone(t *testing.T) {
	// Hand-craft a token with alg=none and an empty signature.
	hdr := `{"alg":"none","typ":"JWT"}`
	cls := `{"sub":"plumb","exp":99999999999}`
	tok := b64([]byte(hdr)) + "." + b64([]byte(cls)) + "."
	_, err := Verify(testSecret, tok, time.Now())
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify alg=none = %v; want ErrInvalidToken (this is the textbook JWT exploit — DO NOT loosen)", err)
	}
}

// TestVerify_RejectsAlgRS256 — same alg-confusion concern for the
// asymmetric variant. Reject anything not HS256.
func TestVerify_RejectsAlgRS256(t *testing.T) {
	hdr := `{"alg":"RS256","typ":"JWT"}`
	cls := `{"sub":"plumb","exp":99999999999}`
	tok := b64([]byte(hdr)) + "." + b64([]byte(cls)) + "." + b64([]byte("fakesig"))
	_, err := Verify(testSecret, tok, time.Now())
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify alg=RS256 = %v; want ErrInvalidToken", err)
	}
}

// TestVerify_RejectsWrongSecret — different secret = same claims =
// different signature. Verification must reject. Critical: a Nexus
// instance whose session_signing_secret leaks to another deployment
// must not accept tokens signed by anyone else.
func TestVerify_RejectsWrongSecret(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	tok, err := Sign(testSecret, sampleClaims(now))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	_, err = Verify([]byte("different-secret"), tok, now)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify with wrong secret = %v; want ErrInvalidToken", err)
	}
}

// TestSign_RejectsEmptySecret — guard against accidental empty-secret
// signing during boot if SessionSigningSecret was never loaded.
func TestSign_RejectsEmptySecret(t *testing.T) {
	_, err := Sign(nil, sampleClaims(time.Now()))
	if err == nil {
		t.Error("Sign with empty secret should error; got nil")
	}
}

// TestVerify_RejectsMalformed — three-part check.
func TestVerify_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"abc.def",
		"abc.def.ghi.jkl",
	}
	for _, tc := range cases {
		_, err := Verify(testSecret, tc, time.Now())
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%q) = %v; want ErrInvalidToken", tc, err)
		}
	}
}

// Package jwt is a minimal HS256 JWT signer + verifier for Nexus
// session tokens. Stdlib-only — we only need one algorithm (HS256), one
// signing path, and one verification path. Pulling in a generic JWT
// library would be more surface area than the spec needs.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §5:
// claims are { iss, sub, iat, exp, kfv, ses }, signed with the
// session_signing_secret from nexus_identity.

package jwt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidToken covers any malformed or unverifiable token. We don't
// expose more granular sentinels (expired vs. bad-signature vs.
// malformed) at this layer — surface a single rejection reason and let
// the validation handler decide whether to log details.
var ErrInvalidToken = errors.New("jwt: invalid token")

// Claims is the Nexus session-token payload. Field tags are JWT-standard
// where applicable; kfv/ses are Nexus-specific.
type Claims struct {
	// Iss is "nexus://<nexus_id>". Lets a Frame federation distinguish
	// tokens issued by different Nexus instances.
	Iss string `json:"iss"`

	// Sub is the aspect_name the token authorises (e.g. "plumb").
	Sub string `json:"sub"`

	// Iat is issued-at as Unix seconds.
	Iat int64 `json:"iat"`

	// Exp is expiry as Unix seconds. Validation rejects any token where
	// now >= exp.
	Exp int64 `json:"exp"`

	// Kfv is the keyfile version this token was issued against. Lets
	// the broker reject a token for a stale keyfile without doing a
	// fresh DB lookup on every request — the token carries the version
	// it was minted at, and a roster-side check during keyfile
	// re-mints (Part 5+) bumps a tombstone.
	Kfv int64 `json:"kfv"`

	// Ses is a UUID identifying this session. Used for supersede
	// signalling (Part 5+) and for log correlation.
	Ses string `json:"ses"`
}

// header is the JWT header. We pin alg=HS256 + typ=JWT; the field is
// non-exported because we never accept "alg=none" or any other algorithm.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Sign assembles a HS256 JWT. Returns the compact-serialisation token.
// Caller is responsible for filling Iat/Exp; Sign does not stamp times
// (so tests can pin clock).
func Sign(secret []byte, c Claims) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("jwt: secret empty")
	}
	hdr, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("jwt: marshal header: %w", err)
	}
	claimBytes, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("jwt: marshal claims: %w", err)
	}
	signingInput := b64(hdr) + "." + b64(claimBytes)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)
	return signingInput + "." + b64(sig), nil
}

// Verify parses + verifies a JWT and returns the claims. Rejects any
// token whose alg is not HS256, signature doesn't match, or exp <= now.
// `now` is injected so tests can pin clock; production callers pass
// time.Now().
func Verify(secret []byte, token string, now time.Time) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: parts=%d", ErrInvalidToken, len(parts))
	}

	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header decode: %v", ErrInvalidToken, err)
	}
	var hdr header
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header unmarshal: %v", ErrInvalidToken, err)
	}
	// Pin alg=HS256. Reject everything else, including "none" — the
	// classic JWT alg-confusion CVE goes here if we let it.
	if hdr.Alg != "HS256" {
		return nil, fmt.Errorf("%w: alg=%q (want HS256)", ErrInvalidToken, hdr.Alg)
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	wantSig := mac.Sum(nil)
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature decode: %v", ErrInvalidToken, err)
	}
	// hmac.Equal is constant-time; never use bytes.Equal here.
	if !hmac.Equal(wantSig, gotSig) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidToken)
	}

	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: claims decode: %v", ErrInvalidToken, err)
	}
	var c Claims
	if err := json.Unmarshal(claimBytes, &c); err != nil {
		return nil, fmt.Errorf("%w: claims unmarshal: %v", ErrInvalidToken, err)
	}
	if c.Exp == 0 || now.Unix() >= c.Exp {
		return nil, fmt.Errorf("%w: expired (exp=%d, now=%d)", ErrInvalidToken, c.Exp, now.Unix())
	}
	return &c, nil
}

// b64 is JWT's base64url-no-padding encoding (RFC 7515 §2).
func b64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

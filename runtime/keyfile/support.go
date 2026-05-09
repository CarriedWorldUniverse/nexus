package keyfile

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// readFile is a thin wrapper so Load can be tested against an
// in-memory fixture by overriding via testFileReader (test-only).
var readFile = os.ReadFile

// jwtSub extracts the `sub` claim from an HS256 JWT *without* verifying
// the signature. agentfunnel doesn't have the session_signing_secret
// — only the Nexus does — so signature verification isn't possible
// here. We trust the JWT because:
//
//  1. We just received it over a TLS connection to the verified Nexus
//     (cert + nexus_id check both passed).
//  2. We're not using the JWT to authorise our own actions; we're
//     just reading the aspect_name claim for log/register frame use.
//  3. The Nexus that issued the JWT will verify its own signature on
//     every subsequent request that uses the JWT as a bearer.
//
// So this is a parse, not a verify. If the Nexus issued garbage, the
// next /connect would fail anyway.
func jwtSub(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("jwtSub: token does not have 3 parts")
	}
	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("jwtSub: decode claims: %w", err)
	}
	var c struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimBytes, &c); err != nil {
		return "", fmt.Errorf("jwtSub: unmarshal claims: %w", err)
	}
	if c.Sub == "" {
		return "", errors.New("jwtSub: sub claim empty")
	}
	return c.Sub, nil
}

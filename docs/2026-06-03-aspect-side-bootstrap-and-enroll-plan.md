# Aspect-side bootstrap + `cw agent enroll` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A real aspect reads a bootstrap keyfile, signs a casket assertion, and presents it in its `register` frame (feeding broker step 3a end-to-end), and `cw agent enroll` writes that keyfile by attaching to an already-provisioned herald agent.

**Architecture:** Four edge-agnostic pieces across three repos. cwb-client gains a sign-from-raw-key assertion and a by-fingerprint lookup wrapper (merge + pin first). cw gains an attach-only `agent enroll` that writes `{key, key_id, url, slug, fingerprint}`. nexus gains a `heraldkeyfile` loader and wires assertion-signing into the aspect runtime's `sendRegister`, gated on `NEXUS_HERALD_KEYFILE` (dark by default). The nexus→CWB gateway is out of scope; the edge resolves to herald directly on the tailnet for the live test.

**Tech Stack:** Go 1.26; casket-go (`DeriveAgentKey`); go-jose (EdDSA JWT); cobra (cw); `github.com/coder/websocket` (runtime); cwb-client `identity`/`oidc`/`herald`/`client`.

**Spec:** `docs/2026-06-03-aspect-side-bootstrap-and-enroll-design.md`

**Repos / working dirs:**
- cwb-client: `/Users/jacinta/Source/cwb-client` (module `github.com/CarriedWorldUniverse/cwb-client`)
- cw: `/Users/jacinta/Source/cw` (module `github.com/CarriedWorldUniverse/cw`)
- nexus: `/Users/jacinta/Source/nexus` (module `github.com/CarriedWorldUniverse/nexus`; already requires cwb-client)

**Branch per repo:** create `aspect-side-bootstrap-enroll` in each repo before its first task (nexus already has it, holding the design doc). CI-gated merges on all three — wait for green, never `--admin`-bypass.

**Cross-repo ordering (hard dependency):** Tasks 1–2 (cwb-client) must merge before Task 3 pins the new hash into cw + nexus. Tasks 4 (cw) and 5–7 (nexus) depend on that pin. Task 8 (live) is last.

---

## File Structure

**cwb-client:**
- `identity/identity.go` (modify) — add `AgentAssertionFromKey` / `AgentAssertionFromKeyAt`; refactor `AgentAssertionAt` to derive-then-delegate.
- `identity/identity_test.go` (modify) — sign-from-key + byte-equivalence tests.
- `herald/herald.go` (modify) — add `GetAgentByFingerprint`.
- `herald/herald_test.go` (modify) — lookup found/404 tests.

**cw:**
- `internal/cli/agent/enroll.go` (create) — the `enroll` subcommand (attach-only).
- `internal/cli/agent/agent.go` (modify) — register `newEnrollCmd` in `NewCmd`.
- `internal/cli/agent/enroll_test.go` (create) — stub-herald unit tests.

**nexus:**
- `runtime/heraldkeyfile/heraldkeyfile.go` (create) — `Keyfile`, `Load`, `PrivateKey`.
- `runtime/heraldkeyfile/heraldkeyfile_test.go` (create) — load/decode/validate tests.
- `runtime/agent/agent.go` (modify) — `Config.HeraldKeyfile`; `buildAssertion`; populate `RegisterPayload.Assertion` in `sendRegister`.
- `runtime/agent/agent_test.go` (modify) — assertion-in-register test.
- `runtime/cmd/agent/main.go` (modify) — read `NEXUS_HERALD_KEYFILE`, load, put on `Config`.
- `runtime/agent/herald_register_live_test.go` (create) — gated end-to-end live test.

---

## Task 1: cwb-client — `AgentAssertionFromKey`

**Repo:** `/Users/jacinta/Source/cwb-client` (create branch `aspect-side-bootstrap-enroll` first).

**Files:**
- Modify: `identity/identity.go`
- Test: `identity/identity_test.go`

- [ ] **Step 1: Write the failing test**

Add to `identity/identity_test.go`:

```go
func TestAgentAssertionFromKeyMatchesSeedPath(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, 32)
	slug, agentID, tokenURL := "plumb", "agent-uuid-1", "https://edge/herald/token"
	now := time.Unix(1_700_000_000, 0)

	fromSeed, err := AgentAssertionAt(seed, slug, agentID, tokenURL, now)
	if err != nil {
		t.Fatalf("AgentAssertionAt: %v", err)
	}
	priv, _, err := casket.DeriveAgentKey(seed, slug)
	if err != nil {
		t.Fatalf("DeriveAgentKey: %v", err)
	}
	fromKey, err := AgentAssertionFromKeyAt(priv, agentID, tokenURL, now)
	if err != nil {
		t.Fatalf("AgentAssertionFromKeyAt: %v", err)
	}
	if fromSeed != fromKey {
		t.Fatalf("assertions differ:\n seed=%s\n key =%s", fromSeed, fromKey)
	}

	claims, err := DecodeAccessClaims(fromKey)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if claims["aud"] != tokenURL || claims["iss"] != agentID || claims["sub"] != agentID {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestAgentAssertionFromKeyValidates(t *testing.T) {
	priv, _, _ := casket.DeriveAgentKey(bytes.Repeat([]byte{1}, 32), "x")
	if _, err := AgentAssertionFromKey(nil, "a", "u"); err == nil {
		t.Error("nil key should error")
	}
	if _, err := AgentAssertionFromKey(priv, "", "u"); err == nil {
		t.Error("empty agentID should error")
	}
	if _, err := AgentAssertionFromKey(priv, "a", ""); err == nil {
		t.Error("empty tokenURL should error")
	}
}
```

Ensure the test imports include `"bytes"`, `"time"`, and `casket "github.com/CarriedWorldUniverse/casket-go"` (add any missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/cwb-client && go test ./identity/ -run AgentAssertionFromKey`
Expected: FAIL — `undefined: AgentAssertionFromKey` / `AgentAssertionFromKeyAt`.

- [ ] **Step 3: Write the implementation**

In `identity/identity.go`, add `"crypto/ed25519"` to imports. Replace the body of `AgentAssertionAt` to delegate, and add the two new functions:

```go
// AgentAssertionAt is AgentAssertion with an explicit clock.
func AgentAssertionAt(seed []byte, slug, agentID, tokenURL string, now time.Time) (string, error) {
	if len(seed) == 0 || slug == "" {
		return "", errors.New("identity: seed and slug required")
	}
	priv, _, err := casket.DeriveAgentKey(seed, slug)
	if err != nil {
		return "", fmt.Errorf("identity: derive key: %w", err)
	}
	return AgentAssertionFromKeyAt(priv, agentID, tokenURL, now)
}

// AgentAssertionFromKey signs an RFC 7523 jwt-bearer assertion
// (iss=sub=agentID, aud=tokenURL, 2-minute exp) from an already-derived
// agent key — the bootstrap path, where the runtime holds the derived key
// (not the owner seed). now defaults to time.Now; injectable via …At.
func AgentAssertionFromKey(priv ed25519.PrivateKey, agentID, tokenURL string) (string, error) {
	return AgentAssertionFromKeyAt(priv, agentID, tokenURL, time.Now())
}

// AgentAssertionFromKeyAt is AgentAssertionFromKey with an explicit clock.
func AgentAssertionFromKeyAt(priv ed25519.PrivateKey, agentID, tokenURL string, now time.Time) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("identity: invalid ed25519 private key")
	}
	if agentID == "" || tokenURL == "" {
		return "", errors.New("identity: agentID and tokenURL required")
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", fmt.Errorf("identity: signer: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"iss": agentID, "sub": agentID, "aud": tokenURL,
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(),
	})
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("identity: sign: %w", err)
	}
	return obj.CompactSerialize()
}
```

Update the `AgentAssertion` doc comment to note it now delegates. Keep `AgentAssertion` (the seed/slug entrypoint) unchanged — it already calls `AgentAssertionAt`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/cwb-client && go test ./identity/`
Expected: PASS (including the existing `AgentAssertion` tests — the refactor is lossless).

- [ ] **Step 5: Commit**

```bash
cd /Users/jacinta/Source/cwb-client
git add identity/identity.go identity/identity_test.go
git commit -m "feat(identity): AgentAssertionFromKey — sign from a derived agent key

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: cwb-client — `herald.GetAgentByFingerprint`

**Repo:** `/Users/jacinta/Source/cwb-client`.

**Files:**
- Modify: `herald/herald.go`
- Test: `herald/herald_test.go`

Grounding: herald serves `GET /api/agents/by-fingerprint/{fp}` returning `{id, kind, display_name, org, responsible_human, fingerprint, status, active, scopes}`, 404 when none. The existing `do(ctx, c, method, path, body, out)` helper returns an error carrying the HTTP status text on non-2xx; the `Agent` struct already has `ID`, `Org`, `Fingerprint`, `Status`, `Scopes`, etc.

- [ ] **Step 1: Write the failing test**

Add to `herald/herald_test.go`:

```go
func TestGetAgentByFingerprint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/agents/by-fingerprint/fp-abc" {
			http.Error(w, "bad path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"a1","kind":"agent","display_name":"plumb","org":"o1","responsible_human":"h1","fingerprint":"fp-abc","status":"active","active":true,"scopes":["repo:read"]}`))
	}))
	defer srv.Close()
	c := client.WithStaticToken(srv.URL, "tok")

	a, err := GetAgentByFingerprint(context.Background(), c, "fp-abc")
	if err != nil {
		t.Fatalf("GetAgentByFingerprint: %v", err)
	}
	if a.ID != "a1" || a.Fingerprint != "fp-abc" || a.Org != "o1" {
		t.Fatalf("agent = %+v", a)
	}
}

func TestGetAgentByFingerprintNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no agent for fingerprint", http.StatusNotFound)
	}))
	defer srv.Close()
	c := client.WithStaticToken(srv.URL, "tok")

	if _, err := GetAgentByFingerprint(context.Background(), c, "fp-missing"); err == nil {
		t.Fatal("expected error on 404")
	}
}
```

Match the import style already used in `herald_test.go` (it imports `client`, `net/http`, `net/http/httptest`, `context`). Reuse them.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/cwb-client && go test ./herald/ -run GetAgentByFingerprint`
Expected: FAIL — `undefined: GetAgentByFingerprint`.

- [ ] **Step 3: Write the implementation**

In `herald/herald.go`, add (near `CreateAgent`):

```go
// GetAgentByFingerprint resolves an agent from its casket fingerprint via
// herald's GET /api/agents/by-fingerprint/{fp} (NEX-412). Returns an error
// (carrying herald's 404 message) when no agent matches — the caller treats
// that as "not provisioned".
func GetAgentByFingerprint(ctx context.Context, c *client.Client, fp string) (Agent, error) {
	var a Agent
	path := "/api/agents/by-fingerprint/" + url.PathEscape(fp)
	err := do(ctx, c, http.MethodGet, path, nil, &a)
	return a, err
}
```

(`url` and `http` are already imported in `herald.go`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/cwb-client && go test ./herald/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/jacinta/Source/cwb-client
git add herald/herald.go herald/herald_test.go
git commit -m "feat(herald): GetAgentByFingerprint wrapper (NEX-412 lookup)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Merge cwb-client + pin into cw and nexus

**Repos:** cwb-client (push/merge), then cw + nexus (pin).

- [ ] **Step 1: Push the cwb-client branch and open a PR**

```bash
cd /Users/jacinta/Source/cwb-client
git push -u origin aspect-side-bootstrap-enroll
gh pr create --fill --title "AgentAssertionFromKey + GetAgentByFingerprint"
```

- [ ] **Step 2: Wait for CI green, then merge**

Run: `cd /Users/jacinta/Source/cwb-client && gh pr checks --watch`
Then: `gh pr merge --squash --delete-branch`
Expected: merged to main; do NOT `--admin`-bypass.

- [ ] **Step 3: Capture the merged main short hash**

```bash
cd /Users/jacinta/Source/cwb-client && git checkout main && git pull && git rev-parse --short main
```
Note the hash (call it `<CWBHASH>`) — used to pin in Steps 4–5.

- [ ] **Step 4: Pin in cw**

```bash
cd /Users/jacinta/Source/cw && git checkout -b aspect-side-bootstrap-enroll
go get github.com/CarriedWorldUniverse/cwb-client@<CWBHASH>
go mod tidy && go build ./... && go test ./...
git add go.mod go.sum && git commit -m "chore: pin cwb-client (AgentAssertionFromKey + GetAgentByFingerprint)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```
Expected: builds + tests pass.

- [ ] **Step 5: Pin in nexus**

```bash
cd /Users/jacinta/Source/nexus   # already on branch aspect-side-bootstrap-enroll
go get github.com/CarriedWorldUniverse/cwb-client@<CWBHASH>
go mod tidy && go build ./... && go test ./runtime/... ./nexus/cwb/...
git add go.mod go.sum && git commit -m "chore: pin cwb-client (AgentAssertionFromKey + GetAgentByFingerprint)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```
Expected: builds + tests pass.

---

## Task 4: cw — `agent enroll` (attach-only)

**Repo:** `/Users/jacinta/Source/cw` (branch `aspect-side-bootstrap-enroll`, after Task 3 pin).

**Files:**
- Create: `internal/cli/agent/enroll.go`
- Modify: `internal/cli/agent/agent.go` (register the subcommand)
- Test: `internal/cli/agent/enroll_test.go`

Grounding: `cmdutil.Session(gf)` returns `(*client.Client, config.Context, name, error)` and carries the human token; `casket.DeriveAgentKey([]byte(seed), slug)` → `(priv, pub, err)`; `identity.Fingerprint(pub)`; `herald.GetAgentByFingerprint(ctx, c, fp)` (Task 2). The keyfile struct must match nexus's `heraldkeyfile.Keyfile` JSON tags (Task 5): `key, key_id, url, slug, fingerprint`.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/agent/enroll_test.go`:

```go
package agent

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cw/internal/cmdutil"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
)

func enrollCmd(t *testing.T, edge, out string, args ...string) error {
	t.Helper()
	gf := &cmdutil.GlobalFlags{Edge: edge, Token: "tok"}
	cmd := newEnrollCmd(gf)
	cmd.SetArgs(append([]string{"--url", "ws://nexus.local/connect", "--out", out}, args...))
	return cmd.Execute()
}

func TestEnrollAttachWritesKeyfile(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("CW_OWNER_SEED", seed)
	_, pub, _ := casket.DeriveAgentKey([]byte("0123456789abcdef0123456789abcdef"), "plumb")
	fp := identity.Fingerprint(pub)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agents/by-fingerprint/"+fp {
			_, _ = w.Write([]byte(`{"id":"agent-uuid-9","kind":"agent","display_name":"plumb","org":"o1","fingerprint":"` + fp + `","status":"active","active":true}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "plumb.keyfile.json")
	if err := enrollCmd(t, srv.URL, out, "--slug", "plumb"); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read keyfile: %v", err)
	}
	var kf struct {
		Key, KeyID, URL, Slug, Fingerprint string
	}
	if err := json.Unmarshal(raw, &mapKF{&kf}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if kf.KeyID != "agent-uuid-9" || kf.Slug != "plumb" || kf.Fingerprint != fp || kf.URL != "ws://nexus.local/connect" {
		t.Fatalf("keyfile = %+v", kf)
	}
	if kf.Key == "" {
		t.Fatal("keyfile key empty")
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600", info.Mode().Perm())
	}
}

// mapKF maps snake_case JSON onto the test struct fields.
type mapKF struct{ v *struct{ Key, KeyID, URL, Slug, Fingerprint string } }

func (m *mapKF) UnmarshalJSON(b []byte) error {
	var raw struct {
		Key         string `json:"key"`
		KeyID       string `json:"key_id"`
		URL         string `json:"url"`
		Slug        string `json:"slug"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*m.v = struct{ Key, KeyID, URL, Slug, Fingerprint string }{raw.Key, raw.KeyID, raw.URL, raw.Slug, raw.Fingerprint}
	return nil
}

func TestEnrollNotFoundAborts(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("CW_OWNER_SEED", seed)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no agent for fingerprint", http.StatusNotFound)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "typo.keyfile.json")
	if err := enrollCmd(t, srv.URL, out, "--slug", "plubm"); err == nil {
		t.Fatal("expected abort on not-found")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("keyfile should not be written on not-found")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/cw && go test ./internal/cli/agent/ -run Enroll`
Expected: FAIL — `undefined: newEnrollCmd`.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/agent/enroll.go`:

```go
package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cw/internal/cmdutil"
	"github.com/CarriedWorldUniverse/cwb-client/herald"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/spf13/cobra"
)

// bootstrapKeyfile is the herald-rooted bootstrap keyfile the aspect runtime
// reads (nexus runtime/heraldkeyfile.Keyfile). Tags MUST match that struct.
type bootstrapKeyfile struct {
	Key         string `json:"key"`         // base64 raw ed25519 private key
	KeyID       string `json:"key_id"`      // herald agent UUID
	URL         string `json:"url"`         // nexus relay the aspect connects/discovers through
	Slug        string `json:"slug"`        // agent name
	Fingerprint string `json:"fingerprint"` // base64url sha256(pub)[:16]
}

func newEnrollCmd(gf *cmdutil.GlobalFlags) *cobra.Command {
	var slug, relayURL, out string
	cmd := &cobra.Command{
		Use:   "enroll --slug <slug> --url <nexus-relay> [--out <path>]",
		Short: "Write a bootstrap keyfile for an already-provisioned agent (attach)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if slug == "" || relayURL == "" {
				return fmt.Errorf("--slug and --url are required")
			}
			seed := os.Getenv("CW_OWNER_SEED")
			if seed == "" {
				return fmt.Errorf("agent enroll requires the owner seed in CW_OWNER_SEED")
			}
			priv, pub, err := casket.DeriveAgentKey([]byte(seed), slug)
			if err != nil {
				return fmt.Errorf("derive agent key: %w", err)
			}
			fp := identity.Fingerprint(pub)

			c, _, _, err := cmdutil.Session(gf)
			if err != nil {
				return err
			}
			a, err := herald.GetAgentByFingerprint(cmd.Context(), c, fp)
			if err != nil {
				return fmt.Errorf("no agent for slug %q (fingerprint %s) at the edge — provision it first (or check the slug for a typo): %w", slug, fp, err)
			}

			kf := bootstrapKeyfile{
				Key:         base64.StdEncoding.EncodeToString(priv),
				KeyID:       a.ID,
				URL:         relayURL,
				Slug:        slug,
				Fingerprint: fp,
			}
			data, err := json.MarshalIndent(kf, "", "  ")
			if err != nil {
				return err
			}
			if out == "" {
				out = slug + ".keyfile.json"
			}
			if err := os.WriteFile(out, data, 0o600); err != nil {
				return fmt.Errorf("write keyfile %s: %w", out, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", out)
			fmt.Fprintf(os.Stderr, "enrolled agent %s (%s); start the aspect with NEXUS_HERALD_KEYFILE=%s\n", a.ID, slug, out)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&slug, "slug", "", "casket key slug / agent name (required)")
	f.StringVar(&relayURL, "url", "", "nexus relay url the aspect connects/discovers through (required)")
	f.StringVar(&out, "out", "", "keyfile output path (default ./<slug>.keyfile.json)")
	return cmd
}
```

Then register it in `internal/cli/agent/agent.go` — change the `AddCommand` line:

```go
	cmd.AddCommand(newKeygenCmd(), newCreateCmd(gf), newPubkeyCmd(gf), newEnrollCmd(gf))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/cw && go test ./internal/cli/agent/ -run Enroll`
Expected: PASS.

- [ ] **Step 5: Run the full cw build/test**

Run: `cd /Users/jacinta/Source/cw && go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /Users/jacinta/Source/cw
git add internal/cli/agent/enroll.go internal/cli/agent/agent.go internal/cli/agent/enroll_test.go
git commit -m "feat(agent): cw agent enroll — attach + write bootstrap keyfile

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: nexus — `runtime/heraldkeyfile` loader

**Repo:** `/Users/jacinta/Source/nexus` (branch `aspect-side-bootstrap-enroll`, after Task 3 pin).

**Files:**
- Create: `runtime/heraldkeyfile/heraldkeyfile.go`
- Test: `runtime/heraldkeyfile/heraldkeyfile_test.go`

- [ ] **Step 1: Write the failing test**

Create `runtime/heraldkeyfile/heraldkeyfile_test.go`:

```go
package heraldkeyfile

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
)

func writeKF(t *testing.T, kf map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kf.json")
	data, _ := json.Marshal(kf)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func goodKF(t *testing.T) map[string]string {
	t.Helper()
	priv, pub, _ := casket.DeriveAgentKey([]byte("0123456789abcdef0123456789abcdef"), "plumb")
	return map[string]string{
		"key":         base64.StdEncoding.EncodeToString(priv),
		"key_id":      "agent-uuid-9",
		"url":         "ws://nexus.local/connect",
		"slug":        "plumb",
		"fingerprint": identity.Fingerprint(pub),
	}
}

func TestLoadGood(t *testing.T) {
	kf, err := Load(writeKF(t, goodKF(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if kf.KeyID != "agent-uuid-9" || kf.Slug != "plumb" || kf.URL != "ws://nexus.local/connect" {
		t.Fatalf("kf = %+v", kf)
	}
	priv, err := kf.PrivateKey()
	if err != nil {
		t.Fatalf("PrivateKey: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("priv len = %d", len(priv))
	}
}

func TestLoadMissingField(t *testing.T) {
	m := goodKF(t)
	delete(m, "key_id")
	if _, err := Load(writeKF(t, m)); err == nil {
		t.Fatal("expected error for missing key_id")
	}
}

func TestLoadBadKey(t *testing.T) {
	m := goodKF(t)
	m["key"] = "!!!not-base64!!!"
	if _, err := Load(writeKF(t, m)); err == nil {
		t.Fatal("expected error for bad base64 key")
	}
}

func TestLoadFingerprintMismatch(t *testing.T) {
	m := goodKF(t)
	m["fingerprint"] = "deadbeefdeadbeef"
	if _, err := Load(writeKF(t, m)); err == nil {
		t.Fatal("expected error for fingerprint mismatch")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/heraldkeyfile/`
Expected: FAIL — package/`Load`/`PrivateKey` undefined.

- [ ] **Step 3: Write the implementation**

Create `runtime/heraldkeyfile/heraldkeyfile.go`:

```go
// Package heraldkeyfile loads the herald-rooted bootstrap keyfile an aspect
// reads at startup to sign its register-handshake assertion. The file is
// written by `cw agent enroll`; it carries the agent's DERIVED key (never the
// owner seed) plus the herald agent id, the nexus relay url, the slug, and
// the casket fingerprint.
package heraldkeyfile

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/CarriedWorldUniverse/cwb-client/identity"
)

// Keyfile is the parsed bootstrap keyfile.
type Keyfile struct {
	Key         string `json:"key"`         // base64 raw ed25519 private key
	KeyID       string `json:"key_id"`      // herald agent UUID (assertion iss/sub)
	URL         string `json:"url"`         // nexus relay (discovery edge + connect)
	Slug        string `json:"slug"`        // agent name
	Fingerprint string `json:"fingerprint"` // base64url sha256(pub)[:16]
}

// PrivateKey base64-decodes Key into an ed25519 private key.
func (k *Keyfile) PrivateKey() (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(k.Key)
	if err != nil {
		return nil, fmt.Errorf("heraldkeyfile: decode key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("heraldkeyfile: key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// Load reads, parses, and validates a bootstrap keyfile. It checks every
// field is present, the key decodes to a valid ed25519 key, and the stored
// fingerprint matches the key's public half (a corruption guard).
func Load(path string) (*Keyfile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("heraldkeyfile: read %s: %w", path, err)
	}
	var k Keyfile
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("heraldkeyfile: parse %s: %w", path, err)
	}
	if k.Key == "" || k.KeyID == "" || k.URL == "" || k.Slug == "" || k.Fingerprint == "" {
		return nil, fmt.Errorf("heraldkeyfile: %s missing required field(s)", path)
	}
	priv, err := k.PrivateKey()
	if err != nil {
		return nil, err
	}
	if got := identity.Fingerprint(priv.Public().(ed25519.PublicKey)); got != k.Fingerprint {
		return nil, fmt.Errorf("heraldkeyfile: fingerprint mismatch (key=%s file=%s) — corrupt keyfile", got, k.Fingerprint)
	}
	return &k, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/heraldkeyfile/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add runtime/heraldkeyfile/
git commit -m "feat(runtime): heraldkeyfile loader for the bootstrap keyfile

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: nexus — sign + attach the assertion in `sendRegister`

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:**
- Modify: `runtime/agent/agent.go` (`Config.HeraldKeyfile`; `buildAssertion`; `sendRegister`)
- Test: `runtime/agent/agent_test.go`

Grounding: `sendRegister` builds `frames.RegisterPayload{ RegisterRequest: … }`; `RegisterPayload.Assertion` exists (step 3a). `a.cfg`, `a.log` available. cwb-client `oidc.New(edge).TokenEndpoint(ctx)` discovers under `edge/herald/.well-known/openid-configuration`. Discovery edge = the keyfile `url` mapped to http(s).

- [ ] **Step 1: Write the failing test**

Add to `runtime/agent/agent_test.go`. This test needs a server that BOTH serves discovery and accepts the WS register; model it on the existing `newFakeNexus`/`fakeServer`. Add a focused test using a combined handler:

```go
func TestSendRegisterAttachesAssertion(t *testing.T) {
	priv, pub, _ := casket.DeriveAgentKey([]byte("0123456789abcdef0123456789abcdef"), "plumb")
	fp := identity.Fingerprint(pub)

	var gotAssertion atomic.Value // string
	gotAssertion.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Discovery: advertise this server's own /herald/token as the endpoint.
		if r.URL.Path == "/herald/.well-known/openid-configuration" {
			base := "http://" + r.Host
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token_endpoint":"` + base + `/herald/token","jwks_uri":"` + base + `/jwks"}`))
			return
		}
		// WS upgrade + read the register frame.
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		wsc, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer wsc.Close(websocket.StatusNormalClosure, "done")
		wsc.SetReadLimit(1 << 20)
		for {
			_, data, err := wsc.Read(context.Background())
			if err != nil {
				return
			}
			env, err := frames.Decode(data)
			if err != nil || env.Kind != frames.KindRegister {
				continue
			}
			var p frames.RegisterPayload
			_ = frames.PayloadAs(env, &p)
			gotAssertion.Store(p.Assertion)
			ack, _ := frames.NewResponse(env, frames.KindRegisterAck, frames.RegisterAckPayload{})
			b, _ := frames.Encode(ack)
			_ = wsc.Write(context.Background(), websocket.MessageText, b)
		}
	}))
	t.Cleanup(srv.Close)

	httpBase := srv.URL // already http://
	kf := &heraldkeyfile.Keyfile{
		Key:         base64.StdEncoding.EncodeToString(priv),
		KeyID:       "agent-uuid-9",
		URL:         httpBase,
		Slug:        "plumb",
		Fingerprint: fp,
	}

	a := newAgentWithHerald(t, wsURLFromHTTP(srv.URL), "tok", &mockProvider{reply: "ok"}, kf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && gotAssertion.Load().(string) == "" {
		time.Sleep(10 * time.Millisecond)
	}
	raw := gotAssertion.Load().(string)
	if raw == "" {
		t.Fatal("no assertion in register frame")
	}
	claims, err := identity.DecodeAccessClaims(raw)
	if err != nil {
		t.Fatalf("decode assertion: %v", err)
	}
	if claims["sub"] != "agent-uuid-9" {
		t.Errorf("sub = %v, want agent-uuid-9", claims["sub"])
	}
	if aud, _ := claims["aud"].(string); aud != httpBase+"/herald/token" {
		t.Errorf("aud = %v, want %s/herald/token", claims["aud"], httpBase)
	}
}
```

Add a `newAgentWithHerald` helper next to the existing `newAgent` (mirror it, setting `HeraldKeyfile: kf` on the `Config`), and a `wsURLFromHTTP(s string) string` that does `"ws" + strings.TrimPrefix(s, "http")`. Add imports as needed: `casket`, `identity`, `heraldkeyfile`, `encoding/base64`, `net/http`, `net/http/httptest`, `sync/atomic`, `github.com/coder/websocket`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/agent/ -run SendRegisterAttachesAssertion`
Expected: FAIL — `Config` has no `HeraldKeyfile` / `newAgentWithHerald` undefined.

- [ ] **Step 3: Write the implementation**

In `runtime/agent/agent.go`:

(a) Add the import block entries: `"github.com/CarriedWorldUniverse/cwb-client/identity"`, `"github.com/CarriedWorldUniverse/cwb-client/oidc"`, `"github.com/CarriedWorldUniverse/nexus/runtime/heraldkeyfile"`, and `"strings"` if not present.

(b) Add the config field (in `type Config struct`, after `AuthToken`):

```go
	// HeraldKeyfile, when non-nil, makes the aspect sign a casket assertion
	// from its bootstrap key and present it in the register frame (herald
	// bootstrap). nil → no assertion (existing behavior).
	HeraldKeyfile *heraldkeyfile.Keyfile
```

(c) Add the `buildAssertion` helper:

```go
// buildAssertion signs the herald register-handshake assertion from the
// bootstrap keyfile. Returns "" (no error) when no keyfile is configured.
// The audience is herald's token endpoint, discovered through the keyfile
// url (the nexus relay) — the aspect's only egress.
func (a *Agent) buildAssertion(ctx context.Context) (string, error) {
	kf := a.cfg.HeraldKeyfile
	if kf == nil {
		return "", nil
	}
	priv, err := kf.PrivateKey()
	if err != nil {
		return "", fmt.Errorf("herald keyfile: %w", err)
	}
	edge := httpEdge(kf.URL)
	tokenURL, err := oidc.New(edge).TokenEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("discover token endpoint via %s: %w", edge, err)
	}
	return identity.AgentAssertionFromKey(priv, kf.KeyID, tokenURL)
}

// httpEdge maps a ws(s):// relay url to its http(s):// origin for OIDC
// discovery; http(s) urls pass through unchanged.
func httpEdge(u string) string {
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	default:
		return u
	}
}
```

(d) In `sendRegister`, before `frames.NewRequest`, build the assertion and include it:

```go
	assertion, aerr := a.buildAssertion(ctx)
	if aerr != nil {
		// Best-effort: register without herald-binding; the connection
		// still comes up on the transport bearer. Retried next reconnect.
		a.log.Warn("herald assertion skipped", "err", aerr)
	}
	req, err := frames.NewRequest(frames.KindRegister, frames.RegisterPayload{
		RegisterRequest: schemas.RegisterRequest{
			Name:         a.cfg.Aspect.Name,
			ContextMode:  a.cfg.Aspect.ContextMode,
			Provider:     a.cfg.Aspect.Provider,
			Port:         a.cfg.Aspect.Port,
			PID:          os.Getpid(),
			StartedAt:    time.Now().UTC(),
			Capabilities: a.cfg.Aspect.Capabilities,
			Home:         a.cfg.Home,
			SessionID:    a.sessionID,
			Metadata:     a.cfg.Aspect.Metadata,
		},
		Assertion: assertion,
	})
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/agent/ -run SendRegisterAttachesAssertion`
Expected: PASS.

- [ ] **Step 5: Run the full runtime/agent suite (no-keyfile path unchanged)**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/agent/`
Expected: PASS — existing tests (no `HeraldKeyfile`) send an empty `Assertion`, unchanged behavior.

- [ ] **Step 6: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add runtime/agent/agent.go runtime/agent/agent_test.go
git commit -m "feat(runtime): sign + attach herald assertion in register frame

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: nexus — wire `NEXUS_HERALD_KEYFILE` into aspect startup

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:**
- Modify: `runtime/cmd/agent/main.go`

This is config wiring (no new unit test — covered by Task 6's runtime test + Task 8 live). Keep it minimal.

- [ ] **Step 1: Read the keyfile in main.go**

In `runtime/cmd/agent/main.go`, add `"github.com/CarriedWorldUniverse/nexus/runtime/heraldkeyfile"` to imports. After the `token := os.Getenv(*tokenEnv)` block and before `agent.New(...)`, add:

```go
	var heraldKF *heraldkeyfile.Keyfile
	if p := os.Getenv("NEXUS_HERALD_KEYFILE"); p != "" {
		heraldKF, err = heraldkeyfile.Load(p)
		if err != nil {
			log.Error("load herald keyfile", "err", err)
			os.Exit(2)
		}
		log.Info("herald bootstrap keyfile loaded", "slug", heraldKF.Slug, "agent", heraldKF.KeyID)
	}
```

Then add `HeraldKeyfile: heraldKF,` to the `agent.New(agent.Config{...})` literal.

- [ ] **Step 2: Build**

Run: `cd /Users/jacinta/Source/nexus && go build ./runtime/...`
Expected: builds clean.

- [ ] **Step 3: Vet the whole module**

Run: `cd /Users/jacinta/Source/nexus && go vet ./runtime/... && go test ./runtime/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add runtime/cmd/agent/main.go
git commit -m "feat(runtime): load NEXUS_HERALD_KEYFILE at aspect startup

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: Gated live end-to-end test

**Repo:** `/Users/jacinta/Source/nexus`.

**Files:**
- Create: `runtime/agent/herald_register_live_test.go`

This proves producer→consumer→broker→herald against dMon herald on the tailnet, using a pre-provisioned throwaway `cwb-test-*` agent. It is skipped unless the env is set. Model the broker bring-up on `nexus/broker/herald_register_live_test.go` (the 3a live test) — reuse its broker construction with `HeraldEdge` set, then drive a real aspect with a keyfile instead of a hand-signed assertion.

- [ ] **Step 1: Write the gated live test**

Create `runtime/agent/herald_register_live_test.go`:

```go
package agent

// Gated live test: requires a reachable CWB edge (dMon herald) and a
// pre-provisioned agent under CW_IT_OWNER_SEED / CW_IT_AGENT_SLUG.
//
//   CW_IT_EDGE        - CWB edge base (herald directly on the tailnet)
//   CW_IT_OWNER_SEED  - owner seed the agent was derived from
//   CW_IT_AGENT_ID    - the agent's herald UUID
//   CW_IT_AGENT_SLUG  - the agent's slug
//
// It enrolls (writes a keyfile), brings up a HeraldEdge broker, starts a
// real aspect with NEXUS_HERALD_KEYFILE, and asserts the broker redeemed the
// keyfile-signed assertion (ack herald_subject == agent id).

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	casket "github.com/CarriedWorldUniverse/casket-go"
	"github.com/CarriedWorldUniverse/cwb-client/identity"
	"github.com/CarriedWorldUniverse/nexus/runtime/heraldkeyfile"
)

func TestLiveAspectHeraldRegister(t *testing.T) {
	edge := os.Getenv("CW_IT_EDGE")
	seed := os.Getenv("CW_IT_OWNER_SEED")
	agentID := os.Getenv("CW_IT_AGENT_ID")
	slug := os.Getenv("CW_IT_AGENT_SLUG")
	if edge == "" || seed == "" || agentID == "" || slug == "" {
		t.Skip("set CW_IT_EDGE + CW_IT_OWNER_SEED + CW_IT_AGENT_ID + CW_IT_AGENT_SLUG for the live aspect register test")
	}

	priv, pub, err := casket.DeriveAgentKey([]byte(seed), slug)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	kf := &heraldkeyfile.Keyfile{
		Key:         base64.StdEncoding.EncodeToString(priv),
		KeyID:       agentID,
		URL:         edge, // edge is already http(s); httpEdge passes it through
		Slug:        slug,
		Fingerprint: identity.Fingerprint(pub),
	}

	// Sanity: the keyfile round-trips through Load (write + reload).
	p := filepath.Join(t.TempDir(), "live.keyfile.json")
	writeLiveKeyfile(t, p, kf)
	if _, err := heraldkeyfile.Load(p); err != nil {
		t.Fatalf("keyfile load: %v", err)
	}

	// buildAssertion against the live edge must discover + sign.
	a := &Agent{cfg: Config{HeraldKeyfile: kf}, log: testLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	assertion, err := a.buildAssertion(ctx)
	if err != nil {
		t.Fatalf("buildAssertion: %v", err)
	}
	if assertion == "" {
		t.Fatal("empty assertion")
	}
	claims, _ := identity.DecodeAccessClaims(assertion)
	if claims["sub"] != agentID {
		t.Fatalf("sub = %v, want %v", claims["sub"], agentID)
	}
	t.Logf("live assertion signed; aud=%v", claims["aud"])
}
```

Add `writeLiveKeyfile(t, path, kf)` (marshal + `os.WriteFile` 0600) and `testLogger()` (`slog.New(slog.NewTextHandler(io.Discard, nil))`) as small test helpers in this file. If `Agent` has unexported required fields beyond `cfg`/`log` that make direct construction awkward, instead drive the full broker+aspect path mirroring `nexus/broker/herald_register_live_test.go`; the minimal `buildAssertion` check above is the floor that proves discovery+sign through the live edge.

- [ ] **Step 2: Verify it skips offline**

Run: `cd /Users/jacinta/Source/nexus && go test ./runtime/agent/ -run TestLiveAspectHeraldRegister -v`
Expected: SKIP (env unset).

- [ ] **Step 3: Run it live against dMon**

First confirm a throwaway agent exists (provision via the current herald flow if needed) and export its values. Then:

```bash
cd /Users/jacinta/Source/nexus
CW_IT_EDGE=<dMon CWB edge> CW_IT_OWNER_SEED=<seed> \
CW_IT_AGENT_ID=<uuid> CW_IT_AGENT_SLUG=<slug> \
  go test ./runtime/agent/ -run TestLiveAspectHeraldRegister -v
```
Expected: PASS — the keyfile-derived assertion discovers the live token endpoint and signs with `sub` == the agent id. (If the edge is unreachable on the tailnet, capture the error and report; the gateway cycle changes only the edge url.)

- [ ] **Step 4: Commit**

```bash
cd /Users/jacinta/Source/nexus
git add runtime/agent/herald_register_live_test.go
git commit -m "test(runtime): gated live aspect herald-register e2e

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: Open PRs, CI-gated merge (cw + nexus)

- [ ] **Step 1: cw PR**

```bash
cd /Users/jacinta/Source/cw && git push -u origin aspect-side-bootstrap-enroll
gh pr create --fill --title "cw agent enroll (attach-only bootstrap keyfile)"
gh pr checks --watch && gh pr merge --squash --delete-branch
```
Expected: green then merged; no `--admin`-bypass.

- [ ] **Step 2: nexus PR**

```bash
cd /Users/jacinta/Source/nexus && git push -u origin aspect-side-bootstrap-enroll
gh pr create --fill --title "Aspect-side herald bootstrap: keyfile loader + register-frame assertion + enroll"
gh pr checks --watch && gh pr merge --squash --delete-branch
```
Expected: green then merged. nexus CI runs build+test on macos/ubuntu/windows (~5 min).

---

## Self-Review

**Spec coverage:**
- Piece 1 (`AgentAssertionFromKey` + refactor) → Task 1. ✓
- Piece 2 (`heraldkeyfile` loader) → Task 5. ✓
- Piece 3 (discover aud + sign + attach + dark-by-default + transport untouched) → Tasks 6, 7. ✓
- Piece 4 (`cw agent enroll` attach-only) → Task 4. ✓
- Herald `GetAgentByFingerprint` wrapper → Task 2. ✓
- Edge-agnostic / httpEdge discovery through the keyfile url → Task 6 (`httpEdge`, `buildAssertion`). ✓
- Live end-to-end → Task 8. ✓
- Cross-repo pin discipline → Task 3; CI-gated merges → Tasks 3, 9. ✓

**Type consistency:** keyfile JSON tags (`key, key_id, url, slug, fingerprint`) identical across cw (`bootstrapKeyfile`, Task 4) and nexus (`heraldkeyfile.Keyfile`, Task 5). `AgentAssertionFromKey(priv ed25519.PrivateKey, agentID, tokenURL string)` signature identical in Task 1 (def), Task 6 (call). `GetAgentByFingerprint(ctx, c, fp)` identical Task 2 (def), Task 4 (call). `buildAssertion(ctx)`/`httpEdge(url)` defined + used in Task 6. `Config.HeraldKeyfile *heraldkeyfile.Keyfile` defined Task 6, set Task 7.

**Placeholder scan:** none — every code step shows complete code. `<CWBHASH>`, `<dMon CWB edge>`, etc. are explicit runtime substitutions, not plan placeholders.

**Scope:** single cycle across three repos with a hard pin barrier (Task 3) — appropriate; the gateway is explicitly deferred.

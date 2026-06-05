# M1 — Custodian-Brokered Auth Foundation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A k3s worker (any nexus agent) can `git push` and call model providers without ever holding a raw secret — credentials flow through a custodian *seam* (broker credential store today, CWB custodian later).

**Architecture:** Add a `git` credential Kind to the broker's existing encrypted credential store; expose an agent-authenticated HTTP "seam" endpoint that mirrors the existing WS `credential.fetch` (resolve → scope-check → decrypt → audit); implement a `cw` git-credential-helper that speaks git's credential protocol and fetches from the seam as the worker's herald identity. Provider keys already flow from the store via the validate path — M1 confirms the worker case and audits it.

**Tech Stack:** Go; nexus broker (`net/http` mux, `nexus/credentials` AES-GCM store, `RecordAudit`); cw CLI (cobra, `cwb-client`); git credential-helper protocol.

**Spec:** `docs/2026-06-05-m1-custodian-auth-foundation-design.md` · **Story:** NEX-435 · **Epic:** NEX-434

---

## File Structure

**nexus repo** (`CarriedWorldUniverse/nexus`):
- `nexus/credentials/credentials.go` — MODIFY: add `KindGit`, `IsKnownKind` case, `validateBundle` case, `GitBundle` type + `GitBundle()` accessor.
- `nexus/credentials/credentials_test.go` — MODIFY: git Kind tests.
- `nexus/broker/agent_credentials_http.go` — CREATE: the agent-auth seam endpoint `POST /api/agent/credential.fetch`.
- `nexus/broker/agent_credentials_http_test.go` — CREATE: endpoint tests.
- `nexus/broker/admin.go` (or the HTTP route file where aspect/admin routes register) — MODIFY: register the new route.

**cw repo** (`CarriedWorldUniverse/cw`):
- `internal/cli/credential/credential.go` — CREATE: `cw credential` group + `git-helper` subcommand.
- `internal/cli/credential/githelper.go` — CREATE: the git credential-protocol parser/responder + seam call.
- `internal/cli/credential/githelper_test.go` — CREATE: protocol round-trip test.
- `cmd/cw/main.go` — MODIFY: register the `credential` group on the root.

Build order: nexus Tasks 1–3 first (the seam must exist for cw to call), then cw Tasks 4–5, then provider confirmation Task 6, then the issue-permission convenience Task 7.

---

## Task 1: `git` credential Kind (nexus)

**Files:**
- Modify: `nexus/credentials/credentials.go`
- Test: `nexus/credentials/credentials_test.go`

The bundle shape is `{"username": "<git user>", "password": "<PAT>", "host": "github.com"}`.

- [ ] **Step 1: Write the failing test**

Add to `nexus/credentials/credentials_test.go`:

```go
func TestGitKind_RoundTrip(t *testing.T) {
	st := newTestStore(t) // existing helper in this package; see other tests
	ctx := context.Background()
	err := st.Set(ctx, credentials.UpsertParams{
		Name:           "worker-git",
		Kind:           credentials.KindGit,
		Bundle:         map[string]any{"username": "nexus-cw", "password": "ghp_x", "host": "github.com"},
		AllowedAspects: []string{"worker-1"},
		Mode:           credentials.ModeFetch,
	})
	if err != nil {
		t.Fatalf("Set git: %v", err)
	}
	cred, err := st.Get(ctx, "worker-git")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cred.Kind != credentials.KindGit {
		t.Fatalf("kind = %q, want git", cred.Kind)
	}
	gb, err := st.GitBundle(cred)
	if err != nil {
		t.Fatalf("GitBundle: %v", err)
	}
	if gb.Username != "nexus-cw" || gb.Password != "ghp_x" || gb.Host != "github.com" {
		t.Fatalf("bundle = %+v", gb)
	}
	if !cred.AllowedFor("worker-1") || cred.AllowedFor("worker-2") {
		t.Fatalf("AllowedFor scoping wrong")
	}
}

func TestGitKind_RejectsIncompleteBundle(t *testing.T) {
	st := newTestStore(t)
	err := st.Set(context.Background(), credentials.UpsertParams{
		Name: "bad-git", Kind: credentials.KindGit,
		Bundle: map[string]any{"username": "x"}, // missing password/host
		AllowedAspects: []string{"*"}, Mode: credentials.ModeFetch,
	})
	if err == nil {
		t.Fatal("want validation error for incomplete git bundle")
	}
}
```

> If `newTestStore` is not the existing helper name, match whatever the other tests in this file use to construct a `*credentials.Store` with an in-memory DB.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd nexus && go test ./nexus/credentials/ -run TestGitKind -v`
Expected: FAIL — `KindGit` / `GitBundle` undefined.

- [ ] **Step 3: Implement the git Kind**

In `nexus/credentials/credentials.go`:

Add the const (in the `Kind` const block):
```go
	KindGit Kind = "git" // Git push credential: username/password(PAT)/host
```

Add to `IsKnownKind`'s switch:
```go
	case KindProvider, KindJira, KindIMAP, KindGit:
		return true
```

Add the typed bundle (next to `IMAPBundle` / `JiraBundle` struct defs):
```go
// GitBundle is the decrypted bundle for a kind='git' credential.
type GitBundle struct {
	Username string `json:"username"`
	Password string `json:"password"` // PAT or token; NEVER log
	Host     string `json:"host"`     // e.g. github.com
}
```

Add to `validateBundle`'s switch:
```go
	case KindGit:
		if err := requireString("username"); err != nil {
			return err
		}
		if err := requireString("password"); err != nil {
			return err
		}
		if err := requireString("host"); err != nil {
			return err
		}
```

Add the accessor (mirror `JiraBundle()`):
```go
// GitBundle returns the decrypted bundle for a kind='git' credential.
func (s *Store) GitBundle(c Credential) (GitBundle, error) {
	if c.Kind != KindGit {
		return GitBundle{}, fmt.Errorf("%w: have %q want %q", ErrKindMismatch, c.Kind, KindGit)
	}
	plaintext, err := s.decrypt(c.encryptedBundle, c.nonce)
	if err != nil {
		return GitBundle{}, fmt.Errorf("decrypt bundle: %w", err)
	}
	var gb GitBundle
	if err := json.Unmarshal(plaintext, &gb); err != nil {
		return GitBundle{}, fmt.Errorf("unmarshal git bundle: %w", err)
	}
	return gb, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd nexus && go test ./nexus/credentials/ -run TestGitKind -v && go vet ./nexus/credentials/`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add nexus/credentials/credentials.go nexus/credentials/credentials_test.go
git commit -m "feat(credentials): add git credential Kind (NEX-435)"
```

---

## Task 2: Agent-auth seam endpoint (nexus)

**Files:**
- Create: `nexus/broker/agent_credentials_http.go`
- Create: `nexus/broker/agent_credentials_http_test.go`

Mirror two existing things: **auth** from `nexus/broker/aspect_self_edit.go` (`ExtractBearer` + verify an aspect JWT → agent identity over HTTP), and **resolution** from `nexus/broker/aspect_credentials.go:43-195` (`handleAspectCredentialFetch`: resolve by name or default → `AllowedFor` → `Bundle` → `RecordAudit`). Read both before writing.

Endpoint: `POST /api/agent/credential.fetch`, body `{"kind":"git","name":"worker-git"}` (name optional → default for that kind+aspect), response `{"name","kind","bundle","expires_at"}`. On scope failure: 403 + `RecordAudit(AuditDenied)`.

- [ ] **Step 1: Write the failing test**

In `nexus/broker/agent_credentials_http_test.go`, construct a broker with an in-memory credential store seeded with a `git` cred allowed for agent `worker-1`, mint a worker JWT (mirror how `aspect_self_edit_test.go` mints/sets a bearer token), and assert:
- `POST /api/agent/credential.fetch` with `worker-1`'s bearer + body `{"kind":"git"}` → 200, body bundle has `username`/`password`/`host`.
- Same with agent `worker-2`'s bearer → 403, and an `AuditDenied` row exists.
- No/invalid bearer → 401.

```go
func TestAgentCredentialFetch_Git(t *testing.T) {
	b, store := newTestBrokerWithCreds(t) // mirror aspect_self_edit_test setup
	mustSetGit(t, store, "worker-git", []string{"worker-1"})

	body := `{"kind":"git","name":"worker-git"}`
	rec := doAgentReq(t, b, "worker-1", body)      // helper: sets Bearer for worker-1
	if rec.Code != 200 {
		t.Fatalf("worker-1 code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct{ Bundle map[string]any `json:"bundle"` }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Bundle["password"] != "ghp_x" {
		t.Fatalf("bundle=%v", got.Bundle)
	}

	rec2 := doAgentReq(t, b, "worker-2", body)
	if rec2.Code != 403 {
		t.Fatalf("worker-2 code=%d, want 403", rec2.Code)
	}
	if !auditHas(t, store, "worker-git", credentials.AuditDenied) {
		t.Fatal("expected AuditDenied row for worker-2")
	}
}
```

> Build `newTestBrokerWithCreds`, `mustSetGit`, `doAgentReq`, `auditHas` by mirroring the helpers in `aspect_self_edit_test.go` and `aspect_credentials` tests. The bearer-minting must match the verification the handler uses (Step 3).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd nexus && go test ./nexus/broker/ -run TestAgentCredentialFetch_Git -v`
Expected: FAIL — handler not registered / undefined.

- [ ] **Step 3: Implement the handler**

Create `nexus/broker/agent_credentials_http.go`. Reuse the bearer-verification helper that `aspect_self_edit.go` uses (`ExtractBearer` + the same JWT verify call that yields the agent identity — copy that call, do not invent a new verifier), and the resolution logic from `handleAspectCredentialFetch`:

```go
package broker

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
)

type agentCredFetchReq struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

type agentCredFetchResp struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Bundle    map[string]any `json:"bundle"`
	ExpiresAt string         `json:"expires_at,omitempty"`
}

// handleAgentCredentialFetch is the HTTP "custodian seam": an agent
// (authenticated by its herald/aspect JWT bearer) fetches a scoped,
// audited credential bundle. Mirrors handleAspectCredentialFetch but
// over HTTP for the cw git-credential-helper.
func (b *Broker) handleAgentCredentialFetch(w http.ResponseWriter, r *http.Request) {
	// AUTH: copy the exact verify path aspect_self_edit.go uses to turn a
	// bearer token into the caller's agent id. Pseudocode shape:
	//   token := ExtractBearer(r.Header.Get("Authorization"))
	//   agentID, err := b.verifyAspectBearer(token)   // <- the real fn used by aspect_self_edit
	token := ExtractBearer(r.Header.Get("Authorization"))
	agentID, err := b.verifyAspectBearer(r.Context(), token) // NAME-MATCH to aspect_self_edit's verifier
	if err != nil || agentID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req agentCredFetchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Kind == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cstore := b.cfg.Credentials // match the field name used elsewhere in broker
	ctx := r.Context()

	var cred credentials.Credential
	if req.Name == "" {
		cred, err = cstore.ResolveDefaultBundle(ctx, agentID, credentials.Kind(req.Kind))
	} else {
		cred, err = cstore.Get(ctx, req.Name)
	}
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if !cred.AllowedFor(agentID) || string(cred.Kind) != req.Kind {
		_ = cstore.RecordAudit(ctx, credentials.AuditEvent{
			CredentialName: cred.Name, Aspect: agentID, Action: credentials.AuditDenied,
		})
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	bundle, err := cstore.Bundle(cred)
	if err != nil {
		http.Error(w, "decrypt", http.StatusInternalServerError)
		return
	}
	_ = cstore.RecordAudit(ctx, credentials.AuditEvent{
		CredentialName: cred.Name, Aspect: agentID, Action: credentials.AuditFetch,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentCredFetchResp{
		Name: cred.Name, Kind: string(cred.Kind), Bundle: bundle,
	})
	_ = errors.Unwrap // keep imports tidy if errors unused; remove if so
}
```

> The two names to bind to the real code: the bearer-verify function (whatever `aspect_self_edit.go` calls — replace `verifyAspectBearer`) and the store accessor (`b.cfg.Credentials` or the actual field; grep `Credentials` in `broker/server.go`). `ResolveDefaultBundle` is the same method `handleAspectCredentialFetch` calls at aspect_credentials.go:106.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd nexus && go test ./nexus/broker/ -run TestAgentCredentialFetch_Git -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/agent_credentials_http.go nexus/broker/agent_credentials_http_test.go
git commit -m "feat(broker): agent-auth HTTP credential seam endpoint (NEX-435)"
```

---

## Task 3: Register the seam route (nexus)

**Files:**
- Modify: the broker file that registers HTTP routes (where `aspect_self_edit`'s route is registered — grep `api/aspect` / the mux that holds aspect HTTP routes, NOT the `requireAdmin` admin mux).

- [ ] **Step 1: Register the route**

Next to the existing aspect-bearer HTTP route registration (the one for `aspect_self_edit`), add:
```go
mux.Handle("POST /api/agent/credential.fetch", http.HandlerFunc(b.handleAgentCredentialFetch))
```

- [ ] **Step 2: Build + smoke**

Run: `cd nexus && go build ./nexus/... && go vet ./nexus/broker/`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add -A && git commit -m "feat(broker): mount /api/agent/credential.fetch (NEX-435)"
```

---

## Task 4: `cw credential git-helper` (cw repo)

**Files:**
- Create: `internal/cli/credential/credential.go`, `internal/cli/credential/githelper.go`, `internal/cli/credential/githelper_test.go`
- Modify: `cmd/cw/main.go`

Git invokes a credential helper with one argv operation (`get`/`store`/`erase`) and feeds `key=value\n` lines on stdin terminated by a blank line; for `get`, the helper writes `username=...\npassword=...\n` to stdout. We only implement `get` (fetch from the seam); `store`/`erase` are no-ops (exit 0).

- [ ] **Step 1: Write the failing test**

`internal/cli/credential/githelper_test.go`:
```go
package credential

import (
	"bytes"
	"strings"
	"testing"
)

func TestGitHelper_Get(t *testing.T) {
	// fetch stub returns a fixed bundle; inject via the seam-call hook.
	fetch := func(host string) (user, pass string, err error) {
		if host != "github.com" {
			t.Fatalf("host=%q", host)
		}
		return "nexus-cw", "ghp_x", nil
	}
	in := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var out bytes.Buffer
	if err := runGitHelper("get", in, &out, fetch); err != nil {
		t.Fatalf("runGitHelper: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "username=nexus-cw") || !strings.Contains(got, "password=ghp_x") {
		t.Fatalf("out=%q", got)
	}
}

func TestGitHelper_StoreEraseAreNoOps(t *testing.T) {
	for _, op := range []string{"store", "erase"} {
		var out bytes.Buffer
		if err := runGitHelper(op, strings.NewReader("host=github.com\n\n"), &out, nil); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if out.Len() != 0 {
			t.Fatalf("%s wrote output: %q", op, out.String())
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cw && go test ./internal/cli/credential/ -v`
Expected: FAIL — `runGitHelper` undefined.

- [ ] **Step 3: Implement the protocol core**

`internal/cli/credential/githelper.go`:
```go
package credential

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// fetchFunc retrieves the git credential for a host from the seam.
type fetchFunc func(host string) (username, password string, err error)

// runGitHelper implements the git credential-helper protocol for one op.
// Only "get" produces output (username/password from the seam); "store"
// and "erase" are no-ops (the seam is the source of truth).
func runGitHelper(op string, in io.Reader, out io.Writer, fetch fetchFunc) error {
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if op != "get" {
		return nil
	}
	host := attrs["host"]
	if host == "" {
		return fmt.Errorf("git-helper: no host attribute")
	}
	user, pass, err := fetch(host)
	if err != nil {
		return err
	}
	// NEVER log user/pass. Write only to the git fd.
	fmt.Fprintf(out, "username=%s\npassword=%s\n", user, pass)
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd cw && go test ./internal/cli/credential/ -v`
Expected: PASS.

- [ ] **Step 5: Wire the cobra command + the real seam fetch**

`internal/cli/credential/credential.go`:
```go
// Package credential implements `cw credential`: the git credential helper
// and worker permission issuance, backed by the custodian seam.
package credential

import (
	"fmt"
	"os"

	"github.com/CarriedWorldUniverse/cw/internal/cmdutil"
	"github.com/spf13/cobra"
)

func NewCmd(gf *cmdutil.GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "credential", Short: "Custodian-brokered credentials"}
	cmd.AddCommand(newGitHelperCmd(gf))
	return cmd
}

func newGitHelperCmd(gf *cmdutil.GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:    "git-helper [get|store|erase]",
		Short:  "git credential helper backed by the custodian seam",
		Args:   cobra.ExactArgs(1),
		Hidden: true, // invoked by git, not humans
		RunE: func(cmd *cobra.Command, args []string) error {
			fetch := func(host string) (string, string, error) {
				return seamFetchGit(gf, host) // see Step 6
			}
			return runGitHelper(args[0], os.Stdin, os.Stdout, fetch)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	_ = fmt.Sprint
}
```

- [ ] **Step 6: Implement `seamFetchGit` against the broker**

Add to `githelper.go` — call the seam endpoint as the worker's herald identity. Reuse `cmdutil.Session(gf)` for the authenticated client and POST to `/api/agent/credential.fetch`. The seam lives on the broker; for M1 the worker passes the broker URL via `--edge`/`CW_EDGE` (or a dedicated `CW_SEAM_URL`). Use the client's authenticated transport / token:

```go
func seamFetchGit(gf *cmdutil.GlobalFlags, host string) (string, string, error) {
	// cmdutil.Session yields an authenticated *client.Client whose
	// transport carries the worker's herald bearer; POST the seam request.
	// Match the client's Do/PostJSON helper used elsewhere (grep cwb-client
	// client.Do in internal/cli/*). Shape:
	//   resp := client.Do("POST", "/api/agent/credential.fetch", {"kind":"git"})
	//   parse {"bundle":{"username":...,"password":...}}
	c, _, _, err := cmdutil.Session(gf)
	if err != nil {
		return "", "", err
	}
	bundle, err := postSeam(c, map[string]string{"kind": "git"}) // implement postSeam with the client's request method
	if err != nil {
		return "", "", err
	}
	return bundle["username"], bundle["password"], nil
}
```

> Bind `postSeam` to the cwb-client `*client.Client` request method actually available (grep an existing `internal/cli/*` call that does a raw authenticated request, e.g. how `pr`/`repo` call cairn endpoints). Return `map[string]string` parsed from the `bundle` field.

- [ ] **Step 7: Register on the root + build**

In `cmd/cw/main.go`, add the import and `root.AddCommand(credential.NewCmd(flags))`.

Run: `cd cw && go build ./... && go test ./internal/cli/credential/ && go vet ./...`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/credential cmd/cw/main.go
git commit -m "feat(cw): credential git-helper backed by the custodian seam (NEX-435)"
```

---

## Task 5: Live end-to-end git-push proof (cw + nexus)

**Files:**
- Create: `internal/cli/credential/githelper_live_test.go` (gated, mirrors cw's existing `*_integration_test.go` gating, e.g. `CW_IT_*` env).

- [ ] **Step 1: Write the gated live test**

Gated on the same env the other cw integration tests use. Steps inside: configure a temp repo's `credential.helper` to `cw credential git-helper`, run `git ls-remote`/`git push --dry-run` against a throwaway test repo, assert success and that no token appears in any captured stdout/log.

```go
//go:build integration
// ... mirror whoami_remote_integration_test.go's env gating + skip
func TestGitHelper_LivePush(t *testing.T) {
	// requireEnv(t, "CW_IT_EDGE", "CW_IT_IDENTITY", "CW_IT_TEST_REPO")
	// git -c credential.helper='cw credential git-helper' ls-remote <repo>
	// assert exit 0; assert no "ghp_"/"password=" leaked to combined output.
}
```

- [ ] **Step 2: Run gated (when env present); otherwise it skips**

Run: `cd cw && go test ./internal/cli/credential/ -run TestGitHelper_LivePush -tags integration -v`
Expected: PASS when `CW_IT_*` set, SKIP otherwise.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/credential/githelper_live_test.go
git commit -m "test(cw): gated live git-push-via-seam proof (NEX-435)"
```

---

## Task 6: Confirm provider keys flow through the seam (nexus)

Provider keys already resolve from the credential store into the worker's provider env via the validate path (`resolveProviderEnv` in `nexus/broker/validate_endpoint.go`, store kind=`provider`). M1's requirement is that a worker's provider key comes from the store (not a mounted long-lived env) and is audited.

**Files:**
- Test: `nexus/broker/validate_endpoint_test.go` (or wherever provider-env resolution is tested) — assert a worker provider key resolves from the store and writes an audit row.

- [ ] **Step 1: Add/extend the test**

Assert: given a `provider` cred allowed for a worker identity, the validate/provider-env path returns the key from the store and records an `AuditFetch` (or `AuditProxyCall`) row. If audit on the provider path is missing, add the `RecordAudit` call to match the git path.

- [ ] **Step 2: Run + (if needed) implement the audit**

Run: `cd nexus && go test ./nexus/broker/ -run Provider -v`
Expected: PASS (after adding audit if it was absent).

- [ ] **Step 3: Commit**

```bash
git add -A && git commit -m "feat(broker): audit worker provider-key issuance via the store (NEX-435)"
```

---

## Task 7: `cw credential issue-git-permission` + broker grant (cw + nexus)

Grants a worker identity scoped access to a git credential by adding it to the credential's `AllowedAspects` (the raw PAT is registered once by the operator via the existing `nexus credential` admin path; this command does the per-worker grant). Requires an admin-gated broker endpoint.

**Files:**
- nexus Create: a handler `POST /api/admin/credentials/{name}/grant` (body `{"aspect":"worker-1"}`) under `requireAdmin`, that loads the credential, appends the aspect to `AllowedAspects`, and re-`Set`s it. Mirror an existing admin handler in `nexus/broker/admin_*.go`.
- cw Create: `newIssueGitPermissionCmd` in `internal/cli/credential/credential.go` calling that endpoint.

- [ ] **Step 1: nexus — test + handler for the grant endpoint**

Test (`nexus/broker/admin_credential_grant_test.go`): admin POST adds `worker-1` to a git cred's `AllowedAspects`; non-admin → 401/403. Implement the handler mirroring an existing `requireAdmin` POST handler; load via `Get`, append aspect (dedup), persist via `Set` with the existing bundle (note: `Set` re-encrypts — fetch bundle via `Bundle()` first to round-trip, or add a dedicated `Grant` store method if `Set` requires the plaintext bundle).

> Decision: add a `Store.Grant(ctx, name, aspect string) error` method to `nexus/credentials/credentials.go` that updates only `allowed_aspects` (a single `UPDATE` — no re-encryption needed), with a unit test. This is cleaner than round-tripping the bundle. Implement that method first, then the handler calls it.

- [ ] **Step 2: cw — the command**

```go
func newIssueGitPermissionCmd(gf *cmdutil.GlobalFlags) *cobra.Command {
	var name, aspect string
	cmd := &cobra.Command{
		Use:   "issue-git-permission",
		Short: "Grant a worker identity scoped access to a git credential",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" || aspect == "" {
				return fmt.Errorf("--name and --aspect are required")
			}
			c, _, _, err := cmdutil.Session(gf)
			if err != nil {
				return err
			}
			return postGrant(c, name, aspect) // POST /api/admin/credentials/<name>/grant
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "git credential name (required)")
	f.StringVar(&aspect, "aspect", "", "worker identity to grant (required)")
	return cmd
}
```
Register it in `NewCmd` alongside `git-helper`.

- [ ] **Step 3: Run + build both repos**

Run: `cd nexus && go test ./nexus/broker/ ./nexus/credentials/ && cd ../cw && go build ./...`
Expected: clean.

- [ ] **Step 4: Commit (per repo)**

```bash
# nexus
git add -A && git commit -m "feat(credentials): Store.Grant + admin grant endpoint (NEX-435)"
# cw
git add -A && git commit -m "feat(cw): credential issue-git-permission (NEX-435)"
```

---

## Self-Review

**Spec coverage:**
- git Kind → Task 1 ✓
- agent-auth seam endpoint (contract) → Tasks 2–3 ✓
- `cw git-credential-helper` → Task 4 ✓
- live no-secret push proof → Task 5 ✓
- provider keys through the seam → Task 6 ✓
- `cw issue-git-permission` → Task 7 ✓
- Non-goals (CWB custodian service, Ledger, delegated handles, store→CWB migration) → correctly excluded; tracked as NEX-438.

**Type consistency:** `KindGit`, `GitBundle`/`Store.GitBundle`, `AuditFetch`/`AuditDenied`, `UpsertParams`, `Store.Grant`, `runGitHelper`/`fetchFunc`/`seamFetchGit`, `/api/agent/credential.fetch` used consistently across tasks.

**Bind-to-real-code markers (resolve at execution by reading the named file):**
1. `verifyAspectBearer` — the actual bearer-verify fn `aspect_self_edit.go` uses (Task 2).
2. `b.cfg.Credentials` — the real store field on `Broker` (grep `server.go`).
3. The aspect-HTTP route mux (Task 3) — same mux `aspect_self_edit`'s route registers on.
4. `postSeam`/`postGrant` — the cwb-client `*client.Client` request method used by existing `internal/cli/*` commands (Task 4/7).
5. cw integration-test env gating convention (Task 5).
These are the only seams to real code; everything else is complete above.

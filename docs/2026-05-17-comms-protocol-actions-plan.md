# Comms-protocol actions + `!dispatch` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Operationalise hand-dispatch v0.1 via the comms protocol — `!dispatch <context>` becomes a broker-side action that spawns an identity-framed fresh-context hand of the calling aspect, with results returning as chat messages on the originating thread. Also ship `!actions` / `!skills` discoverability and finish the deferred identity-bundle loading in `runtime/handexec`.

**Architecture:** The broker's existing `HandleChatSend` (nexus/broker/chat_send.go) grows a leading-symbol parser. Content beginning with `!` is routed to an `Action` handler from a registry; content beginning with `/` is reserved for Epic B (skill invocation) and currently passes through as plain text. Three actions register at startup: `!dispatch` (calls `handqueue.Submit`), `!actions` (lists registry), `!skills` (consumes a `SkillRegistry` interface — `NullSkillRegistry` for now). `runtime/handexec` gains an identity loader that composes NEXUS.md / SOUL.md / PRIMER from the aspect home into a system prompt before invoking the provider.

**Tech Stack:** Go 1.22+, sqlite-backed broker, in-process WebSocket frames, `nexus/handqueue` + `runtime/handexec` (existing), `nexus/frames` envelope types, `nexus/broker` HTTP/WS handlers, `claude-code` subprocess via the configured provider.

---

## File Structure

**New files (broker, runtime/handexec):**

```
nexus/broker/
    action.go                       Action interface, ActionRegistry, parser
    action_test.go                  parser + registry unit tests
    action_dispatch.go              !dispatch handler — calls handqueue.Submit
    action_dispatch_test.go         dispatch action handler tests
    action_listactions.go           !actions handler — list registry
    action_listactions_test.go
    action_listskills.go            !skills handler — consume SkillRegistry
    action_listskills_test.go
    skillregistry.go                SkillRegistry interface + NullSkillRegistry
    skillregistry_test.go

runtime/handexec/
    identity.go                     load NEXUS.md / SOUL.md / PRIMER, compose prompt
    identity_test.go
```

**Modified files:**

```
nexus/broker/chat_send.go           wire parseLeadingAction at top of HandleChatSend
nexus/broker/server.go              construct ActionRegistry at broker startup, register handlers
nexus/frames/frames.go              add KindDispatchAccepted
nexus/frames/payloads.go            add DispatchAcceptedPayload
runtime/handexec/handexec.go        call identity.LoadAndCompose; pass composed prompt + payload to provider
```

Each task below produces self-contained, testable changes. Tasks run sequentially within their phase; phases parallelise as noted.

---

## Phase 1 — Action registry primitive

Lays the foundation: parser, interface, registry. No actions registered yet.

### Task 1: Parser for leading actions

**Files:**
- Create: `nexus/broker/action.go`
- Create: `nexus/broker/action_test.go`

- [ ] **Step 1: Write the failing test for `parseLeadingAction`**

Create `nexus/broker/action_test.go`:

```go
package broker

import "testing"

func TestParseLeadingAction(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantName  string
		wantArgs  string
		wantIsAct bool
	}{
		{"empty", "", "", "", false},
		{"plain chat", "hello there", "", "", false},
		{"slash skill", "/review pr/58", "", "", false},
		{"bang only", "!", "", "", true},
		{"action no args", "!dispatch", "dispatch", "", true},
		{"action with args", "!dispatch implement NEX-179", "dispatch", "implement NEX-179", true},
		{"action with multi-space args", "!dispatch  two  spaces", "dispatch", " two  spaces", true},
		{"action with newline args", "!dispatch line1\nline2", "dispatch", "line1\nline2", true},
		// Positional rule: '!' must be the first non-whitespace byte.
		// Anywhere else in the content is plain text — operator and
		// aspects can discuss actions ("let's use !dispatch for this")
		// without accidentally triggering one.
		{"mid-text bang ignored", "let's use !dispatch for this", "", "", false},
		{"backtick-wrapped", "use `!dispatch` to spawn", "", "", false},
		{"quoted in prose", `remember: "!actions" works`, "", "", false},
		// Lenient whitespace policy: leading spaces / tabs / newlines
		// are skipped before checking for '!'. Friendly to pasted
		// content and multi-line messages whose first line is blank.
		{"leading spaces fire", "  !dispatch foo", "dispatch", "foo", true},
		{"leading newline fires", "\n!dispatch foo", "dispatch", "foo", true},
		{"mixed leading whitespace fires", " \t !dispatch foo", "dispatch", "foo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotArgs, gotIsAct := parseLeadingAction(tc.content)
			if gotName != tc.wantName || gotArgs != tc.wantArgs || gotIsAct != tc.wantIsAct {
				t.Errorf("parseLeadingAction(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.content, gotName, gotArgs, gotIsAct, tc.wantName, tc.wantArgs, tc.wantIsAct)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd ~/Source/nexus
go test ./nexus/broker/ -run TestParseLeadingAction -v
```

Expected: FAIL with `undefined: parseLeadingAction`.

- [ ] **Step 3: Implement `parseLeadingAction`**

Create `nexus/broker/action.go`:

```go
// Package broker — action.go: comms-protocol action handling.
//
// Chat messages whose content starts with "!<action-name>" are
// intercepted by HandleChatSend (chat_send.go) and routed to a
// registered Action handler. The original content is NOT delivered
// to thread recipients; the action's response is sent back to the
// caller via the existing chat.send path.
//
// See docs/2026-05-17-comms-protocol-actions-spec.md for the model.

package broker

import (
	"context"
	"strings"
	"sync"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// parseLeadingAction inspects chat content for a leading "!action-name".
// The rule is positional: '!' must be the first non-whitespace byte
// in the message. Anywhere else (mid-prose, backtick-wrapped, quoted)
// is plain text and is NOT intercepted — operators and aspects can
// freely discuss actions without accidentally triggering them.
//
// Leading whitespace (spaces, tabs, newlines) is skipped before the
// '!' check. This is friendly to pasted content and to multi-line
// messages whose first line is blank.
//
// Returns (name, args, true) on a leading-position action; otherwise
// returns ("", "", false). The split between name and args is on the
// first space byte after '!'; args preserve their internal whitespace
// exactly. A bare "!" (possibly after whitespace) returns ("", "", true).
func parseLeadingAction(content string) (name, args string, isAction bool) {
	trimmed := strings.TrimLeft(content, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '!' {
		return "", "", false
	}
	rest := trimmed[1:]
	idx := strings.IndexByte(rest, ' ')
	if idx < 0 {
		return rest, "", true
	}
	return rest[:idx], rest[idx+1:], true
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestParseLeadingAction -v
```

Expected: PASS — all 14 cases (8 original + 3 mid-text/wrapped/quoted lock-ins + 3 lenient-whitespace).

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/action.go nexus/broker/action_test.go
git commit -m "feat(broker): parseLeadingAction for comms-protocol action ingress"
```

### Task 2: Action interface

**Files:**
- Modify: `nexus/broker/action.go`
- Modify: `nexus/broker/action_test.go`

- [ ] **Step 1: Write the failing test for Action interface compile-time satisfaction**

Append to `nexus/broker/action_test.go`:

```go
// stubAction is a test-only Action implementation used to verify
// interface satisfaction at compile time and to test the registry.
type stubAction struct {
	name        string
	description string
	help        string
	handleFn    func(ctx context.Context, env frames.Envelope, args string) (frames.Envelope, error)
}

func (s stubAction) Name() string        { return s.name }
func (s stubAction) Description() string { return s.description }
func (s stubAction) Help() string        { return s.help }
func (s stubAction) Handle(ctx context.Context, env frames.Envelope, args string) (frames.Envelope, error) {
	if s.handleFn != nil {
		return s.handleFn(ctx, env, args)
	}
	return frames.Envelope{}, nil
}

// Compile-time check that stubAction satisfies Action.
var _ Action = stubAction{}
```

Add the imports `"context"` and `"github.com/CarriedWorldUniverse/nexus/nexus/frames"` to the test file if not present.

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./nexus/broker/ -run TestParseLeadingAction -v
```

Expected: FAIL with `undefined: Action`.

- [ ] **Step 3: Define the Action interface**

Append to `nexus/broker/action.go`:

```go
// Action handles a comms-protocol action invocation. Implementations
// are registered with an ActionRegistry at broker startup. Per spec
// §5.1, Name and Description/Help are static (set at registration);
// Handle is called once per invocation with the calling aspect's
// connection envelope and the rest-of-content args.
type Action interface {
	// Name is the registry key, without the leading "!".
	Name() string

	// Description is a one-line summary for !actions list output.
	Description() string

	// Help is multi-line detail for !actions <name> output.
	Help() string

	// Handle processes the action. envelope carries the calling
	// aspect's connection context (identity in From, originating
	// thread in Topic / ReplyTo). args is the raw rest-of-content
	// after "!<name> ". Returns the response frame the broker
	// sends back to the caller (typically KindDispatchAccepted or
	// KindDispatchError).
	Handle(ctx context.Context, envelope frames.Envelope, args string) (frames.Envelope, error)
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestParseLeadingAction -v
go vet ./nexus/broker/
```

Expected: PASS, no vet errors.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/action.go nexus/broker/action_test.go
git commit -m "feat(broker): Action interface for comms-protocol handlers"
```

### Task 3: ActionRegistry

**Files:**
- Modify: `nexus/broker/action.go`
- Modify: `nexus/broker/action_test.go`

- [ ] **Step 1: Write the failing test for ActionRegistry**

Append to `nexus/broker/action_test.go`:

```go
func TestActionRegistry_RegisterAndLookup(t *testing.T) {
	reg := NewActionRegistry()
	a := stubAction{name: "dispatch", description: "spawn a hand"}
	reg.Register(a)

	got, ok := reg.Lookup("dispatch")
	if !ok {
		t.Fatalf("Lookup(dispatch) returned ok=false; want true")
	}
	if got.Name() != "dispatch" {
		t.Errorf("got.Name()=%q, want %q", got.Name(), "dispatch")
	}

	_, ok = reg.Lookup("nonexistent")
	if ok {
		t.Errorf("Lookup(nonexistent) returned ok=true; want false")
	}
}

func TestActionRegistry_RegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate Register, got none")
		}
	}()
	reg := NewActionRegistry()
	a := stubAction{name: "dispatch"}
	reg.Register(a)
	reg.Register(a) // should panic
}

func TestActionRegistry_ListSorted(t *testing.T) {
	reg := NewActionRegistry()
	reg.Register(stubAction{name: "skills", description: "list skills"})
	reg.Register(stubAction{name: "dispatch", description: "spawn a hand"})
	reg.Register(stubAction{name: "actions", description: "list actions"})

	got := reg.List()
	if len(got) != 3 {
		t.Fatalf("List() returned %d, want 3", len(got))
	}
	wantOrder := []string{"actions", "dispatch", "skills"}
	for i, d := range got {
		if d.Name != wantOrder[i] {
			t.Errorf("List()[%d].Name=%q, want %q", i, d.Name, wantOrder[i])
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./nexus/broker/ -run TestActionRegistry -v
```

Expected: FAIL with `undefined: NewActionRegistry`.

- [ ] **Step 3: Implement ActionRegistry**

Append to `nexus/broker/action.go`:

```go
// ActionDescriptor is the public face of a registered Action for the
// !actions discovery listing. Pulled from Action.Name + Description.
type ActionDescriptor struct {
	Name        string
	Description string
}

// ActionRegistry is a name→Action lookup populated at broker startup.
// All mutation happens during startup; the runtime path is read-only,
// so RWMutex is appropriate.
type ActionRegistry struct {
	mu      sync.RWMutex
	actions map[string]Action
}

// NewActionRegistry constructs an empty registry.
func NewActionRegistry() *ActionRegistry {
	return &ActionRegistry{actions: make(map[string]Action)}
}

// Register adds an action. Panics on duplicate name — this is a
// programmer error caught at startup, not a runtime concern.
func (r *ActionRegistry) Register(a Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.actions[a.Name()]; exists {
		panic("broker.ActionRegistry: duplicate action " + a.Name())
	}
	r.actions[a.Name()] = a
}

// Lookup returns the Action for a name and a presence bool.
func (r *ActionRegistry) Lookup(name string) (Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[name]
	return a, ok
}

// List returns descriptors for all registered actions, sorted by name.
// Used by the !actions discovery action.
func (r *ActionRegistry) List() []ActionDescriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ActionDescriptor, 0, len(r.actions))
	for _, a := range r.actions {
		out = append(out, ActionDescriptor{Name: a.Name(), Description: a.Description()})
	}
	// stable alphabetical order
	sortActionDescriptors(out)
	return out
}

func sortActionDescriptors(s []ActionDescriptor) {
	// simple bubble for tiny lists; len(s) ≤ ~10 in practice
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Name > s[j].Name; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./nexus/broker/ -run TestActionRegistry -v
```

Expected: PASS — `RegisterAndLookup`, `RegisterDuplicatePanics`, `ListSorted`.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/action.go nexus/broker/action_test.go
git commit -m "feat(broker): ActionRegistry with Register/Lookup/List"
```

---

## Phase 2 — Skill registry interface (stub)

Reserves the Epic B surface so `!skills` has something to consume.

### Task 4: SkillRegistry interface + NullSkillRegistry

**Files:**
- Create: `nexus/broker/skillregistry.go`
- Create: `nexus/broker/skillregistry_test.go`

- [ ] **Step 1: Write the failing test**

Create `nexus/broker/skillregistry_test.go`:

```go
package broker

import "testing"

func TestNullSkillRegistry_ListReturnsEmpty(t *testing.T) {
	var r SkillRegistry = NullSkillRegistry{}
	if got := r.List(); len(got) != 0 {
		t.Errorf("NullSkillRegistry.List() = %v, want empty", got)
	}
}

func TestNullSkillRegistry_GetReturnsFalse(t *testing.T) {
	var r SkillRegistry = NullSkillRegistry{}
	_, ok := r.Get("any-name")
	if ok {
		t.Errorf("NullSkillRegistry.Get(any) = ok=true, want false")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./nexus/broker/ -run TestNullSkillRegistry -v
```

Expected: FAIL with `undefined: SkillRegistry`.

- [ ] **Step 3: Implement SkillRegistry + NullSkillRegistry**

Create `nexus/broker/skillregistry.go`:

```go
// Package broker — skillregistry.go: skill registry interface.
//
// Skills are named bundles loaded into a recipient's context for a
// single turn. The wire surface (/<skill-name> in chat content) is
// Epic B; Epic A locks the interface and ships a null implementation
// so the !skills discovery action has something to consume.
//
// Per spec §6.3, skill bundles live on disk at:
//   <aspect-home>/.nexus/skills/<skill-name>/
//       SKILL.md             — frontmatter + body
//       <supporting>.md      — referenced from SKILL.md
//
// Epic B fills in a real implementation that scans those dirs.

package broker

// SkillDescriptor is one skill's metadata + content.
type SkillDescriptor struct {
	Name        string
	Description string
	Content     string // SKILL.md body; supporting files inlined or referenced
}

// SkillRegistry resolves skills by name. Epic A consumes the interface
// from the !skills action; Epic B provides a real implementation.
type SkillRegistry interface {
	// List returns all known skill descriptors. Order is implementation-defined.
	List() []SkillDescriptor
	// Get returns the descriptor for a skill by name, plus a presence bool.
	Get(name string) (SkillDescriptor, bool)
}

// NullSkillRegistry is the Epic A stub — no skills registered.
type NullSkillRegistry struct{}

// List always returns nil.
func (NullSkillRegistry) List() []SkillDescriptor { return nil }

// Get always returns the zero descriptor and false.
func (NullSkillRegistry) Get(name string) (SkillDescriptor, bool) {
	return SkillDescriptor{}, false
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestNullSkillRegistry -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/skillregistry.go nexus/broker/skillregistry_test.go
git commit -m "feat(broker): SkillRegistry interface + NullSkillRegistry (Epic A stub)"
```

---

## Phase 3 — Discoverability actions

`!actions` and `!skills` are simple registry-consumers; build them before `!dispatch` to keep complexity low while exercising the action surface.

### Task 5: ListActionsAction

**Files:**
- Create: `nexus/broker/action_listactions.go`
- Create: `nexus/broker/action_listactions_test.go`

- [ ] **Step 1: Write the failing test**

Create `nexus/broker/action_listactions_test.go`:

```go
package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

func TestListActionsAction_NoArgs_ListsAll(t *testing.T) {
	reg := NewActionRegistry()
	reg.Register(stubAction{name: "dispatch", description: "spawn a hand"})
	reg.Register(stubAction{name: "actions", description: "list actions"})
	action := NewListActionsAction(reg)

	env, err := action.Handle(context.Background(), frames.Envelope{}, "")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	got := envelopeText(t, env)
	if !strings.Contains(got, "!dispatch") {
		t.Errorf("output missing !dispatch listing; got:\n%s", got)
	}
	if !strings.Contains(got, "!actions") {
		t.Errorf("output missing !actions listing; got:\n%s", got)
	}
	if !strings.Contains(got, "spawn a hand") {
		t.Errorf("output missing dispatch description; got:\n%s", got)
	}
}

func TestListActionsAction_WithName_ReturnsHelp(t *testing.T) {
	reg := NewActionRegistry()
	reg.Register(stubAction{name: "dispatch", description: "spawn a hand", help: "Spawn a fresh-context hand...\nMulti-line detail."})
	action := NewListActionsAction(reg)

	env, err := action.Handle(context.Background(), frames.Envelope{}, "dispatch")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	got := envelopeText(t, env)
	if !strings.Contains(got, "Spawn a fresh-context hand") {
		t.Errorf("output missing help text; got:\n%s", got)
	}
	if !strings.Contains(got, "Multi-line detail") {
		t.Errorf("output missing multi-line help body; got:\n%s", got)
	}
}

func TestListActionsAction_UnknownName_Errors(t *testing.T) {
	reg := NewActionRegistry()
	action := NewListActionsAction(reg)

	env, err := action.Handle(context.Background(), frames.Envelope{}, "nonexistent")
	if err != nil {
		t.Fatalf("Handle returned go error (want envelope-level error): %v", err)
	}
	if env.Kind != frames.KindDispatchError {
		t.Errorf("env.Kind = %q, want %q", env.Kind, frames.KindDispatchError)
	}
}

// envelopeText extracts the human-readable text from a chat-send-style
// response envelope. The action returns its output as a frame whose
// payload carries a "content" field; tests inspect that.
func envelopeText(t *testing.T, env frames.Envelope) string {
	t.Helper()
	if env.Payload == nil {
		return ""
	}
	if s, ok := env.Payload["content"].(string); ok {
		return s
	}
	return ""
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./nexus/broker/ -run TestListActionsAction -v
```

Expected: FAIL with `undefined: NewListActionsAction`.

- [ ] **Step 3: Implement ListActionsAction**

Create `nexus/broker/action_listactions.go`:

```go
// Package broker — action_listactions.go: !actions discovery handler.

package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// ListActionsAction handles "!actions [name]". No args lists all
// registered actions; an arg returns the help text for that action.
type ListActionsAction struct {
	registry *ActionRegistry
}

// NewListActionsAction constructs the action against an existing registry.
func NewListActionsAction(reg *ActionRegistry) *ListActionsAction {
	return &ListActionsAction{registry: reg}
}

// Name implements Action.
func (a *ListActionsAction) Name() string { return "actions" }

// Description implements Action.
func (a *ListActionsAction) Description() string { return "List actions, or detail on one" }

// Help implements Action.
func (a *ListActionsAction) Help() string {
	return `!actions [name]

Lists all registered comms-protocol actions when called without args.
With a name argument, returns multi-line help for that action.

Examples:
  !actions
  !actions dispatch`
}

// Handle implements Action.
func (a *ListActionsAction) Handle(ctx context.Context, env frames.Envelope, args string) (frames.Envelope, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return a.listAll(env), nil
	}
	return a.detailFor(env, args), nil
}

func (a *ListActionsAction) listAll(env frames.Envelope) frames.Envelope {
	var b strings.Builder
	b.WriteString("Available actions:\n\n")
	for _, d := range a.registry.List() {
		fmt.Fprintf(&b, "  !%-20s %s\n", d.Name, d.Description)
	}
	b.WriteString("\nUse `!actions <name>` for detail.\n")
	return frames.Envelope{
		Kind:    frames.KindChatSend,
		Payload: map[string]any{"content": b.String()},
	}
}

func (a *ListActionsAction) detailFor(env frames.Envelope, name string) frames.Envelope {
	action, ok := a.registry.Lookup(name)
	if !ok {
		return frames.Envelope{
			Kind: frames.KindDispatchError,
			Payload: map[string]any{
				"reason": fmt.Sprintf("unknown action: %s", name),
				"code":   "unknown_action",
			},
		}
	}
	return frames.Envelope{
		Kind:    frames.KindChatSend,
		Payload: map[string]any{"content": action.Help()},
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestListActionsAction -v
```

Expected: PASS — all three cases.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/action_listactions.go nexus/broker/action_listactions_test.go
git commit -m "feat(broker): !actions discovery handler"
```

### Task 6: ListSkillsAction (Epic A stub)

**Files:**
- Create: `nexus/broker/action_listskills.go`
- Create: `nexus/broker/action_listskills_test.go`

- [ ] **Step 1: Write the failing test**

Create `nexus/broker/action_listskills_test.go`:

```go
package broker

import (
	"context"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

func TestListSkillsAction_NullRegistry_ReturnsStubMessage(t *testing.T) {
	action := NewListSkillsAction(NullSkillRegistry{})

	env, err := action.Handle(context.Background(), frames.Envelope{}, "")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	got := envelopeText(t, env)
	if !strings.Contains(got, "not yet implemented") {
		t.Errorf("null-registry response missing stub message; got:\n%s", got)
	}
	if !strings.Contains(got, "Epic B") {
		t.Errorf("null-registry response missing Epic B reference; got:\n%s", got)
	}
}

// fakeSkillRegistry feeds a non-null registry into the action for
// the happy-path test path. Used here AND in future Epic B tests
// when a real registry is wired in.
type fakeSkillRegistry struct {
	descriptors []SkillDescriptor
}

func (f fakeSkillRegistry) List() []SkillDescriptor { return f.descriptors }
func (f fakeSkillRegistry) Get(name string) (SkillDescriptor, bool) {
	for _, d := range f.descriptors {
		if d.Name == name {
			return d, true
		}
	}
	return SkillDescriptor{}, false
}

func TestListSkillsAction_PopulatedRegistry_ListsSkills(t *testing.T) {
	reg := fakeSkillRegistry{descriptors: []SkillDescriptor{
		{Name: "review", Description: "two-stage code review"},
		{Name: "brainstorm", Description: "idea exploration"},
	}}
	action := NewListSkillsAction(reg)

	env, err := action.Handle(context.Background(), frames.Envelope{}, "")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	got := envelopeText(t, env)
	if !strings.Contains(got, "/review") {
		t.Errorf("output missing /review; got:\n%s", got)
	}
	if !strings.Contains(got, "two-stage code review") {
		t.Errorf("output missing review description; got:\n%s", got)
	}
	if !strings.Contains(got, "/brainstorm") {
		t.Errorf("output missing /brainstorm; got:\n%s", got)
	}
}

func TestListSkillsAction_WithName_NotFound(t *testing.T) {
	action := NewListSkillsAction(NullSkillRegistry{})

	env, err := action.Handle(context.Background(), frames.Envelope{}, "nonexistent")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if env.Kind != frames.KindDispatchError {
		t.Errorf("env.Kind = %q, want %q", env.Kind, frames.KindDispatchError)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./nexus/broker/ -run TestListSkillsAction -v
```

Expected: FAIL with `undefined: NewListSkillsAction`.

- [ ] **Step 3: Implement ListSkillsAction**

Create `nexus/broker/action_listskills.go`:

```go
// Package broker — action_listskills.go: !skills discovery handler.

package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

const skillStubMessage = `Skill registry not yet implemented (Epic B).
This action will list shared network skills once the registry ships.
For now, skills exist only in the local CC session via claude-code's
/<skill> mechanism.
`

// ListSkillsAction handles "!skills [name]". Consumes a SkillRegistry;
// Epic A wires the NullSkillRegistry, Epic B wires a real one.
type ListSkillsAction struct {
	registry SkillRegistry
}

// NewListSkillsAction constructs the action against a SkillRegistry.
func NewListSkillsAction(reg SkillRegistry) *ListSkillsAction {
	return &ListSkillsAction{registry: reg}
}

// Name implements Action.
func (a *ListSkillsAction) Name() string { return "skills" }

// Description implements Action.
func (a *ListSkillsAction) Description() string { return "List skills, or detail on one" }

// Help implements Action.
func (a *ListSkillsAction) Help() string {
	return `!skills [name]

Lists all known skills when called without args. With a name argument,
returns the skill's description.

Skills are recipient-side context loaders — invoking /<name> in chat
content loads the skill's SKILL.md into the recipient's boot context
for that turn. Skill registry implementation lands in Epic B.`
}

// Handle implements Action.
func (a *ListSkillsAction) Handle(ctx context.Context, env frames.Envelope, args string) (frames.Envelope, error) {
	args = strings.TrimSpace(args)
	descriptors := a.registry.List()

	if args == "" {
		if len(descriptors) == 0 {
			return frames.Envelope{
				Kind:    frames.KindChatSend,
				Payload: map[string]any{"content": skillStubMessage},
			}, nil
		}
		return a.listAll(descriptors), nil
	}
	return a.detailFor(args), nil
}

func (a *ListSkillsAction) listAll(descriptors []SkillDescriptor) frames.Envelope {
	var b strings.Builder
	b.WriteString("Available skills:\n\n")
	for _, d := range descriptors {
		fmt.Fprintf(&b, "  /%-20s %s\n", d.Name, d.Description)
	}
	b.WriteString("\nUse `!skills <name>` for detail.\n")
	return frames.Envelope{
		Kind:    frames.KindChatSend,
		Payload: map[string]any{"content": b.String()},
	}
}

func (a *ListSkillsAction) detailFor(name string) frames.Envelope {
	d, ok := a.registry.Get(name)
	if !ok {
		return frames.Envelope{
			Kind: frames.KindDispatchError,
			Payload: map[string]any{
				"reason": fmt.Sprintf("unknown skill: %s", name),
				"code":   "unknown_skill",
			},
		}
	}
	return frames.Envelope{
		Kind:    frames.KindChatSend,
		Payload: map[string]any{"content": fmt.Sprintf("/%s\n\n%s", d.Name, d.Description)},
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestListSkillsAction -v
```

Expected: PASS — all three cases.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/action_listskills.go nexus/broker/action_listskills_test.go
git commit -m "feat(broker): !skills discovery handler (Epic A stub via NullSkillRegistry)"
```

---

## Phase 4 — Identity framing in handexec

Finish the v0.1 deferred work in `runtime/handexec`. Independent of broker work; can parallelise with Phases 1–3.

### Task 7: Identity loader — read files

**Files:**
- Create: `runtime/handexec/identity.go`
- Create: `runtime/handexec/identity_test.go`

- [ ] **Step 1: Write the failing test**

Create `runtime/handexec/identity_test.go`:

```go
package handexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadIdentity_ReadsAllFiles(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "NEXUS.md"), []byte("role: builder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "SOUL.md"), []byte("personality: dry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "PRIMER.md"), []byte("primer: tools\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := LoadIdentity(home)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !strings.Contains(id.Nexus, "role: builder") {
		t.Errorf("Nexus content missing: %q", id.Nexus)
	}
	if !strings.Contains(id.Soul, "personality: dry") {
		t.Errorf("Soul content missing: %q", id.Soul)
	}
	if !strings.Contains(id.Primer, "primer: tools") {
		t.Errorf("Primer content missing: %q", id.Primer)
	}
}

func TestLoadIdentity_MissingNexusFails(t *testing.T) {
	home := t.TempDir()
	// no NEXUS.md

	_, err := LoadIdentity(home)
	if err == nil {
		t.Fatal("LoadIdentity returned nil error for missing NEXUS.md; want error")
	}
	if !strings.Contains(err.Error(), "NEXUS.md") {
		t.Errorf("error message missing NEXUS.md: %v", err)
	}
}

func TestLoadIdentity_OptionalFilesMissing_OK(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "NEXUS.md"), []byte("role: builder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// no SOUL.md, no PRIMER.md

	id, err := LoadIdentity(home)
	if err != nil {
		t.Fatalf("LoadIdentity: %v (want success with empty optional files)", err)
	}
	if id.Soul != "" {
		t.Errorf("Soul = %q, want empty", id.Soul)
	}
	if id.Primer != "" {
		t.Errorf("Primer = %q, want empty", id.Primer)
	}
}

func TestLoadIdentity_MissingHomeDir(t *testing.T) {
	_, err := LoadIdentity("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("LoadIdentity returned nil error for missing home; want error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd ~/Source/nexus
go test ./runtime/handexec/ -run TestLoadIdentity -v
```

Expected: FAIL with `undefined: LoadIdentity`.

- [ ] **Step 3: Implement LoadIdentity**

Create `runtime/handexec/identity.go`:

```go
// Package handexec — identity.go: NEXUS.md / SOUL.md / PRIMER loading
// and system-prompt composition per spec §4 / hand-dispatch v0.1 §2.1.
//
// On worker boot, identity files are read from the dispatching aspect's
// home directory and composed into a single system prompt. The hand
// runs as a fresh-context instance of the dispatching aspect, with
// that aspect's identity loaded — not a generic worker.

package handexec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Identity holds the three files that compose an aspect's persona.
// Nexus is required (role definition is mandatory per spec §4.3).
// Soul and Primer are optional enrichment — empty strings if absent.
type Identity struct {
	Nexus  string
	Soul   string
	Primer string
}

// LoadIdentity reads NEXUS.md (required), SOUL.md (optional), and
// PRIMER.md (optional) from aspectHome. Returns an error if NEXUS.md
// is missing or unreadable, if aspectHome itself doesn't exist, or
// on any I/O error.
func LoadIdentity(aspectHome string) (Identity, error) {
	if aspectHome == "" {
		return Identity{}, errors.New("handexec.LoadIdentity: aspectHome required")
	}
	info, err := os.Stat(aspectHome)
	if err != nil {
		return Identity{}, fmt.Errorf("handexec.LoadIdentity: aspect home not accessible: %w", err)
	}
	if !info.IsDir() {
		return Identity{}, fmt.Errorf("handexec.LoadIdentity: aspect home not a directory: %s", aspectHome)
	}

	nexus, err := readRequired(filepath.Join(aspectHome, "NEXUS.md"))
	if err != nil {
		return Identity{}, err
	}
	soul, err := readOptional(filepath.Join(aspectHome, "SOUL.md"))
	if err != nil {
		return Identity{}, err
	}
	primer, err := readOptional(filepath.Join(aspectHome, "PRIMER.md"))
	if err != nil {
		return Identity{}, err
	}
	return Identity{Nexus: nexus, Soul: soul, Primer: primer}, nil
}

// readRequired returns the file contents or an error if the file is
// missing or unreadable.
func readRequired(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("handexec.LoadIdentity: required file %s: %w", filepath.Base(path), err)
	}
	return string(data), nil
}

// readOptional returns the file contents, or empty string if the file
// is missing. Returns an error only on read failures other than os.IsNotExist.
func readOptional(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("handexec.LoadIdentity: optional file %s: %w", filepath.Base(path), err)
	}
	return string(data), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./runtime/handexec/ -run TestLoadIdentity -v
```

Expected: PASS — all four cases.

- [ ] **Step 5: Commit**

```bash
git add runtime/handexec/identity.go runtime/handexec/identity_test.go
git commit -m "feat(handexec): LoadIdentity — read NEXUS.md/SOUL.md/PRIMER from aspect home"
```

### Task 8: Compose system prompt

**Files:**
- Modify: `runtime/handexec/identity.go`
- Modify: `runtime/handexec/identity_test.go`

- [ ] **Step 1: Write the failing test**

Append to `runtime/handexec/identity_test.go`:

```go
func TestComposeSystemPrompt_AllPresent(t *testing.T) {
	id := Identity{
		Nexus:  "role: builder\n",
		Soul:   "personality: dry\n",
		Primer: "primer: tools\n",
	}
	got := ComposeSystemPrompt(id, "thread-42")

	for _, want := range []string{"role: builder", "personality: dry", "primer: tools", "thread-42", "fresh-context instance", "one turn"} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing %q; full prompt:\n%s", want, got)
		}
	}
}

func TestComposeSystemPrompt_OnlyNexus(t *testing.T) {
	id := Identity{Nexus: "role: builder\n"}
	got := ComposeSystemPrompt(id, "thread-1")

	if !strings.Contains(got, "role: builder") {
		t.Errorf("missing nexus content; got:\n%s", got)
	}
	// no SOUL.md / PRIMER.md content but the framing footer should still appear
	if !strings.Contains(got, "fresh-context instance") {
		t.Errorf("missing framing footer; got:\n%s", got)
	}
}

func TestComposeSystemPrompt_SeparatorBetweenSections(t *testing.T) {
	id := Identity{
		Nexus: "ROLE-MARKER",
		Soul:  "SOUL-MARKER",
	}
	got := ComposeSystemPrompt(id, "")

	roleIdx := strings.Index(got, "ROLE-MARKER")
	soulIdx := strings.Index(got, "SOUL-MARKER")
	if roleIdx < 0 || soulIdx < 0 {
		t.Fatalf("missing markers; roleIdx=%d soulIdx=%d", roleIdx, soulIdx)
	}
	between := got[roleIdx+len("ROLE-MARKER") : soulIdx]
	if !strings.Contains(between, "---") {
		t.Errorf("expected --- separator between role and soul; got: %q", between)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./runtime/handexec/ -run TestComposeSystemPrompt -v
```

Expected: FAIL with `undefined: ComposeSystemPrompt`.

- [ ] **Step 3: Implement ComposeSystemPrompt**

Append to `runtime/handexec/identity.go`:

```go
// ComposeSystemPrompt assembles the aspect's identity files into the
// system prompt the worker subprocess receives. Layout:
//
//	[NEXUS.md content]
//	---
//	[SOUL.md content]              # if non-empty
//	---
//	[PRIMER.md content]            # if non-empty
//	---
//	You are running as a hand — a fresh-context instance...
//
// thread is the originating thread ID; included in the framing footer
// so the worker knows where its reply will land.
func ComposeSystemPrompt(id Identity, thread string) string {
	var sections []string
	sections = append(sections, id.Nexus)
	if id.Soul != "" {
		sections = append(sections, id.Soul)
	}
	if id.Primer != "" {
		sections = append(sections, id.Primer)
	}
	footer := fmt.Sprintf(
		"You are running as a hand — a fresh-context instance of yourself "+
			"dispatched to do a focused side task. Your reply lands back on "+
			"thread %s as a chat message. You have one turn.",
		threadOrUnknown(thread),
	)
	sections = append(sections, footer)

	return joinWithSeparator(sections, "\n---\n")
}

func threadOrUnknown(thread string) string {
	if thread == "" {
		return "(unspecified)"
	}
	return thread
}

func joinWithSeparator(sections []string, sep string) string {
	if len(sections) == 0 {
		return ""
	}
	out := sections[0]
	for _, s := range sections[1:] {
		out += sep + s
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./runtime/handexec/ -run TestComposeSystemPrompt -v
```

Expected: PASS — all three cases.

- [ ] **Step 5: Commit**

```bash
git add runtime/handexec/identity.go runtime/handexec/identity_test.go
git commit -m "feat(handexec): ComposeSystemPrompt — assemble identity files + footer"
```

### Task 9: Wire identity composition into handexec.Run

**Files:**
- Modify: `runtime/handexec/handexec.go`
- Modify: `runtime/handexec/handexec_test.go`

- [ ] **Step 1: Read the existing Run function and identify the integration point**

Read `runtime/handexec/handexec.go` lines 55–105. Locate the deferred-identity comment block (around line 60–75 per the existing source) and the provider invocation that follows.

- [ ] **Step 2: Write a test that asserts the system prompt is composed and passed**

Append to `runtime/handexec/handexec_test.go` (create if absent):

```go
package handexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/runtime/providers"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// recordingProvider captures the args it was invoked with so tests can
// assert what the harness composed and passed.
type recordingProvider struct {
	gotSystemPrompt string
	gotUserMessage  string
}

func (p *recordingProvider) Name() string { return "recording" }

// Invoke is the minimal subset of providers.Provider that handexec.Run
// exercises. Concrete signature mirrors what Run currently calls. If
// the provider interface differs, adapt the test to match the real
// surface in runtime/providers/.
func (p *recordingProvider) Invoke(ctx context.Context, req providers.InvokeRequest) (providers.InvokeResponse, error) {
	p.gotSystemPrompt = req.SystemPrompt
	p.gotUserMessage = req.UserMessage
	return providers.InvokeResponse{Output: map[string]any{"text": "OK"}}, nil
}

func TestRun_ComposesIdentityAndPassesPayload(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "NEXUS.md"), []byte("ROLE-MARKER"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "SOUL.md"), []byte("SOUL-MARKER"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Build a fake stdin that handexec.Run will parse as a Request.
	stdin := strings.NewReader(`{"aspect":"anvil","thread":"t-1","dispatch_id":"d-1","payload":{"prompt":"do the thing"}}`)
	prevStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = prevStdin }()
	go func() {
		_, _ = w.Write([]byte(stdin.String()))
		_ = w.Close()
	}()

	provider := &recordingProvider{}
	if err := Run(context.Background(), home, schemas.AspectConfig{}, provider); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(provider.gotSystemPrompt, "ROLE-MARKER") {
		t.Errorf("system prompt missing NEXUS.md content; got:\n%s", provider.gotSystemPrompt)
	}
	if !strings.Contains(provider.gotSystemPrompt, "SOUL-MARKER") {
		t.Errorf("system prompt missing SOUL.md content; got:\n%s", provider.gotSystemPrompt)
	}
	if !strings.Contains(provider.gotSystemPrompt, "t-1") {
		t.Errorf("system prompt missing thread ID; got:\n%s", provider.gotSystemPrompt)
	}
	if !strings.Contains(provider.gotUserMessage, "do the thing") {
		t.Errorf("user message missing payload; got:\n%s", provider.gotUserMessage)
	}
}
```

**Note for implementer:** the test uses `providers.InvokeRequest` / `InvokeResponse` shapes. Inspect `runtime/providers/` for the real signatures and adapt. If the current Run function doesn't currently accept a `SystemPrompt` parameter on the provider call, the implementation step adds that wiring (existing code passes the payload directly without identity framing; this task closes that gap).

- [ ] **Step 3: Run the test to verify it fails**

```bash
go test ./runtime/handexec/ -run TestRun_ComposesIdentityAndPassesPayload -v
```

Expected: FAIL — provider doesn't receive a composed system prompt today.

- [ ] **Step 4: Wire identity loading + composition into Run**

Modify `runtime/handexec/handexec.go`. Inside `Run`, after reading the Request from stdin and before invoking the provider:

```go
// Load identity files and compose the system prompt per
// docs/2026-05-17-comms-protocol-actions-spec.md §4. Fail
// closed: a missing or unreadable NEXUS.md is a hard error.
identity, err := LoadIdentity(aspectHome)
if err != nil {
    return writeAndReturnError(req, "identity load", err)
}
systemPrompt := ComposeSystemPrompt(identity, req.Thread)

// Forward to provider with the composed system prompt and the
// payload as the first user message. The exact field names depend
// on the provider interface in runtime/providers/.
userMessage := extractPromptFromPayload(req.Payload)
resp, err := provider.Invoke(ctx, providers.InvokeRequest{
    SystemPrompt: systemPrompt,
    UserMessage:  userMessage,
})
if err != nil {
    return writeAndReturnProviderError(req, err)
}
```

Add helper if not present:

```go
// extractPromptFromPayload pulls the user-facing prompt text from a
// dispatch payload. v0.1 payload shape: map with "prompt" key holding
// the text. Future enhancements may add structured fields; until then
// fall back to JSON-encoded payload if "prompt" is absent so the
// worker still gets something to act on.
func extractPromptFromPayload(payload map[string]any) string {
    if s, ok := payload["prompt"].(string); ok {
        return s
    }
    encoded, err := json.Marshal(payload)
    if err != nil {
        return ""
    }
    return string(encoded)
}
```

If the existing `provider.Invoke` signature differs from `InvokeRequest{SystemPrompt, UserMessage}`, adapt: extend the existing request struct in `runtime/providers/` with a `SystemPrompt` field if missing, or use the closest available field (e.g., a leading message in a message list). Open a small PR against the providers package if the surface needs extending — link from this PR's description.

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./runtime/handexec/ -v
```

Expected: PASS — new test plus existing tests still pass.

- [ ] **Step 6: Update the deferred-identity NOTE in the file header**

Remove or edit the comment block at the top of `runtime/handexec/handexec.go` (around line 17) that says "identity-bundle loading ... lives behind the Frame harness work. v0.1 keeps the spawn machinery in place but currently invokes the provider with the dispatch payload directly". Replace with:

```go
// Identity bundle loading (NEXUS.md / SOUL.md / PRIMER → composed
// system prompt) is implemented per spec §4 of
// docs/2026-05-17-comms-protocol-actions-spec.md. Each worker boot
// reads identity files from aspectHome and composes them into the
// system prompt passed to the provider.
```

- [ ] **Step 7: Commit**

```bash
git add runtime/handexec/handexec.go runtime/handexec/handexec_test.go
git commit -m "feat(handexec): compose identity system prompt before provider invocation

Loads NEXUS.md (required), SOUL.md, PRIMER.md from aspect home and
composes them into the system prompt the worker subprocess receives.
Payload becomes the first user message. Finishes the deferred §2.1
identity-framing invariant from hand-dispatch v0.1."
```

---

## Phase 5 — `!dispatch` action

The headline feature. Wires the action surface to handqueue. Depends on Phases 1 (registry primitives), 2 (skill stub — `!skills` co-registered), and 4 (handexec identity, so dispatches actually behave as the calling aspect).

### Task 10: KindDispatchAccepted frame + payload

**Files:**
- Modify: `nexus/frames/frames.go`
- Modify: `nexus/frames/payloads.go`
- Modify: `nexus/frames/frames_test.go` (existing tests at line 169 already iterate kind lists)

- [ ] **Step 1: Add the new frame kind constant**

In `nexus/frames/frames.go`, alongside the existing `KindDispatch / KindDispatchResult / KindDispatchError`:

```go
// KindDispatchAccepted is the synchronous ack sent back to a caller
// after the broker has accepted a !dispatch action invocation and
// queued (or immediately spawned) the worker. Per spec §3.3, this
// lets the caller obtain a dispatch_id for correlation before the
// result eventually arrives as a chat frame on the originating thread.
KindDispatchAccepted Kind = "dispatch.accepted"
```

Add `KindDispatchAccepted` to any kind-iteration test lists in `frames.go` and `frames_test.go` that already enumerate `KindDispatch`, `KindDispatchResult`, `KindDispatchError`.

- [ ] **Step 2: Add the payload type**

In `nexus/frames/payloads.go`, after `DispatchErrorPayload`:

```go
// DispatchAcceptedPayload is the synchronous ack returned to the
// caller of !dispatch. Carries the assigned dispatch_id (so the
// caller can correlate the eventual result chat frame) and the
// caller's queue position (0 if a worker spawned immediately, >0 if
// queued behind other dispatches per handqueue fairness scheduling).
type DispatchAcceptedPayload struct {
	Aspect        string `json:"aspect"`
	DispatchID    string `json:"dispatch_id"`
	QueuePosition int    `json:"queue_position"`
}
```

- [ ] **Step 3: Run existing frame tests to verify no regression**

```bash
cd ~/Source/nexus
go test ./nexus/frames/ -v
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add nexus/frames/frames.go nexus/frames/payloads.go nexus/frames/frames_test.go
git commit -m "feat(frames): KindDispatchAccepted + DispatchAcceptedPayload for !dispatch ack"
```

### Task 11: DispatchAction handler — happy path

**Files:**
- Create: `nexus/broker/action_dispatch.go`
- Create: `nexus/broker/action_dispatch_test.go`

- [ ] **Step 1: Write the failing test for happy-path submission**

Create `nexus/broker/action_dispatch_test.go`:

```go
package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// fakeHandQueue implements the small surface DispatchAction needs.
// Records Submit calls; lets tests assert payload shape.
type fakeHandQueue struct {
	submitted []FakeSubmission
	nextID    string
	nextPos   int
	submitErr error
}

type FakeSubmission struct {
	Aspect     string
	Thread     string
	DispatchID string
	Payload    map[string]any
}

func (f *fakeHandQueue) Submit(ctx context.Context, req HandQueueRequest) (HandQueueAck, error) {
	if f.submitErr != nil {
		return HandQueueAck{}, f.submitErr
	}
	id := f.nextID
	if id == "" {
		id = "test-dispatch-id"
	}
	f.submitted = append(f.submitted, FakeSubmission{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: id,
		Payload:    req.Payload,
	})
	return HandQueueAck{DispatchID: id, QueuePosition: f.nextPos}, nil
}

func TestDispatchAction_HappyPath(t *testing.T) {
	q := &fakeHandQueue{nextID: "d-42", nextPos: 0}
	action := NewDispatchAction(q)

	env := frames.Envelope{
		From:  "anvil",
		Topic: "anvil-nex-141",
	}
	resp, err := action.Handle(context.Background(), env, "implement NEX-179 release-fetcher")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if resp.Kind != frames.KindDispatchAccepted {
		t.Errorf("resp.Kind = %q, want %q", resp.Kind, frames.KindDispatchAccepted)
	}
	if got, ok := resp.Payload["dispatch_id"].(string); !ok || got != "d-42" {
		t.Errorf("dispatch_id = %v, want d-42", resp.Payload["dispatch_id"])
	}
	if got, ok := resp.Payload["queue_position"].(int); !ok || got != 0 {
		t.Errorf("queue_position = %v, want 0", resp.Payload["queue_position"])
	}

	if len(q.submitted) != 1 {
		t.Fatalf("submitted %d, want 1", len(q.submitted))
	}
	sub := q.submitted[0]
	if sub.Aspect != "anvil" {
		t.Errorf("submitted Aspect=%q, want anvil", sub.Aspect)
	}
	if sub.Thread != "anvil-nex-141" {
		t.Errorf("submitted Thread=%q, want anvil-nex-141", sub.Thread)
	}
	if prompt, ok := sub.Payload["prompt"].(string); !ok || prompt != "implement NEX-179 release-fetcher" {
		t.Errorf("submitted payload prompt=%v, want %q", sub.Payload["prompt"], "implement NEX-179 release-fetcher")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./nexus/broker/ -run TestDispatchAction_HappyPath -v
```

Expected: FAIL with `undefined: NewDispatchAction`, `HandQueueRequest`, `HandQueueAck`.

- [ ] **Step 3: Implement DispatchAction + HandQueue interface**

Create `nexus/broker/action_dispatch.go`:

```go
// Package broker — action_dispatch.go: !dispatch handler.
//
// !dispatch <payload>:
//   1. Read calling aspect from envelope (authenticated wsConn name).
//   2. Validate non-empty payload.
//   3. Submit to handqueue with calling aspect identity.
//   4. Return KindDispatchAccepted with dispatch_id + queue position.
//
// Per spec §3.2 and §7.1: identity is self-only — taken from the
// envelope's From field, not user-supplied. Hands always boot as the
// caller.
//
// Per spec §7.5 edge cases: empty payload, unregistered caller, and
// hard-ceiling rejection produce KindDispatchError with distinct
// error_class values.

package broker

import (
	"context"
	"errors"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// HandQueue is the surface DispatchAction needs from the existing
// handqueue package. Defined as a broker-local interface so the action
// can be tested with a fake without depending on the real handqueue's
// machinery in unit tests.
//
// The real binding adapts handqueue.Queue.Submit to this shape.
type HandQueue interface {
	Submit(ctx context.Context, req HandQueueRequest) (HandQueueAck, error)
}

// HandQueueRequest is the broker-local shape of a dispatch submission.
type HandQueueRequest struct {
	Aspect  string
	Thread  string
	Payload map[string]any
}

// HandQueueAck is the broker-local shape of the queue's accept response.
type HandQueueAck struct {
	DispatchID    string
	QueuePosition int
}

// Common error sentinels the action uses to construct KindDispatchError
// envelopes. The handqueue adapter (not built here) maps the real
// handqueue errors to these.
var (
	ErrEmptyPayload      = errors.New("empty_payload")
	ErrCallerUnregistered = errors.New("caller_not_registered")
	ErrHardCeiling       = errors.New("hard_ceiling_reached")
	ErrIdentityLoadFailed = errors.New("identity_load_failed")
)

// DispatchAction handles "!dispatch <payload>".
type DispatchAction struct {
	queue HandQueue
}

// NewDispatchAction constructs the action against a HandQueue.
func NewDispatchAction(q HandQueue) *DispatchAction {
	return &DispatchAction{queue: q}
}

// Name implements Action.
func (a *DispatchAction) Name() string { return "dispatch" }

// Description implements Action.
func (a *DispatchAction) Description() string {
	return "Spawn a fresh-context hand of yourself, run with context"
}

// Help implements Action.
func (a *DispatchAction) Help() string {
	return `!dispatch <context>

Spawn a fresh-context hand of yourself with the given context.
The hand runs as you (same identity, same NEXUS.md / SOUL.md / PRIMER),
processes the context as its first turn input, returns the result
on this thread as a chat message.

Concurrent multi-dispatch supported — fire several !dispatch calls,
each returns when complete with a unique dispatch_id. Reply correlates
back via the dispatch_id in the chat frame's metadata.

Identity is self-only — you cannot dispatch as another aspect.

See: docs/2026-04-30-hand-dispatch-v0_1.md
     docs/2026-05-17-comms-protocol-actions-spec.md`
}

// Handle implements Action.
func (a *DispatchAction) Handle(ctx context.Context, env frames.Envelope, args string) (frames.Envelope, error) {
	if env.From == "" {
		return errorEnvelope(ErrCallerUnregistered.Error(), "caller_not_registered"), nil
	}
	if args == "" {
		return errorEnvelope(ErrEmptyPayload.Error(), "empty_payload"), nil
	}

	ack, err := a.queue.Submit(ctx, HandQueueRequest{
		Aspect:  env.From,
		Thread:  env.Topic,
		Payload: map[string]any{"prompt": args},
	})
	if err != nil {
		return errorEnvelope(err.Error(), classifyDispatchError(err)), nil
	}

	return frames.Envelope{
		Kind: frames.KindDispatchAccepted,
		Payload: map[string]any{
			"aspect":         env.From,
			"dispatch_id":    ack.DispatchID,
			"queue_position": ack.QueuePosition,
		},
	}, nil
}

// errorEnvelope builds a KindDispatchError frame with the given reason
// + error class. Used for both validation failures (empty payload,
// unregistered caller) and downstream errors from handqueue.Submit.
func errorEnvelope(reason, code string) frames.Envelope {
	return frames.Envelope{
		Kind: frames.KindDispatchError,
		Payload: map[string]any{
			"reason": reason,
			"code":   code,
		},
	}
}

// classifyDispatchError maps a queue error to the error_class string
// that spec §7 / §3.4 defines. Defaults to "internal" for unknown.
func classifyDispatchError(err error) string {
	switch {
	case errors.Is(err, ErrHardCeiling):
		return "hard_ceiling_reached"
	case errors.Is(err, ErrIdentityLoadFailed):
		return "identity_load_failed"
	default:
		return "internal"
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestDispatchAction_HappyPath -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/action_dispatch.go nexus/broker/action_dispatch_test.go
git commit -m "feat(broker): !dispatch action handler — happy path + queue submission"
```

### Task 12: DispatchAction — error paths

**Files:**
- Modify: `nexus/broker/action_dispatch_test.go`

- [ ] **Step 1: Write the failing tests for error paths**

Append to `nexus/broker/action_dispatch_test.go`:

```go
func TestDispatchAction_EmptyPayload(t *testing.T) {
	action := NewDispatchAction(&fakeHandQueue{})
	env := frames.Envelope{From: "anvil", Topic: "anvil-nex-141"}

	resp, err := action.Handle(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if resp.Kind != frames.KindDispatchError {
		t.Errorf("resp.Kind = %q, want %q", resp.Kind, frames.KindDispatchError)
	}
	if code, _ := resp.Payload["code"].(string); code != "empty_payload" {
		t.Errorf("error code = %q, want empty_payload", code)
	}
}

func TestDispatchAction_UnregisteredCaller(t *testing.T) {
	action := NewDispatchAction(&fakeHandQueue{})
	env := frames.Envelope{From: "", Topic: "any"}

	resp, err := action.Handle(context.Background(), env, "any work")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if resp.Kind != frames.KindDispatchError {
		t.Errorf("resp.Kind = %q, want %q", resp.Kind, frames.KindDispatchError)
	}
	if code, _ := resp.Payload["code"].(string); code != "caller_not_registered" {
		t.Errorf("error code = %q, want caller_not_registered", code)
	}
}

func TestDispatchAction_HardCeilingRejection(t *testing.T) {
	q := &fakeHandQueue{submitErr: ErrHardCeiling}
	action := NewDispatchAction(q)
	env := frames.Envelope{From: "anvil", Topic: "anvil-nex-141"}

	resp, err := action.Handle(context.Background(), env, "any work")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if resp.Kind != frames.KindDispatchError {
		t.Errorf("resp.Kind = %q, want %q", resp.Kind, frames.KindDispatchError)
	}
	if code, _ := resp.Payload["code"].(string); code != "hard_ceiling_reached" {
		t.Errorf("error code = %q, want hard_ceiling_reached", code)
	}
}

func TestDispatchAction_IdentityLoadFailed(t *testing.T) {
	q := &fakeHandQueue{submitErr: ErrIdentityLoadFailed}
	action := NewDispatchAction(q)
	env := frames.Envelope{From: "anvil", Topic: "anvil-nex-141"}

	resp, err := action.Handle(context.Background(), env, "any work")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if resp.Kind != frames.KindDispatchError {
		t.Errorf("resp.Kind = %q, want %q", resp.Kind, frames.KindDispatchError)
	}
	if code, _ := resp.Payload["code"].(string); code != "identity_load_failed" {
		t.Errorf("error code = %q, want identity_load_failed", code)
	}
}

func TestDispatchAction_InternalQueueError(t *testing.T) {
	q := &fakeHandQueue{submitErr: errors.New("queue exploded somehow")}
	action := NewDispatchAction(q)
	env := frames.Envelope{From: "anvil", Topic: "anvil-nex-141"}

	resp, err := action.Handle(context.Background(), env, "any work")
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if resp.Kind != frames.KindDispatchError {
		t.Errorf("resp.Kind = %q, want %q", resp.Kind, frames.KindDispatchError)
	}
	if code, _ := resp.Payload["code"].(string); code != "internal" {
		t.Errorf("error code = %q, want internal", code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they pass**

The implementation in Task 11 already covers all five cases.

```bash
go test ./nexus/broker/ -run TestDispatchAction -v
```

Expected: PASS — all six tests (happy path + 5 error cases).

- [ ] **Step 3: Commit**

```bash
git add nexus/broker/action_dispatch_test.go
git commit -m "test(broker): !dispatch error path coverage (5 error classes)"
```

### Task 13: HandQueue adapter — bridge action to real queue

**Files:**
- Create: `nexus/broker/handqueue_adapter.go`
- Create: `nexus/broker/handqueue_adapter_test.go`

- [ ] **Step 1: Inspect the real handqueue Submit signature**

```bash
cd ~/Source/nexus
grep -n "func.*Submit\|type Queue\|type DispatchAck" nexus/handqueue/queue.go nexus/handqueue/spawn.go | head -10
```

Note the real method signature, payload struct, and error types. You'll adapt around them.

- [ ] **Step 2: Write the failing test**

Create `nexus/broker/handqueue_adapter_test.go`:

```go
package broker

import (
	"context"
	"testing"
)

func TestHandQueueAdapter_PassesThroughHappyPath(t *testing.T) {
	// Construct a minimal real handqueue.Queue (or a test-double of it
	// that mirrors handqueue's interface). Then wrap with the adapter
	// and confirm a Submit flows through cleanly.
	//
	// Implementation note: handqueue.Queue construction may require an
	// AspectHomeResolver, TokenResolver, etc. Use t.TempDir() + minimal
	// stubs for these. If the real Queue is hard to instantiate in a
	// unit test, define a small interface in handqueue/ that both the
	// real Queue and a test double satisfy, and have the adapter
	// consume that interface.
	t.Skip("implement against the real handqueue.Queue signature discovered in Step 1")
}

func TestHandQueueAdapter_MapsHardCeilingError(t *testing.T) {
	t.Skip("implement against the real handqueue.Queue signature discovered in Step 1")
}
```

The skipped placeholders document what the adapter must verify. Fill them in once the real signature is in hand.

- [ ] **Step 3: Implement the adapter**

Create `nexus/broker/handqueue_adapter.go`:

```go
// Package broker — handqueue_adapter.go: bridges the broker-local
// HandQueue interface (consumed by DispatchAction) to the real
// nexus/handqueue.Queue implementation.
//
// The adapter exists so:
//   1. The action layer can be unit-tested with a fake HandQueue
//      without depending on handqueue's full machinery.
//   2. handqueue's error types (sentinel values from queue.go) get
//      translated to the broker-local sentinels (ErrHardCeiling,
//      ErrIdentityLoadFailed) that DispatchAction.classifyDispatchError
//      understands.
//   3. handqueue's payload struct (frames.DispatchPayload) is built
//      from the broker-local HandQueueRequest shape.

package broker

import (
	"context"
	"errors"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
	"github.com/CarriedWorldUniverse/nexus/nexus/handqueue"
)

// HandQueueAdapter wraps a handqueue.Queue to satisfy the broker's
// HandQueue interface.
type HandQueueAdapter struct {
	q *handqueue.Queue
}

// NewHandQueueAdapter constructs an adapter around the real queue.
func NewHandQueueAdapter(q *handqueue.Queue) *HandQueueAdapter {
	return &HandQueueAdapter{q: q}
}

// Submit implements HandQueue. Builds a handqueue submission, maps
// the response and any error back to broker-local types.
func (a *HandQueueAdapter) Submit(ctx context.Context, req HandQueueRequest) (HandQueueAck, error) {
	hqReq := handqueue.SubmissionRequest{
		Aspect:  req.Aspect,
		Thread:  req.Thread,
		Payload: req.Payload,
	}
	resp, err := a.q.Submit(ctx, hqReq)
	if err != nil {
		return HandQueueAck{}, translateHandqueueError(err)
	}
	return HandQueueAck{
		DispatchID:    resp.DispatchID,
		QueuePosition: resp.QueuePosition,
	}, nil
}

// translateHandqueueError maps handqueue's sentinel errors to the
// broker-local sentinels DispatchAction.classifyDispatchError understands.
//
// The exact handqueue sentinels (ErrHardCeiling, etc.) are confirmed
// in nexus/handqueue/queue.go. If the names differ from this draft,
// adapt accordingly.
func translateHandqueueError(err error) error {
	if errors.Is(err, handqueue.ErrHardCeiling) {
		return ErrHardCeiling
	}
	return err // unmapped errors pass through; DispatchAction classifies as "internal"
}
```

If `handqueue.SubmissionRequest`, `handqueue.Queue.Submit`, or `handqueue.ErrHardCeiling` don't match these names in the real source, rename in the adapter and add comments noting the actual handqueue API. The adapter is the single seam where naming differences are reconciled — DispatchAction stays stable.

If `handqueue` uses `frames.DispatchPayload` directly (it does, per the earlier survey of `runtime/handexec/handexec.go:46-50`), construct that instead:

```go
import "github.com/google/uuid"

func (a *HandQueueAdapter) Submit(ctx context.Context, req HandQueueRequest) (HandQueueAck, error) {
	dispatchID := uuid.NewString()
	payload := frames.DispatchPayload{
		Aspect:     req.Aspect,
		Thread:     req.Thread,
		DispatchID: dispatchID,
		Payload:    req.Payload,
	}
	// ... call a.q.Submit with the real signature
}
```

Verify with the real source which struct the queue expects.

- [ ] **Step 4: Fill in the skipped tests against the real handqueue.Queue**

Replace the `t.Skip` calls with real assertions. Construct a `handqueue.Queue` with stub resolvers (look at `nexus/handqueue/spawn_test.go` for patterns). Submit one request, verify the adapter translates correctly. Force a hard-ceiling rejection (small `HardCeiling: 0`?) and verify error translation.

- [ ] **Step 5: Run all broker tests**

```bash
go test ./nexus/broker/ -v
```

Expected: PASS — adapter tests + existing tests.

- [ ] **Step 6: Commit**

```bash
git add nexus/broker/handqueue_adapter.go nexus/broker/handqueue_adapter_test.go
git commit -m "feat(broker): HandQueueAdapter — bridge DispatchAction to nexus/handqueue.Queue"
```

---

## Phase 6 — Wire into HandleChatSend

Connect the parser + registry to the live ingress path.

### Task 14: Parser dispatch in HandleChatSend

**Files:**
- Modify: `nexus/broker/chat_send.go`
- Modify: `nexus/broker/chat_send_test.go` (existing tests at `observe_test.go:142` cover normal flow; new tests live alongside)

- [ ] **Step 1: Write the failing tests**

Create or append to `nexus/broker/chat_send_test.go`:

```go
package broker

import (
	"context"
	"testing"
)

// brokerWithActions constructs a broker test instance with an
// ActionRegistry attached. Use the project's existing broker test
// constructor; see observe_test.go for the canonical setup.
func brokerWithActions(t *testing.T) *Broker {
	t.Helper()
	// Reuse whatever helper observe_test.go uses to spin up a Broker
	// with a ChatStore. Add registry wiring after construction.
	b := newTestBroker(t)
	b.actions = NewActionRegistry()
	return b
}

func TestHandleChatSend_NormalContent_ForwardsAsBefore(t *testing.T) {
	b := brokerWithActions(t)
	id, err := b.HandleChatSend(context.Background(), "anvil", "hello there", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	if id == 0 {
		t.Errorf("normal chat should persist and return id > 0; got 0")
	}
	// Verify message is in the store via the existing ChatStore surface.
	// (Adapt to project's existing query helpers.)
}

func TestHandleChatSend_ActionContent_NotPersistedAsChat(t *testing.T) {
	b := brokerWithActions(t)
	b.actions.Register(stubAction{name: "test-action", handleFn: func(ctx context.Context, env frames.Envelope, args string) (frames.Envelope, error) {
		return frames.Envelope{Kind: frames.KindChatSend, Payload: map[string]any{"content": "ok"}}, nil
	}})

	// !test-action should be intercepted; the original content not
	// persisted to thread.
	id, err := b.HandleChatSend(context.Background(), "anvil", "!test-action arg", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	if id != 0 {
		t.Errorf("action content should NOT persist as a normal chat message; got id=%d", id)
	}
}

func TestHandleChatSend_UnknownAction_ReturnsError(t *testing.T) {
	b := brokerWithActions(t)
	// Empty registry.

	_, err := b.HandleChatSend(context.Background(), "anvil", "!nonexistent arg", 0, "")
	// The exact contract here depends on the implementation choice in
	// Step 3: either return an error from HandleChatSend, or surface
	// via a response frame to the caller. Pick one and assert it.
	if err == nil {
		t.Errorf("expected error or response-frame for unknown action")
	}
}

func TestHandleChatSend_SlashContent_TreatedAsNormalText_EpicA(t *testing.T) {
	b := brokerWithActions(t)

	// In Epic A, /<skill> content is forwarded as plain text — no
	// skill resolution yet. Verify it's persisted and delivered like
	// any other message.
	id, err := b.HandleChatSend(context.Background(), "anvil", "/review pr/58", 0, "")
	if err != nil {
		t.Fatalf("HandleChatSend: %v", err)
	}
	if id == 0 {
		t.Errorf("/skill content (Epic A) should persist as a normal chat message; got id=0")
	}
}
```

If `newTestBroker` doesn't exist as a shared helper, extract one from existing `observe_test.go` / `chat_send` tests. Don't duplicate setup.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./nexus/broker/ -run TestHandleChatSend -v
```

Expected: FAIL — actions aren't wired into the handler yet.

- [ ] **Step 3: Wire the parser into HandleChatSend**

Modify `nexus/broker/chat_send.go`:

```go
func (b *Broker) HandleChatSend(ctx context.Context, from, content string, replyTo int64, topic string) (int64, error) {
	if b.cfg.ChatStore == nil {
		return 0, errors.New("broker.HandleChatSend: ChatStore not configured")
	}
	if from == "" {
		return 0, errors.New("broker.HandleChatSend: from required")
	}

	// Comms-protocol action interception. Content starting with "!"
	// is routed to the ActionRegistry; the original content is not
	// persisted as a chat message. See docs/2026-05-17-comms-protocol-actions-spec.md.
	if name, args, isAction := parseLeadingAction(content); isAction {
		return 0, b.dispatchAction(ctx, from, name, args, replyTo, topic)
	}

	// ... existing chat-send persistence + fan-out path stays unchanged.
}

// dispatchAction looks up the named action, invokes it, and routes
// the response back to the caller. Returns an error if the action is
// unknown or if the handler itself returned a Go error (handler-level
// envelope errors are sent to the caller as a response frame and
// return nil here).
func (b *Broker) dispatchAction(ctx context.Context, from, name, args string, replyTo int64, topic string) error {
	if b.actions == nil {
		return fmt.Errorf("broker.HandleChatSend: action registry not configured")
	}
	action, ok := b.actions.Lookup(name)
	if !ok {
		// Build the available-action list for the error response.
		var names []string
		for _, d := range b.actions.List() {
			names = append(names, d.Name)
		}
		return b.sendActionResponse(ctx, from, frames.Envelope{
			Kind: frames.KindDispatchError,
			Payload: map[string]any{
				"reason":            fmt.Sprintf("unknown action: %s", name),
				"code":              "unknown_action",
				"available_actions": names,
			},
		})
	}

	env := frames.Envelope{
		From:    from,
		Topic:   topic,
		ReplyTo: replyTo,
	}
	resp, err := action.Handle(ctx, env, args)
	if err != nil {
		return fmt.Errorf("action %s: %w", name, err)
	}
	return b.sendActionResponse(ctx, from, resp)
}

// sendActionResponse delivers an action's response envelope back to
// the caller. For Epic A, the response is rendered as a chat-send
// message addressed only to the caller (single-recipient). Future
// enhancement: send out-of-band frames for structured responses.
func (b *Broker) sendActionResponse(ctx context.Context, to string, env frames.Envelope) error {
	content, _ := env.Payload["content"].(string)
	if content == "" {
		// For non-chat envelopes (KindDispatchAccepted, KindDispatchError),
		// serialize the payload as JSON so the caller sees structured info.
		raw, err := json.Marshal(env.Payload)
		if err != nil {
			return err
		}
		content = string(raw)
	}
	// Deliver to the caller's connection only. The exact method here
	// depends on the broker's existing single-recipient delivery API.
	// If a helper like b.deliverChatToAspect(to, content) exists, use
	// it; otherwise add one.
	return b.deliverChatToAspect(ctx, to, content, env.Kind)
}
```

If `b.actions` field doesn't exist on Broker yet, add it. Same for `b.deliverChatToAspect`. The exact name/shape depends on the existing broker code; pick names that match.

Add the imports:

```go
import (
	// ... existing
	"encoding/json"
	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./nexus/broker/ -v
```

Expected: PASS — new tests plus all existing chat_send / observe tests still pass.

- [ ] **Step 5: Commit**

```bash
git add nexus/broker/chat_send.go nexus/broker/chat_send_test.go
git commit -m "feat(broker): wire ActionRegistry into HandleChatSend ingress

Content beginning with ! is intercepted, routed to the registered
Action handler, and the response sent back to the caller as a
single-recipient chat message. Original content is not persisted to
thread.

Per docs/2026-05-17-comms-protocol-actions-spec.md §3.1."
```

---

## Phase 7 — Audit frame emission

Emit `dispatch_audit`, `dispatch_result`, `dispatch_error` chat frames to the thread so other participants (operator dashboard, other aspects) see dispatch activity.

### Task 15: Audit-frame emission on submit + result

**Files:**
- Modify: `nexus/broker/action_dispatch.go`
- Modify: `nexus/broker/action_dispatch_test.go`
- New: integration point between handqueue completion events and broker audit emission. Survey `nexus/handqueue/` for an existing completion hook; if absent, this task adds one.

- [ ] **Step 1: Survey handqueue for a completion hook**

```bash
cd ~/Source/nexus
grep -n "OnComplete\|callback\|notify\|hook" nexus/handqueue/queue.go nexus/handqueue/spawn.go
```

If there's already a callback mechanism, plug into it. If not, this task adds a `Queue.OnCompletion(func(...))` registration and invokes it from the worker-reap path.

- [ ] **Step 2: Write the failing test for audit frame on submit**

Append to `nexus/broker/action_dispatch_test.go`:

```go
// auditCollector captures audit frames emitted by DispatchAction so
// tests can assert on the metadata.
type auditCollector struct {
	frames []frames.Envelope
}

func (c *auditCollector) Emit(env frames.Envelope) error {
	c.frames = append(c.frames, env)
	return nil
}

func TestDispatchAction_EmitsAuditFrameOnSubmit(t *testing.T) {
	collector := &auditCollector{}
	q := &fakeHandQueue{nextID: "d-99", nextPos: 2}
	action := NewDispatchActionWithAudit(q, collector)
	env := frames.Envelope{From: "anvil", Topic: "anvil-nex-141"}

	_, err := action.Handle(context.Background(), env, "do the thing")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(collector.frames) != 1 {
		t.Fatalf("expected 1 audit frame, got %d", len(collector.frames))
	}
	audit := collector.frames[0]
	if md, _ := audit.Payload["metadata"].(map[string]any); md == nil {
		t.Fatal("audit frame missing metadata")
	} else {
		if kind, _ := md["kind"].(string); kind != "dispatch_audit" {
			t.Errorf("metadata.kind = %q, want dispatch_audit", kind)
		}
		if status, _ := md["status"].(string); status != "submitted" {
			t.Errorf("metadata.status = %q, want submitted", status)
		}
		if did, _ := md["dispatch_id"].(string); did != "d-99" {
			t.Errorf("metadata.dispatch_id = %q, want d-99", did)
		}
		if qp, _ := md["queue_position"].(int); qp != 2 {
			t.Errorf("metadata.queue_position = %v, want 2", qp)
		}
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
go test ./nexus/broker/ -run TestDispatchAction_EmitsAuditFrameOnSubmit -v
```

Expected: FAIL with `undefined: NewDispatchActionWithAudit`.

- [ ] **Step 4: Add audit emission to DispatchAction**

Modify `nexus/broker/action_dispatch.go`. Add an `AuditEmitter` interface and a constructor variant that takes one:

```go
// AuditEmitter sends an audit chat frame to the originating thread.
// Implementations route to the broker's chat-send fan-out.
type AuditEmitter interface {
	Emit(env frames.Envelope) error
}

// DispatchAction holds an optional auditor. Nil auditor disables
// audit emission (used in early tests; production wiring always sets it).
type DispatchAction struct {
	queue   HandQueue
	auditor AuditEmitter
}

// NewDispatchAction constructs the action without an auditor (audit
// frames skipped). Used in lightweight tests.
func NewDispatchAction(q HandQueue) *DispatchAction {
	return &DispatchAction{queue: q}
}

// NewDispatchActionWithAudit constructs the action with an auditor.
// Production wiring uses this constructor.
func NewDispatchActionWithAudit(q HandQueue, auditor AuditEmitter) *DispatchAction {
	return &DispatchAction{queue: q, auditor: auditor}
}
```

In `Handle`, after a successful `queue.Submit`:

```go
if a.auditor != nil {
	auditEnv := frames.Envelope{
		Kind: frames.KindChatSend,
		From: "broker",
		Topic: env.Topic,
		Payload: map[string]any{
			"content": fmt.Sprintf("!dispatch %s (submitted by %s)", truncate(args, 80), env.From),
			"metadata": map[string]any{
				"kind":           "dispatch_audit",
				"dispatch_id":    ack.DispatchID,
				"status":         "submitted",
				"queue_position": ack.QueuePosition,
			},
		},
	}
	if err := a.auditor.Emit(auditEnv); err != nil {
		// Audit emission failure is non-fatal — log only.
		// The dispatch itself succeeded; missing audit is an
		// observability gap, not a correctness issue.
		// (Use the broker's logger; placeholder here.)
		_ = err
	}
}

// truncate returns s clipped to maxLen with "..." suffix if clipped.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./nexus/broker/ -run TestDispatchAction_EmitsAuditFrameOnSubmit -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add nexus/broker/action_dispatch.go nexus/broker/action_dispatch_test.go
git commit -m "feat(broker): emit dispatch_audit chat frame on !dispatch submit"
```

### Task 16: Result + error audit frames on completion

**Files:**
- Modify: `nexus/handqueue/queue.go` (or wherever worker completion is reaped)
- Modify: `nexus/broker/handqueue_adapter.go` (register completion callback)
- Modify: `nexus/broker/handqueue_adapter_test.go`

- [ ] **Step 1: Identify the completion path in handqueue**

Find where `handqueue` reaps a worker and produces the result. This is the moment to emit the `dispatch_result` or `dispatch_error` audit frame.

```bash
grep -n "DispatchResult\|finish\|reap\|exit\|Wait" nexus/handqueue/queue.go nexus/handqueue/spawn.go
```

- [ ] **Step 2: Add a completion-callback registration to Queue**

If handqueue.Queue has no callback API, add one:

```go
// CompletionHandler is called when a worker completes (success or
// failure). Implementations route to broker-side audit emission and
// result delivery. Per spec §3.2, completion produces a chat frame
// on the originating thread (success: dispatch_result, failure:
// dispatch_error) with metadata describing the outcome.
type CompletionHandler func(ctx context.Context, result CompletionEvent)

// CompletionEvent is the broker-facing summary of a finished worker.
type CompletionEvent struct {
	Aspect     string
	Thread     string
	DispatchID string
	Success    bool
	Output     map[string]any
	Error      string
	ErrorClass string // "crashed" | "timeout" | "malformed" | ...
	ExitCode   int
	DurationMS int
}

// SetCompletionHandler installs the handler. Must be called before
// Submit; in production this is wired at broker startup.
func (q *Queue) SetCompletionHandler(h CompletionHandler) {
	q.completion = h
}
```

In the worker-reap path, call `q.completion(ctx, CompletionEvent{...})` with the captured event.

- [ ] **Step 3: Wire the broker side**

In `handqueue_adapter.go`, register a handler at adapter-construction time:

```go
func NewHandQueueAdapter(q *handqueue.Queue, auditor AuditEmitter) *HandQueueAdapter {
	a := &HandQueueAdapter{q: q, auditor: auditor}
	q.SetCompletionHandler(a.onCompletion)
	return a
}

// onCompletion emits the dispatch_result or dispatch_error audit
// frame when a worker finishes.
func (a *HandQueueAdapter) onCompletion(ctx context.Context, ev handqueue.CompletionEvent) {
	kind := "dispatch_result"
	status := "success"
	if !ev.Success {
		kind = "dispatch_error"
		status = "failure"
	}
	content := ""
	if ev.Success {
		if s, ok := ev.Output["text"].(string); ok {
			content = s
		} else {
			raw, _ := json.Marshal(ev.Output)
			content = string(raw)
		}
	} else {
		content = ev.Error
	}

	auditEnv := frames.Envelope{
		Kind:  frames.KindChatSend,
		From:  fmt.Sprintf("%s-hand-%s", ev.Aspect, shortID(ev.DispatchID)),
		Topic: ev.Thread,
		Payload: map[string]any{
			"content": content,
			"metadata": map[string]any{
				"kind":         kind,
				"dispatch_id":  ev.DispatchID,
				"status":       status,
				"duration_ms":  ev.DurationMS,
				"exit_code":    ev.ExitCode,
				"error_class":  ev.ErrorClass,
			},
		},
	}
	_ = a.auditor.Emit(auditEnv)
}

// shortID returns the first 6 chars of a dispatch_id for synthetic
// hand-identity display. dispatch_id is a uuid_v4.
func shortID(id string) string {
	if len(id) < 6 {
		return id
	}
	return id[:6]
}
```

- [ ] **Step 4: Write the test asserting completion → audit emission**

Append to `nexus/broker/handqueue_adapter_test.go`:

```go
func TestHandQueueAdapter_EmitsResultAuditOnSuccess(t *testing.T) {
	collector := &auditCollector{}
	q := newTestHandqueueQueue(t) // adapter test helper; see Task 13 Step 4
	adapter := NewHandQueueAdapter(q, collector)
	_ = adapter

	// Trigger a successful worker completion via the queue's test
	// hook (or by submitting and letting a real worker run). Adapt
	// to the testing pattern in nexus/handqueue/queue_test.go.

	// Assert collector.frames contains a single envelope with
	// metadata.kind == "dispatch_result", status == "success".
	t.Skip("fill in once Step 1 surveys the real completion hook")
}

func TestHandQueueAdapter_EmitsErrorAuditOnFailure(t *testing.T) {
	t.Skip("symmetric to success path; fill in alongside Step 1's findings")
}
```

- [ ] **Step 5: Implement the tests against the discovered handqueue API**

Replace `t.Skip` with actual assertions. Use the existing `queue_test.go` patterns to construct a queue, submit a job, force success/failure, and verify the completion event reaches the adapter.

- [ ] **Step 6: Run all broker + handqueue tests**

```bash
go test ./nexus/broker/ ./nexus/handqueue/ -v
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add nexus/handqueue/queue.go nexus/broker/handqueue_adapter.go nexus/broker/handqueue_adapter_test.go
git commit -m "feat(broker,handqueue): emit dispatch_result/dispatch_error audit frame on completion

handqueue.Queue gains SetCompletionHandler; HandQueueAdapter wires
its onCompletion to emit a chat frame on the originating thread with
metadata describing success/failure, duration, exit code, error class.
Synthetic hand-identity From field for distinct rendering."
```

---

## Phase 8 — Broker startup wiring

Construct the registry, register all three actions, attach to the broker.

### Task 17: Register actions at broker startup

**Files:**
- Modify: `nexus/broker/server.go` (or wherever Broker is constructed — look for `func NewBroker` or similar)
- Modify: `nexus/cmd/nexus/main.go` (if startup wiring lives there)

- [ ] **Step 1: Locate broker construction**

```bash
cd ~/Source/nexus
grep -n "func NewBroker\|func.*Broker)\s*$\|return &Broker{" nexus/broker/*.go nexus/cmd/nexus/main.go | head -10
```

- [ ] **Step 2: Add action-registry field and registration step**

In `nexus/broker/server.go` (or wherever the Broker struct lives):

```go
type Broker struct {
	// ... existing fields
	actions *ActionRegistry
}
```

In the broker constructor (or in a separate `initActions` method called from startup):

```go
// initActions populates the ActionRegistry with v0.1 actions. Called
// once during broker startup, after handqueue and skill registry are
// available. See docs/2026-05-17-comms-protocol-actions-spec.md §5.3.
func (b *Broker) initActions(handQueue *handqueue.Queue, skills SkillRegistry) {
	b.actions = NewActionRegistry()

	auditor := newBrokerAuditor(b) // implements AuditEmitter, routes to chat fan-out
	adapter := NewHandQueueAdapter(handQueue, auditor)

	b.actions.Register(NewDispatchActionWithAudit(adapter, auditor))
	b.actions.Register(NewListActionsAction(b.actions))
	b.actions.Register(NewListSkillsAction(skills))
}

// newBrokerAuditor adapts the broker's chat fan-out to AuditEmitter.
// Audit frames are persisted as normal chat messages (so the operator
// dashboard and other thread participants see them) with metadata
// distinguishing them from regular content.
func newBrokerAuditor(b *Broker) AuditEmitter {
	return brokerAuditor{broker: b}
}

type brokerAuditor struct {
	broker *Broker
}

func (a brokerAuditor) Emit(env frames.Envelope) error {
	content, _ := env.Payload["content"].(string)
	topic, _ := env.Payload["topic"].(string)
	if topic == "" {
		topic = env.Topic
	}
	from := env.From
	if from == "" {
		from = "broker"
	}
	// HandleChatSend persists + fans out. Metadata is preserved on
	// the persisted row if ChatStore supports it; otherwise the
	// metadata travels on the live WS frame only.
	_, err := a.broker.HandleChatSend(context.Background(), from, content, 0, topic)
	return err
}
```

Call `b.initActions(handQueue, NullSkillRegistry{})` from wherever the broker finishes setup (most likely in `main.go` after handqueue is constructed, or in `NewBroker`).

- [ ] **Step 3: Build to confirm wiring**

```bash
go build ./...
```

Expected: clean build.

- [ ] **Step 4: Run all tests**

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Manual smoke test (optional, recommended)**

Start a local broker, connect with `nexus-comms-mcp`-equivalent, send a chat message `!actions`. Expect the action listing back as a single-recipient chat message.

- [ ] **Step 6: Commit**

```bash
git add nexus/broker/server.go nexus/cmd/nexus/main.go
git commit -m "feat(broker): wire ActionRegistry at startup with !dispatch, !actions, !skills"
```

---

## Phase 9 — Documentation propagation

Update the canonical NEXUS.md template so aspects know `!dispatch` exists.

### Task 18: NEXUS.md template — comms-protocol section

**Files:**
- Modify: canonical NEXUS.md template (path: see operator's Drive layout — typically `~/Google Drive/My Drive/nexus/<aspect>/NEXUS.md`. The "template" is the structural pattern shared across aspects, not a single file. Plan covers the change; the operator distributes.)
- Modify: any in-repo NEXUS.md exemplars under `nexus/agents/*/NEXUS.md`

- [ ] **Step 1: Identify all NEXUS.md files to update**

```bash
find ~/Source/nexus/agents/ -name "NEXUS.md" 2>/dev/null
find ~/Source/nexus/docs/ -name "NEXUS*.md" 2>/dev/null
```

The Drive copies (`~/Google Drive/My Drive/nexus/<aspect>/NEXUS.md`) are operator-curated. This task adds the section to the in-repo exemplar and documents the pattern.

- [ ] **Step 2: Add the comms-protocol section**

Insert the following section into the in-repo exemplar NEXUS.md (after the "Boundaries" section or wherever per-aspect role docs sit):

```markdown
## Comms protocol actions

Chat content beginning with `!<name>` triggers a broker-side action.
Currently registered:

- `!dispatch <context>` — spawn a fresh-context hand of yourself, run with context.
  Result lands on this thread as a chat message. Hands run as you (same identity,
  same NEXUS.md / SOUL.md / PRIMER), one turn each. Concurrent dispatches supported;
  each gets a unique `dispatch_id` for correlation. Identity is self-only.
- `!actions [name]` — discover available actions. With no args, lists all. With a
  name, returns multi-line help.
- `!skills [name]` — discover available skills. Stubbed in Epic A; full registry
  lands in Epic B.

Chat content beginning with `/<name>` invokes a skill (recipient loads the skill's
content into their context for that turn). Skill registry implementation lands
in Epic B; Epic A passes `/<name>` content through as plain text.

See `docs/2026-04-30-hand-dispatch-v0_1.md` and
`docs/2026-05-17-comms-protocol-actions-spec.md` for full details.
```

- [ ] **Step 3: Commit**

```bash
git add agents/*/NEXUS.md
git commit -m "docs(agents): NEXUS.md exemplar — comms-protocol actions section

Documents the !dispatch / !actions / !skills surface and the / vs !
symbol convention so aspects know about the action layer."
```

The Drive-canonical copies are a separate distribution step (operator-owned, outside this plan's scope).

---

## Phase 10 — Skill rewrite

Update `subagent-driven-development` to use `!dispatch /role-skill ...` composition. Doc-only change in `~/.claude/skills/` (or `<aspect-home>/.nexus/skills/` once Epic B lands).

### Task 19: Rewrite subagent-driven-development SKILL.md

**Files:**
- Modify: `~/.claude/plugins/cache/claude-plugins-official/superpowers/5.1.0/skills/subagent-driven-development/SKILL.md`
- Modify: helper prompt files in the same directory if the rewrite changes their use pattern

- [ ] **Step 1: Read the current skill**

Already surveyed in earlier exploration. Key change points:
- Replace "dispatch implementer subagent (Task tool)" with "fire `!dispatch /implementer-role <task brief>` via send_chat"
- Replace "dispatch spec reviewer subagent" with "fire `!dispatch /spec-reviewer-role <task brief>`"
- Replace "dispatch code quality reviewer subagent" with "fire `!dispatch /code-quality-reviewer-role <task brief>`"
- Update "Handling Implementer Status" section: results now arrive as chat messages on the thread, not as Task tool returns

- [ ] **Step 2: Update the SKILL.md content**

Rewrite the workflow section, the "Process" diagram, and the example workflow to reflect the new pattern. Keep:
- The two-stage review structure (spec compliance, then code quality)
- The four implementer statuses (DONE, DONE_WITH_CONCERNS, NEEDS_CONTEXT, BLOCKED)
- The "fresh subagent per task" principle (now realised as fresh hand per dispatch)
- The model-selection guidance

Change:
- Tool invocation from `Task()` to `send_chat(content="!dispatch /implementer ...")`
- Result handling from "subagent returns" to "wait for chat message on thread with metadata.kind=dispatch_result and matching dispatch_id"
- The graph showing the per-task flow updated to reflect chat-mediated dispatch

The full rewrite is detailed in the spec doc §10. Author the new SKILL.md against that pattern.

- [ ] **Step 3: Test the rewrite (manually)**

Write a small fake plan and run through the skill's described workflow on paper. Verify:
- Each dispatched role gets a `!dispatch /<role>` invocation
- Multi-dispatch is supported (parallel implementer + reviewer ok)
- The status handling section works with chat-message-arrival semantics

- [ ] **Step 4: Commit**

The skill lives in a plugin cache (`~/.claude/plugins/cache/...`), which is not a project-level git repo. Options:
- (a) Vendor the skill into the nexus repo under `nexus/skills/subagent-driven-development/SKILL.md` and document the override in nexus docs.
- (b) Author the rewrite in a personal fork of the superpowers plugin and submit upstream.
- (c) Distribute via the Drive `nexus/<aspect>/.nexus/skills/` path once Epic B's skill registry lands.

Recommend (a) for v0.1 — vendoring under nexus repo gives version-controlled history and lets future Epic B skill-registry implementation pick up `nexus/skills/` as the canonical source.

```bash
mkdir -p ~/Source/nexus/skills/subagent-driven-development
# author the rewritten SKILL.md + helper prompt files in that dir
git add nexus/skills/subagent-driven-development/
git commit -m "docs(skills): rewrite subagent-driven-development to use !dispatch

Replaces in-process Task subagent dispatch with broker-mediated
!dispatch /role-skill calls. Results arrive as chat messages on the
thread; status correlated by dispatch_id metadata.

Implements the workflow change called out in
docs/2026-05-17-comms-protocol-actions-spec.md §10."
```

---

## Self-Review

### Spec coverage check

Walking through `docs/2026-05-17-comms-protocol-actions-spec.md` section by section:

- §1 Summary, §2 Goals/non-goals — informational, no implementation tasks needed.
- §3.1 Symbol disambiguation → Task 1 (parser).
- §3.2 `!dispatch` invocation flow → Tasks 11, 14, 17.
- §3.3 Concurrent multi-dispatch → covered by handqueue (existing) + DispatchAction (Task 11).
- §3.4 Audit chat frame metadata → Tasks 15, 16.
- §4 handexec identity framing → Tasks 7, 8, 9.
- §5 Action registry → Tasks 2, 3, 14.
- §6.1–6.2 `!actions` / `!skills` → Tasks 5, 6.
- §6.3 Skill on-disk layout — locked in spec; consumed by Epic B; no Epic A task.
- §6.4 Skill registry interface → Task 4.
- §6.5 Response delivery — covered by Task 14 (`sendActionResponse`).
- §6.6 NEXUS.md addition → Task 18.
- §7 Security/limits/edge cases — covered by Tasks 11 (identity binding, empty payload, unregistered caller), 14 (unknown action), 9 (identity load failure).
- §8 Build sequence — this plan implements all 10 phases.
- §9 Acceptance criteria — each listed criterion has a corresponding task.
- §10 Open questions — informational.

No gaps detected.

### Placeholder scan

Search for plan-failure patterns:
- No "TBD" or "TODO" left in implementation steps.
- A few `t.Skip` placeholders in Task 13 and Task 16 with clear instructions to fill them in once the real handqueue API surface is inspected — these aren't "TODOs in code", they're "implement against the real signature you discover" guidance. Each comes with a specific assertion the test must end up making.
- Task 9 has notes about "if the existing signature differs, adapt" because the providers package surface was not fully inspected in plan-writing. The implementer adapts; the guidance is precise about what to adapt and where.
- Task 17 includes "`b.deliverChatToAspect` may need adding" because the exact single-recipient delivery helper name wasn't verified. Same pattern — adapt to existing helpers; if absent, add one.

These are calibrated to the unknowns; not actually placeholder failures.

### Type consistency

- `Action` interface (Tasks 2, 5, 6, 11) — signatures consistent.
- `HandQueue` / `HandQueueRequest` / `HandQueueAck` (Tasks 11, 13) — consistent.
- `Identity` struct + `LoadIdentity` + `ComposeSystemPrompt` (Tasks 7, 8, 9) — consistent.
- `SkillRegistry` / `SkillDescriptor` (Tasks 4, 6) — consistent.
- `AuditEmitter` (Tasks 15, 16, 17) — consistent.
- `frames.KindDispatchAccepted` (Tasks 10, 11) — added in 10, used in 11.
- `parseLeadingAction` (Tasks 1, 14) — same return signature throughout.

No drift detected.

---

## Worktree + PR discipline

Per developer standards (`docs/2026-05-17-developer-standards.md`):

- Each task lands on its own branch off the latest `main`. Branch name `feature/nex-XXX-<short-slug>` where XXX is the ticket number assigned per task (see filing step below).
- Worktrees at `<aspect-home>/repos/nexus-<branch-slug>/`.
- One ticket per PR. No bundling unrelated changes.
- Rebase before opening and before requesting review.
- Tests cover handlers and DB writes (HandleChatSend, audit emission paths).
- CI green on all platforms (ubuntu-latest, macos-latest, windows-latest).
- No self-approve; reviewer is shadow by default.

The implementer aspect should file 19 tickets corresponding to Tasks 1–19, parented under a new Epic-A ticket (suggested key: NEX-XXX "Epic A — comms-protocol actions + !dispatch") which itself sits under the broader hand-dispatch initiative.

Lane assignment per task:

| Tasks | Suggested lane | Why |
|---|---|---|
| 1, 2, 3 (parser + registry) | anvil | broker primitives |
| 4 (skill registry stub) | anvil | broker package |
| 5, 6 (discovery actions) | anvil | broker package |
| 7, 8, 9 (handexec identity) | keel | runtime / Frame harness lane |
| 10 (frames constant) | anvil | broker-adjacent |
| 11, 12, 13 (dispatch action + adapter) | anvil | broker package, depends on Tasks 1–4 |
| 14 (HandleChatSend wiring) | anvil | broker package |
| 15, 16 (audit emission) | anvil + keel | broker side (anvil) + handqueue hook (keel-or-anvil) |
| 17 (startup wiring) | anvil | broker startup |
| 18 (NEXUS.md exemplar) | shadow | doc work |
| 19 (skill rewrite) | shadow | doc work |

Tasks 1–6 parallelise independently. Task 9 requires 7–8. Task 11 requires 1–3, 4, 10. Task 14 requires 1–6, 11. Tasks 15–16 require 11. Task 17 requires 11, 13, 14, 15. Tasks 18–19 require nothing in this phase technically but should land after 17 so the documented behaviour is exercisable.

---

## Status

v0.1 plan, ready for implementation. Operator confirms lane assignments and ticket creation. Implementer reads this plan task-by-task; uses `subagent-driven-development` (post-Task 19, the new version of itself) for execution.

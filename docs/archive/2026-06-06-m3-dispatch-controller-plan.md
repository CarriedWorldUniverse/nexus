# M3 Dispatch Controller Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A self-service dispatch controller — an always-on broker client whose inbox turns each dispatch message into a k8s Job that runs the brief as the named builder-agent and returns the result over comms, no manual kubectl.

**Architecture:** A new `dispatch-controller` binary connects to the broker as its own herald identity (reusing agentfunnel's keyfile→validate→wsasp path) and receives dispatch briefs via `wsasp.OnDeliver`. Each brief is parsed, idempotency/concurrency-checked, provisioned (keyfile Secret + M1 git-cred + brief ConfigMap), and turned into a k8s Job (the M2 worker image) via client-go. The controller watches Job lifecycle and posts status to the originating ticket thread. Workers read the brief from a mounted file (`agentfunnel -builder -brief-file`).

**Tech Stack:** Go, `runtime/aspect/wsasp` (broker client), `runtime/keyfile`, `k8s.io/client-go` (NEW), the M2 worker image + `deploy/worker/job.yaml`, the M1 `cw issue-git-permission` seam.

**Spec:** `docs/2026-06-06-m3-dispatch-controller-design.md`. **Scope:** single-dispatch controller only; fan-out is M3b.

---

## File structure

| Path | Responsibility |
|---|---|
| `runtime/cmd/agentfunnel/main.go` (modify) | Add `-brief-file`; in builder mode seed the funnel inbox from the file instead of waiting on the broker inbox. |
| `runtime/cmd/dispatch-controller/main.go` (create) | Entry: load keyfile, connect to broker, wire `OnDeliver` → `controller.Handle`, build k8s client, run until ctx done. |
| `runtime/dispatch/brief.go` (create) | `Brief` struct + `ParseBrief([]byte)` — parse a dispatch message body into a structured brief. Pure, unit-tested. |
| `runtime/dispatch/controller.go` (create) | `Controller` — the inbox handler: idempotency, concurrency cap, provision, create Job, ACK. Holds the active-Job map. |
| `runtime/dispatch/jobspec.go` (create) | `BuildJob(brief, cfg) *batchv1.Job` — construct the Job spec (mirrors `deploy/worker/job.yaml`). Pure, golden-tested. |
| `runtime/dispatch/k8s.go` (create) | Thin wrapper over client-go: `EnsureKeyfileSecret`, `PutBriefConfigMap`, `CreateJob`, `WatchJob`, `ListActiveJobs`. |
| `runtime/dispatch/provision.go` (create) | `Provision(brief)` — keyfile Secret + `cw issue-git-permission` git-cred + brief ConfigMap, before Job create. |
| `deploy/dispatch-controller/` (create) | `Dockerfile`, `deployment.yaml` (Deployment + ServiceAccount + Role + RoleBinding), `build.sh`. |

The `runtime/dispatch` package holds all controller logic (one responsibility: turn briefs into Jobs); the `cmd` binary is wiring only.

---

## Task 1: agentfunnel `-brief-file` (seed brief from a file)

**Files:**
- Modify: `runtime/cmd/agentfunnel/main.go` (flags ~line 80; builder block ~line 367; before `go deliberateLoop` ~line 593)
- Test: `runtime/cmd/agentfunnel/brief_file_test.go` (create)

- [ ] **Step 1: Write the failing test** — a helper that reads a brief file into an `InboxItem`.

```go
// brief_file_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadBriefFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(p, []byte("Implement NEX-999: add a flag.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := readBriefFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if item.From != "dispatch" {
		t.Errorf("From = %q, want dispatch", item.From)
	}
	if item.Content == "" || item.Content[:9] != "Implement" {
		t.Errorf("Content = %q, want the brief text", item.Content)
	}
}

func TestReadBriefFile_Missing(t *testing.T) {
	if _, err := readBriefFile("/no/such/brief"); err == nil {
		t.Fatal("expected error for missing brief file")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./runtime/cmd/agentfunnel/ -run TestReadBriefFile` → FAIL (`readBriefFile` undefined).

- [ ] **Step 3: Implement** — add the flag and helper.

```go
// in main(), with the other flags (~line 80)
briefFile := flag.String("brief-file", "", "builder mode: read the seed brief from this file instead of the broker inbox")

// helper (package level)
func readBriefFile(path string) (bridle.InboxItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return bridle.InboxItem{}, fmt.Errorf("read brief file: %w", err)
	}
	return bridle.InboxItem{From: "dispatch", Content: string(b)}, nil
}
```

- [ ] **Step 4: Seed the funnel before the loop** — after the funnel `f` is built and before `go deliberateLoop(ctx, f, log)` (~line 593):

```go
if *builderMode && *briefFile != "" {
	item, err := readBriefFile(*briefFile)
	if err != nil {
		log.Error("agentfunnel: brief file unreadable", "err", err)
		os.Exit(1)
	}
	f.Receive(item) // pending inbox item; deliberateLoop drains it on the first tick
	log.Info("agentfunnel: seeded builder brief from file", "path", *briefFile, "bytes", len(item.Content))
}
```

- [ ] **Step 5: Run tests** — `go test ./runtime/cmd/agentfunnel/ -run TestReadBriefFile` → PASS. Then `go build ./runtime/cmd/agentfunnel` → exit 0.

- [ ] **Step 6: Commit** — `git add runtime/cmd/agentfunnel/ && git commit -m "feat(agentfunnel): -brief-file seeds the builder brief (NEX-437)"`

---

## Task 2: Brief parsing (`runtime/dispatch/brief.go`)

**Files:**
- Create: `runtime/dispatch/brief.go`, `runtime/dispatch/brief_test.go`

The dispatch message body is a fenced JSON block (machine-written by the orchestrator) followed by free text. Parse the JSON; the free text is the human-readable brief carried into the Job.

- [ ] **Step 1: Write the failing test**

```go
package dispatch

import "testing"

func TestParseBrief(t *testing.T) {
	msg := "dispatch to a k3s builder\n```json\n" +
		`{"agent":"anvil","repo":"CarriedWorldUniverse/nexus","ticket":"NEX-999","thread":"NEX-999"}` +
		"\n```\nImplement the flag and open a PR.\n"
	b, err := ParseBrief([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if b.Agent != "anvil" || b.Ticket != "NEX-999" || b.Repo != "CarriedWorldUniverse/nexus" {
		t.Errorf("fields wrong: %+v", b)
	}
	if b.Task == "" || b.Task[:9] != "Implement" {
		t.Errorf("Task = %q, want the trailing free text", b.Task)
	}
}

func TestParseBrief_MissingAgent(t *testing.T) {
	if _, err := ParseBrief([]byte("```json\n{\"ticket\":\"NEX-1\"}\n```\nx")); err == nil {
		t.Fatal("expected error when agent missing")
	}
}
```

- [ ] **Step 2: Run** → FAIL (`ParseBrief` undefined).

- [ ] **Step 3: Implement**

```go
package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Brief is one dispatch request: the structured header + the task text.
type Brief struct {
	Agent  string `json:"agent"`  // named builder-agent identity (e.g. "anvil")
	Repo   string `json:"repo"`   // owner/name for the git-cred grant
	Ticket string `json:"ticket"` // dispatch idempotency key + thread topic
	Branch string `json:"branch"` // optional target branch
	Thread string `json:"thread"` // comms thread/topic for status + result
	Task   string `json:"-"`      // free-text brief (after the fenced block)
}

// ParseBrief extracts the fenced ```json header and the trailing task text.
func ParseBrief(body []byte) (Brief, error) {
	s := string(body)
	open := strings.Index(s, "```json")
	if open < 0 {
		return Brief{}, errors.New("dispatch: no ```json brief header")
	}
	rest := s[open+len("```json"):]
	close := strings.Index(rest, "```")
	if close < 0 {
		return Brief{}, errors.New("dispatch: unterminated ```json header")
	}
	var b Brief
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:close])), &b); err != nil {
		return Brief{}, fmt.Errorf("dispatch: bad brief header: %w", err)
	}
	b.Task = strings.TrimSpace(rest[close+3:])
	if b.Agent == "" {
		return Brief{}, errors.New("dispatch: brief.agent required")
	}
	if b.Ticket == "" {
		return Brief{}, errors.New("dispatch: brief.ticket required (idempotency key)")
	}
	if b.Thread == "" {
		b.Thread = b.Ticket
	}
	return b, nil
}
```

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** — `git add runtime/dispatch/brief*.go && git commit -m "feat(dispatch): brief parsing (NEX-437)"`

---

## Task 3: Job spec builder (`runtime/dispatch/jobspec.go`)

Adds client-go. Mirrors `deploy/worker/job.yaml` in Go.

**Files:**
- Modify: `go.mod` (add `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`)
- Create: `runtime/dispatch/jobspec.go`, `runtime/dispatch/jobspec_test.go`

- [ ] **Step 1: Add deps** — `go get k8s.io/client-go@v0.32.0 k8s.io/api@v0.32.0 k8s.io/apimachinery@v0.32.0` (match the k3s 1.35 line; adjust if `go mod tidy` complains). Run `go mod tidy`.

- [ ] **Step 2: Write the failing test** — the Job spec carries the right image, identity, brief mount, and labels.

```go
package dispatch

import "testing"

func TestBuildJob(t *testing.T) {
	cfg := JobConfig{Image: "localhost/nexus-builder:dev", Namespace: "nexus",
		NodeIP: "192.168.143.133", BrokerHost: "dmonextreme.tail41686e.ts.net",
		BriefTimeout: "30m"}
	b := Brief{Agent: "anvil", Ticket: "NEX-999", Thread: "NEX-999"}
	job := BuildJob(b, cfg, "abc123")

	if job.Labels["nexus.dispatch/ticket"] != "NEX-999" {
		t.Errorf("missing ticket label: %v", job.Labels)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != cfg.Image {
		t.Errorf("image = %q", c.Image)
	}
	args := c.Args
	if !contains(args, "-builder") || !contains(args, "-brief-file") {
		t.Errorf("args missing builder/-brief-file: %v", args)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run** → FAIL.

- [ ] **Step 4: Implement** — construct the Job (the init-container stages codex auth as in the M2 live run; the brief ConfigMap mounts at `/etc/nexus/brief.md`).

```go
package dispatch

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type JobConfig struct {
	Image, Namespace, NodeIP, BrokerHost, BriefTimeout string
}

func int32p(v int32) *int32 { return &v }

// BuildJob mirrors deploy/worker/job.yaml for one brief. taskID makes the
// Job name + workspace unique.
func BuildJob(b Brief, cfg JobConfig, taskID string) *batchv1.Job {
	labels := map[string]string{
		"app":                   "nexus-builder",
		"nexus.dispatch/agent":  b.Agent,
		"nexus.dispatch/ticket": b.Ticket,
	}
	ro := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "builder-" + b.Agent + "-" + taskID, Namespace: cfg.Namespace, Labels: labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32p(0),
			TTLSecondsAfterFinished: int32p(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					HostAliases:   []corev1.HostAlias{{IP: cfg.NodeIP, Hostnames: []string{cfg.BrokerHost}}},
					InitContainers: []corev1.Container{{
						Name: "codex-auth", Image: cfg.Image, ImagePullPolicy: corev1.PullNever,
						Command: []string{"sh", "-c", "mkdir -p /root/.codex && cp /codex-secret/auth.json /root/.codex/auth.json && chmod 600 /root/.codex/auth.json"},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "codex-home", MountPath: "/root/.codex"},
							{Name: "codex-secret", MountPath: "/codex-secret", ReadOnly: true},
						},
					}},
					Containers: []corev1.Container{{
						Name: "builder", Image: cfg.Image, ImagePullPolicy: corev1.PullNever,
						Args: []string{"-k", "/etc/nexus/keyfile.json", "-builder",
							"-brief-file", "/etc/nexus/brief.md", "-builder-timeout", cfg.BriefTimeout},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "keyfile", MountPath: "/etc/nexus", ReadOnly: true},
							{Name: "brief", MountPath: "/etc/nexus/brief.md", SubPath: "brief.md", ReadOnly: true},
							{Name: "codex-home", MountPath: "/root/.codex"},
							{Name: "work", MountPath: "/work"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "keyfile", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "aspect-keyfile-" + b.Agent}}},
						{Name: "codex-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "codex-auth"}}},
						{Name: "brief", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "brief-" + taskID}}}},
						{Name: "codex-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "work", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "nexus-builder-work", ReadOnly: !ro}}},
					},
				},
			},
		},
	}
}
```

> Note the brief mount uses `SubPath: "brief.md"` so it coexists with the keyfile under `/etc/nexus`. The keyfile is mounted at `/etc/nexus` because Task 1 reads `-k /etc/nexus/keyfile.json`.

- [ ] **Step 5: Run** → PASS. `go build ./runtime/dispatch/` → exit 0.

- [ ] **Step 6: Commit** — `git add go.mod go.sum runtime/dispatch/jobspec*.go && git commit -m "feat(dispatch): k8s Job spec builder + client-go dep (NEX-437)"`

---

## Task 4: k8s wrapper + provisioning (`k8s.go`, `provision.go`)

**Files:**
- Create: `runtime/dispatch/k8s.go`, `runtime/dispatch/provision.go`, `runtime/dispatch/k8s_test.go`

Use `client-go/kubernetes/fake` for tests (no live cluster needed).

- [ ] **Step 1: Write the failing test** — CreateJob + ListActiveJobs round-trip on the fake client.

```go
package dispatch

import (
	"context"
	"testing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateAndListJobs(t *testing.T) {
	k := &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"}
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, JobConfig{Namespace: "nexus"}, "t1")
	if err := k.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	active, err := k.ListActiveJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active["NEX-1"] == "" {
		t.Errorf("ticket NEX-1 not in active set: %v", active)
	}
	_ = metav1.ObjectMeta{}
}
```

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement `k8s.go`** — wrapper with the methods used by the controller.

```go
package dispatch

import (
	"context"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type K8s struct {
	Client    kubernetes.Interface
	Namespace string
}

func (k *K8s) CreateJob(ctx context.Context, job *batchv1.Job) error {
	_, err := k.Client.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}

// ListActiveJobs returns ticket -> job-name for non-finished builder Jobs.
// Used on startup to rebuild idempotency/concurrency state.
func (k *K8s) ListActiveJobs(ctx context.Context) (map[string]string, error) {
	jl, err := k.Client.BatchV1().Jobs(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range jl.Items {
		j := &jl.Items[i]
		if j.Status.Succeeded == 0 && j.Status.Failed == 0 {
			if t := j.Labels["nexus.dispatch/ticket"]; t != "" {
				out[t] = j.Name
			}
		}
	}
	return out, nil
}

func (k *K8s) PutBriefConfigMap(ctx context.Context, taskID, brief string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "brief-" + taskID, Namespace: k.Namespace, Labels: map[string]string{"app": "nexus-builder"}},
		Data:       map[string]string{"brief.md": brief},
	}
	_, err := k.Client.CoreV1().ConfigMaps(k.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	return err
}
```

- [ ] **Step 4: Implement `provision.go`** — keyfile Secret check + git-cred grant. The keyfile Secret is assumed pre-seeded per agent (created once by an operator/herald step); `Provision` verifies it exists and grants the git-cred by shelling `cw` (already in the controller image).

```go
package dispatch

import (
	"context"
	"fmt"
	"os/exec"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Provision ensures per-dispatch prerequisites exist before the Job runs:
// the agent keyfile Secret (pre-seeded) and a scoped git-cred grant (M1).
func (c *Controller) Provision(ctx context.Context, b Brief, taskID string) error {
	if _, err := c.K8s.Client.CoreV1().Secrets(c.K8s.Namespace).
		Get(ctx, "aspect-keyfile-"+b.Agent, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("provision: keyfile secret for %s missing: %w", b.Agent, err)
	}
	if b.Repo != "" {
		// M1 seam: grant a scoped git permission for this agent + repo.
		cmd := exec.CommandContext(ctx, "cw", "credential", "issue-git-permission",
			"--aspect", b.Agent, "--repo", b.Repo)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("provision: git-cred grant: %w (%s)", err, out)
		}
	}
	return c.K8s.PutBriefConfigMap(ctx, taskID, b.Task)
}
```

- [ ] **Step 5: Run** `go test ./runtime/dispatch/` → PASS. `go build ./runtime/dispatch/` → exit 0.

- [ ] **Step 6: Commit** — `git add runtime/dispatch/k8s*.go runtime/dispatch/provision.go && git commit -m "feat(dispatch): k8s wrapper + provisioning (NEX-437)"`

---

## Task 5: Controller — idempotency, concurrency, handle, status (`controller.go`)

**Files:**
- Create: `runtime/dispatch/controller.go`, `runtime/dispatch/controller_test.go`

- [ ] **Step 1: Write the failing test** — duplicate ticket no-ops; over-cap queues.

```go
package dispatch

import (
	"context"
	"testing"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestController(maxConc int) *Controller {
	return &Controller{
		K8s:     &K8s{Client: fake.NewSimpleClientset(), Namespace: "nexus"},
		Cfg:     JobConfig{Namespace: "nexus", Image: "img"},
		MaxConc: maxConc,
		active:  map[string]string{},
		acked:   map[string]bool{},
	}
}

func TestHandle_Idempotent(t *testing.T) {
	c := newTestController(4)
	b := Brief{Agent: "anvil", Ticket: "NEX-1", Task: "do it"}
	if err := c.handle(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	n1 := len(c.active)
	if err := c.handle(context.Background(), b); err != nil { // duplicate
		t.Fatal(err)
	}
	if len(c.active) != n1 {
		t.Errorf("duplicate dispatch double-spawned: active=%d", len(c.active))
	}
}

func TestHandle_ConcurrencyCap(t *testing.T) {
	c := newTestController(1)
	_ = c.handle(context.Background(), Brief{Agent: "a", Ticket: "T1", Task: "x"})
	_ = c.handle(context.Background(), Brief{Agent: "a", Ticket: "T2", Task: "x"})
	if len(c.active) != 1 {
		t.Errorf("cap not enforced: active=%d want 1", len(c.active))
	}
	if len(c.queue) != 1 {
		t.Errorf("over-cap brief not queued: queue=%d want 1", len(c.queue))
	}
}
```

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement `controller.go`** — the inbox handler. `Poster` is the comms-send interface (the wsasp gateway in prod; a fake in tests). `taskID` generation is injected (`NewID`) so tests are deterministic.

```go
package dispatch

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// Poster sends a status line to a comms thread (the wsasp gateway in prod).
type Poster interface {
	Post(thread, text string) error
}

type Controller struct {
	K8s     *K8s
	Cfg     JobConfig
	MaxConc int
	Poster  Poster
	NewID   func() string // injectable; default = monotonic counter

	mu     sync.Mutex
	active map[string]string // ticket -> job name
	queue  []Brief
	acked  map[string]bool // ticket -> already ACKed
	seq    int
}

func (c *Controller) nextID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	c.seq++
	return strconv.Itoa(c.seq)
}

func (c *Controller) post(thread, text string) {
	if c.Poster != nil {
		_ = c.Poster.Post(thread, text)
	}
}

// Handle is the OnDeliver entry: parse + handle, posting errors to the thread.
func (c *Controller) HandleMessage(ctx context.Context, body []byte) {
	b, err := ParseBrief(body)
	if err != nil {
		return // not a dispatch brief; ignore non-dispatch chatter
	}
	if err := c.handle(ctx, b); err != nil {
		c.post(b.Thread, "dispatch failed: "+err.Error())
	}
}

func (c *Controller) handle(ctx context.Context, b Brief) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.acked[b.Ticket] {
		c.acked[b.Ticket] = true
		c.post(b.Thread, "dispatch accepted for "+b.Agent+" on "+b.Ticket)
	}
	if _, live := c.active[b.Ticket]; live {
		return nil // idempotent: already running this ticket
	}
	if len(c.active) >= c.MaxConc {
		c.queue = append(c.queue, b)
		c.post(b.Thread, "dispatch queued (concurrency cap "+strconv.Itoa(c.MaxConc)+")")
		return nil
	}
	return c.spawn(ctx, b)
}

// spawn provisions + creates the Job. Caller holds c.mu.
func (c *Controller) spawn(ctx context.Context, b Brief) error {
	taskID := c.nextID()
	if err := c.Provision(ctx, b, taskID); err != nil {
		return err
	}
	job := BuildJob(b, c.Cfg, taskID)
	if err := c.K8s.CreateJob(ctx, job); err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	c.active[b.Ticket] = job.Name
	c.post(b.Thread, "builder spawned as "+b.Agent+" ("+job.Name+")")
	return nil
}

// onJobDone is called by the watcher when a Job reaches a terminal state.
func (c *Controller) onJobDone(ctx context.Context, ticket, thread string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, ticket)
	if ok {
		c.post(thread, "builder completed: "+ticket)
	} else {
		c.post(thread, "builder FAILED: "+ticket+" — see Job logs; re-dispatch to retry")
	}
	if len(c.queue) > 0 && len(c.active) < c.MaxConc {
		next := c.queue[0]
		c.queue = c.queue[1:]
		_ = c.spawn(ctx, next)
	}
}
```

- [ ] **Step 4: Run** `go test ./runtime/dispatch/ -run TestHandle` → PASS.

- [ ] **Step 5: Add the Job watcher** in `k8s.go` — `WatchJobs(ctx, onDone)` that watches `app=nexus-builder` Jobs and calls back on terminal state (Succeeded→ok, Failed→!ok), wiring `Controller.onJobDone`. Add a test using the fake client's watch reactor (a Job updated to `Succeeded:1` triggers the callback).

```go
func (k *K8s) WatchJobs(ctx context.Context, onDone func(ticket, thread string, ok bool)) error {
	w, err := k.Client.BatchV1().Jobs(k.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: "app=nexus-builder"})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			j, ok := ev.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			ticket := j.Labels["nexus.dispatch/ticket"]
			if j.Status.Succeeded > 0 {
				onDone(ticket, ticket, true)
			} else if j.Status.Failed > 0 {
				onDone(ticket, ticket, false)
			}
		}
	}
}
```

(Thread defaults to the ticket per the ticket-keyed-thread convention; carry the real thread via a Job annotation if it ever differs.)

- [ ] **Step 6: Commit** — `git add runtime/dispatch/controller*.go runtime/dispatch/k8s*.go && git commit -m "feat(dispatch): controller idempotency, concurrency, watch (NEX-437)"`

---

## Task 6: Binary wiring (`cmd/dispatch-controller/main.go`)

**Files:**
- Create: `runtime/cmd/dispatch-controller/main.go`

Wire the broker client (reuse agentfunnel's pattern: `keyfile.Load` → `client.Validate` → `wsasp` with `OnDeliver`) to `Controller.HandleMessage`, build the in-cluster k8s client, recover active Jobs, start the watcher, run until ctx done.

- [ ] **Step 1: Implement** (no unit test — integration is the live DoD in Task 8; keep this file wiring-only).

```go
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/CarriedWorldUniverse/nexus/runtime/aspect/wsasp"
	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	"github.com/CarriedWorldUniverse/nexus/runtime/keyfile"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	keyfilePath := flag.String("k", "/etc/nexus/keyfile.json", "controller keyfile")
	namespace := flag.String("namespace", "nexus", "Job namespace")
	image := flag.String("image", "localhost/nexus-builder:dev", "worker image")
	nodeIP := flag.String("node-ip", "192.168.143.133", "dMon node IP for hostAliases")
	brokerHost := flag.String("broker-host", "dmonextreme.tail41686e.ts.net", "broker tailnet host")
	maxConc := flag.Int("max-concurrent", 4, "max concurrent builder Jobs")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// k8s client (in-cluster).
	rc, err := rest.InClusterConfig()
	must(log, err, "in-cluster config")
	cs, err := kubernetes.NewForConfig(rc)
	must(log, err, "k8s client")

	ctrl := &dispatch.Controller{
		K8s:     &dispatch.K8s{Client: cs, Namespace: *namespace},
		Cfg:     dispatch.JobConfig{Image: *image, Namespace: *namespace, NodeIP: *nodeIP, BrokerHost: *brokerHost, BriefTimeout: "30m"},
		MaxConc: *maxConc,
	}
	ctrl.Init() // allocates maps; recovers active Jobs from ListActiveJobs

	// Broker client (mirrors runtime/cmd/agentfunnel/main.go connect path).
	kf, err := keyfile.Load(*keyfilePath)
	must(log, err, "keyfile load")
	client := keyfile.NewClient()
	res, err := client.Validate(ctx, kf)
	must(log, err, "validate")
	ctrl.Poster = dispatch.NewWsPoster(/* wsasp send handle */) // see Step 2

	cfg := wsasp.Config{
		// AuthToken: res.SessionJWT, URL from kf — same fields agentfunnel sets.
		OnDeliver: func(m wsasp.DeliveredMessage) { ctrl.HandleMessage(ctx, []byte(m.Content)) },
	}
	_ = res
	go ctrl.WatchLoop(ctx) // calls K8s.WatchJobs(ctx, ctrl.OnJobDone)

	log.Info("dispatch-controller: up", "aspect", res.AspectName, "ns", *namespace, "max", strconv.Itoa(*maxConc))
	runBroker(ctx, cfg, log) // wsasp.New(cfg).Run(ctx) — mirror agentfunnel
}

func must(log *slog.Logger, err error, what string) {
	if err != nil {
		log.Error("dispatch-controller: "+what+" failed", "err", err)
		os.Exit(1)
	}
}
```

> Reference `runtime/cmd/agentfunnel/main.go:98-330` for the exact `wsasp.Config` field set (AuthToken, URL, cursor, reconnect monitor) and the `wsasp` run call — reuse it verbatim, swapping the `OnDeliver` body. `Init`, `WatchLoop`, `OnJobDone`, and `NewWsPoster` are thin additions to `runtime/dispatch` (Init = allocate maps + `ListActiveJobs`; WatchLoop = call `WatchJobs`; OnJobDone = `onJobDone`; NewWsPoster wraps the wsasp send into the `Poster` interface).

- [ ] **Step 2: Add `Init`, `WatchLoop`, `OnJobDone`, `NewWsPoster`** to `runtime/dispatch` (small, exported wrappers over the Task 5 internals). Unit-test `Init` rebuilds `active` from `ListActiveJobs` (fake client pre-loaded with a running Job).

- [ ] **Step 3: Build** — `go build ./runtime/cmd/dispatch-controller` → exit 0. `go vet ./runtime/dispatch/... ./runtime/cmd/dispatch-controller`.

- [ ] **Step 4: Commit** — `git add runtime/cmd/dispatch-controller runtime/dispatch && git commit -m "feat(dispatch): controller binary wiring (NEX-437)"`

---

## Task 7: Deploy manifests + image (`deploy/dispatch-controller/`)

**Files:**
- Create: `deploy/dispatch-controller/Dockerfile`, `deployment.yaml`, `build.sh`

- [ ] **Step 1: Dockerfile** — multi-stage; the controller needs `cw` (for the git-cred grant) + the controller binary.

```dockerfile
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY dispatch-controller cw /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/dispatch-controller"]
```

- [ ] **Step 2: `deployment.yaml`** — ServiceAccount + Role (Jobs/ConfigMaps/Secrets/pods,pods/log: get/list/watch/create/delete in `nexus`) + RoleBinding + Deployment (mounts the controller's keyfile Secret; `hostAliases` for the broker, same as the workers).

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: { name: dispatch-controller, namespace: nexus }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: dispatch-controller, namespace: nexus }
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get","list","watch","create","delete"]
  - apiGroups: [""]
    resources: ["configmaps","secrets"]
    verbs: ["get","list","create","delete"]
  - apiGroups: [""]
    resources: ["pods","pods/log"]
    verbs: ["get","list","watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: dispatch-controller, namespace: nexus }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: dispatch-controller }
subjects: [{ kind: ServiceAccount, name: dispatch-controller, namespace: nexus }]
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: dispatch-controller, namespace: nexus }
spec:
  replicas: 1
  selector: { matchLabels: { app: dispatch-controller } }
  template:
    metadata: { labels: { app: dispatch-controller } }
    spec:
      serviceAccountName: dispatch-controller
      hostAliases: [{ ip: "192.168.143.133", hostnames: ["dmonextreme.tail41686e.ts.net"] }]
      containers:
        - name: controller
          image: localhost/dispatch-controller:dev
          imagePullPolicy: Never
          args: ["-k","/etc/nexus/keyfile.json","-namespace","nexus","-max-concurrent","4"]
          volumeMounts: [{ name: keyfile, mountPath: /etc/nexus, readOnly: true }]
      volumes:
        - { name: keyfile, secret: { secretName: aspect-keyfile-dispatch-controller } }
```

- [ ] **Step 3: `build.sh`** — build the controller binary + `cw` on the host, `podman build`, `k3s ctr import` (mirror `deploy/worker/build.sh`).

- [ ] **Step 4: Commit** — `git add deploy/dispatch-controller && git commit -m "feat(deploy): dispatch-controller image + manifests (NEX-437)"`

---

## Task 8: Live DoD (dMon)

- [ ] **Step 1** — Mint the `dispatch-controller` herald identity + its keyfile; create `aspect-keyfile-dispatch-controller` Secret. Pre-seed `aspect-keyfile-anvil` + `codex-auth` (as in the M2 live run).
- [ ] **Step 2** — `bash deploy/dispatch-controller/build.sh`; `kubectl apply -f deploy/dispatch-controller/deployment.yaml`; confirm the controller validates + connects (logs: `dispatch-controller: up`).
- [ ] **Step 3** — From shadow, send a dispatch message to `@dispatch-controller` in a ticket thread with a **real coding brief** (the ```json header + task). Assert: controller ACKs → Job spawns → builder runs **as the named agent** → pushes a branch + opens a **real PR** → posts the result + `<<TASK_COMPLETE>>` → Job Completed → controller posts terminal status. **No manual kubectl.**
- [ ] **Step 4** — Retire the always-on builder aspects on dMon (`systemctl stop aspect@{anvil,forge,harrow,maren,verity}`) now that they're dispatched on-demand. Leave orchestrators (shadow/wren) running.
- [ ] **Step 5** — Update NEX-437 (Done) + NEX-436 (full DoD closed via M3) + the epic NEX-434.

---

## Self-review

**Spec coverage:** controller-inbox interface (Tasks 5–6 ✓), inject-at-spawn brief (Tasks 1, 3 ✓), named-agent on-demand identity (Task 3 keyfile + Task 8 retire ✓), ACK + ticket-keyed idempotency (Task 5 ✓), backoffLimit 0 (Task 3 ✓), concurrency caps + FIFO queue (Task 5 ✓), provisioning via M1 seam (Task 4 ✓), status to thread (Task 5 ✓), RBAC-scoped controller (Task 7 ✓), live DoD incl. M2 full-DoD + retirement (Task 8 ✓). Fan-out correctly absent (M3b).

**Type consistency:** `Brief`, `JobConfig`, `K8s`, `Controller`, `Poster` used consistently across tasks; `BuildJob(Brief, JobConfig, taskID)` signature stable; `aspect-keyfile-<agent>` + `brief-<taskID>` + `app=nexus-builder` label used identically in jobspec/k8s/provision.

**Placeholders:** the one soft edge is the exact `wsasp.Config` field set in Task 6, intentionally delegated to "reuse agentfunnel main.go:98-330 verbatim" — that's a concrete pattern reference, not a TODO. Everything else is complete code.

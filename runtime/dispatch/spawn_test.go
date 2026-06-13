package dispatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CarriedWorldUniverse/nexus/runtime/dispatch"
	corev1 "k8s.io/api/core/v1"
)

type auditPost struct {
	from    string
	content string
	topic   string
}

type fakeAudit struct {
	posts []auditPost
	err   error
}

func (f *fakeAudit) PostFrom(_ context.Context, from, content string, _ int64, topic string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.posts = append(f.posts, auditPost{from: from, content: content, topic: topic})
	return 42, nil
}

type spawnStart struct {
	runID         string
	ticket        string
	agent         string
	thread        string
	dispatchMsgID int64
}

type spawnRecorder struct {
	starts []spawnStart
	done   []recorderDoneCall
}

func (r *spawnRecorder) RecordRunStart(_ context.Context, runID, ticket, agent, thread, _, _, _ string, dispatchMsgID int64) {
	r.starts = append(r.starts, spawnStart{runID: runID, ticket: ticket, agent: agent, thread: thread, dispatchMsgID: dispatchMsgID})
}

func (r *spawnRecorder) RecordRunDone(_ context.Context, runID, status string, _ time.Time, _ string, _ int) {
	r.done = append(r.done, recorderDoneCall{runID: runID, status: status})
}

func (r *spawnRecorder) RecordRunLogs(context.Context, string, string) {}

func newSpawnFixture(fk *fakeK8s) (*dispatch.Runner, *recordingPoster, *fakeAudit, *spawnRecorder) {
	r := newRunner(fk)
	p := &recordingPoster{}
	a := &fakeAudit{}
	rec := &spawnRecorder{}
	r.Poster = p
	r.Audit = a
	r.Recorder = rec
	r.MintHandCredential = func(_ context.Context, _, derived string) (string, error) {
		return "jwt-for-" + derived, nil
	}
	_ = r.Init(context.Background())
	return r, p, a, rec
}

// The core spawn contract: N hands as derived identities of the parent,
// no keyfile provisioning, audit root attributed to the parent, briefs
// threaded under it, RunsStore rows keyed on the derived agent with
// DispatchMsgID = the audit root.
func TestSubmitSpawnCreatesDerivedHands(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, audit, rec := newSpawnFixture(fk)

	handles, err := r.SubmitSpawn(context.Background(), "plumb", "summarize the runner package", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	// plumb's kindred pool leases bob, then fathom (the P2 naming amendment).
	if len(handles) != 2 || handles[0].Name != "plumb.bob" || handles[1].Name != "plumb.fathom" {
		t.Fatalf("handles = %+v", handles)
	}
	for _, h := range handles {
		if h.RunID == "" {
			t.Errorf("launched hand %s should carry a RunID", h.Name)
		}
	}
	if !r.AgentBusy("plumb.bob") || !r.AgentBusy("plumb.fathom") {
		t.Error("both hands should be busy")
	}
	if r.AgentBusy("plumb") {
		t.Error("the parent itself must stay free — hands run beside it")
	}
	if len(fk.jobs) != 2 || !strings.HasPrefix(fk.jobs[0], "builder-plumb-bob-") {
		t.Errorf("jobs = %v", fk.jobs)
	}
	if len(fk.secrets) != 0 {
		t.Errorf("hands must not provision keyfile secrets, got %v", fk.secrets)
	}
	if !fk.homes["plumb.bob"] {
		t.Error("hand home repo PVC should be provisioned for the derived name")
	}

	// Audit root: from=<parent>, fresh thread (no topic on the root).
	if len(audit.posts) != 1 || audit.posts[0].from != "plumb" || audit.posts[0].topic != "" {
		t.Fatalf("audit posts = %+v", audit.posts)
	}
	if !strings.Contains(audit.posts[0].content, "2 hand(s) of plumb") {
		t.Errorf("root content = %q", audit.posts[0].content)
	}

	// Brief posts precede the spawned posts, all in the spawn-42 thread.
	if len(poster.posts) < 4 {
		t.Fatalf("posts = %v", poster.posts)
	}
	if !strings.Contains(poster.posts[0], "hand plumb.bob brief:") ||
		!strings.Contains(poster.posts[1], "hand plumb.fathom brief:") {
		t.Errorf("brief posts wrong/missing: %v", poster.posts)
	}
	if !strings.Contains(poster.posts[2], "hand spawned as plumb.bob") {
		t.Errorf("spawned post wrong: %v", poster.posts)
	}

	// RunsStore rows: agent = derived name, DispatchMsgID = audit root,
	// thread = the rooted spawn thread.
	if len(rec.starts) != 2 {
		t.Fatalf("run starts = %+v", rec.starts)
	}
	for _, s := range rec.starts {
		if !strings.HasPrefix(s.agent, "plumb.") {
			t.Errorf("run agent = %q", s.agent)
		}
		if s.dispatchMsgID != 42 {
			t.Errorf("DispatchMsgID = %d, want the audit root 42", s.dispatchMsgID)
		}
		if s.thread != "spawn-42" {
			t.Errorf("thread = %q, want spawn-42", s.thread)
		}
		if !strings.HasPrefix(s.ticket, "hand-") {
			t.Errorf("ticket = %q", s.ticket)
		}
	}
}

// BuildJob for a hand brief: derived credential as env in place of the
// keyfile volume, no -k arg, lineage label.
func TestBuildJobHandSpec(t *testing.T) {
	b := dispatch.Brief{
		Agent:       "plumb.sub-1",
		SpawnParent: "plumb",
		SessionJWT:  "hand-jwt",
		Ticket:      "hand-abc",
		Thread:      "spawn-42",
		RunID:       "run-1",
	}
	job := dispatch.BuildJob(b, dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"}, "task-1", "claude")

	if got := job.Labels["nexus.dispatch/lineage"]; got != "plumb" {
		t.Errorf("lineage label = %q", got)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "keyfile" {
			t.Error("hand Job must not mount a keyfile volume")
		}
	}
	c := job.Spec.Template.Spec.Containers[0]
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/etc/nexus" {
			t.Error("hand Job must not mount /etc/nexus")
		}
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["CW_SESSION_JWT"] != "hand-jwt" || env["CW_ASPECT_NAME"] != "plumb.sub-1" || env["CW_SPAWN_PARENT"] != "plumb" {
		t.Errorf("hand env = %v", env)
	}
	for i, a := range c.Args {
		if a == "-k" {
			t.Errorf("hand args must not carry -k (args=%v, idx=%d)", c.Args, i)
		}
	}
}

// A hand Job built with JobConfig.BrokerCAFile set carries the CA into
// the pod: CW_SEAM_CA points at the in-pod mount path, and the Job has the
// broker-CA Secret volume + read-only mount so the file exists for
// agentfunnel's BrokerTLSConfigFromCAFile (self-signed/internal-CA broker).
func TestBuildJobHandInjectsBrokerCA(t *testing.T) {
	b := dispatch.Brief{
		Agent:       "plumb.sub-1",
		SpawnParent: "plumb",
		SessionJWT:  "hand-jwt",
		Ticket:      "hand-abc",
		Thread:      "spawn-42",
		RunID:       "run-1",
	}
	job := dispatch.BuildJob(b, dispatch.JobConfig{
		Namespace:    "nexus",
		BrokerHost:   "nexus.internal",
		BrokerCAFile: "/etc/nexus-ca/ca.crt",
	}, "task-1", "claude")

	c := job.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["CW_SEAM_CA"] != "/etc/nexus-ca/ca.crt" {
		t.Errorf("CW_SEAM_CA = %q, want the in-pod CA path /etc/nexus-ca/ca.crt", env["CW_SEAM_CA"])
	}

	var hasMount bool
	for _, m := range c.VolumeMounts {
		if m.Name == "broker-ca" {
			hasMount = true
			if !m.ReadOnly {
				t.Error("broker-ca mount must be read-only")
			}
			if m.MountPath != "/etc/nexus-ca" {
				t.Errorf("broker-ca mount path = %q, want /etc/nexus-ca", m.MountPath)
			}
		}
	}
	if !hasMount {
		t.Error("hand Job with BrokerCAFile must mount the broker-ca volume")
	}
	var hasVol bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "broker-ca" {
			hasVol = true
			if v.Secret == nil || v.Secret.SecretName != "nexus-broker-ca" {
				t.Errorf("broker-ca volume must source the nexus-broker-ca Secret, got %+v", v.Secret)
			}
		}
	}
	if !hasVol {
		t.Error("hand Job with BrokerCAFile must define the broker-ca Secret volume")
	}
}

// Empty BrokerCAFile → no CW_SEAM_CA, no broker-ca volume/mount: the hand
// falls back to system trust (the CA-signed / LE broker default, unchanged
// behavior).
func TestBuildJobHandNoBrokerCAByDefault(t *testing.T) {
	b := dispatch.Brief{
		Agent:       "plumb.sub-1",
		SpawnParent: "plumb",
		SessionJWT:  "hand-jwt",
		Ticket:      "hand-abc",
		RunID:       "run-1",
	}
	job := dispatch.BuildJob(b, dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"}, "task-1", "claude")

	c := job.Spec.Template.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "CW_SEAM_CA" {
			t.Errorf("empty BrokerCAFile must not inject CW_SEAM_CA, got %q", e.Value)
		}
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "broker-ca" {
			t.Error("empty BrokerCAFile must not mount a broker-ca volume")
		}
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "broker-ca" {
			t.Error("empty BrokerCAFile must not define a broker-ca volume")
		}
	}
}

// Ticket dispatches keep the keyfile mount + -k arg (regression guard
// for the spawn-conditional jobspec).
func TestBuildJobTicketDispatchKeepsKeyfile(t *testing.T) {
	b := dispatch.Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "t", RunID: "run-1"}
	job := dispatch.BuildJob(b, dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"}, "task-1", "claude")
	var hasKeyfile bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "keyfile" && v.Secret != nil && v.Secret.SecretName == "aspect-keyfile-anvil" {
			hasKeyfile = true
		}
	}
	if !hasKeyfile {
		t.Error("ticket dispatch lost its keyfile volume")
	}
	c := job.Spec.Template.Spec.Containers[0]
	if len(c.Args) < 2 || c.Args[0] != "-k" {
		t.Errorf("ticket args = %v, want leading -k", c.Args)
	}
	if _, ok := job.Labels["nexus.dispatch/lineage"]; ok {
		t.Error("ticket dispatch must not carry a lineage label")
	}
	var envNames []string
	for _, e := range c.Env {
		envNames = append(envNames, e.Name)
	}
	for _, n := range envNames {
		if n == "CW_SESSION_JWT" {
			t.Error("ticket dispatch must not carry CW_SESSION_JWT")
		}
	}
	_ = corev1.EnvVar{} // keep corev1 imported for future spec assertions
}

// NEX-610: a hand of an ollama-provider parent must carry the provider
// endpoint env — without OLLAMA_BASE_URL the in-Job agentfunnel dials
// localhost:11434 and the hand's turn dies on connect.
func TestBuildJobHandOllamaProviderEnv(t *testing.T) {
	b := dispatch.Brief{
		Agent:       "harrow.tine",
		SpawnParent: "harrow",
		SessionJWT:  "hand-jwt",
		Ticket:      "hand-abc",
		Thread:      "spawn-42",
		RunID:       "run-1",
	}
	cfg := dispatch.JobConfig{
		Namespace:       "nexus",
		BrokerHost:      "nexus.internal",
		OllamaBaseURL:   "http://gemma-ollama.nexus.svc.cluster.local:11434",
		OllamaKeepAlive: "-1s",
	}
	for _, provider := range []string{"ollama", "ollama-local"} {
		c := dispatch.BuildJob(b, cfg, "task-1", provider).Spec.Template.Spec.Containers[0]
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if env["OLLAMA_BASE_URL"] != cfg.OllamaBaseURL {
			t.Errorf("provider %q: OLLAMA_BASE_URL = %q, want %q", provider, env["OLLAMA_BASE_URL"], cfg.OllamaBaseURL)
		}
		if env["OLLAMA_KEEP_ALIVE"] != "-1s" {
			t.Errorf("provider %q: OLLAMA_KEEP_ALIVE = %q, want -1s", provider, env["OLLAMA_KEEP_ALIVE"])
		}
	}

	// Non-ollama providers must not pick the env up.
	c := dispatch.BuildJob(b, cfg, "task-1", "claude").Spec.Template.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "OLLAMA_BASE_URL" || e.Name == "OLLAMA_KEEP_ALIVE" {
			t.Errorf("claude provider must not carry %s", e.Name)
		}
	}

	// Empty config → omitted even for ollama (agentfunnel default applies).
	c = dispatch.BuildJob(b, dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal"}, "task-1", "ollama").Spec.Template.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "OLLAMA_BASE_URL" || e.Name == "OLLAMA_KEEP_ALIVE" {
			t.Errorf("empty JobConfig must not inject %s", e.Name)
		}
	}
}

// NEX-610: a hand of a codex-provider parent inherits the raw store
// value ("codex", not the canonical "codex-cli") and must still get the
// codex-auth init container + mounts that ticket dispatches get.
func TestBuildJobHandCodexAliasAuthMount(t *testing.T) {
	b := dispatch.Brief{
		Agent:       "plumb.bob",
		SpawnParent: "plumb",
		SessionJWT:  "hand-jwt",
		Ticket:      "hand-abc",
		Thread:      "spawn-42",
		RunID:       "run-1",
	}
	job := dispatch.BuildJob(b, dispatch.JobConfig{Namespace: "nexus", BrokerHost: "nexus.internal", Image: "img"}, "task-1", "codex")
	pod := job.Spec.Template.Spec
	var hasInit bool
	for _, ic := range pod.InitContainers {
		if ic.Name == "codex-auth" {
			hasInit = true
		}
	}
	if !hasInit {
		t.Error("codex-alias hand Job must carry the codex-auth init container")
	}
	var hasSecret, hasHome bool
	for _, v := range pod.Volumes {
		if v.Name == "codex-secret" {
			hasSecret = true
		}
		if v.Name == "codex-home" {
			hasHome = true
		}
	}
	if !hasSecret || !hasHome {
		t.Errorf("codex-alias hand Job volumes: codex-secret=%v codex-home=%v, want both", hasSecret, hasHome)
	}
}

// Per-parent cap: hands beyond SpawnMaxConcurrent queue and drain when
// a sibling completes — Submit's queue semantics.
func TestSubmitSpawnPerParentCapQueuesAndDrains(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, rec := newSpawnFixture(fk)
	r.SpawnMaxConcurrent = 2

	handles, err := r.SubmitSpawn(context.Background(), "plumb", "work", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 3 {
		t.Fatalf("handles = %+v", handles)
	}
	if len(fk.jobs) != 2 {
		t.Fatalf("jobs = %v, want 2 launched (third queued)", fk.jobs)
	}
	var queued *dispatch.SpawnHandle
	for i := range handles {
		if handles[i].RunID == "" {
			queued = &handles[i]
		}
	}
	if queued == nil || queued.Name != "plumb.sound" {
		t.Fatalf("expected plumb.sound queued with empty RunID, handles=%+v", handles)
	}

	// Complete the first hand → the queued third drains.
	r.OnJobDone(dispatch.JobDone{Ticket: rec.starts[0].ticket, OK: true})
	if len(fk.jobs) != 3 {
		t.Errorf("jobs after drain = %d, want 3", len(fk.jobs))
	}
	if !r.AgentBusy("plumb.sound") {
		t.Error("plumb.sound should run after a sibling freed capacity")
	}
}

// #5: the per-parent QUEUE is bounded too — at most SpawnMaxConcurrent
// hands queued on top of SpawnMaxConcurrent running, so a chatty parent
// can't grow the queue without bound. The overflowing request errors
// (the broker relays it as spawn.request.error); draining a running
// hand frees a queue slot.
func TestSubmitSpawnQueueBoundRejectsOverflow(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, rec := newSpawnFixture(fk)
	// Default SpawnMaxConcurrent = 4: first request fills the run slots…
	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 4, ""); err != nil {
		t.Fatal(err)
	}
	// …second fills the queue.
	handles, err := r.SubmitSpawn(context.Background(), "plumb", "work", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, h := range handles {
		if h.RunID == "" {
			queued++
		}
	}
	if queued != 4 {
		t.Fatalf("queued = %d, want 4 (run slots all busy)", queued)
	}
	// The 9th hand exceeds cap-running + cap-queued: rejected outright.
	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err == nil {
		t.Fatal("9th hand must be rejected: per-parent queue is full")
	}
	if len(fk.jobs) != 4 {
		t.Fatalf("jobs = %d, want 4 (nothing extra launched)", len(fk.jobs))
	}

	// Drain one running hand → one queued hand launches, freeing a
	// queue slot → a new spawn queues cleanly again.
	r.OnJobDone(dispatch.JobDone{Ticket: rec.starts[0].ticket, OK: true})
	if len(fk.jobs) != 5 {
		t.Fatalf("jobs after drain = %d, want 5", len(fk.jobs))
	}
	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err != nil {
		t.Fatalf("drain must free a queue slot: %v", err)
	}
}

// #9: a partial launch failure is visible in the returned handles — the
// failed hand carries Error instead of being silently omitted, beside
// its successfully-launched sibling.
func TestSubmitSpawnPartialFailureMarksHandle(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)
	r.MintHandCredential = func(_ context.Context, _, derived string) (string, error) {
		if derived == "plumb.fathom" {
			return "", errors.New("mint boom")
		}
		return "jwt-for-" + derived, nil
	}

	handles, err := r.SubmitSpawn(context.Background(), "plumb", "work", 2, "")
	if err != nil {
		t.Fatalf("one hand launched fine — partial failure must not error the request: %v", err)
	}
	if len(handles) != 2 {
		t.Fatalf("handles = %+v, want both hands visible", handles)
	}
	byName := map[string]dispatch.SpawnHandle{}
	for _, h := range handles {
		byName[h.Name] = h
	}
	ok1 := byName["plumb.bob"]
	if ok1.RunID == "" || ok1.Error != "" {
		t.Errorf("plumb.bob = %+v, want launched with no error", ok1)
	}
	failed := byName["plumb.fathom"]
	if failed.Error == "" || !strings.Contains(failed.Error, "mint boom") {
		t.Errorf("plumb.fathom = %+v, want Error carrying the mint failure", failed)
	}
}

// The global MaxConc applies on top of the per-parent cap.
func TestSubmitSpawnGlobalMaxConcApplies(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)
	r.MaxConc = 1

	handles, err := r.SubmitSpawn(context.Background(), "plumb", "work", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fk.jobs) != 1 {
		t.Fatalf("jobs = %v, want 1 (global cap)", fk.jobs)
	}
	launched := 0
	for _, h := range handles {
		if h.RunID != "" {
			launched++
		}
	}
	if launched != 1 {
		t.Errorf("launched handles = %d, want 1", launched)
	}
}

// Overlapping spawns never collide on a derived name: pool words
// already busy (or queued) are skipped, so the next free kindred word
// is leased.
func TestSubmitSpawnPicksFreeIndices(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)

	if _, err := r.SubmitSpawn(context.Background(), "plumb", "first", 1, ""); err != nil {
		t.Fatal(err)
	}
	handles, err := r.SubmitSpawn(context.Background(), "plumb", "second", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0].Name != "plumb.fathom" {
		t.Fatalf("handles = %+v, want plumb.fathom (bob busy)", handles)
	}
}

// A caller-supplied thread means the hands JOIN that thread: no extra
// audit root, DispatchMsgID stays 0, posts target the given topic.
func TestSubmitSpawnExistingThreadSkipsRoot(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, audit, rec := newSpawnFixture(fk)

	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, "NEX-571"); err != nil {
		t.Fatal(err)
	}
	if len(audit.posts) != 0 {
		t.Errorf("no audit root expected, got %+v", audit.posts)
	}
	if rec.starts[0].thread != "NEX-571" || rec.starts[0].dispatchMsgID != 0 {
		t.Errorf("start = %+v", rec.starts[0])
	}
	if len(poster.posts) == 0 {
		t.Fatal("brief post missing")
	}
}

func TestSubmitSpawnRejections(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)

	if _, err := r.SubmitSpawn(context.Background(), "plumb.sub-1", "work", 1, ""); err == nil {
		t.Error("derived parent must be rejected (no sub-of-sub)")
	}
	if _, err := r.SubmitSpawn(context.Background(), "plumb", "  ", 1, ""); err == nil {
		t.Error("empty brief must be rejected")
	}
	r.MintHandCredential = nil
	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err == nil {
		t.Error("missing credential minter must be rejected")
	}
	if len(fk.jobs) != 0 {
		t.Errorf("no jobs expected, got %v", fk.jobs)
	}
}

// A mint failure rolls the hand back: agent freed, run recorded failed,
// failure posted to the thread, no handle returned.
func TestSubmitSpawnMintFailureRollsBack(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, _, rec := newSpawnFixture(fk)
	r.MintHandCredential = func(context.Context, string, string) (string, error) {
		return "", errors.New("mint boom")
	}

	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err == nil {
		t.Fatal("expected error when every hand fails to launch")
	}
	if r.AgentBusy("plumb.bob") {
		t.Error("failed hand must be rolled back")
	}
	if len(rec.done) != 1 || rec.done[0].status != "failed" {
		t.Errorf("recorder done = %+v", rec.done)
	}
	found := false
	for _, p := range poster.posts {
		if strings.Contains(p, "spawn failed") && strings.Contains(p, "mint boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("failure not posted: %v", poster.posts)
	}
	if len(fk.jobs) != 0 {
		t.Errorf("no job should be created, got %v", fk.jobs)
	}
}

// Kindred-name leasing (the P2 naming amendment): hands take distinct
// words from the parent's pool in order, and a completed hand returns
// its word to the pool for the next spawn to reuse.
func TestSubmitSpawnLeasesKindredNamesAndReturnsOnComplete(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, rec := newSpawnFixture(fk)

	// shadow's pool: umbra, gloam, shade, dusk, …
	handles, err := r.SubmitSpawn(context.Background(), "shadow", "work", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{handles[0].Name, handles[1].Name, handles[2].Name}
	want := []string{"shadow.umbra", "shadow.gloam", "shadow.shade"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("leased names = %v, want %v", got, want)
		}
	}

	// Complete the first hand (umbra) → its word frees.
	r.OnJobDone(dispatch.JobDone{Ticket: rec.starts[0].ticket, OK: true})
	if r.AgentBusy("shadow.umbra") {
		t.Fatal("umbra must return to the pool on completion")
	}
	// Next spawn reuses the freed umbra ahead of an unused word.
	more, err := r.SubmitSpawn(context.Background(), "shadow", "again", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if more[0].Name != "shadow.umbra" {
		t.Fatalf("reused name = %q, want shadow.umbra (returned word)", more[0].Name)
	}
}

// Pool exhaustion falls through to the `<parent>.hand-N` overflow
// naming without wedging (a small custom pool forces it).
func TestSubmitSpawnOverflowsExhaustedPool(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)
	r.SpawnMaxConcurrent = 8
	r.MaxConc = 8
	// Two-word custom pool for a made-up parent → the 3rd+ hand overflows.
	r.AspectHandNames = map[string][]string{"probe": {"alpha", "beta"}}

	handles, err := r.SubmitSpawn(context.Background(), "probe", "work", 4, "")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{handles[0].Name, handles[1].Name, handles[2].Name, handles[3].Name}
	want := []string{"probe.alpha", "probe.beta", "probe.hand-1", "probe.hand-2"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v (pool then overflow)", names, want)
		}
	}
}

// Provider inheritance (Task D): a hand runs the PARENT's provider
// binding, not the launch default — surfaced via the Job's provider
// (codex-cli wires a codex-auth init container; claude does not).
func TestSubmitSpawnInheritsParentProvider(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)
	r.HandProvider = func(_ context.Context, parent string) string {
		if parent == "plumb" {
			return "codex-cli"
		}
		return ""
	}

	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err != nil {
		t.Fatal(err)
	}
	if len(fk.jobObjs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(fk.jobObjs))
	}
	hasCodexAuth := false
	for _, ic := range fk.jobObjs[0].Spec.Template.Spec.InitContainers {
		if ic.Name == "codex-auth" {
			hasCodexAuth = true
		}
	}
	if !hasCodexAuth {
		t.Error("hand should inherit the parent's codex-cli provider (codex-auth init container missing)")
	}
}

// With no HandProvider wired, a hand falls back to the launch default
// (claude) — no codex-auth init container.
func TestSubmitSpawnDefaultsProviderWhenNoInheritance(t *testing.T) {
	fk := &fakeK8s{}
	r, _, _, _ := newSpawnFixture(fk)

	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err != nil {
		t.Fatal(err)
	}
	for _, ic := range fk.jobObjs[0].Spec.Template.Spec.InitContainers {
		if ic.Name == "codex-auth" {
			t.Error("no provider inheritance → must use the claude launch default, not codex")
		}
	}
}

// OnJobDone's completion post for a hand carries the lineage, not the
// builder branch/PR block.
func TestHandCompletionSummaryCarriesLineage(t *testing.T) {
	fk := &fakeK8s{}
	r, poster, _, rec := newSpawnFixture(fk)

	if _, err := r.SubmitSpawn(context.Background(), "plumb", "work", 1, ""); err != nil {
		t.Fatal(err)
	}
	r.OnJobDone(dispatch.JobDone{Ticket: rec.starts[0].ticket, OK: true})

	got := poster.posts[len(poster.posts)-1]
	if !strings.Contains(got, "hand done: plumb.bob (hand of plumb)") {
		t.Errorf("summary = %q", got)
	}
	if strings.Contains(got, "PR:") || strings.Contains(got, "branch:") {
		t.Errorf("hand summary must not carry the builder PR block: %q", got)
	}
	if r.AgentBusy("plumb.bob") {
		t.Error("hand should be free after completion")
	}
}

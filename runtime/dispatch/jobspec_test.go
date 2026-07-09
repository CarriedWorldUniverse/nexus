package dispatch

import (
	"regexp"
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/frame/funnel"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildJob(t *testing.T) {
	cfg := JobConfig{
		Image: "localhost/nexus-builder:dev", Namespace: "nexus",
		NodeIP: "192.168.143.133", BrokerHost: "dmonextreme.tail41686e.ts.net",
		BriefTimeout: "30m",
	}
	b := Brief{Agent: "anvil", Ticket: "NEX-999", Thread: "NEX-999"}
	job := BuildJob(b, cfg, "abc123", "codex-cli")

	if job.Labels["nexus.dispatch/ticket"] != "NEX-999" {
		t.Errorf("missing ticket label: %v", job.Labels)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != cfg.Image {
		t.Errorf("image = %q", c.Image)
	}
	if !contains(c.Args, "-builder") || !contains(c.Args, "-brief-file") {
		t.Errorf("args missing builder/-brief-file: %v", c.Args)
	}
	if !argValueEquals(c.Args, "-reply-topic", "NEX-999") {
		t.Errorf("args missing -reply-topic NEX-999: %v", c.Args)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Errorf("activeDeadlineSeconds = %v, want 1800", job.Spec.ActiveDeadlineSeconds)
	}
	// NEX-437: the brief must mount in its own directory, never as a file
	// inside /etc/nexus (the keyfile Secret's mount) — else the OCI runtime
	// fails the container with "not a directory".
	for _, m := range c.VolumeMounts {
		if m.Name == "brief" {
			if strings.HasPrefix(m.MountPath, "/etc/nexus") {
				t.Errorf("brief mount %q collides with the keyfile Secret at /etc/nexus", m.MountPath)
			}
		}
	}
	if !contains(c.Args, "/etc/dispatch/brief.md") {
		t.Errorf("-brief-file should point at /etc/dispatch/brief.md, args: %v", c.Args)
	}
	if !argValueEquals(c.Args, "-builder-idle-timeout", "2m") {
		t.Errorf("args missing default -builder-idle-timeout 2m: %v", c.Args)
	}
	if !envValueEquals(c.Env, "CW_IDLE_TIMEOUT", "2m") {
		t.Errorf("env missing CW_IDLE_TIMEOUT=2m: %v", c.Env)
	}
}

func TestBuildJob_ProviderCodexBits(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		wantCodex bool
	}{
		{name: "empty is codex-cli", wantCodex: true},
		{name: "codex-cli", provider: "codex-cli", wantCodex: true},
		// Store aliases (NEX-610): the aspects.provider column carries
		// whatever the operator set; hand briefs inherit it raw.
		{name: "codex alias", provider: "codex", wantCodex: true},
		{name: "codexcli alias", provider: "codexcli", wantCodex: true},
		{name: "openai", provider: "openai", wantCodex: false},
		{name: "ollama", provider: "ollama", wantCodex: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := JobConfig{
				Image:         "img",
				Namespace:     "nexus",
				BrokerHost:    "broker.example",
				LynxAIBaseURL: "https://lynx.example",
				LynxAIKey:     "lynx-key",
			}
			job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1"}, cfg, "t1", tt.provider)
			pod := job.Spec.Template.Spec
			c := pod.Containers[0]

			hasCodexInit := containerExists(pod.InitContainers, "codex-auth")
			hasCodexHomeMount := volumeMountExists(c.VolumeMounts, "codex-home")
			hasCodexSecretVolume := volumeExists(pod.Volumes, "codex-secret")
			hasCodexHomeVolume := volumeExists(pod.Volumes, "codex-home")
			if hasCodexInit != tt.wantCodex || hasCodexHomeMount != tt.wantCodex || hasCodexSecretVolume != tt.wantCodex || hasCodexHomeVolume != tt.wantCodex {
				t.Fatalf("codex resources mismatch for provider %q: init=%v mount=%v secret=%v home=%v want %v",
					tt.provider, hasCodexInit, hasCodexHomeMount, hasCodexSecretVolume, hasCodexHomeVolume, tt.wantCodex)
			}

			if !volumeMountExists(c.VolumeMounts, "keyfile") || !volumeExists(pod.Volumes, "keyfile") {
				t.Fatalf("keyfile mount/volume missing for provider %q", tt.provider)
			}
			if !envValueEquals(c.Env, "CW_SEAM_URL", "https://broker.example:7888") {
				t.Fatalf("CW_SEAM_URL missing or wrong: %v", c.Env)
			}
			if !envValueEquals(c.Env, "CW_AGENT_HOME_REPO", "/var/lib/nexus/home.git") || !envValueEquals(c.Env, "CW_AGENT_HOME_WORKDIR", "/agent-home") {
				t.Fatalf("home env missing: %v", c.Env)
			}
			if !envValueEquals(c.Env, "CW_SHARED_REPOS_DIR", "/src") {
				t.Fatalf("shared repos env missing: %v", c.Env)
			}
			if !envValueEquals(c.Env, "LYNXAI_BASE_URL", "https://lynx.example") || !envValueEquals(c.Env, "LYNXAI_KEY", "lynx-key") {
				t.Fatalf("LYNXAI env missing: %v", c.Env)
			}
		})
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

func argValueEquals(ss []string, key, want string) bool {
	for i := 0; i < len(ss)-1; i++ {
		if ss[i] == key {
			return ss[i+1] == want
		}
	}
	return false
}

func containerExists(containers []corev1.Container, name string) bool {
	for _, c := range containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func volumeMountExists(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

func volumeExists(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func envValueEquals(env []corev1.EnvVar, name, want string) bool {
	for _, v := range env {
		if v.Name == name {
			return v.Value == want
		}
	}
	return false
}

func TestBuildJob_ProviderAntigravityBits(t *testing.T) {
	cfg := JobConfig{Image: "img", Namespace: "nexus", BrokerHost: "broker.example"}

	// antigravity-cli mounts the antigravity-auth secret read-only; the
	// agentfunnel stages it into $HOME/.gemini at runtime (no init-container).
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "antigravity-cli")
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	if !volumeExists(pod.Volumes, "antigravity-secret") || !volumeMountExists(c.VolumeMounts, "antigravity-secret") {
		t.Fatalf("antigravity-cli: antigravity-secret volume/mount missing")
	}
	if containerExists(pod.InitContainers, "codex-auth") {
		t.Fatalf("antigravity-cli should not get the codex-auth init container")
	}

	// Other providers must not get the antigravity secret.
	pod2 := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t2", "codex-cli").Spec.Template.Spec
	if volumeExists(pod2.Volumes, "antigravity-secret") {
		t.Fatalf("codex-cli should not get the antigravity-secret volume")
	}
}

func TestBuildJob_WorkspaceCleanPerJob(t *testing.T) {
	// NEX-465: /work must be a per-job emptyDir so a fresh clone never sees a
	// leftover clone from a prior run on the shared PVC (which caused a builder
	// to work in the wrong repo). The Go build cache stays on the PVC for speed.
	cfg := JobConfig{Image: "img", Namespace: "nexus", NodeIP: "1.2.3.4", BrokerHost: "h"}
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "codex-cli")
	vols := map[string]corev1.Volume{}
	for _, v := range job.Spec.Template.Spec.Volumes {
		vols[v.Name] = v
	}
	if vols["work"].EmptyDir == nil {
		t.Errorf("work volume must be EmptyDir (clean per job), got %+v", vols["work"].VolumeSource)
	}
	if vols["work"].PersistentVolumeClaim != nil {
		t.Error("work volume must NOT be a shared PVC — stale clones leak between runs")
	}
	if vols["cache"].PersistentVolumeClaim == nil {
		t.Error("cache volume should stay a PVC for the Go build cache")
	}
	if vols["home-work"].EmptyDir == nil {
		t.Errorf("home-work volume must be EmptyDir (per-run expressed home), got %+v", vols["home-work"].VolumeSource)
	}
	if vols["home-repo"].PersistentVolumeClaim == nil || vols["home-repo"].PersistentVolumeClaim.ClaimName != "aspect-home-anvil" {
		t.Errorf("home-repo must mount the per-agent PVC, got %+v", vols["home-repo"].VolumeSource)
	}
	if vols["shared-repos"].PersistentVolumeClaim == nil || vols["shared-repos"].PersistentVolumeClaim.ClaimName != SharedReposPVCName() {
		t.Errorf("shared-repos must mount the global repos PVC, got %+v", vols["shared-repos"].VolumeSource)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if !volumeMountExists(c.VolumeMounts, "home-repo") || !volumeMountExists(c.VolumeMounts, "home-work") || !volumeMountExists(c.VolumeMounts, "shared-repos") {
		t.Fatalf("home/shared repo mounts missing: %v", c.VolumeMounts)
	}
}

func TestBuildJob_PassesRepoTicket(t *testing.T) {
	// NEX-471/NEX-468: the builder needs -repo + -ticket to verify the PR exists
	// before exiting on a judge "complete".
	cfg := JobConfig{Namespace: "nexus", Image: "img"}
	b := Brief{Agent: "anvil", Ticket: "NEX-7", Repo: "CarriedWorldUniverse/nexus", Thread: "NEX-7"}
	c := BuildJob(b, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
	if !argValueEquals(c.Args, "-repo", "CarriedWorldUniverse/nexus") {
		t.Errorf("args missing -repo: %v", c.Args)
	}
	if !argValueEquals(c.Args, "-ticket", "NEX-7") {
		t.Errorf("args missing -ticket: %v", c.Args)
	}
	if !argValueEquals(c.Args, "-branch", "") {
		t.Errorf("args missing empty -branch: %v", c.Args)
	}
}

func TestBuildJob_PassesIdleTimeoutAndHardCeiling(t *testing.T) {
	cfg := JobConfig{Namespace: "nexus", Image: "img", BriefTimeout: "45m", IdleTimeout: "90s"}
	c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-654"}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
	job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-654"}, cfg, "t1", "codex-cli")

	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 2700 {
		t.Fatalf("activeDeadlineSeconds = %v, want 2700", job.Spec.ActiveDeadlineSeconds)
	}
	if !argValueEquals(c.Args, "-builder-idle-timeout", "90s") {
		t.Fatalf("args missing -builder-idle-timeout 90s: %v", c.Args)
	}
	if !envValueEquals(c.Env, "CW_IDLE_TIMEOUT", "90s") {
		t.Fatalf("env missing CW_IDLE_TIMEOUT=90s: %v", c.Env)
	}
}

func TestBuildJob_TTLAndRunUUIDName(t *testing.T) {
	cfg := JobConfig{Namespace: "nexus", Image: "img"}
	runID := "run-550e8400-e29b-41d4-a716-446655440000"
	job := BuildJob(Brief{
		Agent:  "anvil.with.invalid.characters.and-a-very-long-name",
		Ticket: "NEX-7",
		RunID:  runID,
	}, cfg, runID, "codex-cli")

	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 300 {
		t.Fatalf("ttlSecondsAfterFinished = %v, want 300", job.Spec.TTLSecondsAfterFinished)
	}
	if len(job.Name) > 63 {
		t.Fatalf("job name length = %d, want <= 63: %q", len(job.Name), job.Name)
	}
	if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(job.Name) {
		t.Fatalf("job name is not a DNS label: %q", job.Name)
	}
	if !strings.HasSuffix(job.Name, "-run-550e8400e29b41d4a716") {
		t.Fatalf("job name should include a long UUID-derived suffix, got %q", job.Name)
	}
}

// TestBuildJob_RoleAtSpawn is a table test of the M1 Unit 3 threading: an
// empty Brief (no RolePrompt/SkillAllowlist/PolicyFragment/WorkItemID/
// Personality) must reproduce today's exact Job args/env/labels, and
// each field, when set, must surface at its documented touchpoint.
func TestBuildJob_RoleAtSpawn(t *testing.T) {
	cfg := JobConfig{Namespace: "nexus", Image: "img", BrokerHost: "broker.example"}

	t.Run("empty brief: no role-at-spawn args/env/labels", func(t *testing.T) {
		c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if contains(c.Args, "-role-file") || contains(c.Args, "-policy-fragment-file") || contains(c.Args, "-acceptance-file") {
			t.Errorf("empty brief must not pass role-at-spawn flags: %v", c.Args)
		}
		for _, name := range []string{"CW_ROLE", "CW_WORK_ITEM_ID", "CW_PERSONALITY", "CW_SKILL_ALLOWLIST"} {
			for _, e := range c.Env {
				if e.Name == name {
					t.Errorf("empty brief must not set env %s", name)
				}
			}
		}
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "codex-cli")
		if _, ok := job.Labels["nexus.dispatch/work-item"]; ok {
			t.Error("empty brief must not set the work-item label")
		}
		if _, ok := job.Labels["nexus.dispatch/personality"]; ok {
			t.Error("empty brief must not set the personality label")
		}
	})

	t.Run("role sets -role-file pointing at the brief ConfigMap mount", func(t *testing.T) {
		c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", RolePrompt: "you are a builder"}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if !argValueEquals(c.Args, "-role-file", "/etc/dispatch/role.md") {
			t.Errorf("args missing -role-file /etc/dispatch/role.md: %v", c.Args)
		}
	})

	t.Run("policy fragment sets -policy-fragment-file", func(t *testing.T) {
		c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", PolicyFragment: &funnel.ToolPolicy{DefaultAllow: false}}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if !argValueEquals(c.Args, "-policy-fragment-file", "/etc/dispatch/policy.json") {
			t.Errorf("args missing -policy-fragment-file /etc/dispatch/policy.json: %v", c.Args)
		}
	})

	// Unit B (verified task_done, NET-22/23/24): acceptance criteria sets
	// -acceptance-file pointing at the brief ConfigMap mount, mirroring the
	// role-prompt/policy-fragment overlay wiring above exactly.
	t.Run("acceptance criteria sets -acceptance-file", func(t *testing.T) {
		c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", AcceptanceCriteria: "- must produce token X"}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if !argValueEquals(c.Args, "-acceptance-file", "/etc/dispatch/acceptance.md") {
			t.Errorf("args missing -acceptance-file /etc/dispatch/acceptance.md: %v", c.Args)
		}
	})

	t.Run("skill allowlist becomes CW_SKILL_ALLOWLIST env", func(t *testing.T) {
		c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", SkillAllowlist: []string{"test-run", "bash"}}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_SKILL_ALLOWLIST", "test-run,bash") {
			t.Errorf("env missing CW_SKILL_ALLOWLIST=test-run,bash: %v", c.Env)
		}
	})

	t.Run("role label becomes CW_ROLE env (M1 Unit 5 heartbeat source)", func(t *testing.T) {
		c := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", Role: "builder"}, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_ROLE", "builder") {
			t.Errorf("env missing CW_ROLE=builder: %v", c.Env)
		}
	})

	t.Run("work item id becomes label and env", func(t *testing.T) {
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", WorkItemID: "work-item-42"}, cfg, "t1", "codex-cli")
		if job.Labels["nexus.dispatch/work-item"] != "work-item-42" {
			t.Errorf("label nexus.dispatch/work-item = %q", job.Labels["nexus.dispatch/work-item"])
		}
		c := job.Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_WORK_ITEM_ID", "work-item-42") {
			t.Errorf("env missing CW_WORK_ITEM_ID=work-item-42: %v", c.Env)
		}
	})

	t.Run("personality becomes label and env", func(t *testing.T) {
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1", Personality: "anvil"}, cfg, "t1", "codex-cli")
		if job.Labels["nexus.dispatch/personality"] != "anvil" {
			t.Errorf("label nexus.dispatch/personality = %q", job.Labels["nexus.dispatch/personality"])
		}
		c := job.Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_PERSONALITY", "anvil") {
			t.Errorf("env missing CW_PERSONALITY=anvil: %v", c.Env)
		}
	})
}

// envSecretKeyRefEquals reports whether env carries a var named `name`
// sourced from a SecretKeyRef matching (secretName, secretKey).
func envSecretKeyRefEquals(env []corev1.EnvVar, name, secretName, secretKey string) bool {
	for _, v := range env {
		if v.Name != name {
			continue
		}
		if v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
			return false
		}
		ref := v.ValueFrom.SecretKeyRef
		return ref.Name == secretName && ref.Key == secretKey
	}
	return false
}

// TestBuildJob_ImageTagKnob is the §7 build-spec acceptance test: the CLI
// version knob (JobConfig.ImageTagPin) selects the image tag per dispatch —
// default (nil knob, or a knob returning "") uses cfg.Image unchanged;
// a non-empty pin overrides it, and CW_IMAGE_TAG mirrors whichever won.
func TestBuildJob_ImageTagKnob(t *testing.T) {
	baseCfg := JobConfig{Image: "localhost/nexus-runner:latest", Namespace: "nexus", BrokerHost: "h"}

	t.Run("no knob uses cfg.Image (today's behavior)", func(t *testing.T) {
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, baseCfg, "t1", "codex-cli")
		c := job.Spec.Template.Spec.Containers[0]
		if c.Image != baseCfg.Image {
			t.Errorf("image = %q, want default %q", c.Image, baseCfg.Image)
		}
		if !envValueEquals(c.Env, "CW_IMAGE_TAG", baseCfg.Image) {
			t.Errorf("CW_IMAGE_TAG should mirror the default image: %v", c.Env)
		}
	})

	t.Run("knob returning empty string uses cfg.Image (clear-the-pin path)", func(t *testing.T) {
		cfg := baseCfg
		cfg.ImageTagPin = func() string { return "" }
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "codex-cli")
		c := job.Spec.Template.Spec.Containers[0]
		if c.Image != baseCfg.Image {
			t.Errorf("image = %q, want default %q", c.Image, baseCfg.Image)
		}
	})

	t.Run("non-empty pin overrides cfg.Image", func(t *testing.T) {
		cfg := baseCfg
		cfg.ImageTagPin = func() string { return "localhost/nexus-runner:cli-2.1.3" }
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "codex-cli")
		c := job.Spec.Template.Spec.Containers[0]
		if c.Image != "localhost/nexus-runner:cli-2.1.3" {
			t.Errorf("image = %q, want pinned tag", c.Image)
		}
		if !envValueEquals(c.Env, "CW_IMAGE_TAG", "localhost/nexus-runner:cli-2.1.3") {
			t.Errorf("CW_IMAGE_TAG should mirror the pinned image: %v", c.Env)
		}
	})

	t.Run("pin also applies to the codex-auth init container image", func(t *testing.T) {
		cfg := baseCfg
		cfg.ImageTagPin = func() string { return "localhost/nexus-runner:cli-2.1.3" }
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "codex-cli")
		pod := job.Spec.Template.Spec
		for _, ic := range pod.InitContainers {
			if ic.Name == "codex-auth" && ic.Image != "localhost/nexus-runner:cli-2.1.3" {
				t.Errorf("codex-auth init image = %q, want pinned tag", ic.Image)
			}
		}
	})
}

// TestBuildJob_FrontierAuth is the §6 build-spec acceptance test: the
// frontier (claude-code) OAuth token is injected as CLAUDE_CODE_OAUTH_TOKEN
// via secretKeyRef for claude-code-provider dispatches only, sourced from
// whatever JobConfig.FrontierAuthFunc currently returns.
func TestBuildJob_FrontierAuth(t *testing.T) {
	cfg := JobConfig{Image: "img", Namespace: "nexus", BrokerHost: "h"}

	t.Run("no FrontierAuthFunc means no injection (today's behavior)", func(t *testing.T) {
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "claude-code")
		c := job.Spec.Template.Spec.Containers[0]
		for _, v := range c.Env {
			if v.Name == "CLAUDE_CODE_OAUTH_TOKEN" {
				t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN should not be injected without FrontierAuthFunc: %v", c.Env)
			}
		}
	})

	// IS_SANDBOX=1 lets the root-user container's claude-code CLI accept
	// --dangerously-skip-permissions (live-confirmed fix, 2026-07-06); it is
	// keyed to the claude-code provider only and needs no FrontierAuthFunc.
	t.Run("claude-code gets IS_SANDBOX=1, openai does not", func(t *testing.T) {
		envVal := func(env []corev1.EnvVar, name string) (string, bool) {
			for _, e := range env {
				if e.Name == name {
					return e.Value, true
				}
			}
			return "", false
		}
		cc := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "claude-code").Spec.Template.Spec.Containers[0].Env
		if v, ok := envVal(cc, "IS_SANDBOX"); !ok || v != "1" {
			t.Errorf("claude-code Job: IS_SANDBOX = %q,%v; want \"1\",true", v, ok)
		}
		oa := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, cfg, "t1", "openai").Spec.Template.Spec.Containers[0].Env
		if _, ok := envVal(oa, "IS_SANDBOX"); ok {
			t.Error("openai Job must not set IS_SANDBOX (spawns no CLI)")
		}
	})

	t.Run("claude-code dispatch gets the secret (k8s-secret delivery, the fallback path)", func(t *testing.T) {
		withFunc := cfg
		withFunc.FrontierAuthFunc = func() (string, string) {
			return DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey
		}
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, withFunc, "t1", "claude-code")
		c := job.Spec.Template.Spec.Containers[0]
		if !envSecretKeyRefEquals(c.Env, "CLAUDE_CODE_OAUTH_TOKEN", DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey) {
			t.Errorf("missing CLAUDE_CODE_OAUTH_TOKEN secretKeyRef: %v", c.Env)
		}
	})

	t.Run("claude-code provider aliases all get the token", func(t *testing.T) {
		withFunc := cfg
		withFunc.FrontierAuthFunc = func() (string, string) {
			return DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey
		}
		for _, provider := range []string{"claude-code", "claudecode", "claude", "claude-api"} {
			job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, withFunc, "t1", provider)
			c := job.Spec.Template.Spec.Containers[0]
			if !envSecretKeyRefEquals(c.Env, "CLAUDE_CODE_OAUTH_TOKEN", DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey) {
				t.Errorf("provider %q: missing CLAUDE_CODE_OAUTH_TOKEN secretKeyRef: %v", provider, c.Env)
			}
		}
	})

	t.Run("almanac-sourced pointer overrides the default secret", func(t *testing.T) {
		withFunc := cfg
		withFunc.FrontierAuthFunc = func() (string, string) { return "almanac-frontier-secret", "TOKEN" }
		job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, withFunc, "t1", "claude-code")
		c := job.Spec.Template.Spec.Containers[0]
		if !envSecretKeyRefEquals(c.Env, "CLAUDE_CODE_OAUTH_TOKEN", "almanac-frontier-secret", "TOKEN") {
			t.Errorf("missing almanac-sourced secretKeyRef: %v", c.Env)
		}
	})

	t.Run("non-claude-code providers never get the token", func(t *testing.T) {
		withFunc := cfg
		withFunc.FrontierAuthFunc = func() (string, string) {
			return DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey
		}
		for _, provider := range []string{"codex-cli", "ollama", "antigravity-cli"} {
			job := BuildJob(Brief{Agent: "anvil", Ticket: "NEX-1"}, withFunc, "t1", provider)
			c := job.Spec.Template.Spec.Containers[0]
			for _, v := range c.Env {
				if v.Name == "CLAUDE_CODE_OAUTH_TOKEN" {
					t.Errorf("provider %q should not get CLAUDE_CODE_OAUTH_TOKEN: %v", provider, c.Env)
				}
			}
		}
	})
}

// TestBuildJob_RoleBrainProviderModelEnv covers the role-tier-brains
// (2026-07-06) CW_PROVIDER/CW_MODEL injection: a Brief carrying a role-brain
// override (Brief.Provider/Model, threaded from dispatch.PoolItem — see
// pool.go) must both (a) surface as CW_PROVIDER/CW_MODEL env for agentfunnel
// to prefer over the broker resolve response, AND (b) drive the SAME
// `provider` BuildJob parameter that gates which auth gets mounted
// (FrontierAuthFunc's CLAUDE_CODE_OAUTH_TOKEN here) — i.e. the k8s-level
// auth and the worker-process-level provider selection are keyed off the
// same EFFECTIVE provider, never two different ones. Runner.launch is what
// derives that single `provider` argument from Brief.Provider (see
// runner.go); this test exercises BuildJob directly with that already-
// resolved value, mirroring how launch calls it.
func TestBuildJob_RoleBrainProviderModelEnv(t *testing.T) {
	cfg := JobConfig{Image: "img", Namespace: "nexus", BrokerHost: "h"}
	cfg.FrontierAuthFunc = func() (string, string) {
		return DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey
	}

	t.Run("role brain sets CW_PROVIDER/CW_MODEL and the matching auth mounts", func(t *testing.T) {
		b := Brief{Agent: "keel-builder-complex", Ticket: "wi-1", Provider: "claude-code", Model: "claude-sonnet-4-6"}
		job := BuildJob(b, cfg, "t1", b.Provider)
		c := job.Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_PROVIDER", "claude-code") {
			t.Errorf("missing CW_PROVIDER=claude-code: %v", c.Env)
		}
		if !envValueEquals(c.Env, "CW_MODEL", "claude-sonnet-4-6") {
			t.Errorf("missing CW_MODEL=claude-sonnet-4-6: %v", c.Env)
		}
		if !envSecretKeyRefEquals(c.Env, "CLAUDE_CODE_OAUTH_TOKEN", DefaultFrontierAuthSecretName, DefaultFrontierAuthSecretKey) {
			t.Errorf("effective provider claude-code should get CLAUDE_CODE_OAUTH_TOKEN: %v", c.Env)
		}
	})

	t.Run("no role brain — no CW_PROVIDER/CW_MODEL env at all (not even empty)", func(t *testing.T) {
		job := BuildJob(Brief{Agent: "anvil-builder", Ticket: "wi-1"}, cfg, "t1", "codex-cli")
		c := job.Spec.Template.Spec.Containers[0]
		for _, name := range []string{"CW_PROVIDER", "CW_MODEL"} {
			for _, v := range c.Env {
				if v.Name == name {
					t.Errorf("%s should be absent when Brief carries no role brain: %v", name, c.Env)
				}
			}
		}
	})

	t.Run("provider set without model — only CW_PROVIDER injected", func(t *testing.T) {
		b := Brief{Agent: "anvil-builder", Ticket: "wi-1", Provider: "codex-cli"}
		job := BuildJob(b, cfg, "t1", b.Provider)
		c := job.Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_PROVIDER", "codex-cli") {
			t.Errorf("missing CW_PROVIDER=codex-cli: %v", c.Env)
		}
		for _, v := range c.Env {
			if v.Name == "CW_MODEL" {
				t.Errorf("CW_MODEL should be absent when Brief.Model is empty: %v", c.Env)
			}
		}
	})
}

// TestBuildJob_RoleBrainEffortEnv covers the reasoning-EFFORT knob
// (2026-07-06) CW_EFFORT injection: a Brief carrying a role-brain effort
// override (Brief.Effort, threaded from dispatch.PoolItem.Effort — see
// pool.go) must surface as CW_EFFORT env for agentfunnel to map onto the
// claude-api provider's extended-thinking budget. Mirrors
// TestBuildJob_RoleBrainProviderModelEnv's Provider/Model coverage.
func TestBuildJob_RoleBrainEffortEnv(t *testing.T) {
	cfg := JobConfig{Image: "img", Namespace: "nexus", BrokerHost: "h"}

	t.Run("role brain sets CW_EFFORT", func(t *testing.T) {
		b := Brief{Agent: "keel-builder-complex", Ticket: "wi-1", Provider: "claude-api", Effort: "high"}
		job := BuildJob(b, cfg, "t1", b.Provider)
		c := job.Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_EFFORT", "high") {
			t.Errorf("missing CW_EFFORT=high: %v", c.Env)
		}
	})

	t.Run("no role brain — no CW_EFFORT env at all (not even empty)", func(t *testing.T) {
		job := BuildJob(Brief{Agent: "anvil-builder", Ticket: "wi-1"}, cfg, "t1", "codex-cli")
		c := job.Spec.Template.Spec.Containers[0]
		for _, v := range c.Env {
			if v.Name == "CW_EFFORT" {
				t.Errorf("CW_EFFORT should be absent when Brief carries no role brain effort: %v", c.Env)
			}
		}
	})
}

// TestBuildJob_ForwardsAcceptanceGateEnv verifies the ACCEPTANCE-GATE-HARDENING
// knobs are forwarded from the broker's env into the worker Job only when set
// (the gate runs in the worker; a Job does not inherit the broker pod env).
func TestBuildJob_ForwardsAcceptanceGateEnv(t *testing.T) {
	cfg := JobConfig{Image: "img", Namespace: "nexus", BriefTimeout: "30m"}
	b := Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1"}

	t.Run("set → forwarded", func(t *testing.T) {
		t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "1")
		t.Setenv("ACCEPTANCE_MIN_DIFF_LINES", "5")
		c := BuildJob(b, cfg, "t1", "claude-code").Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "ACCEPTANCE_REQUIRE_TEST_DIFF", "1") {
			t.Errorf("ACCEPTANCE_REQUIRE_TEST_DIFF not forwarded: %v", c.Env)
		}
		if !envValueEquals(c.Env, "ACCEPTANCE_MIN_DIFF_LINES", "5") {
			t.Errorf("ACCEPTANCE_MIN_DIFF_LINES not forwarded: %v", c.Env)
		}
	})

	t.Run("unset → absent (worker uses code defaults)", func(t *testing.T) {
		t.Setenv("ACCEPTANCE_REQUIRE_TEST_DIFF", "")
		t.Setenv("ACCEPTANCE_JUDGE_DIFF", "")
		t.Setenv("ACCEPTANCE_MIN_DIFF_LINES", "")
		c := BuildJob(b, cfg, "t2", "claude-code").Spec.Template.Spec.Containers[0]
		for _, k := range []string{"ACCEPTANCE_REQUIRE_TEST_DIFF", "ACCEPTANCE_JUDGE_DIFF", "ACCEPTANCE_MIN_DIFF_LINES"} {
			for _, e := range c.Env {
				if e.Name == k {
					t.Errorf("%s should be absent when unset, got %q", k, e.Value)
				}
			}
		}
	})
}

// TestBuildJob_ForwardsCvcsEnv verifies the CW_VCS broker-env passthrough:
// when the broker deployment sets CW_VCS, BuildJob injects it into the
// worker Job env (so agentfunnel can select the cairn clone path); when
// CW_VCS is unset in the broker env it must be absent from the Job spec.
// Rides the same os.Getenv seam as the ACCEPTANCE_* knobs.
func TestBuildJob_ForwardsCvcsEnv(t *testing.T) {
	cfg := JobConfig{Image: "img", Namespace: "nexus", BriefTimeout: "30m"}
	b := Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1"}

	t.Run("set → forwarded with broker value", func(t *testing.T) {
		t.Setenv("CW_VCS", "cairn")
		c := BuildJob(b, cfg, "t1", "codex-cli").Spec.Template.Spec.Containers[0]
		if !envValueEquals(c.Env, "CW_VCS", "cairn") {
			t.Errorf("CW_VCS not forwarded with broker value 'cairn': %v", c.Env)
		}
	})

	t.Run("unset → absent (agentfunnel uses git default)", func(t *testing.T) {
		t.Setenv("CW_VCS", "")
		c := BuildJob(b, cfg, "t2", "codex-cli").Spec.Template.Spec.Containers[0]
		for _, e := range c.Env {
			if e.Name == "CW_VCS" {
				t.Errorf("CW_VCS should be absent when unset in broker env, got %q", e.Value)
			}
		}
	})
}

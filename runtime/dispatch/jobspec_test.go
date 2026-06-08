package dispatch

import (
	"regexp"
	"strings"
	"testing"

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
}

func TestBuildJob_ProviderCodexBits(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		wantCodex bool
	}{
		{name: "empty is codex-cli", wantCodex: true},
		{name: "codex-cli", provider: "codex-cli", wantCodex: true},
		{name: "openai", provider: "openai", wantCodex: false},
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
	c := job.Spec.Template.Spec.Containers[0]
	if !volumeMountExists(c.VolumeMounts, "home-repo") || !volumeMountExists(c.VolumeMounts, "home-work") {
		t.Fatalf("home mounts missing: %v", c.VolumeMounts)
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

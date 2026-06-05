package dispatch

import (
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

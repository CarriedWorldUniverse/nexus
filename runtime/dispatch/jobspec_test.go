package dispatch

import (
	"strings"
	"testing"
)

func TestBuildJob(t *testing.T) {
	cfg := JobConfig{
		Image: "localhost/nexus-builder:dev", Namespace: "nexus",
		NodeIP: "192.168.143.133", BrokerHost: "dmonextreme.tail41686e.ts.net",
		BriefTimeout: "30m",
	}
	b := Brief{Agent: "anvil", Ticket: "NEX-999", Thread: "NEX-999"}
	job := BuildJob(b, cfg, "abc123")

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

package docregister

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// newTestGitRepo returns a GitCairnContent backed by a freshly `git init`'d
// temp directory — no network, no sovereign cairn required. Real git
// commits, real SHAs, real `git show` reads: this exercises the actual
// mechanics GitCairnContent uses against a live cairn line, just pointed at
// a local repo instead of a cloned one. See README.md's live-verify path for
// exercising this against an actual cairn-hosted line.
func newTestGitRepo(t *testing.T) *GitCairnContent {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "docregister-test@example.invalid")
	run("config", "user.name", "docregister-test")
	return &GitCairnContent{RepoDir: dir}
}

func TestGitCairnContent_CommitAndFetch(t *testing.T) {
	g := newTestGitRepo(t)
	ctx := context.Background()

	ref1, err := g.Commit(ctx, "doc-1", KindSpec, "# v1")
	if err != nil {
		t.Fatalf("Commit v1: %v", err)
	}
	if !strings.Contains(ref1, "docs/spec/doc-1.md@") {
		t.Fatalf("ref1 = %q, want docs/spec/doc-1.md@<sha>", ref1)
	}

	got, err := g.Fetch(ctx, ref1)
	if err != nil {
		t.Fatalf("Fetch v1: %v", err)
	}
	if got != "# v1" {
		t.Fatalf("Fetch v1 = %q, want %q", got, "# v1")
	}

	ref2, err := g.Commit(ctx, "doc-1", KindSpec, "# v2")
	if err != nil {
		t.Fatalf("Commit v2: %v", err)
	}
	if ref2 == ref1 {
		t.Fatal("ref2 == ref1 — commit should produce a new sha for changed content")
	}

	// Old version is still fetchable — versioned + diffable.
	got1, err := g.Fetch(ctx, ref1)
	if err != nil {
		t.Fatalf("Fetch old ref after new commit: %v", err)
	}
	if got1 != "# v1" {
		t.Fatalf("old ref content = %q, want %q", got1, "# v1")
	}

	got2, err := g.Fetch(ctx, ref2)
	if err != nil {
		t.Fatalf("Fetch v2: %v", err)
	}
	if got2 != "# v2" {
		t.Fatalf("Fetch v2 = %q, want %q", got2, "# v2")
	}
}

func TestGitCairnContent_MalformedRef(t *testing.T) {
	g := newTestGitRepo(t)
	if _, err := g.Fetch(context.Background(), "not-a-ref-no-at-sign"); err == nil {
		t.Fatal("expected error for malformed cairn_ref")
	}
}

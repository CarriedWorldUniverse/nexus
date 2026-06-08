package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuilderHomeLifecycleInitializesWorktreeAndMergesCleanDespawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The dispatched builder runs only in a Linux k3s pod; `git worktree
		// remove` hits Windows file-locking ("being used by another process").
		t.Skip("builder home lifecycle is Linux-only")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	root := t.TempDir()
	repo := filepath.Join(root, "home.git")
	work := filepath.Join(root, "work")
	t.Setenv("CW_AGENT_HOME_REPO", repo)
	t.Setenv("CW_AGENT_HOME_WORKDIR", work)
	t.Setenv("CW_DISPATCH_RUN_ID", "run-test-123")
	t.Setenv("GIT_AUTHOR_NAME", "anvil")
	t.Setenv("GIT_AUTHOR_EMAIL", "anvil@agents.carriedworld.com")
	t.Setenv("GIT_COMMITTER_NAME", "anvil")
	t.Setenv("GIT_COMMITTER_EMAIL", "anvil@agents.carriedworld.com")

	ctx := context.Background()
	session, err := setupBuilderHome(ctx, "anvil", os.Getenv("CW_DISPATCH_RUN_ID"))
	if err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HOME"); got != session.worktree {
		t.Fatalf("HOME = %q, want %q", got, session.worktree)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// os.Getwd resolves symlinks (macOS /var -> /private/var), so compare
	// the resolved forms rather than the literal worktree path.
	gotCwd, _ := filepath.EvalSymlinks(cwd)
	wantWT, _ := filepath.EvalSymlinks(session.worktree)
	if gotCwd != wantWT {
		t.Fatalf("cwd = %q, want %q", cwd, session.worktree)
	}
	if err := os.WriteFile(filepath.Join(session.worktree, "memory.md"), []byte("remember this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(session.worktree, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.worktree, ".cache", "ignored"), []byte("cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := session.cleanDespawn(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed, stat err = %v", err)
	}
	out, err := runGitCommand(ctx, "", "--git-dir", repo, "show", "main:memory.md")
	if err != nil {
		t.Fatalf("memory.md not merged to main: %v: %s", err, out)
	}
	if string(out) != "remember this\n" {
		t.Fatalf("memory.md = %q", out)
	}
	if _, err := runGitCommand(ctx, "", "--git-dir", repo, "show", "main:.cache/ignored"); err == nil {
		t.Fatal("ignored cache file should not be committed")
	}
}

func TestHomeRunBranchSanitizesRunID(t *testing.T) {
	if got := homeRunBranch("Run/ABC_123"); got != "run-abc-123" {
		t.Fatalf("homeRunBranch sanitized to %q", got)
	}
	if got := homeRunBranch(""); got != "run" {
		t.Fatalf("empty run branch = %q, want run", got)
	}
}

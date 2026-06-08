package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuilderRepoLifecycleUsesSharedMirrorAndRunWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The dispatched builder runs only in a Linux k3s pod; worktree cleanup
		// has platform-specific file-locking behavior on Windows.
		t.Skip("builder repo lifecycle is Linux-only")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	shared := filepath.Join(root, "src")
	t.Setenv("CW_SHARED_REPOS_DIR", shared)
	t.Setenv("HOME", filepath.Join(root, "home"))

	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) {
		t.Helper()
		out, err := runGitCommand(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	git(source, "init")
	git(source, "checkout", "-b", "main")
	git(source, "config", "user.name", "test")
	git(source, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(source, "add", "README.md")
	git(source, "commit", "-m", "initial")
	git("", "clone", "--bare", source, remote)

	session, err := setupBuilderRepo(ctx, "anvil", "run-test-123", "file://"+remote, "builder/NEX-1")
	if err != nil {
		t.Fatal(err)
	}
	if session.mirror != filepath.Join(shared, "remote", ".git") {
		t.Fatalf("mirror = %q, want %q", session.mirror, filepath.Join(shared, "remote", ".git"))
	}
	if session.worktree != filepath.Join(shared, "remote", "anvil-run-test-123") {
		t.Fatalf("worktree = %q", session.worktree)
	}
	out, err := runGitCommand(ctx, session.worktree, "branch", "--show-current")
	if err != nil {
		t.Fatalf("branch --show-current: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "builder/NEX-1" {
		t.Fatalf("branch = %q, want builder/NEX-1", strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(session.worktree, "README.md")); err != nil {
		t.Fatalf("worktree missing README.md: %v", err)
	}
	if out, err := runGitCommand(ctx, "", "--git-dir", session.mirror, "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("mirror is not bare: out=%q err=%v", out, err)
	}

	if err := session.cleanDespawn(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(session.mirror); err != nil {
		t.Fatalf("mirror should remain: %v", err)
	}
}

func TestBuilderRepoNaming(t *testing.T) {
	if got := repoRemoteURL("CarriedWorldUniverse/nexus"); got != "https://github.com/CarriedWorldUniverse/nexus.git" {
		t.Fatalf("repoRemoteURL owner/name = %q", got)
	}
	if got := repoDirName("https://github.com/CarriedWorldUniverse/nexus.git"); got != "nexus" {
		t.Fatalf("repoDirName = %q, want nexus", got)
	}
	if got := repoWorktreeName("Anvil.Agent", "run-ABC_123"); got != "anvil-agent-run-abc-123" {
		t.Fatalf("repoWorktreeName = %q", got)
	}
	if got := builderBranch("", "NEX-1"); got != "builder/NEX-1" {
		t.Fatalf("builderBranch default = %q", got)
	}
}

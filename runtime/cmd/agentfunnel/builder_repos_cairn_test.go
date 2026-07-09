package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCairnFolderName(t *testing.T) {
	cases := map[string]string{
		"builder/NEX-1":      "builder-NEX-1",
		"main":               "main",
		"feat/a/b":           "feat-a-b",
		"anvil/workers-json": "anvil-workers-json",
	}
	for in, want := range cases {
		if got := cairnFolderName(in); got != want {
			t.Errorf("cairnFolderName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuilderVCS(t *testing.T) {
	t.Setenv("CW_VCS", "cairn")
	if got := builderVCS(); got != vcsCairn {
		t.Errorf("CW_VCS=cairn: got %q", got)
	}
	t.Setenv("CW_VCS", "")
	if got := builderVCS(); got != vcsCairn {
		t.Errorf("CW_VCS unset: got %q, want cairn (the default since 2026-07-09)", got)
	}
	t.Setenv("CW_VCS", "git")
	if got := builderVCS(); got != "git" {
		t.Errorf("CW_VCS=git (the opt-out): got %q", got)
	}
	t.Setenv("CW_VCS", "GIT")
	if got := builderVCS(); got != "git" {
		t.Errorf("CW_VCS=GIT (case-insensitive opt-out): got %q", got)
	}
}

func TestCredentialHost(t *testing.T) {
	cases := map[string]string{
		"https://github.com/CarriedWorldUniverse/nexus.git": "github.com",
		"http://gitea.local:3000/o/r.git":                   "gitea.local:3000",
		"CarriedWorldUniverse/nexus":                        "github.com", // non-URL → default
		"git@github.com:o/r.git":                            "github.com", // ssh → default
	}
	for in, want := range cases {
		if got := credentialHost(in); got != want {
			t.Errorf("credentialHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseGitCredentialPassword(t *testing.T) {
	// A deliberately non-token-shaped fixture (no real credential prefix) so
	// secret scanners don't flag the test; we only assert the parse.
	const fake = "test-credential-value-not-a-token"
	out := "protocol=https\nhost=github.com\nusername=x-access-token\npassword=" + fake + "\n"
	if got := parseGitCredentialPassword(out); got != fake {
		t.Errorf("parseGitCredentialPassword = %q", got)
	}
	if got := parseGitCredentialPassword("protocol=https\nhost=github.com\n"); got != "" {
		t.Errorf("no password line should yield empty, got %q", got)
	}
}

func TestCairnLineHead(t *testing.T) {
	orig := runCairnCommand
	defer func() { runCairnCommand = orig }()
	runCairnCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "status" {
			return []byte("branch:    builder/NEX-1\nlineage:   main → builder/NEX-1\nahead:     3\nconflicts: \nexpressed: builder/NEX-1, main\n"), nil
		}
		return nil, nil
	}
	got, err := cairnLineHead(context.Background(), "/clone", "builder/NEX-1")
	if err != nil {
		t.Fatalf("cairnLineHead: %v", err)
	}
	if got != "3" {
		t.Errorf("cairnLineHead = %q, want \"3\" (the ahead count)", got)
	}
}

func TestWithBranchInstructionCairn(t *testing.T) {
	t.Setenv("CW_VCS", "cairn")
	got := withBranchInstruction("Do NEX-9.", "org/repo", "builder/NEX-9")
	for _, want := range []string{"cairn commit builder/NEX-9", "cairn push origin builder/NEX-9", "Do NOT use git", "exit 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("cairn instruction missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "git push") {
		t.Errorf("cairn instruction should not tell the agent to git push:\n%s", got)
	}
}

func TestWithBranchInstructionGitDefault(t *testing.T) {
	t.Setenv("CW_VCS", "git")
	got := withBranchInstruction("Do NEX-9.", "org/repo", "builder/NEX-9")
	if strings.Contains(got, "cairn") {
		t.Errorf("git-mode instruction should not mention cairn:\n%s", got)
	}
	if !strings.Contains(got, "commit and push") {
		t.Errorf("git-mode instruction missing the git directive:\n%s", got)
	}
}

// TestSpawnCairnCommandSequence verifies the cairn clone-per-run provisioning
// issues the right cairn commands in order and points s.worktree at the
// expressed line folder, using mocked git+cairn subprocesses.
func TestSpawnCairnCommandSequence(t *testing.T) {
	root := t.TempDir()
	mirror := filepath.Join(root, "nexus", ".git")
	cloneDir := filepath.Join(root, "nexus", "anvil-run1")

	origGit, origCairn := runGitCommand, runCairnCommand
	defer func() { runGitCommand, runCairnCommand = origGit, origCairn }()

	runGitCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case argsHave(args, "--is-bare-repository"):
			return []byte("true\n"), nil
		case argsHave(args, "symbolic-ref"):
			return []byte("main\n"), nil
		}
		return nil, nil // fetch etc.
	}
	var cairnCalls [][]string
	runCairnCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		cairnCalls = append(cairnCalls, args)
		if len(args) >= 1 && args[0] == "express" {
			// materialize the expressed folder so the post-express stat passes
			if err := os.MkdirAll(filepath.Join(cloneDir, "builder-NEX-1"), 0o755); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}

	s := &builderRepoSession{
		vcs:      vcsCairn,
		repoName: "nexus",
		remote:   "https://github.com/CarriedWorldUniverse/nexus.git",
		mirror:   mirror,
		cloneDir: cloneDir,
		branch:   "builder/NEX-1",
	}
	if err := s.spawnCairn(context.Background()); err != nil {
		t.Fatalf("spawnCairn: %v", err)
	}

	wantWorktree := filepath.Join(cloneDir, "builder-NEX-1")
	if s.worktree != wantWorktree {
		t.Errorf("worktree = %q, want %q", s.worktree, wantWorktree)
	}
	// Assert the key command shapes appear in order: clone, config x2, remote add origin (github), express --from main.
	joined := make([]string, len(cairnCalls))
	for i, c := range cairnCalls {
		joined[i] = strings.Join(c, " ")
	}
	all := strings.Join(joined, " | ")
	for _, want := range []string{
		"clone " + mirror + " " + cloneDir,
		"config user.name nexus-cw",
		"config user.email nexus@darksoft.co.nz",
		"remote add origin https://github.com/CarriedWorldUniverse/nexus.git",
		"express builder/NEX-1 --from main",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing cairn call %q in sequence:\n%s", want, all)
		}
	}
	// clone must precede express.
	if idxOf(joined, "clone") >= idxOf(joined, "express") {
		t.Errorf("clone must precede express; got order:\n%s", all)
	}
}

func TestCleanDespawnCairnRemovesClone(t *testing.T) {
	root := t.TempDir()
	cloneDir := filepath.Join(root, "clone")
	if err := os.MkdirAll(filepath.Join(cloneDir, "builder-NEX-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &builderRepoSession{vcs: vcsCairn, cloneDir: cloneDir, worktree: filepath.Join(cloneDir, "builder-NEX-1")}
	if err := s.cleanDespawn(context.Background()); err != nil {
		t.Fatalf("cleanDespawn: %v", err)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("clone dir should be removed, stat err = %v", err)
	}
}

func argsHave(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func idxOf(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

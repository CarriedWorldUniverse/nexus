package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Builder VCS: cairn clone-per-run (docs/network/BUILDER-CAIRN-MIGRATION.md).
//
// cairn is the sovereign git replacement. A pool runs CONCURRENT builder
// processes; cairn's working-copy state is per-clone, so the isolation model is
// clone-per-run (each dispatch its own clone), mirroring git's worktree
// isolation. Shared-clone concurrency is now safe too (cairn #81/#85/#88), but
// clone-per-run stays the model — isolation is simpler and needs no lock.
//
// The flow (validated end-to-end): keep the fast local bare git MIRROR on the
// shared-repos PVC (same as the git path) as the clone source, `cairn clone` it
// per run (local object copy, no network), re-point the clone's `origin` from
// the mirror to the real GitHub URL so pushes land on GitHub, `cairn express`
// the run's line, let the agent work + `cairn commit && cairn push`, then
// rm -rf the whole per-run clone on despawn.
//
// This path is gated behind CW_VCS=cairn (default git) so it is dark until
// proven on a live pool ticket.

const vcsCairn = "cairn"

// runCairnCommand runs a cairn subprocess and returns combined output. Overridable
// in tests. dir="" runs from "/" so a stray cwd never leaks into the command.
var runCairnCommand = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "cairn", args...)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = "/"
	}
	return cmd.CombinedOutput()
}

// cairnFolderName is cairn's on-disk folder for a line: slashes flatten to
// dashes so a path-like branch ("builder/NEX-1") never nests ("builder-NEX-1").
// Mirrors internal/worktree.FolderName in the cairn CLI.
func cairnFolderName(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

// spawnCairn provisions a per-run cairn clone for the dispatch and points
// s.worktree at the expressed line's folder (where the agent works). It reuses
// the git mirror kept fresh by the shared-repos PVC as the local clone source,
// then re-points origin to GitHub so the agent's `cairn push` lands there.
func (s *builderRepoSession) spawnCairn(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.mirror), 0o755); err != nil {
		return fmt.Errorf("create builder repo cache dir: %w", err)
	}
	// Fresh local mirror with refs/heads/* current, so `cairn clone` sees the
	// latest default branch (the git path fetches into refs/remotes/origin/*,
	// which cairn clone does not read — hence a cairn-specific refresh).
	if err := s.ensureCairnMirror(ctx); err != nil {
		return err
	}
	// A stale per-run clone from a re-used run dir must go before we re-clone.
	if err := os.RemoveAll(s.cloneDir); err != nil {
		return fmt.Errorf("clear old builder cairn clone: %w", err)
	}
	if err := s.cairnWithRetry(ctx, "", "clone", s.mirror, s.cloneDir); err != nil {
		return err
	}
	// Identity — else commits are stamped a "@users.noreply.cairn" placeholder.
	if err := s.cairnWithRetry(ctx, s.cloneDir, "config", "user.name", cairnAuthorName()); err != nil {
		return err
	}
	if err := s.cairnWithRetry(ctx, s.cloneDir, "config", "user.email", cairnAuthorEmail()); err != nil {
		return err
	}
	// Re-point origin from the local mirror to the real remote so `cairn push
	// origin <branch>` publishes to GitHub, not the mirror. `remote add` on an
	// existing name re-points it.
	if err := s.cairnWithRetry(ctx, s.cloneDir, "remote", "add", "origin", s.remote); err != nil {
		return err
	}
	// The run's working line, forked from the repo's default branch.
	base, err := s.cairnBaseBranch(ctx)
	if err != nil {
		return err
	}
	if err := s.cairnWithRetry(ctx, s.cloneDir, "express", s.branch, "--from", base); err != nil {
		return err
	}
	s.worktree = filepath.Join(s.cloneDir, cairnFolderName(s.branch))
	if _, err := os.Stat(s.worktree); err != nil {
		return fmt.Errorf("builder cairn: expressed folder %q missing: %w", s.worktree, err)
	}
	return nil
}

// ensureCairnMirror keeps the local bare mirror current with refs/heads/* (and
// tags) matching origin, so a subsequent `cairn clone` imports the latest
// branches. Uses the mirror's own +refs/heads/*:refs/heads/* refspec rather than
// the git path's remotes/origin mapping.
func (s *builderRepoSession) ensureCairnMirror(ctx context.Context) error {
	if out, err := runGitCommand(ctx, "", "--git-dir", s.mirror, "rev-parse", "--is-bare-repository"); err == nil && strings.TrimSpace(string(out)) == "true" {
		return s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "fetch", "--prune", "origin",
			"+refs/heads/*:refs/heads/*",
			"+refs/tags/*:refs/tags/*")
	}
	if err := os.RemoveAll(s.mirror); err != nil {
		return fmt.Errorf("clear incomplete builder repo mirror: %w", err)
	}
	if err := s.gitWithRetry(ctx, "", "clone", "--mirror", s.remote, s.mirror); err != nil {
		return err
	}
	return s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "fetch", "--prune", "origin",
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*")
}

// cairnBaseBranch is the default branch of the mirror to fork the run's line
// from. Prefers the mirror's HEAD symbolic ref, then common defaults.
func (s *builderRepoSession) cairnBaseBranch(ctx context.Context) (string, error) {
	if out, err := runGitCommand(ctx, "", "--git-dir", s.mirror, "symbolic-ref", "--short", "HEAD"); err == nil {
		if b := strings.TrimSpace(string(out)); b != "" {
			return b, nil
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := runGitCommand(ctx, "", "--git-dir", s.mirror, "rev-parse", "--verify", "refs/heads/"+candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("builder cairn: no usable default branch in %s", s.mirror)
}

// cleanDespawnCairn disposes the whole per-run clone (the isolation unit).
func (s *builderRepoSession) cleanDespawnCairn(_ context.Context) error {
	if cwd, err := os.Getwd(); err == nil && isWithinPath(cwd, s.cloneDir) {
		if home := os.Getenv("HOME"); home != "" {
			_ = os.Chdir(home)
		} else {
			_ = os.Chdir("/")
		}
	}
	if err := os.RemoveAll(s.cloneDir); err != nil {
		return fmt.Errorf("remove builder cairn clone: %w", err)
	}
	return nil
}

// cairnWithRetry runs a cairn command with the same transient-error retry posture
// as the git path (lock contention / transient FS races).
func (s *builderRepoSession) cairnWithRetry(ctx context.Context, dir string, args ...string) error {
	const attempts = 5
	var last error
	for i := 0; i < attempts; i++ {
		out, err := runCairnCommand(ctx, dir, args...)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("cairn %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		if !gitLockContention(string(out), err) || i == attempts-1 {
			return last
		}
		if !sleepBackoff(ctx, i) {
			return ctx.Err()
		}
	}
	return last
}

// headFn returns the per-VCS commit-progress probe for the watcher: git
// rev-parse HEAD in the worktree (git mode) or the cairn line's sealed-commit
// count (cairn mode), which increments on each `cairn commit`.
func (s *builderRepoSession) headFn() func(context.Context) (string, error) {
	if s.vcs == vcsCairn {
		cloneDir, branch := s.cloneDir, s.branch
		return func(ctx context.Context) (string, error) { return cairnLineHead(ctx, cloneDir, branch) }
	}
	worktree := s.worktree
	return func(ctx context.Context) (string, error) { return gitHead(ctx, worktree) }
}

// cairnLineHead returns the line's sealed-commit count (`ahead:` from cairn
// status), which increments on every `cairn commit` — a monotonic progress
// signal that, unlike the working change's id, does not flap on unsealed edits.
func cairnLineHead(ctx context.Context, cloneDir, branch string) (string, error) {
	out, err := runCairnCommand(ctx, cloneDir, "status", branch)
	if err != nil {
		return "", fmt.Errorf("cairn status %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ahead:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("cairn status %s: no ahead field", branch)
}

func cairnAuthorName() string  { return envOrDefault("CW_CAIRN_AUTHOR_NAME", "nexus-cw") }
func cairnAuthorEmail() string { return envOrDefault("CW_CAIRN_AUTHOR_EMAIL", "nexus@darksoft.co.nz") }

// bridgeCairnGitHubAuth makes cairn's push authenticate to GitHub. cairn resolves
// CAIRN_TOKEN > GITHUB_TOKEN > GITLAB_TOKEN > credstore — it does NOT use git's
// credential helper (the `cw` bridge the git path relies on). So when either
// token is already set we leave it; otherwise we ask git's configured helper for
// the github.com credential (the same token the git path pushes with) and export
// it as GITHUB_TOKEN for the agent's `cairn push` subprocess to inherit.
//
// Best-effort: a failure is returned for the caller to log non-fatally — an
// already-working credstore or a token set another way can still authenticate.
func bridgeCairnGitHubAuth(ctx context.Context, remote string) (string, error) {
	if strings.TrimSpace(os.Getenv("CAIRN_TOKEN")) != "" || strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != "" {
		return "already set", nil
	}
	host := credentialHost(remote)
	in := fmt.Sprintf("protocol=https\nhost=%s\n\n", host)
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git credential fill (%s): %w: %s", host, err, strings.TrimSpace(string(out)))
	}
	token := parseGitCredentialPassword(string(out))
	if token == "" {
		return "", fmt.Errorf("git credential fill (%s): no password in output", host)
	}
	if err := os.Setenv("GITHUB_TOKEN", token); err != nil {
		return "", fmt.Errorf("set GITHUB_TOKEN: %w", err)
	}
	return "bridged from git credential", nil
}

// credentialHost is the host git credential should be asked for. Defaults to
// github.com (the builder's remote host) for the owner/repo and https forms.
func credentialHost(remote string) string {
	r := remote
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(r, p) {
			r = strings.TrimPrefix(r, p)
			if i := strings.IndexByte(r, '/'); i >= 0 {
				return r[:i]
			}
			return r
		}
	}
	return "github.com"
}

// parseGitCredentialPassword extracts the password= line from `git credential
// fill` key=value output.
func parseGitCredentialPassword(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "password="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

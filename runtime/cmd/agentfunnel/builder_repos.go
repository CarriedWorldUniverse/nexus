package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultSharedReposDir = "/src"
	defaultGitHubOwner    = "CarriedWorldUniverse"
)

type builderRepoSession struct {
	vcs      string // "git" (default) or "cairn"
	reposDir string
	repo     string
	repoName string
	remote   string
	mirror   string
	worktree string // where the agent works + chdir target (git: worktree; cairn: expressed line folder)
	cloneDir string // cairn only: the per-run clone root, disposed whole on despawn
	branch   string
}

func setupBuilderRepo(ctx context.Context, aspect, runID, repo, branch string) (*builderRepoSession, error) {
	if repo == "" {
		return nil, nil
	}
	reposDir := envOrDefault("CW_SHARED_REPOS_DIR", defaultSharedReposDir)
	repoName := repoDirName(repo)
	if repoName == "" {
		return nil, fmt.Errorf("builder repo: invalid repo %q", repo)
	}
	if branch == "" {
		return nil, fmt.Errorf("builder repo: branch not set for repo %q", repo)
	}
	s := &builderRepoSession{
		vcs:      builderVCS(),
		reposDir: reposDir,
		repo:     repo,
		repoName: repoName,
		remote:   repoRemoteURL(repo),
		mirror:   filepath.Join(reposDir, repoName, ".git"),
		worktree: filepath.Join(reposDir, repoName, repoWorktreeName(aspect, runID)),
		branch:   branch,
	}
	if s.vcs == vcsCairn {
		// Per-run clone lives at the same path the git worktree would; the
		// expressed line folder inside it becomes s.worktree (the chdir target).
		s.cloneDir = filepath.Join(reposDir, repoName, repoWorktreeName(aspect, runID))
		if err := s.spawnCairn(ctx); err != nil {
			return nil, err
		}
	} else if err := s.spawn(ctx); err != nil {
		return nil, err
	}
	if err := os.Chdir(s.worktree); err != nil {
		return nil, fmt.Errorf("chdir builder repo worktree: %w", err)
	}
	return s, nil
}

// builderVCS is the builder's VCS mode: CW_VCS=cairn opts into the cairn
// clone-per-run path; anything else (default) keeps the git mirror+worktree path.
func builderVCS() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CW_VCS")), vcsCairn) {
		return vcsCairn
	}
	return "git"
}

func (s *builderRepoSession) spawn(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.mirror), 0o755); err != nil {
		return fmt.Errorf("create builder repo cache dir: %w", err)
	}
	if err := s.ensureMirror(ctx); err != nil {
		return err
	}
	if err := s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "worktree", "prune"); err != nil {
		return err
	}
	if err := s.removeExistingWorktreePath(ctx); err != nil {
		return err
	}
	base, err := s.worktreeBase(ctx)
	if err != nil {
		return err
	}
	return s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "worktree", "add", "-B", s.branch, s.worktree, base)
}

func (s *builderRepoSession) ensureMirror(ctx context.Context) error {
	if out, err := runGitCommand(ctx, "", "--git-dir", s.mirror, "rev-parse", "--is-bare-repository"); err == nil && strings.TrimSpace(string(out)) == "true" {
		return s.fetch(ctx)
	}
	if err := os.RemoveAll(s.mirror); err != nil {
		return fmt.Errorf("clear incomplete builder repo mirror: %w", err)
	}
	if err := s.gitWithRetry(ctx, "", "clone", "--mirror", s.remote, s.mirror); err != nil {
		return err
	}
	return s.fetch(ctx)
}

func (s *builderRepoSession) fetch(ctx context.Context) error {
	return s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "fetch", "--prune", "origin",
		"+refs/heads/*:refs/remotes/origin/*",
		"+refs/tags/*:refs/tags/*")
}

func (s *builderRepoSession) removeExistingWorktreePath(ctx context.Context) error {
	if _, err := os.Stat(s.worktree); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat old builder repo worktree: %w", err)
	}
	if err := s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "worktree", "remove", "--force", s.worktree); err == nil {
		return nil
	}
	if err := os.RemoveAll(s.worktree); err != nil {
		return fmt.Errorf("remove old builder repo worktree path: %w", err)
	}
	return nil
}

func (s *builderRepoSession) worktreeBase(ctx context.Context) (string, error) {
	remoteBranch := "refs/remotes/origin/" + s.branch
	if _, err := runGitCommand(ctx, "", "--git-dir", s.mirror, "rev-parse", "--verify", remoteBranch); err == nil {
		return remoteBranch, nil
	}
	for _, candidate := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master", "HEAD"} {
		if _, err := runGitCommand(ctx, "", "--git-dir", s.mirror, "rev-parse", "--verify", candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("builder repo: no usable base ref in %s", s.mirror)
}

func (s *builderRepoSession) cleanDespawn(ctx context.Context) error {
	if s.vcs == vcsCairn {
		return s.cleanDespawnCairn(ctx)
	}
	if cwd, err := os.Getwd(); err == nil && isWithinPath(cwd, s.worktree) {
		if home := os.Getenv("HOME"); home != "" {
			_ = os.Chdir(home)
		} else {
			_ = os.Chdir("/")
		}
	}
	if _, err := os.Stat(s.worktree); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat builder repo worktree: %w", err)
	}
	return s.gitWithRetry(ctx, "", "--git-dir", s.mirror, "worktree", "remove", "--force", s.worktree)
}

func (s *builderRepoSession) gitWithRetry(ctx context.Context, dir string, args ...string) error {
	const attempts = 5
	var last error
	for i := 0; i < attempts; i++ {
		out, err := runGitCommand(ctx, dir, args...)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		if !gitLockContention(string(out), err) || i == attempts-1 {
			return last
		}
		if !sleepBackoff(ctx, i) {
			return ctx.Err()
		}
	}
	return last
}

// sleepBackoff waits an increasing (attempt+1)*200ms before a retry, returning
// false if ctx is cancelled first. Shared by the git and cairn retry wrappers.
func sleepBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		return true
	}
}

func gitLockContention(out string, err error) bool {
	msg := strings.ToLower(out + " " + err.Error())
	return strings.Contains(msg, "lock") ||
		strings.Contains(msg, "another git process") ||
		strings.Contains(msg, "unable to create") ||
		strings.Contains(msg, "could not create")
}

func builderBranch(branch, ticket string) string {
	if branch != "" {
		return branch
	}
	if ticket == "" {
		return ""
	}
	return "builder/" + ticket
}

func repoRemoteURL(repo string) string {
	// Strip trailing slash so owner/repo shorthand normalises cleanly.
	repo = strings.TrimSuffix(repo, "/")
	if filepath.IsAbs(repo) || strings.HasPrefix(repo, "file://") || strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	if strings.Count(repo, "/") == 0 {
		repo = defaultGitHubOwner + "/" + repo
	}
	if strings.HasSuffix(repo, ".git") {
		return "https://github.com/" + repo
	}
	return "https://github.com/" + repo + ".git"
}

func repoDirName(repo string) string {
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.TrimRight(repo, "/")
	if repo == "" {
		return ""
	}
	if strings.HasPrefix(repo, "git@") {
		if i := strings.LastIndex(repo, ":"); i >= 0 {
			repo = repo[i+1:]
		}
	}
	name := path.Base(repo)
	return dnsPathPart(name)
}

func repoWorktreeName(aspect, runID string) string {
	aspect = dnsPathPart(aspect)
	runID = dnsPathPart(runID)
	if aspect == "" {
		aspect = "agent"
	}
	if runID == "" {
		runID = "run"
	}
	return aspect + "-" + runID
}

func dnsPathPart(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func isWithinPath(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

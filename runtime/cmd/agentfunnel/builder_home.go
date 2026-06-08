package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultAgentHomeRepo    = "/var/lib/nexus/home.git"
	defaultAgentHomeWorkDir = "/agent-home"
)

var runGitCommand = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = "/"
	}
	return cmd.CombinedOutput()
}

type agentHomeSession struct {
	repo     string
	workDir  string
	worktree string
	mergeDir string
	branch   string
}

func setupBuilderHome(ctx context.Context, aspect, runID string) (*agentHomeSession, error) {
	repo := envOrDefault("CW_AGENT_HOME_REPO", defaultAgentHomeRepo)
	workDir := envOrDefault("CW_AGENT_HOME_WORKDIR", defaultAgentHomeWorkDir)
	branch := homeRunBranch(runID)
	s := &agentHomeSession{
		repo:     repo,
		workDir:  workDir,
		worktree: filepath.Join(workDir, "home"),
		mergeDir: filepath.Join(workDir, "merge-"+branch),
		branch:   branch,
	}
	if err := s.initBareRepo(ctx, aspect); err != nil {
		return nil, err
	}
	if err := s.spawn(ctx); err != nil {
		return nil, err
	}
	if err := os.Setenv("HOME", s.worktree); err != nil {
		return nil, fmt.Errorf("set HOME: %w", err)
	}
	if err := os.Chdir(s.worktree); err != nil {
		return nil, fmt.Errorf("chdir HOME: %w", err)
	}
	return s, nil
}

func (s *agentHomeSession) initBareRepo(ctx context.Context, aspect string) error {
	if out, err := runGitCommand(ctx, "", "--git-dir", s.repo, "rev-parse", "--is-bare-repository"); err == nil && strings.TrimSpace(string(out)) == "true" {
		if _, err := runGitCommand(ctx, "", "--git-dir", s.repo, "rev-parse", "--verify", "refs/heads/main"); err == nil {
			return nil
		}
	}
	if err := os.RemoveAll(filepath.Join(s.repo, "worktrees")); err != nil {
		return fmt.Errorf("clear incomplete home repo worktree state: %w", err)
	}
	if err := os.Remove(filepath.Join(s.repo, "index")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear incomplete home repo index: %w", err)
	}
	if err := os.Remove(filepath.Join(s.repo, "config.lock")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear incomplete home repo config lock: %w", err)
	}
	if err := os.Remove(filepath.Join(s.repo, "HEAD.lock")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear incomplete home repo HEAD lock: %w", err)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "HEAD")); err == nil {
		if err := s.git(ctx, "", "--git-dir", s.repo, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat home repo HEAD: %w", err)
	}
	if _, err := runGitCommand(ctx, "", "--git-dir", s.repo, "rev-parse", "--verify", "refs/heads/main"); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.repo, 0o755); err != nil {
		return fmt.Errorf("create home repo dir: %w", err)
	}
	if err := s.git(ctx, "", "init", "--bare", s.repo); err != nil {
		return err
	}
	if err := s.git(ctx, "", "--git-dir", s.repo, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "agent-home-init-*")
	if err != nil {
		return fmt.Errorf("create temp home init worktree: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"), []byte(agentHomeGitignore), 0o644); err != nil {
		return fmt.Errorf("write home .gitignore: %w", err)
	}
	if err := s.git(ctx, tmp, "init"); err != nil {
		return err
	}
	if err := s.git(ctx, tmp, "checkout", "-b", "main"); err != nil {
		return err
	}
	if err := s.git(ctx, tmp, "add", ".gitignore"); err != nil {
		return err
	}
	if err := s.git(ctx, tmp, "-c", "user.name="+aspect, "-c", "user.email="+aspect+"@agents.carriedworld.com", "commit", "-m", "Initialize agent home"); err != nil {
		return err
	}
	if err := s.git(ctx, tmp, "remote", "add", "home", s.repo); err != nil {
		return err
	}
	return s.git(ctx, tmp, "push", "home", "main")
}

func (s *agentHomeSession) spawn(ctx context.Context) error {
	if err := os.RemoveAll(s.worktree); err != nil {
		return fmt.Errorf("remove old home worktree path: %w", err)
	}
	if err := os.RemoveAll(s.mergeDir); err != nil {
		return fmt.Errorf("remove old home merge path: %w", err)
	}
	if err := os.MkdirAll(s.workDir, 0o755); err != nil {
		return fmt.Errorf("create home work dir: %w", err)
	}
	if err := s.git(ctx, "", "--git-dir", s.repo, "worktree", "prune"); err != nil {
		return err
	}
	return s.git(ctx, "", "--git-dir", s.repo, "worktree", "add", "-b", s.branch, s.worktree, "main")
}

func (s *agentHomeSession) cleanDespawn(ctx context.Context) error {
	if err := s.git(ctx, s.worktree, "add", "-A"); err != nil {
		return err
	}
	if out, err := runGitCommand(ctx, s.worktree, "diff", "--cached", "--quiet"); err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			return fmt.Errorf("git diff --cached --quiet: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if err := s.git(ctx, s.worktree, "commit", "-m", "Update agent home for "+s.branch); err != nil {
			return err
		}
	}
	if err := s.git(ctx, "", "--git-dir", s.repo, "worktree", "add", s.mergeDir, "main"); err != nil {
		return err
	}
	mergeErr := s.git(ctx, s.mergeDir, "merge", "--no-ff", s.branch, "-m", "Merge agent home "+s.branch)
	removeMergeErr := s.git(ctx, "", "--git-dir", s.repo, "worktree", "remove", "--force", s.mergeDir)
	removeRunErr := s.git(ctx, "", "--git-dir", s.repo, "worktree", "remove", "--force", s.worktree)
	deleteBranchErr := s.git(ctx, "", "--git-dir", s.repo, "branch", "-D", s.branch)
	switch {
	case mergeErr != nil:
		return mergeErr
	case removeMergeErr != nil:
		return removeMergeErr
	case removeRunErr != nil:
		return removeRunErr
	case deleteBranchErr != nil:
		return deleteBranchErr
	default:
		return nil
	}
}

func (s *agentHomeSession) git(ctx context.Context, dir string, args ...string) error {
	out, err := runGitCommand(ctx, dir, args...)
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func homeRunBranch(runID string) string {
	s := strings.ToLower(runID)
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
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "run"
	}
	if out != "run" && !strings.HasPrefix(out, "run-") {
		out = "run-" + out
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	return out
}

const agentHomeGitignore = `# Recreatable local state.
.cache/
.npm/
.pnpm-store/
.yarn/
go/pkg/mod/
go-build/
node_modules/
tmp/
temp/

# Working repositories are not home memory.
src/
repos/
work/

# Runtime noise.
*.log
*.tmp
`

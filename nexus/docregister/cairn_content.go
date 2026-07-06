package docregister

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// CairnContent stores/fetches a document's MD body in cairn. There is no
// cairn gRPC RPC for arbitrary blob content — cwb-proto's cwb.cairn.v1
// package (RepoService/PullService/OrgService) is repo administration and
// pull-request lifecycle only; the git wire protocol (Smart-HTTP/SSH) is
// cairn's actual content-storage transport and deliberately stays off gRPC
// (see cairn.proto's package comment). So "content in cairn" means a real
// git commit against a cairn-hosted line, not an RPC call — see README.md's
// "ledger-vs-cairn split" section for the full rationale and the cairn_ref
// convention.
type CairnContent interface {
	// Commit writes content as docID's MD body and commits it to the cairn
	// line, returning the new cairn_ref (see RefFormat). kind namespaces the
	// path (docs/<kind>/<docID>.md) so specs/plans/designs/reports don't
	// collide.
	Commit(ctx context.Context, docID string, kind Kind, content string) (cairnRef string, err error)

	// Fetch returns the MD body a cairn_ref points at.
	Fetch(ctx context.Context, cairnRef string) (content string, err error)
}

// GitCairnContent is the real CairnContent: a git working copy of a cairn
// line (RepoDir) that this process has already checked out — the same "cairn
// line checkout is the shared workspace" convention the builder pool itself
// uses (PHASE2-DESIGN.md §8). Commit writes the file, `git add`s it, and
// `git commit`s locally; it deliberately does NOT push — pushing to the
// sovereign cairn remote is a separate, credentialed operation the caller
// (or an operator-run `cairn commit`/push, mirroring this very build's
// workflow) performs out of band. See README.md's live-verify path.
type GitCairnContent struct {
	// RepoDir is the working directory of a git checkout of the cairn line
	// documents are stored on. Required.
	RepoDir string
	// Author is the "Name <email>" passed to git commit --author. Empty
	// uses the repo's configured user.name/user.email.
	Author string
}

// docPath returns the RepoDir-relative path a document's MD body lives at:
// docs/<kind>/<docID>.md.
func docPath(kind Kind, docID string) string {
	// path.Join, NOT filepath.Join: this is a git-repo-relative path used in
	// cairn_ref strings ("<path>@<sha>") and git commands — git paths are
	// ALWAYS forward-slash, on every OS. filepath.Join produced backslashes
	// on Windows (caught by CI on the landing PR: ref "docs\\spec\\doc-1.md@…").
	return path.Join("docs", string(kind), docID+".md")
}

// RefFormat documents the cairn_ref convention: "<repo-relative-path>@<git-commit-sha>".
// Fetch/Commit on GitCairnContent both use this shape; a different
// CairnContent implementation is free to use another convention as long as
// it's consistent between its own Commit and Fetch.
const RefFormat = "<path>@<sha>"

func (g *GitCairnContent) Commit(ctx context.Context, docID string, kind Kind, content string) (string, error) {
	if g.RepoDir == "" {
		return "", fmt.Errorf("docregister.GitCairnContent: RepoDir not configured")
	}
	rel := docPath(kind, docID)
	abs := filepath.Join(g.RepoDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("docregister.GitCairnContent.Commit: mkdir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("docregister.GitCairnContent.Commit: write: %w", err)
	}
	if _, err := g.git(ctx, "add", rel); err != nil {
		return "", fmt.Errorf("docregister.GitCairnContent.Commit: git add: %w", err)
	}
	commitArgs := []string{"commit", "-m", fmt.Sprintf("docregister: %s %s", kind, docID)}
	if g.Author != "" {
		commitArgs = append(commitArgs, "--author", g.Author)
	}
	if _, err := g.git(ctx, commitArgs...); err != nil {
		// "nothing to commit" (identical content re-committed) is not an
		// error for this caller's purposes — the ref just points at the
		// existing HEAD, which already has this exact content.
		if !strings.Contains(err.Error(), "nothing to commit") {
			return "", fmt.Errorf("docregister.GitCairnContent.Commit: git commit: %w", err)
		}
	}
	sha, err := g.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("docregister.GitCairnContent.Commit: rev-parse: %w", err)
	}
	return rel + "@" + strings.TrimSpace(sha), nil
}

func (g *GitCairnContent) Fetch(ctx context.Context, cairnRef string) (string, error) {
	if g.RepoDir == "" {
		return "", fmt.Errorf("docregister.GitCairnContent: RepoDir not configured")
	}
	rel, sha, ok := strings.Cut(cairnRef, "@")
	if !ok {
		return "", fmt.Errorf("docregister.GitCairnContent.Fetch: malformed cairn_ref %q (want %s)", cairnRef, RefFormat)
	}
	out, err := g.git(ctx, "show", sha+":"+rel)
	if err != nil {
		return "", fmt.Errorf("docregister.GitCairnContent.Fetch: git show %s:%s: %w", sha, rel, err)
	}
	return out, nil
}

func (g *GitCairnContent) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.RepoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// nextVersion is a small helper: ApproveWithChanges bumps Document.Version
// by 1 on every edited-body commit. Kept here (not in register.go) so the
// version-numbering convention lives next to the content-storage code it's
// paired with.
func nextVersion(current int) int {
	return current + 1
}

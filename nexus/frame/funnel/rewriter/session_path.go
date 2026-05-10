package rewriter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SessionPath resolves the full path to a claude-code session jsonl
// given the aspect's working directory and the bridle session id.
//
// claude-code stores sessions at:
//
//	~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
//
// where encoded-cwd is the absolute path with separators replaced by
// "-" (Windows backslashes become "-", drive colons become "-", e.g.
// `C:\src\agent-network\agents\keel` → `C--src-agent-network-agents-keel`).
//
// On Windows the home is %USERPROFILE%; on Unix it's $HOME.
//
// Returns the path even if the jsonl does not yet exist — the rewriter
// is invoked AFTER claude-code's first --session-id call writes the
// file, so absence at construction time is normal.
func SessionPath(aspectCwd, sessionID string) string {
	home, _ := os.UserHomeDir()
	encoded := encodeCwd(aspectCwd)
	return filepath.Join(home, ".claude", "projects", encoded, sessionID+".jsonl")
}

// encodeCwd reproduces claude-code's projects-directory encoding:
// every path separator and (on Windows) the drive colon is replaced
// with a single "-". The encoding is irreversible — multiple distinct
// paths can collide — but matches claude-code's algorithm so we read
// the same file it wrote.
func encodeCwd(cwd string) string {
	c := cwd
	if runtime.GOOS == "windows" {
		// Normalise to backslash-form (claude-code on Windows uses
		// backslashes) and strip the drive colon: `C:\foo` → `C\foo`.
		c = strings.ReplaceAll(c, "/", "\\")
		c = strings.Replace(c, ":", "", 1)
	}
	// Replace every separator with a dash. Both / and \ are mapped
	// so behavior is platform-agnostic — claude-code does the same.
	c = strings.ReplaceAll(c, "\\", "-")
	c = strings.ReplaceAll(c, "/", "-")
	return c
}

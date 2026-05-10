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
// EVERY character that's a path separator OR drive colon becomes a
// single "-". `C:\Users\jacin` → `C--Users-jacin` (the colon AND the
// following backslash both produce dashes; they don't collapse).
// The encoding is irreversible — distinct paths can collide — but it
// matches claude-code's algorithm so we read the same file it wrote.
//
// Verified empirically against ~/.claude/projects/ entries on
// Windows: `C:\Users\jacin\AppData\Local\Temp\nexus-diligence` is
// stored as `C--Users-jacin-AppData-Local-Temp-nexus-diligence`.
func encodeCwd(cwd string) string {
	c := cwd
	if runtime.GOOS == "windows" {
		// Normalise to backslash-form (claude-code on Windows uses
		// backslashes); leave the colon — it gets dashed below.
		c = strings.ReplaceAll(c, "/", "\\")
	}
	// Replace every separator AND the drive colon with a dash.
	c = strings.ReplaceAll(c, "\\", "-")
	c = strings.ReplaceAll(c, "/", "-")
	c = strings.ReplaceAll(c, ":", "-")
	return c
}

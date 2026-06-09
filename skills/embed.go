// Package skills is the canonical store of nexus dev-lifecycle skills.
// The SKILL.md files double as the dir CLI agents (codex, claude-code)
// load natively; this package embeds them for the nexus-skills-mcp
// server (go:embed can't reach up from runtime/cmd, so it lives here).
package skills

import "embed"

//go:embed all:*
var files embed.FS

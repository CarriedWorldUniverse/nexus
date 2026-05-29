package funnel

import (
	"encoding/json"
	"fmt"
	"strings"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

// ToolPolicy is a per-aspect autonomous permission policy. Zero value
// (DefaultAllow=false, empty maps) denies everything — set DefaultAllow
// true for a permissive base and carve out denials.
type ToolPolicy struct {
	// DefaultAllow is the decision when no tool-specific rule matches.
	DefaultAllow bool
	// Tools overrides per tool name: true=allow, false=deny. Absent → DefaultAllow.
	Tools map[string]bool
	// BashDeny: a bash call whose command contains any of these substrings
	// is denied (checked only for the "bash" tool, when bash is otherwise allowed).
	BashDeny []string
	// WritePathAllow: if non-empty, write/edit are allowed only when their
	// path has one of these prefixes. Empty → no path restriction.
	WritePathAllow []string
}

// Evaluate returns whether the call is allowed and, when denied, a reason.
func (p ToolPolicy) Evaluate(call bridle.ToolCall) (allow bool, reason string) {
	// Per-tool allow/deny.
	allowed := p.DefaultAllow
	if v, ok := p.Tools[call.Name]; ok {
		allowed = v
	}
	if !allowed {
		return false, fmt.Sprintf("tool %q not permitted for this aspect", call.Name)
	}
	// Bash command denylist.
	if call.Name == "bash" && len(p.BashDeny) > 0 {
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Args, &a)
		for _, bad := range p.BashDeny {
			if bad != "" && strings.Contains(a.Command, bad) {
				return false, fmt.Sprintf("bash command matches denylist pattern %q", bad)
			}
		}
	}
	// Write/edit path allowlist.
	if (call.Name == "write" || call.Name == "edit") && len(p.WritePathAllow) > 0 {
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(call.Args, &a)
		ok := false
		for _, pre := range p.WritePathAllow {
			if strings.HasPrefix(a.Path, pre) {
				ok = true
				break
			}
		}
		if !ok {
			return false, fmt.Sprintf("write path %q outside permitted prefixes", a.Path)
		}
	}
	return true, ""
}

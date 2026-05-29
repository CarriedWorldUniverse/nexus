package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	bridle "github.com/CarriedWorldUniverse/bridle"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
)

// Verdict is the outcome of a policy decision for a single tool call.
type Verdict int

const (
	// VerdictAllow runs the tool normally.
	VerdictAllow Verdict = iota
	// VerdictDeny refuses the call (model gets a refusal tool_result).
	VerdictDeny
	// VerdictEscalate pauses the call and asks a human operator to
	// approve or deny it (P3c).
	VerdictEscalate
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
	// Escalate names tools that, when otherwise allowed, require operator
	// approval on every call (P3c). A tool that is outright denied (Tools=false,
	// BashDeny match, WritePathAllow miss) stays denied — Deny outranks Escalate.
	Escalate map[string]bool
}

// Decide classifies a tool call. Precedence: outright Deny > Escalate >
// Allow. The returned reason is non-empty for Deny and Escalate (it is
// surfaced to the operator on escalate, and to the model on deny).
func (p ToolPolicy) Decide(call bridle.ToolCall) (Verdict, string) {
	// Per-tool allow/deny.
	allowed := p.DefaultAllow
	if v, ok := p.Tools[call.Name]; ok {
		allowed = v
	}
	if !allowed {
		return VerdictDeny, fmt.Sprintf("tool %q not permitted for this aspect", call.Name)
	}
	// Bash command denylist.
	if call.Name == "bash" && len(p.BashDeny) > 0 {
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Args, &a)
		for _, bad := range p.BashDeny {
			if bad != "" && strings.Contains(a.Command, bad) {
				return VerdictDeny, fmt.Sprintf("bash command matches denylist pattern %q", bad)
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
			return VerdictDeny, fmt.Sprintf("write path %q outside permitted prefixes", a.Path)
		}
	}
	// The call is allowed by the deny rules above — does it require
	// operator approval each time?
	if p.Escalate[call.Name] {
		return VerdictEscalate, fmt.Sprintf("policy requires operator approval for tool %q", call.Name)
	}
	return VerdictAllow, ""
}

// Evaluate is the P3b-compatible wrapper: it reports allow=true only for
// VerdictAllow. Escalate counts as "not unconditionally allowed" so
// pre-P3c callers (which can't perform the round-trip) treat it as a
// deny — fail-safe. New callers should use Decide + PermissionHook.
func (p ToolPolicy) Evaluate(call bridle.ToolCall) (allow bool, reason string) {
	switch v, reason := p.Decide(call); v {
	case VerdictAllow:
		return true, ""
	default:
		return false, reason
	}
}

// Requester is the round-trip primitive the Escalator needs: send a
// request frame and block for the correlated response. Satisfied by
// wsasp.Client.Request (and wsclient.Client.Request). Kept as a narrow
// interface so the funnel doesn't depend on the WS client packages and
// tests can inject a fake operator.
type Requester interface {
	Request(context.Context, frames.Envelope) (frames.Envelope, error)
}

// Escalator performs the operator round-trip for a VerdictEscalate tool
// call. It is per-aspect: AspectID is funnel-injected into every
// request so the operator (and the broker's identity check) always sees
// who is asking; the model cannot forge it.
type Escalator struct {
	Requester Requester
	AspectID  string
}

// EscalationOutcome is the decoded operator decision.
type EscalationOutcome struct {
	Approved bool
	Note     string
}

// Ask builds an escalation.request for call (with the aspect identity
// injected), sends it via the Requester, and BLOCKS until the operator
// answers or ctx is cancelled (no timeout — the operator releases the
// turn). It decodes the resulting escalation.decision. A cancelled ctx
// (broker shutdown) or a transport error returns an error; the hook
// treats that fail-safe as a deny.
func (e *Escalator) Ask(ctx context.Context, call bridle.ToolCall, reason string) (EscalationOutcome, error) {
	env, err := frames.NewRequest(frames.KindEscalationRequest, frames.EscalationRequestPayload{
		Aspect: e.AspectID,
		Tool:   call.Name,
		Args:   call.Args,
		Reason: reason,
	})
	if err != nil {
		return EscalationOutcome{}, fmt.Errorf("build escalation.request: %w", err)
	}
	resp, err := e.Requester.Request(ctx, env)
	if err != nil {
		return EscalationOutcome{}, fmt.Errorf("escalation round-trip: %w", err)
	}
	var dec frames.EscalationDecisionPayload
	if err := frames.PayloadAs(resp, &dec); err != nil {
		return EscalationOutcome{}, fmt.Errorf("decode escalation.decision: %w", err)
	}
	return EscalationOutcome{
		Approved: dec.Decision == frames.EscalationApprove,
		Note:     dec.Note,
	}, nil
}

// PermissionHook returns a bridle BeforeToolCall hook that enforces p.
// On deny it sets Deny+Err and returns HookContinue (bridle then hands the
// model the refusal as a tool_result and continues — see bridle P3a).
//
// esc handles the VerdictEscalate path. When esc is nil (headless /
// claude-code / tests with no operator wire) an escalate verdict
// degrades to a deny — fail-safe: never silently run a tool that policy
// said needed a human.
func PermissionHook(p ToolPolicy, esc *Escalator) bridle.Hook[bridle.BeforeToolCallCtx] {
	return func(ctx context.Context, in bridle.BeforeToolCallCtx) (bridle.BeforeToolCallCtx, bridle.HookAction, error) {
		switch v, reason := p.Decide(in.Call); v {
		case VerdictAllow:
			return in, bridle.HookContinue, nil
		case VerdictDeny:
			in.Deny = true
			in.Err = "permission denied: " + reason
			return in, bridle.HookContinue, nil
		case VerdictEscalate:
			if esc == nil {
				// No operator wire: fail-safe deny.
				in.Deny = true
				in.Err = "permission denied: " + reason + " (no operator available)"
				return in, bridle.HookContinue, nil
			}
			outcome, err := esc.Ask(ctx, in.Call, reason)
			if err != nil {
				in.Deny = true
				in.Err = "escalation failed: " + err.Error()
				return in, bridle.HookContinue, nil
			}
			if outcome.Approved {
				return in, bridle.HookContinue, nil // run the tool
			}
			in.Deny = true
			in.Err = "operator denied" + noteSuffix(outcome.Note)
			return in, bridle.HookContinue, nil
		default:
			// Unreachable; treat unknown verdict as deny.
			in.Deny = true
			in.Err = "permission denied: unknown verdict"
			return in, bridle.HookContinue, nil
		}
	}
}

// noteSuffix renders an operator note for the deny tool_result, empty
// when there is no note.
func noteSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return ": " + note
}

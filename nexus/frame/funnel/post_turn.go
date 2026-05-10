// Post-turn hook — runs between provider turns, after the deliberation
// loop has accepted the model's output and before the next inbox is
// drained into a new turn. Concrete implementation: the rewriter
// runner (nexus/frame/funnel/rewriter), which distills the just-
// completed turn's tail in claude-code's session jsonl.
//
// The interface lives in funnel (not rewriter) so the funnel can
// depend on it without importing rewriter — keeps the dependency
// direction one-way.

package funnel

import "context"

// PostTurnHook is called after each successful provider turn.
// Implementations:
//   - MUST run synchronously and return only when their work is done.
//     We can't fire the next --resume while the rewriter is still
//     mutating the jsonl.
//   - SHOULD complete in well under a turn's worth of time. The
//     rewriter currently distills with bounded haiku timeouts; if
//     a future hook is slower, surface that here.
//   - SHOULD log their own errors. The funnel does not surface a
//     hook failure to the caller — distillation degradation is not
//     a fatal condition.
//
// ShouldResetSession reports whether the funnel should rotate the
// session id before the next turn. Used by the rewriter runner after
// sustained distillation failures to recover by starting fresh.
// Acknowledged via AcknowledgeReset once the rotation happens.
type PostTurnHook interface {
	AfterTurn(ctx context.Context)
	ShouldResetSession() bool
	AcknowledgeReset()
}

// NoopPostTurn is the default when Config.PostTurn is nil. The funnel
// uses it via a never-nil indirect to keep AfterTurn calls safe
// without per-call nil checks.
type NoopPostTurn struct{}

func (NoopPostTurn) AfterTurn(context.Context)  {}
func (NoopPostTurn) ShouldResetSession() bool   { return false }
func (NoopPostTurn) AcknowledgeReset()          {}

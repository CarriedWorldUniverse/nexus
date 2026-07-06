// Goal-loop primitive (NEX-210). Wraps a Funnel to auto-continue work
// when the post-turn judge classifies a turn as goal_not_met — the
// Definition of Done is not yet satisfied.
//
// The goal-loop enqueues synthetic continuation briefs as inbox items
// on the same thread, so the existing Frame main loop (drain-inbox
// until ErrEmptyInbox) picks them up automatically. Loop protection
// caps at MaxTurns per ticket to prevent runaway.

package funnel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// GoalConfig carries the ticket context for a goal-pursuit loop.
type GoalConfig struct {
	// TicketID identifies the ticket being pursued. Used in
	// continuation briefs and loop-protection keying.
	TicketID string

	// DoD is the Definition of Done. Injected into every judge
	// prompt so the judge can compare turn artifacts against DoD
	// criteria. Required — empty DoD disables the goal-loop
	// (Pursue returns immediately).
	DoD string

	// MaxTurns is the safety cap. Default 20 when zero.
	MaxTurns int

	// ThreadRoot is the chat thread the ticket lives on.
	// Continuation briefs are enqueued with this thread root so
	// per-thread session isolation (ContextThreadIsolated) keeps
	// the goal-pursuit on the right jsonl.
	ThreadRoot int64
}

// GoalResult is the outcome of a single Pursue call.
type GoalResult struct {
	// Done is true when the goal is complete (judge returned
	// "complete") or the loop cap was reached.
	Done bool

	// Blocked is true when the judge returned "blocked" or when the
	// repeat-goal_not_met safety cap escalated to blocked (NEX-249).
	Blocked bool

	// TurnsRun is how many turns the goal-loop executed in this
	// Pursue call. May be zero when no work was pending.
	TurnsRun int

	// Reason is a short label for the terminal state. Documented
	// values: "complete", "scratch", "blocked", "loop_cap",
	// "empty_inbox", "no_dod", "unknown_class", "goal_not_met"
	// (intermediate), "repeated_goal_not_met" (NEX-249 safety cap).
	Reason string
}

// consecutiveGoalNotMetCap is the NEX-249 safety net. When the judge
// returns goal_not_met this many turns in a row, the loop terminates
// as Blocked regardless of judge verdict — covers the case where
// fix A (prior-turn-aware judge) misfires or the judge model
// degrades. Three is empirical: enough to absorb a brief stall, low
// enough to keep the failure window short.
const consecutiveGoalNotMetCap = 3

// GoalLoop wraps a Funnel for ticket-driven goal pursuit.
// Safe for a single calling goroutine (the Frame's main loop).
type GoalLoop struct {
	funnel    *Funnel
	cfg       GoalConfig
	turnCount int

	// priorFinalText is the prior turn's natural reply, threaded into
	// the next judge invocation (NEX-249 fix A) so it can detect
	// "looping, same output as prior turn". Empty on the first turn.
	priorFinalText string

	// consecutiveGoalNotMet counts consecutive goal_not_met verdicts.
	// Reset on any non-goal_not_met class. NEX-249 fix B safety cap.
	consecutiveGoalNotMet int
}

// NewGoalLoop creates a goal-loop wrapping the given funnel.
func NewGoalLoop(f *Funnel, cfg GoalConfig) *GoalLoop {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 20
	}
	return &GoalLoop{funnel: f, cfg: cfg}
}

// TurnCount reports how many turns have run in the current goal
// pursuit. Persists across Pursue calls so the MaxTurns cap applies
// to the whole pursuit; call Reset to start a fresh count when the
// operator unblocks or overrides the loop cap.
func (g *GoalLoop) TurnCount() int {
	return g.turnCount
}

// LastFinalText returns the most recently completed turn's natural reply —
// the REAL posted output, not a model self-report. Set unconditionally at
// the top of Pursue's switch (before any class-specific branch), so it is
// valid immediately after any Pursue call, including a Done/FilterClassComplete
// result. Unit B (verified task_done, NET-22/23/24/27) uses this as the
// acceptance-verifier input for the judge-complete exit path: the judge that
// classified the turn "complete" only ever saw the task text, never the
// work item's acceptance criteria, so a second, criteria-aware check against
// the SAME text the judge just approved is the only way to catch a
// confabulated or merely-plausible-looking "done".
func (g *GoalLoop) LastFinalText() string {
	return g.priorFinalText
}

// Pursue runs one iteration of the goal-pursuit loop. It calls
// Deliberate once; if the judge returns goal_not_met and the loop
// cap hasn't been reached, it enqueues a continuation brief and
// returns GoalResult{Continue: true}. The caller should loop on
// Pursue until Done or Blocked.
//
// Pursue is designed to be called from the Frame's main loop:
//
//	gl := NewGoalLoop(f, cfg)
//	for {
//	    result, err := gl.Pursue(ctx)
//	    if err != nil { break }
//	    if result.Done || result.Blocked { break }
//	}
func (g *GoalLoop) Pursue(ctx context.Context) (GoalResult, error) {
	if g.cfg.DoD == "" {
		return GoalResult{Done: true, Reason: "no_dod"}, nil
	}

	// Set the DoD for this turn's judge.
	g.funnel.SetDoD(g.cfg.DoD)
	// NEX-249 fix A: thread the prior turn's reply to the judge so it
	// can detect zero forward progress. Empty on the first turn.
	g.funnel.SetPriorTurnFinalText(g.priorFinalText)

	result, err := g.funnel.Deliberate(ctx, "")
	if err != nil {
		if errors.Is(err, ErrEmptyInbox) {
			return GoalResult{Done: true, Reason: "empty_inbox"}, nil
		}
		return GoalResult{}, err
	}

	g.turnCount++
	// Always remember this turn's reply for the next judge call. Saved
	// before any return so even the goal_not_met branch (which is the
	// only one that loops) carries it forward.
	g.priorFinalText = result.TurnResult.FinalText

	class := result.Filter.Class
	if class == "" {
		// Legacy filter — derive from ShouldPost.
		if result.Filter.ShouldPost {
			class = FilterClassComplete
		} else {
			class = FilterClassScratch
		}
	}

	// Reset the consecutive counter on any non-goal_not_met outcome.
	// Counter only matters as a "stuck in a row" signal.
	if class != FilterClassGoalNotMet {
		g.consecutiveGoalNotMet = 0
	}

	switch class {
	case FilterClassComplete:
		return GoalResult{Done: true, TurnsRun: g.turnCount, Reason: "complete"}, nil

	case FilterClassScratch:
		// Non-substantive output — the turn didn't produce anything
		// meaningful. Don't continue; this isn't progress.
		return GoalResult{Done: true, TurnsRun: g.turnCount, Reason: "scratch"}, nil

	case FilterClassGoalNotMet:
		g.consecutiveGoalNotMet++
		// NEX-249 fix B: safety cap. If the judge keeps returning
		// goal_not_met without forward progress (fix A's prior-turn
		// signal would normally surface this as blocked, but if the
		// judge ignores it the loop would still run to MaxTurns), force
		// terminate as blocked so the operator sees the stall instead
		// of N more vacuous chat posts.
		if g.consecutiveGoalNotMet >= consecutiveGoalNotMetCap {
			return GoalResult{
				Blocked:  true,
				TurnsRun: g.turnCount,
				Reason:   "repeated_goal_not_met",
			}, nil
		}
		if g.turnCount >= g.cfg.MaxTurns {
			return GoalResult{Done: true, TurnsRun: g.turnCount, Reason: "loop_cap"}, nil
		}
		// Enqueue continuation brief as a synthetic inbox item.
		brief := g.buildContinuationBrief(result.TurnResult.FinalText)
		g.funnel.ReceiveSynthetic(bridle.InboxItem{
			From:       "system",
			Source:     "goal_loop",
			Content:    brief,
			ThreadRoot: g.cfg.ThreadRoot,
		})
		return GoalResult{TurnsRun: g.turnCount, Reason: "goal_not_met"}, nil

	case FilterClassBlocked:
		return GoalResult{Blocked: true, TurnsRun: g.turnCount, Reason: "blocked"}, nil

	default:
		// Unknown class — treat as complete to avoid looping.
		return GoalResult{Done: true, TurnsRun: g.turnCount, Reason: "unknown_class"}, nil
	}
}

// buildContinuationBrief generates a synthetic inbox item body that
// prompts the aspect to continue working toward the DoD. The brief
// includes the DoD, the turn counter, and a truncated excerpt of the
// prior turn's output so the next turn has context without re-reading
// the full session.
//
// This is a deterministic template — no model call needed. The
// continuation IS the inbox trigger; the model sees it as its next
// user message and picks up from there.
func (g *GoalLoop) buildContinuationBrief(priorFinalText string) string {
	excerpt := strings.TrimSpace(priorFinalText)
	const maxExcerpt = 2000
	if len(excerpt) > maxExcerpt {
		excerpt = excerpt[:maxExcerpt] + "…"
	}

	return fmt.Sprintf(
		"[CONTINUATION] Ticket %s — turn %d/%d.\n\n"+
			"Definition of Done:\n%s\n\n"+
			"Prior turn output (excerpt):\n%s\n\n"+
			"Continue working toward the DoD above. "+
			"If the DoD is now met, state so clearly in your reply. "+
			"If you are blocked, say so explicitly and name the blocker.",
		g.cfg.TicketID, g.turnCount, g.cfg.MaxTurns,
		g.cfg.DoD,
		excerpt,
	)
}

// Reset clears the turn counter + repetition tracking for a new goal
// pursuit. Call when the operator manually unblocks or overrides the
// loop cap.
func (g *GoalLoop) Reset() {
	g.turnCount = 0
	g.priorFinalText = ""
	g.consecutiveGoalNotMet = 0
}

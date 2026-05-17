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

	// Blocked is true when the judge returned "blocked".
	Blocked bool

	// TurnsRun is how many turns the goal-loop executed in this
	// Pursue call. May be zero when no work was pending.
	TurnsRun int

	// Reason is a short label for the terminal state:
	// "complete", "blocked", "loop_cap", "empty_inbox".
	Reason string
}

// GoalLoop wraps a Funnel for ticket-driven goal pursuit.
// Safe for a single calling goroutine (the Frame's main loop).
type GoalLoop struct {
	funnel    *Funnel
	cfg       GoalConfig
	turnCount int
}

// NewGoalLoop creates a goal-loop wrapping the given funnel.
func NewGoalLoop(f *Funnel, cfg GoalConfig) *GoalLoop {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 20
	}
	return &GoalLoop{funnel: f, cfg: cfg}
}

// TurnCount reports how many turns have run in the current goal
// pursuit. Resets on each new Pursue call.
func (g *GoalLoop) TurnCount() int {
	return g.turnCount
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

	result, err := g.funnel.Deliberate(ctx, "")
	if err != nil {
		if err == ErrEmptyInbox {
			return GoalResult{Done: true, Reason: "empty_inbox"}, nil
		}
		return GoalResult{}, err
	}

	g.turnCount++

	class := result.Filter.Class
	if class == "" {
		// Legacy filter — derive from ShouldPost.
		if result.Filter.ShouldPost {
			class = FilterClassComplete
		} else {
			class = FilterClassScratch
		}
	}

	switch class {
	case FilterClassComplete:
		return GoalResult{Done: true, TurnsRun: g.turnCount, Reason: "complete"}, nil

	case FilterClassScratch:
		// Non-substantive output — the turn didn't produce anything
		// meaningful. Don't continue; this isn't progress.
		return GoalResult{Done: true, TurnsRun: g.turnCount, Reason: "scratch"}, nil

	case FilterClassGoalNotMet:
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

// Reset clears the turn counter for a new goal pursuit. Call when
// the operator manually unblocks or overrides the loop cap.
func (g *GoalLoop) Reset() {
	g.turnCount = 0
}

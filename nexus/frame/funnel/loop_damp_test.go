package funnel

import (
	"strconv"
	"testing"
)

// NEX-365 Tier-1 loop damping: the state machine. The operator channel is
// an absolute carve-out; peer turns damp only after K consecutive
// unproductive turns (empty / suppressed / repeated); varied productive
// turns never trip it.
func TestLoopDamping(t *testing.T) {
	// Operator carve-out: never damped, even with the counter pinned high.
	op := &Funnel{}
	op.dampConsecutive = 99
	if op.shouldDampen(TurnTrigger{Source: "tty"}) {
		t.Error("tty source must never be damped")
	}
	if op.shouldDampen(TurnTrigger{From: "operator"}) {
		t.Error("operator sender must never be damped")
	}
	if !op.shouldDampen(TurnTrigger{From: "peer", Source: "chat"}) {
		t.Error("a peer over the threshold should be damped")
	}

	// Varied, productive output never damps a peer.
	varied := &Funnel{}
	for i := 0; i < 6; i++ {
		varied.recordTurnOutcome("reply number "+strconv.Itoa(i), true)
	}
	if varied.shouldDampen(TurnTrigger{From: "peer"}) {
		t.Error("varied productive turns must not damp")
	}

	// Repeated identical output → damps after the threshold (the echo).
	repeat := &Funnel{}
	for i := 0; i < loopDampThreshold+2; i++ {
		repeat.recordTurnOutcome("@peer ping", true)
	}
	if !repeat.shouldDampen(TurnTrigger{From: "peer"}) {
		t.Error("repeated identical output should damp")
	}

	// Empty / judge-suppressed turns → damp after the threshold.
	empty := &Funnel{}
	for i := 0; i < loopDampThreshold+1; i++ {
		empty.recordTurnOutcome("", false)
	}
	if !empty.shouldDampen(TurnTrigger{From: "peer"}) {
		t.Error("empty/suppressed turns should damp")
	}

	// A productive turn resets the counter (escape hatch).
	empty.recordTurnOutcome("something fresh and useful", true)
	if empty.shouldDampen(TurnTrigger{From: "peer"}) {
		t.Error("a productive turn should reset the damp counter")
	}
}

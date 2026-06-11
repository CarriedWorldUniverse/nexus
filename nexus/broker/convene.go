// convene.go — the !convene roundtable driver (roundtable spec component
// 3, P3 plan Task A/B/C). The broker stays a lean hub: it does the
// plumbing (parse, thread root, participant + facilitator briefs, a
// convene record, close semantics) and NONE of the judging. Convergence
// is an aspect behavior — the facilitator's funnel receives a brief and
// owns the verdict, the CONSENSUS: summary, and the convene.close.
//
// How it reuses the existing machinery (thin-over-them, per the plan):
//
//   - Thread root: the !convene post is stored as the audit-thread root,
//     exactly the !dispatch pattern (chat_send.go).
//   - Wake: each per-participant brief is posted INTO the thread
//     @-mentioning that participant. HandleChatSend computes the mention
//     as a recipient and, when that aspect is napping wake-on-mention with
//     no live conn, calls wake.MaybeWake — so naming a participant in a
//     brief wakes its pod with ZERO new wake code.
//   - Mediation seam: participant/facilitator briefs are ordinary thread
//     chatter (full to aspects, audit to operators). The operator-facing
//     output is the facilitator's job — its brief tells it to DM shadow
//     a digest/decision-point, keeping the inter-aspect turns off the
//     operator's 1:1 (P4 formalizes digest delivery mode; v1 = DM shadow).

package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CarriedWorldUniverse/nexus/nexus/convene"
)

// knownBaseAspects returns the set of base aspect names eligible for a
// convene: the union of the live roster and the configured wake-policy
// map (so napping aspects — scaled to zero, absent from the roster —
// still validate). In-memory only; no DB round-trip on the chat hot path.
func (b *Broker) knownBaseAspects() map[string]bool {
	known := map[string]bool{}
	if b.roster != nil {
		for _, n := range b.roster.AspectNames() {
			known[n] = true
		}
	}
	for n := range b.cfg.AspectWakePolicy {
		known[n] = true
	}
	return known
}

// submitConvene drives an intercepted !convene post. The post itself is
// stored by the caller (HandleChatSend) as the audit-thread root; rootMsgID
// and thread identify it. This parses the command, records the convene, and
// posts the participant + facilitator briefs into the thread (which wakes
// nappers via the mention→MaybeWake path). Returns an error only on a bad
// command or a record failure; brief-post failures are logged, never fatal
// (a partially-briefed convene is recoverable in-thread).
func (b *Broker) submitConvene(ctx context.Context, sender, content, thread string, rootMsgID int64) error {
	if b.cfg.ConveneStore == nil {
		return errors.New("convene: no convene store configured")
	}
	cmd, err := parseConveneCommand(content, b.knownBaseAspects())
	if err != nil {
		b.log.Warn("convene: parse failed", "err", err, "from", sender)
		return err
	}

	facilitator := resolveFacilitator(cmd.Facilitator, sender)
	conveneID := "cv-" + uuid.NewString()

	rec := convene.Convene{
		ConveneID:    conveneID,
		RootMsgID:    rootMsgID,
		Facilitator:  facilitator,
		Participants: cmd.Participants,
		Problem:      cmd.Problem,
		Status:       convene.StatusOpen,
		CreatedAt:    time.Now(),
	}
	if err := b.cfg.ConveneStore.Insert(ctx, rec); err != nil {
		b.log.Warn("convene: record insert failed", "err", err, "convene_id", conveneID)
		return fmt.Errorf("convene: insert record: %w", err)
	}
	b.log.Info("convene: opened",
		"convene_id", conveneID, "facilitator", facilitator,
		"participants", strings.Join(cmd.Participants, ","), "thread", thread)

	// Per-participant briefs — each @-mentions its participant so the
	// recipient policy delivers it and the wake controller wakes a napping
	// participant. Posted as the convener (sender) into the thread, replying
	// to the root so they thread under it.
	for _, p := range cmd.Participants {
		lens := cmd.Lenses[p]
		brief := renderParticipantBrief(p, facilitator, cmd.Problem, lens, cmd.Participants)
		if _, perr := b.HandleChatSend(ctx, sender, brief, rootMsgID, thread); perr != nil {
			b.log.Warn("convene: participant brief post failed", "participant", p, "err", perr)
		}
	}

	// Facilitator brief — the behavioral contract for judging + summary +
	// close lives here (the one place; the runbook references it).
	fbrief := renderFacilitatorBrief(facilitator, conveneID, cmd.Problem, cmd.Participants)
	if _, ferr := b.HandleChatSend(ctx, sender, fbrief, rootMsgID, thread); ferr != nil {
		b.log.Warn("convene: facilitator brief post failed", "facilitator", facilitator, "err", ferr)
	}

	return nil
}

// resolveFacilitator applies the default rule: an explicit facilitator=
// override wins; otherwise the convener (sender) facilitates, except an
// operator-sent convene defaults to shadow (the operator's mediator).
func resolveFacilitator(override, sender string) string {
	if override != "" {
		return override
	}
	if sender == "operator" {
		return "shadow"
	}
	return sender
}

// renderParticipantBrief builds one participant's in-thread brief. The
// leading @mention is load-bearing: it routes the message to the
// participant AND triggers the wake controller for a napping one.
func renderParticipantBrief(participant, facilitator, problem, lens string, all []string) string {
	if lens == "" {
		lens = "your standing perspective"
	}
	others := make([]string, 0, len(all)-1)
	for _, a := range all {
		if a != participant {
			others = append(others, a)
		}
	}
	return fmt.Sprintf(
		"@%s — you've been convened on a roundtable.\n\n"+
			"PROBLEM: %s\n\n"+
			"YOUR LENS: %s\n\n"+
			"Co-deliberating: %s. Facilitator: @%s.\n\n"+
			"Rules: reply in THIS thread; argue your lens honestly; keep turns short; "+
			"converge where you genuinely agree and name where you don't. "+
			"The facilitator judges convergence and posts the summary.",
		participant, problem, lens, strings.Join(others, ", "), facilitator,
	)
}

// renderFacilitatorBrief builds the facilitator's in-thread brief. This is
// the single home of the facilitation behavioral contract (Task C); the
// aspect runbook references it rather than restating it.
func renderFacilitatorBrief(facilitator, conveneID, problem string, participants []string) string {
	return fmt.Sprintf(
		"@%s — you are FACILITATING convene %s.\n\n"+
			"PROBLEM: %s\n\n"+
			"Participants: %s.\n\n"+
			"Your job:\n"+
			"1. Let every participant speak before you judge a round.\n"+
			"2. After each round judge: progressing / converged / stuck.\n"+
			"3. Convergence test: would each participant sign the summary?\n"+
			"4. On CONVERGED, post a single message starting `CONSENSUS:` with the "+
			"decision, the rationale, any dissents named, and follow-up tickets; "+
			"then close the convene (convene.close converged, summary_msg_id = that post).\n"+
			"5. On STUCK (no progress, or a participant unreachable), surface ONE "+
			"decision-point to the operator by DMing shadow with the open question "+
			"and the context to answer it; if it can't resolve, close the convene "+
			"abandoned.\n\n"+
			"MEDIATION: the operator is NOT in this thread. Do not narrate every turn "+
			"to them — only the digest and batched decision-points reach shadow. The "+
			"inter-aspect deliberation stays here for audit.",
		facilitator, conveneID, problem, strings.Join(participants, ", "),
	)
}

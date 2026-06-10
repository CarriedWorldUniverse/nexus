// Aspect-owned fan-out — SubmitSpawn (NEX-571 Task C).
//
// Sibling shape to Submit's ticket dispatch: same Job machinery, same
// queue + drain, same RunsStore rows and OnJobDone result path — but
// the workers are hands of the REQUESTING aspect (derived identities
// `<parent>.sub-N`, parent's image + persona, broker-minted credential)
// and the audit thread is rooted at a spawn summary attributed to the
// parent (the proven !dispatch post-as-thread-root pattern).

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
)

// defaultSpawnMaxConcurrent caps live hands per parent aspect when
// Runner.SpawnMaxConcurrent is unset.
const defaultSpawnMaxConcurrent = 4

// SpawnHandle identifies one spawned hand — a fresh-context worker
// running AS a derived identity of its parent aspect (`<parent>.sub-N`,
// roundtable P2 / NEX-571). RunID is empty when the hand is accepted
// but queued (per-parent spawn cap or global MaxConc); it launches when
// capacity frees, mirroring Submit's queue semantics.
type SpawnHandle struct {
	RunID string
	Name  string
}

// AuditPoster stores a chat post AS a named sender and returns the
// stored message id. The Runner uses it for the spawn audit-thread
// root (from=<parent>); plain status lines keep going through Poster.
// cmd/nexus wires it to the broker's HandleChatSend, beside the
// existing NewWsPoster wiring.
type AuditPoster interface {
	PostFrom(ctx context.Context, from, content string, replyTo int64, topic string) (int64, error)
}

// SubmitSpawn fans out count hands of parent running brief. Returns a
// handle per accepted hand; hands beyond the per-parent cap
// (SpawnMaxConcurrent) or the global MaxConc are queued and drain on
// OnJobDone exactly like queued ticket briefs.
//
// Audit threading: when thread is empty a root message attributed to
// the PARENT is stored (Audit) and everything threads under
// `spawn-<rootMsgID>`; the root's msg id is recorded as each hand's
// DispatchMsgID. When the caller supplies a thread, the hands join it
// and no extra root is posted. Each hand's brief is posted on creation;
// its result lands via the ordinary OnJobDone completion post, tagged
// with the lineage.
func (r *Runner) SubmitSpawn(ctx context.Context, parent, brief string, count int, thread string) ([]SpawnHandle, error) {
	if parent == "" {
		return nil, fmt.Errorf("spawn: parent required")
	}
	// Defense in depth — the broker's frame handler already rejects
	// derived callers, but the Runner is also reachable in-process.
	if aspects.IsDerivedName(parent) {
		return nil, fmt.Errorf("spawn: %s is a derived identity (no sub-of-sub)", parent)
	}
	if strings.TrimSpace(brief) == "" {
		return nil, fmt.Errorf("spawn: brief required")
	}
	if count < 1 {
		count = 1
	}
	if r.MintHandCredential == nil {
		return nil, fmt.Errorf("spawn: no hand-credential minter configured")
	}

	// Audit root (only when the caller didn't bind an existing thread).
	var dispatchMsgID int64
	if thread == "" {
		rootText := fmt.Sprintf("spawn: %d hand(s) of %s — %s", count, parent, briefHead(brief))
		if r.Audit != nil {
			id, err := r.Audit.PostFrom(ctx, parent, rootText, 0, "")
			if err != nil {
				return nil, fmt.Errorf("spawn: audit root post: %w", err)
			}
			dispatchMsgID = id
			thread = fmt.Sprintf("spawn-%d", id)
		} else {
			// No audit store wired (tests / legacy boots): keep the
			// posts correlated under a synthetic topic.
			thread = "spawn-" + parent
		}
	}

	r.mu.Lock()
	names := r.freeHandNames(parent, count)
	var launches []*Run
	var queued []string
	for _, name := range names {
		b := Brief{
			Agent:         name,
			SpawnParent:   parent,
			Ticket:        handTicket(r.nextID()),
			Thread:        thread,
			Task:          brief,
			DispatchMsgID: dispatchMsgID,
		}
		if r.canRun(name) {
			launches = append(launches, r.reserve(b))
		} else {
			r.queue = append(r.queue, b)
			queued = append(queued, name)
		}
	}
	r.mu.Unlock()
	slog.Info("runner: spawn accepted", "parent", parent, "count", count,
		"launching", len(launches), "queued", len(queued), "thread", thread)

	// Per-hand brief posts under the root — "what was each hand told",
	// recorded at creation regardless of launch outcome.
	head := briefHead(brief)
	for _, run := range launches {
		r.post(thread, "hand "+run.Brief.Agent+" brief: "+head)
	}
	for _, name := range queued {
		r.post(thread, "hand "+name+" brief: "+head+" (queued: hand capacity busy)")
	}

	// Launch outside the lock, mirroring Submit. A failed hand is
	// rolled back + recorded failed; surviving hands keep going.
	handles := make([]SpawnHandle, 0, len(names))
	var lastErr error
	for _, run := range launches {
		if err := r.launch(ctx, run); err != nil {
			r.mu.Lock()
			delete(r.active, run.ID)
			delete(r.agentBusy, run.Brief.Agent)
			r.mu.Unlock()
			if r.Recorder != nil {
				doneCtx := ctx
				if doneCtx == nil {
					doneCtx = context.Background()
				}
				r.Recorder.RecordRunDone(doneCtx, run.ID, "failed", time.Now(), "", 0)
			}
			r.post(thread, "hand "+run.Brief.Agent+" spawn failed: "+err.Error())
			lastErr = err
			continue
		}
		handles = append(handles, SpawnHandle{RunID: run.ID, Name: run.Brief.Agent})
	}
	for _, name := range queued {
		handles = append(handles, SpawnHandle{Name: name})
	}
	if len(handles) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return handles, nil
}

// freeHandNames picks the lowest free hand indices for parent —
// skipping names that are busy or already queued, so two overlapping
// spawns never collide on a derived name (one-session-per-name holds
// per hand). Caller holds r.mu.
func (r *Runner) freeHandNames(parent string, count int) []string {
	used := map[string]bool{}
	for name := range r.agentBusy {
		used[name] = true
	}
	for _, q := range r.queue {
		used[q.Agent] = true
	}
	out := make([]string, 0, count)
	for i := 1; len(out) < count; i++ {
		if name := aspects.DerivedName(parent, i); !used[name] {
			out = append(out, name)
		}
	}
	return out
}

// handTicket derives a hand's unique ticket (the Job correlation key +
// OnJobDone lookup) from a fresh run-style id.
func handTicket(id string) string {
	return "hand-" + strings.TrimPrefix(id, "run-")
}

// briefHead is the first line of the brief, bounded, for audit posts.
func briefHead(brief string) string {
	head := strings.TrimSpace(brief)
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		head = strings.TrimSpace(head[:i])
	}
	const max = 120
	if len(head) > max {
		head = head[:max] + "…"
	}
	return head
}

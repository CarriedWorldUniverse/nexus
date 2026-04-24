// Package compactor implements the proactive compaction trigger per
// registration spec §2.7. Runtime-owned; invokes the provider to
// summarise older entries when the active branch grows past the
// window - reserve threshold, then emits a compaction entry into the
// session tree. Active head advances to the compaction entry so
// subsequent replays see the summary as the cut point.
package compactor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus-cw/nexus/runtime/context/tree"
	"github.com/nexus-cw/nexus/runtime/providers"
)

// Policy configures the compaction trigger (§2.7).
type Policy struct {
	// Model is the provider-scoped model id used for token counting
	// and the summary call. Empty → adapter default.
	Model string

	// Window is the model's context window in tokens.
	Window int

	// Reserve is the headroom kept free above active context. Default
	// 20% of window or 10_000, whichever larger.
	Reserve int

	// KeepRecentTurns is how many trailing turns to preserve verbatim
	// across a compaction (everything older gets summarised).
	// Default 6 per §2.7.
	KeepRecentTurns int

	// SummaryHint is passed through to the provider as additional
	// context on what to prioritise. Optional.
	SummaryHint string
}

func (p Policy) effectiveReserve() int {
	if p.Reserve > 0 {
		return p.Reserve
	}
	reserve := p.Window / 5
	if reserve < 10_000 {
		reserve = 10_000
	}
	return reserve
}

func (p Policy) effectiveKeep() int {
	if p.KeepRecentTurns > 0 {
		return p.KeepRecentTurns
	}
	return 6
}

// ShouldCompact implements the spec formula tokens > window - reserve.
// Callers pre-compute the token count via the provider.
func ShouldCompact(tokens int, p Policy) bool {
	if p.Window <= 0 {
		return false
	}
	return tokens > p.Window-p.effectiveReserve()
}

// Result is what Run returns so the runtime can log / observe the
// cut without re-reading the tree.
type Result struct {
	CompactionEntry  tree.Entry
	FirstKeptEntryID string
	SummarisedCount  int
	TokensBefore     int
	TokensAfter      int
	SkippedReason    string // set when Run decided compaction wasn't needed or possible
}

// Run executes a compaction cycle on the session tree. Steps:
//  1. Replay the active branch.
//  2. Bail early if there aren't enough entries to cut.
//  3. Ask the provider to count tokens on the serialised history.
//  4. If below threshold, skip.
//  5. Determine firstKeptEntryId (keep the last KeepRecentTurns + any
//     active system.prompt entries verbatim).
//  6. Ask the provider to summarise the entries being dropped.
//  7. Append a KindCompaction entry whose parentId is the previous
//     active head — this advances the head atomically (tree.Append
//     is the active-head-advance primitive).
//
// Returns a Result with SkippedReason set when nothing was done.
func Run(ctx context.Context, t *tree.Tree, p providers.Provider, policy Policy) (Result, error) {
	if t == nil {
		return Result{}, errors.New("compactor.Run: nil tree")
	}
	if p == nil {
		return Result{}, errors.New("compactor.Run: nil provider")
	}

	entries, err := t.Replay(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("compactor.Run: replay: %w", err)
	}
	keep := policy.effectiveKeep()
	if len(entries) <= keep {
		return Result{SkippedReason: "branch shorter than keep threshold"}, nil
	}

	// Token count: concatenate serialised payloads. Crude but good
	// enough for the trigger — provider-native tokenisation would be
	// more accurate but the spec's shouldCompact formula is tolerant
	// of approximation (we only need the ordering to be roughly
	// right, not the exact count).
	payload := serialiseForTokenCount(entries)
	tokens, err := p.TokenCount(ctx, policy.Model, payload)
	if err != nil {
		return Result{}, fmt.Errorf("compactor.Run: token count: %w", err)
	}

	if !ShouldCompact(tokens, policy) {
		return Result{
			SkippedReason: fmt.Sprintf("tokens=%d under threshold (window=%d reserve=%d)",
				tokens, policy.Window, policy.effectiveReserve()),
			TokensBefore: tokens,
		}, nil
	}

	// Partition: entries[:cut] summarised, entries[cut:] kept verbatim.
	// "Cut" is the index of firstKeptEntryId.
	cut := len(entries) - keep

	// Preserve the LATEST system.prompt entry that would otherwise be
	// summarised. Spec §2.7 says "keep the active system.prompt" —
	// active = most recent. Scan forward and track the highest index
	// that is a system prompt within the summarise region; move cut
	// back to that index so the prompt (and everything after) stays
	// verbatim. Walking backward and breaking on the first match
	// would preserve the OLDEST stale prompt, not the active one.
	sysPromptIdx := -1
	for i := 0; i < cut; i++ {
		if entries[i].Kind == tree.KindSystemPrompt {
			sysPromptIdx = i
		}
	}
	if sysPromptIdx >= 0 {
		cut = sysPromptIdx
	}

	if cut <= 0 {
		return Result{SkippedReason: "nothing to summarise after system-prompt preservation"}, nil
	}

	toSummarise := entries[:cut]
	firstKept := entries[cut].ID

	// Convert tree entries to provider entries for the summariser.
	providerEntries := make([]providers.Entry, 0, len(toSummarise))
	for _, e := range toSummarise {
		providerEntries = append(providerEntries, providers.Entry{
			ID:       e.ID,
			ParentID: e.ParentID,
			Kind:     providers.EntryKind(e.Kind),
			TS:       e.TS,
			Payload:  e.Payload,
		})
	}

	// TODO(cost-accounting): attribute this call to the aspect's
	// budget. CompactionResult carries Model + tokens; the later
	// cost-tracking part needs to wire it through.
	summary, err := p.Compact(ctx, providerEntries, policy.SummaryHint)
	if err != nil {
		return Result{}, fmt.Errorf("compactor.Run: provider compact: %w", err)
	}
	if strings.TrimSpace(summary.Summary) == "" {
		return Result{}, errors.New("compactor.Run: provider returned empty summary")
	}

	// Prefer the measured pre-count from TokenCount over the
	// provider's internal transcript-estimate when stamping the
	// compaction entry — it's the real context size that triggered us.
	tokensBefore := tokens
	if summary.TokensBefore > tokensBefore {
		// Fallback: provider may have counted more accurately against
		// its own tokenizer; take the larger (defensive — prefer to
		// over-estimate past usage than under).
		tokensBefore = summary.TokensBefore
	}

	// Emit the CompactionEntry. tree.Append advances head to this entry.
	entry, err := t.Append(ctx, tree.Entry{
		Kind: tree.KindCompaction,
		TS:   time.Now().UTC(),
		Payload: map[string]any{
			"firstKeptEntryId": firstKept,
			"summary":          summary.Summary,
			"tokensBefore":     tokensBefore,
			"tokensAfter":      summary.TokensAfter,
			"model":            summary.Model,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("compactor.Run: append compaction: %w", err)
	}

	return Result{
		CompactionEntry:  entry,
		FirstKeptEntryID: firstKept,
		SummarisedCount:  len(toSummarise),
		TokensBefore:     tokensBefore,
		TokensAfter:      summary.TokensAfter,
	}, nil
}

// serialiseForTokenCount folds the entries into a single text blob
// for the token counter. The exact shape doesn't matter — it just
// needs to be representative of the on-wire context size.
func serialiseForTokenCount(entries []tree.Entry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(string(e.Kind))
		b.WriteString(": ")
		if text, ok := e.Payload["text"].(string); ok {
			b.WriteString(text)
		} else if content, ok := e.Payload["content"].(string); ok {
			b.WriteString(content)
		}
		b.WriteString("\n")
	}
	return b.String()
}

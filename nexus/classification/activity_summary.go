package classification

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/bridle"
)

// ActivitySummary summarises a stream of structured activity events
// (broker turn frames, chat frames, presence, filter decisions)
// over a time window, producing an operator-readable markdown digest
// alongside deterministic per-kind / per-aspect counts.
//
// Lane 3 of NEX-243 (NEX-246). Source-agnostic — the caller
// (Slice 2: CLI subcommand vs scheduled vs dashboard widget) maps
// its source (jsonlsink files, broker Hub, etc) into []ActivityEvent.
//
// Fail-open: on harness / parse error, the deterministic counts are
// still returned alongside a placeholder markdown blurb. Counts come
// from the input, not the model, so operator never loses the
// quantitative signal on an outage.
type ActivitySummary struct {
	Harness  *bridle.Harness
	Provider bridle.ProviderID
	Model    string

	// Logger, when set, logs window + counts + raw model output for
	// post-hoc audit.
	Logger *slog.Logger
}

// ActivityEvent is the source-agnostic projection the summariser
// consumes. The caller pre-renders one line per source frame into
// Summary — keeps the summariser independent of broker types.
type ActivityEvent struct {
	TS      time.Time
	Aspect  string
	Kind    string // "turn" | "chat" | "presence" | "filter_decision" | ...
	Summary string // 1-line human-readable description
}

// ActivitySummaryInput bundles the per-call data.
type ActivitySummaryInput struct {
	Events        []ActivityEvent
	WindowStart   time.Time
	WindowEnd     time.Time
	ModelOverride string // per-call override; empty = env/default
}

// ActivitySummaryOutput is the structured digest result.
type ActivitySummaryOutput struct {
	Markdown string // operator-readable prose digest (model-generated)
	Counts   ActivitySummaryCounts
}

// ActivitySummaryCounts are computed deterministically from the input
// events — independent of the model so they survive classifier
// errors and feed dashboard widgets directly.
type ActivitySummaryCounts struct {
	Total    int
	ByKind   map[string]int
	ByAspect map[string]int
}

// maxActivitySummaryEvents bounds the number of events sent to the
// model. Most windows sit well under this; the cap is a safety net
// against pathological bursts (1000s of turn frames in an hour). The
// counts always reflect the full input regardless of the cap.
const maxActivitySummaryEvents = 200

// Summarise runs the activity summariser. Counts are deterministic
// and always populated. Markdown comes from the model on the happy
// path; on classifier error a placeholder is returned alongside
// the counts so callers always have something to render.
func (a *ActivitySummary) Summarise(ctx context.Context, in ActivitySummaryInput) (ActivitySummaryOutput, error) {
	if len(in.Events) == 0 {
		return ActivitySummaryOutput{}, fmt.Errorf("activity summary: no events")
	}

	counts := computeActivityCounts(in.Events)

	model := ResolveModel("NEXUS_ACTIVITY_SUMMARY_MODEL", a.Model, in.ModelOverride)

	events := in.Events
	truncated := 0
	if len(events) > maxActivitySummaryEvents {
		truncated = len(events) - maxActivitySummaryEvents
		events = events[len(events)-maxActivitySummaryEvents:]
	}

	req := bridle.TurnRequest{
		AppendSystemPrompt: buildActivitySummarySystemPrompt(),
		UserMessage:        buildActivitySummaryUserMessage(events, in.WindowStart, in.WindowEnd, counts, truncated),
		Provider:           a.Provider,
		Model:              model,
		MaxSteps:           1,
	}

	result, err := a.Harness.RunTurn(ctx, req, nullRunner{}, discardSink{})
	if err != nil {
		if a.Logger != nil {
			a.Logger.Warn("activity summary: harness error — returning counts with placeholder markdown",
				"err", err, "events", len(in.Events))
		}
		return ActivitySummaryOutput{
			Markdown: "_(summary unavailable: classifier error — counts below are still accurate)_",
			Counts:   counts,
		}, nil
	}

	md := strings.TrimSpace(result.FinalText)
	if md == "" {
		md = "_(summary unavailable: empty model response — counts below are still accurate)_"
	}

	if a.Logger != nil {
		a.Logger.Info("activity summary",
			"events", len(in.Events),
			"truncated", truncated,
			"by_kind", counts.ByKind,
			"by_aspect", counts.ByAspect,
			"model", model)
	}
	return ActivitySummaryOutput{Markdown: md, Counts: counts}, nil
}

// computeActivityCounts tallies by kind + aspect. Deterministic —
// no model involved. Powers dashboard widgets independently of the
// markdown digest path.
func computeActivityCounts(events []ActivityEvent) ActivitySummaryCounts {
	c := ActivitySummaryCounts{
		Total:    len(events),
		ByKind:   map[string]int{},
		ByAspect: map[string]int{},
	}
	for _, e := range events {
		c.ByKind[e.Kind]++
		c.ByAspect[e.Aspect]++
	}
	return c
}

// buildActivitySummarySystemPrompt is the shared classifier prompt.
// Asks for short paragraphs — markdown digest is read by a human
// scanning, not a parser.
func buildActivitySummarySystemPrompt() string {
	return `You summarise a stream of activity events from a multi-aspect agent network over a time window. Produce a SHORT operator-readable markdown digest.

Structure:
- One paragraph headline of what happened in the window.
- Bulleted notable events: failures, decisions, dispatches, anomalies (NOT routine traffic).
- One closing line on anything the operator should look at on return.

Be concrete. Reference aspect names + counts. Skip "the network was active" filler.

DO NOT produce a generic intro/outro. DO NOT repeat all events verbatim — the events list is already shown alongside the digest. Output ONLY the markdown body, no fences or wrapper.`
}

// buildActivitySummaryUserMessage formats the per-call context for
// the model: window, deterministic counts (so it knows the shape),
// per-event one-liners (capped at maxActivitySummaryEvents).
func buildActivitySummaryUserMessage(events []ActivityEvent, start, end time.Time, counts ActivitySummaryCounts, truncated int) string {
	var b strings.Builder

	b.WriteString("WINDOW: ")
	b.WriteString(start.UTC().Format(time.RFC3339))
	b.WriteString(" → ")
	b.WriteString(end.UTC().Format(time.RFC3339))
	b.WriteByte('\n')

	fmt.Fprintf(&b, "TOTAL EVENTS: %d\n", counts.Total)
	if truncated > 0 {
		fmt.Fprintf(&b, "(showing tail %d of %d; older %d truncated)\n",
			len(events), counts.Total, truncated)
	}

	b.WriteString("\nBY KIND: ")
	b.WriteString(joinSortedCounts(counts.ByKind))
	b.WriteString("\nBY ASPECT: ")
	b.WriteString(joinSortedCounts(counts.ByAspect))

	b.WriteString("\n\nEVENTS:\n")
	for _, e := range events {
		fmt.Fprintf(&b, "- %s  aspect=%s  kind=%s  %s\n",
			e.TS.UTC().Format("15:04:05"),
			e.Aspect, e.Kind, e.Summary)
	}
	return b.String()
}

// joinSortedCounts renders a {key: count} map as "k=v k=v ..."
// sorted by count desc then key asc for stable prompt input.
func joinSortedCounts(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s=%d", p.k, p.v)
	}
	return strings.Join(parts, " ")
}

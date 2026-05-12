package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/observability"
)

// ANSI helpers. Kept tiny — no terminfo, no detection magic. When
// useColor is false every wrap returns the input unchanged so output
// pipes cleanly into files / less / scripts.
const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiDim      = "\x1b[2m"
	ansiRed      = "\x1b[31m"
	ansiGreen    = "\x1b[32m"
	ansiYellow   = "\x1b[33m"
	ansiCyan     = "\x1b[36m"
	ansiGrey     = "\x1b[90m"
	operatorName = "operator"
)

// renderer owns stdout writes and the per-aspect history buffer. All
// public methods serialise on mu so concurrent calls (e.g. the WS
// reader pushing a frame while the input loop fires /history) don't
// interleave ANSI output.
type renderer struct {
	mu     sync.Mutex
	out    io.Writer
	color  bool
	cap    int
	recent map[string][]observability.Frame
	// loc controls the timezone used when formatting ChatFrame timestamps.
	// Defaults to time.Local (production behaviour); tests inject time.UTC
	// for determinism without mutating the time.Local global.
	loc *time.Location
}

func newRenderer(out io.Writer, color bool, cap int) *renderer {
	if cap <= 0 {
		cap = 50
	}
	return &renderer{
		out:    out,
		color:  color,
		cap:    cap,
		recent: make(map[string][]observability.Frame),
		loc:    time.Local,
	}
}

// handleFrame renders one observability.Frame and appends it to the
// per-aspect recent buffer. currentAspect is used only for renderer
// hints (e.g. dimming frames not addressed to the focused aspect —
// for v0.1 we always render).
func (r *renderer) handleFrame(f observability.Frame, currentAspect string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.appendRecent(f)
	r.renderFrameLocked(r.out, f, currentAspect)
}

func (r *renderer) appendRecent(f observability.Frame) {
	buf := r.recent[f.Aspect]
	buf = append(buf, f)
	if len(buf) > r.cap {
		buf = buf[len(buf)-r.cap:]
	}
	r.recent[f.Aspect] = buf
}

// renderFrameLocked is the dispatch by Kind. Caller holds r.mu.
func (r *renderer) renderFrameLocked(w io.Writer, f observability.Frame, currentAspect string) {
	switch f.Kind {
	case observability.FrameChat:
		var cf observability.ChatFrame
		if err := json.Unmarshal(f.Payload, &cf); err != nil {
			r.dimf(w, "(chat decode error: %v)", err)
			return
		}
		renderChatFrame(w, f.Aspect, cf, r.color, r.loc)
	case observability.FramePresence:
		var pf observability.PresenceFrame
		if err := json.Unmarshal(f.Payload, &pf); err != nil {
			r.dimf(w, "(presence decode error: %v)", err)
			return
		}
		renderPresenceFrame(w, f.Aspect, pf, r.color)
	case observability.FrameTurn:
		var tf observability.TurnFrame
		if err := json.Unmarshal(f.Payload, &tf); err != nil {
			r.dimf(w, "(turn decode error: %v)", err)
			return
		}
		renderTurnFrame(w, f.Aspect, tf, r.color)
	default:
		r.dimf(w, "(unknown frame kind: %s)", f.Kind)
	}
}

func (r *renderer) banner(aspect string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := fmt.Sprintf("─── nexus-watch · @%s · connected · type /help ───", aspect)
	fmt.Fprintln(r.out, colorize(line, ansiCyan, r.color))
}

func (r *renderer) statusDisconnected() {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.out, colorize("─ disconnected; retrying…", ansiRed, r.color))
}

func (r *renderer) statusReconnected() {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.out, colorize("─ reconnected ─", ansiGreen, r.color))
}

func (r *renderer) systemf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.out, colorize("· "+fmt.Sprintf(format, args...), ansiGrey, r.color))
}

func (r *renderer) dimf(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, colorize(fmt.Sprintf(format, args...), ansiGrey, r.color))
}

func (r *renderer) help() {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := []string{
		"slash commands:",
		"  /switch <aspect>   change observation focus",
		"  /say <message>     send chat (same as typing without leading slash)",
		"  /new               clear last-seen msg id for current aspect (next send is top-level)",
		"  /history [N]       print last N frames from this aspect's in-memory buffer",
		"  /help              show this list",
		"  /quit              graceful exit",
	}
	for _, l := range lines {
		fmt.Fprintln(r.out, colorize(l, ansiGrey, r.color))
	}
}

func (r *renderer) history(aspect string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := r.recent[aspect]
	if n <= 0 || n > len(buf) {
		n = len(buf)
	}
	if n == 0 {
		fmt.Fprintln(r.out, colorize(fmt.Sprintf("· no history for @%s", aspect), ansiGrey, r.color))
		return
	}
	tail := buf[len(buf)-n:]
	header := fmt.Sprintf("─── history · @%s · last %d frame(s) ───", aspect, n)
	fmt.Fprintln(r.out, colorize(header, ansiCyan, r.color))
	for _, f := range tail {
		r.renderFrameLocked(r.out, f, aspect)
	}
	fmt.Fprintln(r.out, colorize("─── end of history ───", ansiCyan, r.color))
}

// -------------------------------------------------------------------
// Pure render functions. Exported in spirit (called from tests via the
// package; lowercase because the binary doesn't expose a Go API).
// -------------------------------------------------------------------

// renderChatFrame writes the ChatFrame in the format spec'd in §4.2:
//
//	< #N [@from HH:MM] content       (inbound)
//	→ #N [@from HH:MM] content       (outbound)
//	   ↳ reply to #M                  (only if ReplyTo > 0)
func renderChatFrame(w io.Writer, aspect string, cf observability.ChatFrame, color bool, loc *time.Location) {
	var marker string
	switch cf.Direction {
	case observability.DirectionOutbound:
		marker = "→"
	case observability.DirectionInbound:
		marker = "<"
	default:
		marker = "·"
	}
	if loc == nil {
		loc = time.Local
	}
	ts := cf.CreatedAt.In(loc).Format("15:04")
	from := cf.From
	fromColored := colorizeFrom(from, color)
	head := fmt.Sprintf("%s #%d [%s %s] %s",
		marker,
		cf.MsgID,
		fromColored,
		ts,
		cf.Content,
	)
	fmt.Fprintln(w, head)
	if cf.ReplyTo > 0 {
		fmt.Fprintln(w, colorize(fmt.Sprintf("   ↳ reply to #%d", cf.ReplyTo), ansiGrey, color))
	}
}

// renderPresenceFrame: single-line marker for connect/disconnect.
func renderPresenceFrame(w io.Writer, aspect string, pf observability.PresenceFrame, color bool) {
	verb := "disconnected"
	if pf.Connected {
		verb = "connected"
	}
	tail := ""
	if pf.Reason != "" {
		tail = fmt.Sprintf(" (%s)", pf.Reason)
	}
	line := fmt.Sprintf("─ presence: @%s %s%s", aspect, verb, tail)
	fmt.Fprintln(w, colorize(line, ansiGrey, color))
}

// renderTurnFrame is intentionally minimal for v0.1 — bridle events
// aren't wired so TurnFrames won't normally arrive. Render defensively
// so a stray one is legible without aspiring to Phase F's rich layout.
// renderTurnFrame writes one TurnFrame's structured content.
// Header shows aspect + label + status + model/provider/trigger.
// Body iterates events: text runs as prose, tool calls show their
// name + key arg + result preview (or pending state), step boundaries
// render as a thin dashed separator, orphan results bear an explicit
// warning. Footer reports usage stats and any final error.
func renderTurnFrame(w io.Writer, aspect string, tf observability.TurnFrame, color bool) {
	label := tf.Label
	if label == "" {
		label = "main"
	}
	trig := ""
	if tf.TriggerMsg > 0 {
		trig = fmt.Sprintf(" trigger #%d", tf.TriggerMsg)
	}
	modelBit := ""
	if tf.Model != "" {
		modelBit = " " + tf.Model
	}
	if tf.Provider != "" {
		modelBit += "/" + tf.Provider
	}
	header := fmt.Sprintf("─── turn @%s [%s · %s%s%s] ───", aspect, label, tf.Status, trig, modelBit)
	fmt.Fprintln(w, colorize(header, statusColor(tf.Status), color))

	for _, ev := range tf.Events {
		renderTurnEvent(w, ev, color)
	}

	if tf.Error != "" {
		fmt.Fprintln(w, colorize("  ✗ "+tf.Error, ansiRed, color))
	}

	footer := turnFooter(tf)
	if footer != "" {
		fmt.Fprintln(w, colorize(footer, ansiCyan, color))
	}
}

// renderTurnEvent writes one entry from TurnFrame.Events. Centralised
// so the dispatch is testable and so new event kinds can be added in
// one place when bridle grows them.
func renderTurnEvent(w io.Writer, ev observability.TurnEvent, color bool) {
	switch ev.Kind {
	case observability.TurnEventText:
		// Indent multi-line text for legibility — keeps the "💭" gutter
		// alignment intact across line breaks.
		for i, line := range strings.Split(strings.TrimRight(ev.Text, "\n"), "\n") {
			prefix := "  💭 "
			if i > 0 {
				prefix = "     "
			}
			fmt.Fprintln(w, prefix+line)
		}

	case observability.TurnEventToolCall:
		renderToolCall(w, ev.Tool, false, color)

	case observability.TurnEventOrphanResult:
		renderToolCall(w, ev.Tool, true, color)

	case observability.TurnEventStep:
		fmt.Fprintln(w, colorize(fmt.Sprintf("  ╶─ step %d ─╴", ev.Step), ansiGrey, color))
	}
}

// renderToolCall writes a tool call with its result inline. Artifact-
// bearing calls (Edit/Write/MultiEdit) prefer artifact rendering over
// raw input JSON.
func renderToolCall(w io.Writer, tc *observability.ToolCall, isOrphan bool, color bool) {
	if tc == nil {
		return
	}
	icon := "🔧"
	if isOrphan {
		icon = "⚠"
	}
	state := toolState(tc, isOrphan)
	stateColor := toolStateColor(state)
	preview := toolArgPreview(tc)
	header := fmt.Sprintf("  %s %s %s%s", icon, tc.Name, colorize("["+state+"]", stateColor, color), preview)
	fmt.Fprintln(w, header)

	if tc.Artifact != nil {
		renderArtifact(w, tc.Artifact, color)
	}
	if tc.ArtifactParseErr != "" {
		fmt.Fprintln(w, colorize("     artifact parse failed: "+tc.ArtifactParseErr, ansiYellow, color))
	}
	if tc.Result != nil {
		resColor := ansiGrey
		label := "→"
		if tc.Result.IsError {
			resColor = ansiRed
			label = "✗"
		}
		body := tc.Result.Preview
		if body == "" {
			body = "(empty)"
		}
		// Indent multi-line preview under the call header so the eye
		// follows the flow without manually mapping result to call.
		for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			prefix := "     " + label + " "
			if i > 0 {
				prefix = "       "
			}
			fmt.Fprintln(w, colorize(prefix+line, resColor, color))
		}
	}
}

// renderArtifact writes the structured-edit view. Truncates each side
// of an Edit/MultiEdit to keep the stream readable; a heavy file
// rewrite is more useful as a "this happened" signal than a full
// diff dump in a terminal.
const artifactPreviewChars = 200

func renderArtifact(w io.Writer, a *observability.Artifact, color bool) {
	if a == nil {
		return
	}
	fmt.Fprintln(w, colorize("     📄 "+a.FilePath, ansiCyan, color))
	switch a.Kind {
	case observability.ArtifactFileEdit, observability.ArtifactNotebookEdit:
		writeDiffSide(w, "-", a.OldText, ansiRed, color)
		writeDiffSide(w, "+", a.NewText, ansiGreen, color)
	case observability.ArtifactFileWrite:
		writeDiffSide(w, "+", a.NewText, ansiGreen, color)
	case observability.ArtifactMultiEdit:
		for i, ep := range a.Edits {
			fmt.Fprintln(w, colorize(fmt.Sprintf("     edit %d/%d", i+1, len(a.Edits)), ansiGrey, color))
			writeDiffSide(w, "-", ep.OldText, ansiRed, color)
			writeDiffSide(w, "+", ep.NewText, ansiGreen, color)
		}
	}
}

func writeDiffSide(w io.Writer, marker, text, lineColor string, color bool) {
	if text == "" {
		return
	}
	// Truncate by rune count, not byte index — a Go string slice mid-
	// UTF-8 sequence would emit garbled bytes before the ellipsis,
	// which a terminal renders as a replacement character. Cheap enough
	// at preview sizes (≤200 runes) to take the materialised []rune
	// allocation.
	shown := text
	if runes := []rune(text); len(runes) > artifactPreviewChars {
		shown = string(runes[:artifactPreviewChars]) + "…"
	}
	for _, line := range strings.Split(strings.TrimRight(shown, "\n"), "\n") {
		fmt.Fprintln(w, colorize("       "+marker+" "+line, lineColor, color))
	}
}

// toolState reports the rendering-relevant state for a tool call: ok,
// error, pending (no result yet), orphan (result without prior start).
func toolState(tc *observability.ToolCall, isOrphan bool) string {
	switch {
	case isOrphan:
		return "orphan"
	case tc.Result == nil:
		return "pending"
	case tc.Result.IsError:
		return "error"
	default:
		return "ok"
	}
}

func toolStateColor(state string) string {
	switch state {
	case "error", "orphan":
		return ansiRed
	case "pending":
		return ansiYellow
	case "ok":
		return ansiGreen
	default:
		return ansiGrey
	}
}

// toolArgPreview prefers the artifact's file_path when present,
// otherwise truncates the raw input JSON. Mirrors the dashboard's
// ToolCall arg-preview heuristic so operators get consistent
// scanning across the two renderers.
func toolArgPreview(tc *observability.ToolCall) string {
	if tc.Artifact != nil && tc.Artifact.FilePath != "" {
		return " " + tc.Artifact.FilePath
	}
	if len(tc.Input) == 0 {
		return ""
	}
	p := string(tc.Input)
	if len(p) > 80 {
		p = p[:77] + "..."
	}
	return " " + p
}

// statusColor maps a turn status to the header colour. Cyan for
// neutral, accent-green while in-flight, red on error.
func statusColor(status observability.TurnStatus) string {
	switch status {
	case observability.TurnInFlight:
		return ansiGreen
	case observability.TurnErrored:
		return ansiRed
	default:
		return ansiCyan
	}
}

// turnFooter assembles the closing line. Reports cache + cost when
// non-zero so the operator notices when a turn ran expensive without
// having to peek at the dashboard.
func turnFooter(tf observability.TurnFrame) string {
	if tf.Usage == nil && tf.Ended == nil {
		return ""
	}
	bits := []string{}
	if tf.Ended != nil {
		dur := tf.Ended.Sub(tf.Started).Truncate(time.Millisecond * 100)
		bits = append(bits, dur.String())
	}
	if u := tf.Usage; u != nil {
		bits = append(bits, fmt.Sprintf("%d in", u.InputTokens))
		bits = append(bits, fmt.Sprintf("%d out", u.OutputTokens))
		if u.CacheReadInputTokens > 0 {
			bits = append(bits, fmt.Sprintf("%d cache↺", u.CacheReadInputTokens))
		}
		if u.CacheCreationInputTokens > 0 {
			bits = append(bits, fmt.Sprintf("%d cache+", u.CacheCreationInputTokens))
		}
		if u.CostUSD > 0 {
			bits = append(bits, fmt.Sprintf("$%.4f", u.CostUSD))
		}
	}
	if len(bits) == 0 {
		return ""
	}
	return "─── end · " + strings.Join(bits, " · ") + " ───"
}

// colorizeFrom picks a stable colour for an aspect name via an
// FNV hash → 256-colour index. Operator gets a fixed cyan so its
// outbound messages stand out from aspect-coloured inbound ones.
func colorizeFrom(name string, color bool) string {
	display := "@" + name
	if !color {
		return display
	}
	if name == operatorName {
		return ansiCyan + ansiBold + display + ansiReset
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	// Restrict to a readable swath of 256-colour palette:
	// 17..231 skips the very-dark and the very-pale tail; 215 entries.
	idx := 17 + int(h.Sum32()%215)
	return fmt.Sprintf("\x1b[38;5;%dm%s%s", idx, display, ansiReset)
}

// colorize wraps s in the colour code when color is true; otherwise
// returns s unchanged. Used for fixed-role markers (status, history
// headers, system messages).
func colorize(s, code string, color bool) string {
	if !color {
		return s
	}
	return code + s + ansiReset
}

// stripANSI removes any CSI escape sequences. Test helper; cheap
// enough to leave at runtime for /history piping via tools that
// strip — but currently only the test uses it.
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// Skip until letter terminator (final byte 0x40..0x7e).
			j := i + 2
			for j < len(s) {
				c := s[j]
				if c >= 0x40 && c <= 0x7e {
					j++
					break
				}
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

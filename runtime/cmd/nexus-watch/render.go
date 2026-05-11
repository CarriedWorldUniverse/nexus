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
func renderTurnFrame(w io.Writer, aspect string, tf observability.TurnFrame, color bool) {
	trig := ""
	if tf.TriggerMsg > 0 {
		trig = fmt.Sprintf(" trigger #%d", tf.TriggerMsg)
	}
	model := ""
	if tf.Model != "" {
		model = " " + tf.Model
	}
	header := fmt.Sprintf("─── turn @%s [%s%s%s] ───", aspect, tf.Status, trig, model)
	fmt.Fprintln(w, colorize(header, ansiCyan, color))
	for _, ev := range tf.Events {
		switch ev.Kind {
		case observability.TurnEventText:
			fmt.Fprintln(w, "  💭 "+ev.Text)
		case observability.TurnEventToolCall:
			if ev.Tool != nil {
				preview := ""
				if len(ev.Tool.Input) > 0 {
					p := string(ev.Tool.Input)
					if len(p) > 80 {
						p = p[:77] + "..."
					}
					preview = " " + p
				}
				fmt.Fprintln(w, "  🔧 "+ev.Tool.Name+preview)
			}
		}
	}
	if tf.Usage != nil && tf.Ended != nil {
		dur := tf.Ended.Sub(tf.Started).Truncate(time.Millisecond * 100)
		footer := fmt.Sprintf("─── end · %s · %d in · %d out ───",
			dur, tf.Usage.InputTokens, tf.Usage.OutputTokens)
		fmt.Fprintln(w, colorize(footer, ansiCyan, color))
	}
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

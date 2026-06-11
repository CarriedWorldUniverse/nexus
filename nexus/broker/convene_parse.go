// convene_parse.go — parsing of the !convene chat command (roundtable
// spec component 3, P3 plan Task A).
//
//	!convene [facilitator=<name>] <a> <b> [<c>…] [lens:<p>=<text>…] — <problem>
//
// The separator between the participant head and the problem statement is
// an em-dash (—) or a bare colon token (" : "). Participants are a
// whitespace/comma list of KNOWN BASE aspects; derived hand identities
// (<parent>.<word>, carrying a dot) and unknown names are rejected — only
// base aspects argue at the roundtable. Facilitator defaults to the sender
// (resolved by the caller, not here) unless facilitator=<name> is given.
//
// The parser is pure: it does NOT touch storage, the roster, or chat. It
// takes the command line plus the set of known aspect names and returns a
// validated conveneCommand or an error. submitConvene (convene.go) drives
// the side effects.

package broker

import (
	"errors"
	"fmt"
	"strings"
)

// conveneCommand is the parsed, validated form of a !convene line.
type conveneCommand struct {
	// Facilitator is the explicit facilitator=<name> override, or "" when
	// the caller should default it (sender; operator → shadow).
	Facilitator string
	// Participants are the base aspects pulled into the roundtable, in
	// command order, deduplicated.
	Participants []string
	// Lenses maps a participant → its explicit per-participant lens text
	// (from lens:<p>=<text> segments). Absent participants get their
	// standing perspective.
	Lenses map[string]string
	// Problem is the trailing problem statement (after the separator).
	Problem string
}

// parseConveneCommand parses a !convene line. known is the set of base
// aspect names; membership gates both participants and a facilitator
// override.
func parseConveneCommand(line string, known map[string]bool) (conveneCommand, error) {
	line = strings.TrimSpace(line)
	if line != "!convene" && !strings.HasPrefix(line, "!convene ") {
		return conveneCommand{}, errors.New("convene: not a !convene command")
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "!convene"))

	head, problem, err := splitOnSeparator(rest)
	if err != nil {
		return conveneCommand{}, err
	}
	problem = strings.TrimSpace(problem)
	if problem == "" {
		return conveneCommand{}, errors.New("convene: empty problem statement after separator")
	}

	cmd := conveneCommand{Lenses: map[string]string{}}
	// facilitator=<name> may appear anywhere in the head; pull it out first so
	// the participant/lens tokeniser doesn't have to special-case it past the
	// front position.
	headTokens := strings.Fields(strings.ReplaceAll(head, ",", " "))
	var remaining []string
	for _, tok := range headTokens {
		if strings.HasPrefix(tok, "facilitator=") {
			cmd.Facilitator = strings.TrimPrefix(tok, "facilitator=")
			continue
		}
		remaining = append(remaining, tok)
	}

	cmd.Participants, cmd.Lenses, err = parseParticipantsAndLenses(remaining, known)
	if err != nil {
		return conveneCommand{}, err
	}

	if len(cmd.Participants) < 2 {
		return conveneCommand{}, fmt.Errorf("convene: needs >=2 participants, got %d", len(cmd.Participants))
	}
	if cmd.Facilitator != "" && !known[cmd.Facilitator] {
		return conveneCommand{}, fmt.Errorf("convene: facilitator %q is not a known aspect", cmd.Facilitator)
	}
	cmd.Problem = problem
	return cmd, nil
}

// splitOnSeparator splits the command body into the participant head and
// the problem statement. The separator is an em-dash (—) or a bare colon
// token surrounded by whitespace (" : "); a colon inside lens:/facilitator=
// tokens is NOT a separator. The em-dash takes precedence when both appear.
func splitOnSeparator(body string) (head, problem string, err error) {
	if i := strings.Index(body, "—"); i >= 0 {
		return body[:i], body[i+len("—"):], nil
	}
	// Bare " : " token. Search for a colon flanked by spaces so lens:foo=bar
	// (no surrounding spaces) doesn't match.
	if i := strings.Index(body, " : "); i >= 0 {
		return body[:i], body[i+len(" : "):], nil
	}
	// Trailing/leading colon forms (" :" at end is an empty problem; handled
	// by the empty-problem check upstream when we still split).
	if strings.HasSuffix(strings.TrimRight(body, " "), " :") {
		trimmed := strings.TrimRight(body, " ")
		return trimmed[:len(trimmed)-len(" :")], "", nil
	}
	return "", "", errors.New("convene: missing — or : separator before the problem statement")
}

// parseParticipantsAndLenses tokenises the participant head (facilitator
// already stripped) into base-aspect participants and lens:<p>=<text>
// segments. A lens segment extends to the next lens: token or known base
// aspect, so a multi-word lens ("play the skeptic") is captured whole;
// unknown bare words after a lens belong to that lens, not a participant.
func parseParticipantsAndLenses(toks []string, known map[string]bool) ([]string, map[string]string, error) {
	var participants []string
	seen := map[string]bool{}
	lenses := map[string]string{}

	i := 0
	for i < len(toks) {
		tok := toks[i]
		if strings.HasPrefix(tok, "lens:") {
			who, first, ok := strings.Cut(strings.TrimPrefix(tok, "lens:"), "=")
			if !ok || who == "" {
				return nil, nil, fmt.Errorf("convene: malformed lens segment %q (want lens:<aspect>=<text>)", tok)
			}
			parts := []string{first}
			i++
			for i < len(toks) {
				nt := toks[i]
				if strings.HasPrefix(nt, "lens:") || known[nt] {
					break
				}
				parts = append(parts, nt)
				i++
			}
			lenses[who] = strings.TrimSpace(strings.Join(parts, " "))
			continue
		}
		if err := validateBaseAspect(tok, known); err != nil {
			return nil, nil, err
		}
		if !seen[tok] {
			seen[tok] = true
			participants = append(participants, tok)
		}
		i++
	}
	return participants, lenses, nil
}

// validateBaseAspect rejects derived hand identities (a dot in the name)
// and names not in the known base-aspect set.
func validateBaseAspect(name string, known map[string]bool) error {
	if strings.Contains(name, ".") {
		return fmt.Errorf("convene: %q is a derived identity; only base aspects can be convened", name)
	}
	if !known[name] {
		return fmt.Errorf("convene: unknown aspect %q", name)
	}
	return nil
}

package dispatch

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Brief is one dispatch request: the structured header plus the task text.
type Brief struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider,omitempty"`
	Repo     string `json:"repo"`
	Ticket   string `json:"ticket"`
	Branch   string `json:"branch"`
	Thread   string `json:"thread"`
	Task     string `json:"-"`
}

// ParseBrief extracts either a fenced JSON header or a !dispatch command brief.
func ParseBrief(body []byte) (Brief, error) {
	s := string(body)
	if b, ok, err := parseDispatchCommand(s); ok || err != nil {
		return b, err
	}

	open := strings.Index(s, "```json")
	if open < 0 {
		return Brief{}, errors.New("dispatch: no ```json brief header")
	}
	rest := s[open+len("```json"):]
	close := strings.Index(rest, "```")
	if close < 0 {
		return Brief{}, errors.New("dispatch: unterminated ```json header")
	}

	var b Brief
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:close])), &b); err != nil {
		return Brief{}, fmt.Errorf("dispatch: bad brief header: %w", err)
	}
	b.Task = strings.TrimSpace(rest[close+3:])
	if b.Agent == "" {
		return Brief{}, errors.New("dispatch: brief.agent required")
	}
	if b.Ticket == "" {
		return Brief{}, errors.New("dispatch: brief.ticket required (idempotency key)")
	}
	if b.Thread == "" {
		b.Thread = b.Ticket
	}
	return b, nil
}

func parseDispatchCommand(s string) (Brief, bool, error) {
	line := strings.TrimSpace(s)
	if line != "!dispatch" && !strings.HasPrefix(line, "!dispatch ") {
		return Brief{}, false, nil
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return Brief{}, true, errors.New("dispatch: !dispatch requires agent and task")
	}
	target := fields[1]
	agent, provider, ok := strings.Cut(target, "%")
	if agent == "" {
		return Brief{}, true, errors.New("dispatch: !dispatch agent required")
	}
	if ok && provider == "" {
		return Brief{}, true, errors.New("dispatch: !dispatch provider required after %")
	}
	task := strings.TrimSpace(strings.TrimPrefix(line, fields[0]+" "+fields[1]))
	if task == "" {
		return Brief{}, true, errors.New("dispatch: !dispatch task required")
	}
	ticket := "dispatch-" + fmt.Sprintf("%x", sha256.Sum256([]byte(line)))[:16]
	return Brief{
		Agent:    agent,
		Provider: provider,
		Ticket:   ticket,
		Thread:   ticket,
		Task:     task,
	}, true, nil
}

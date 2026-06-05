package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Brief is one dispatch request: the structured header plus the task text.
type Brief struct {
	Agent  string `json:"agent"`
	Repo   string `json:"repo"`
	Ticket string `json:"ticket"`
	Branch string `json:"branch"`
	Thread string `json:"thread"`
	Task   string `json:"-"`
}

// ParseBrief extracts the fenced JSON header and trailing free-text task.
func ParseBrief(body []byte) (Brief, error) {
	s := string(body)
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

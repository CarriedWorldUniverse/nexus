package hooks

import (
	"encoding/json"
	"strings"
)

const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
	DecisionBlock = "block"
)

// Decision is the Claude-Code/Codex hook decision response model.
type Decision struct {
	Continue           bool   `json:"continue,omitempty"`
	Decision           string `json:"decision,omitempty"`
	PermissionDecision string `json:"permissionDecision,omitempty"`
	AdditionalContext  string `json:"additionalContext,omitempty"`
	SystemMessage      string `json:"systemMessage,omitempty"`

	continueSet bool
}

func defaultDecision() Decision {
	return Decision{Continue: true}
}

func (d Decision) WithContinue(continueValue bool) Decision {
	d.Continue = continueValue
	d.continueSet = true
	return d
}

func (d *Decision) UnmarshalJSON(data []byte) error {
	type decisionAlias Decision
	var raw struct {
		decisionAlias
		Continue *bool `json:"continue"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*d = Decision(raw.decisionAlias)
	if raw.Continue == nil {
		d.Continue = true
		return nil
	}
	d.Continue = *raw.Continue
	d.continueSet = true
	return nil
}

func normalizeDecision(d Decision) Decision {
	if !d.Continue && !d.continueSet {
		d.Continue = true
	}
	if d.Decision == "" && d.PermissionDecision != "" {
		d.Decision = d.PermissionDecision
	}
	if d.PermissionDecision == "" && d.Decision != "" {
		d.PermissionDecision = d.Decision
	}
	if d.Decision == "" {
		d.Continue = true
	}
	return d
}

// MergeDecisions combines handler decisions in registration order.
func MergeDecisions(decisions ...Decision) Decision {
	merged := defaultDecision()
	var contexts []string
	for _, next := range decisions {
		next = normalizeDecision(next)
		if strings.TrimSpace(next.AdditionalContext) != "" {
			contexts = append(contexts, strings.TrimSpace(next.AdditionalContext))
		}
		if next.SystemMessage != "" {
			merged.SystemMessage = next.SystemMessage
		}
		if !next.Continue {
			merged.Continue = false
		}
		merged.Decision = strongerDecision(merged.Decision, next.Decision)
	}
	merged.PermissionDecision = merged.Decision
	merged.AdditionalContext = strings.Join(contexts, "\n")
	return merged
}

func strongerDecision(current, next string) string {
	if decisionRank(next) > decisionRank(current) {
		return next
	}
	return current
}

func decisionRank(decision string) int {
	switch strings.ToLower(decision) {
	case DecisionBlock:
		return 3
	case DecisionDeny:
		return 2
	case DecisionAllow:
		return 1
	default:
		return 0
	}
}

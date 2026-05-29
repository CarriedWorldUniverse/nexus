package funnel

import (
	"encoding/json"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"
)

func TestPolicyEvaluate(t *testing.T) {
	p := ToolPolicy{
		DefaultAllow:   true,
		Tools:          map[string]bool{"bash": false}, // bash denied outright
		WritePathAllow: []string{"work/"},              // writes only under work/
	}
	cases := []struct {
		name  string
		call  bridle.ToolCall
		allow bool
	}{
		{"bash denied", bridle.ToolCall{Name: "bash", Args: json.RawMessage(`{"command":"id"}`)}, false},
		{"read allowed (default)", bridle.ToolCall{Name: "read", Args: json.RawMessage(`{"path":"x"}`)}, true},
		{"write inside allow", bridle.ToolCall{Name: "write", Args: json.RawMessage(`{"path":"work/a.txt","content":"x"}`)}, true},
		{"write outside allow", bridle.ToolCall{Name: "write", Args: json.RawMessage(`{"path":"etc/a.txt","content":"x"}`)}, false},
	}
	for _, c := range cases {
		got, reason := p.Evaluate(c.call)
		if got != c.allow {
			t.Errorf("%s: allow=%v want %v (reason=%q)", c.name, got, c.allow, reason)
		}
		if !got && reason == "" {
			t.Errorf("%s: deny must carry a reason", c.name)
		}
	}
}

func TestPolicyBashDenylist(t *testing.T) {
	p := ToolPolicy{DefaultAllow: true, BashDeny: []string{"rm -rf", "mkfs"}}
	if allow, _ := p.Evaluate(bridle.ToolCall{Name: "bash", Args: json.RawMessage(`{"command":"rm -rf /tmp/x"}`)}); allow {
		t.Error("bash matching denylist must be denied")
	}
	if allow, _ := p.Evaluate(bridle.ToolCall{Name: "bash", Args: json.RawMessage(`{"command":"ls"}`)}); !allow {
		t.Error("benign bash should be allowed when tool not outright denied")
	}
}

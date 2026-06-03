package funnel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bridle "github.com/CarriedWorldUniverse/bridle"

	"github.com/CarriedWorldUniverse/nexus/nexus/frames"
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

func TestPolicyJSONRoundTrip(t *testing.T) {
	// The snake_case wire shape agentfunnel's -policy file uses.
	const src = `{"default_allow":true,"tools":{"bash":false},"escalate":{"write":true},"bash_deny":["rm -rf"],"write_path_allow":["work/"]}`
	var p ToolPolicy
	if err := json.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.DefaultAllow {
		t.Error("default_allow should decode to true")
	}
	if p.Tools["bash"] {
		t.Error("tools.bash should decode to false")
	}
	if !p.Escalate["write"] {
		t.Error("escalate.write should decode to true")
	}

	cases := []struct {
		name string
		call bridle.ToolCall
		want Verdict
	}{
		{"bash denied outright", bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"ls"}`)}, VerdictDeny},
		{"write escalated (path inside allow)", bridle.ToolCall{Name: "write", Args: []byte(`{"path":"work/a.txt","content":"x"}`)}, VerdictEscalate},
		{"read allowed by default", bridle.ToolCall{Name: "read", Args: []byte(`{"path":"x"}`)}, VerdictAllow},
	}
	for _, c := range cases {
		got, reason := p.Decide(c.call)
		if got != c.want {
			t.Errorf("%s: verdict=%v want %v (reason=%q)", c.name, got, c.want, reason)
		}
	}

	// Round-trip the other direction: marshal then re-decode and confirm
	// the verdicts survive (json tags + omitempty are consistent).
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var p2 ToolPolicy
	if err := json.Unmarshal(out, &p2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if v, _ := p2.Decide(bridle.ToolCall{Name: "write", Args: []byte(`{"path":"work/a.txt"}`)}); v != VerdictEscalate {
		t.Errorf("re-decoded policy: write verdict=%v want Escalate", v)
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

func TestPermissionHookDeniesViaBridleDeny(t *testing.T) {
	p := ToolPolicy{DefaultAllow: true, Tools: map[string]bool{"bash": false}}
	hook := PermissionHook(p, nil)
	in := bridle.BeforeToolCallCtx{Call: bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"id"}`)}}
	out, action, err := hook(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if action != bridle.HookContinue {
		t.Fatalf("deny must use HookContinue+Deny, not %v", action)
	}
	if !out.Deny || out.Err == "" {
		t.Fatalf("expected Deny set with reason, got %+v", out)
	}

	in2 := bridle.BeforeToolCallCtx{Call: bridle.ToolCall{Name: "read", Args: []byte(`{"path":"x"}`)}}
	out2, _, _ := hook(context.Background(), in2)
	if out2.Deny {
		t.Fatal("allowed tool must not be denied")
	}
}

func TestPolicyDecidePrecedence(t *testing.T) {
	// Deny outranks Escalate: bash is both escalated AND denylisted.
	p := ToolPolicy{
		DefaultAllow: true,
		Tools:        map[string]bool{"forbidden": false},
		BashDeny:     []string{"rm -rf"},
		Escalate:     map[string]bool{"bash": true, "forbidden": true},
	}
	cases := []struct {
		name string
		call bridle.ToolCall
		want Verdict
	}{
		{"outright deny outranks escalate", bridle.ToolCall{Name: "forbidden"}, VerdictDeny},
		{"bash escalated when benign", bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"ls"}`)}, VerdictEscalate},
		{"bash denylist outranks escalate", bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"rm -rf /"}`)}, VerdictDeny},
		{"non-escalated tool allowed", bridle.ToolCall{Name: "read", Args: []byte(`{"path":"x"}`)}, VerdictAllow},
	}
	for _, c := range cases {
		got, reason := p.Decide(c.call)
		if got != c.want {
			t.Errorf("%s: verdict=%v want %v (reason=%q)", c.name, got, c.want, reason)
		}
		if (got == VerdictDeny || got == VerdictEscalate) && reason == "" {
			t.Errorf("%s: deny/escalate must carry a reason", c.name)
		}
	}
}

// fakeRequester is an in-process operator: it captures the request and
// returns a canned decision frame correlated to the request ID.
type fakeRequester struct {
	decision string
	note     string
	gotReq   frames.EscalationRequestPayload
	err      error
}

func (f *fakeRequester) Request(_ context.Context, env frames.Envelope) (frames.Envelope, error) {
	if f.err != nil {
		return frames.Envelope{}, f.err
	}
	_ = frames.PayloadAs(env, &f.gotReq)
	return frames.NewResponse(frames.KindEscalationDecision, env.ID, frames.EscalationDecisionPayload{
		Aspect:    f.gotReq.Aspect,
		Decision:  f.decision,
		Operator:  "test-operator",
		Note:      f.note,
		RequestID: env.ID,
	})
}

func TestPermissionHookEscalateApprove(t *testing.T) {
	p := ToolPolicy{DefaultAllow: true, Escalate: map[string]bool{"bash": true}}
	fake := &fakeRequester{decision: frames.EscalationApprove}
	hook := PermissionHook(p, &Escalator{Requester: fake, AspectID: "plumb"})

	in := bridle.BeforeToolCallCtx{Call: bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"id"}`)}}
	out, action, err := hook(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if action != bridle.HookContinue {
		t.Fatalf("approve must HookContinue, got %v", action)
	}
	if out.Deny {
		t.Fatalf("approve must NOT deny; got Deny=true Err=%q", out.Err)
	}
	// Aspect identity must have been injected (model can't forge it).
	if fake.gotReq.Aspect != "plumb" {
		t.Errorf("escalation.request Aspect=%q, want plumb (funnel-injected)", fake.gotReq.Aspect)
	}
	if fake.gotReq.Tool != "bash" {
		t.Errorf("escalation.request Tool=%q, want bash", fake.gotReq.Tool)
	}
}

func TestPermissionHookEscalateDeny(t *testing.T) {
	p := ToolPolicy{DefaultAllow: true, Escalate: map[string]bool{"bash": true}}
	fake := &fakeRequester{decision: frames.EscalationDeny, note: "not on prod"}
	hook := PermissionHook(p, &Escalator{Requester: fake, AspectID: "plumb"})

	in := bridle.BeforeToolCallCtx{Call: bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"id"}`)}}
	out, action, err := hook(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if action != bridle.HookContinue {
		t.Fatalf("deny must HookContinue, got %v", action)
	}
	if !out.Deny {
		t.Fatal("operator deny must set Deny=true")
	}
	if !strings.Contains(out.Err, "operator denied") {
		t.Errorf("Err=%q, want it to contain 'operator denied'", out.Err)
	}
	if !strings.Contains(out.Err, "not on prod") {
		t.Errorf("Err=%q, want it to surface the operator note", out.Err)
	}
}

func TestPermissionHookEscalateNilEscalatorFailsSafe(t *testing.T) {
	p := ToolPolicy{DefaultAllow: true, Escalate: map[string]bool{"bash": true}}
	hook := PermissionHook(p, nil) // no operator wire

	in := bridle.BeforeToolCallCtx{Call: bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"id"}`)}}
	out, _, err := hook(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deny {
		t.Fatal("escalate with no operator must fail-safe to deny")
	}
}

func TestPermissionHookEscalateTransportErrorFailsSafe(t *testing.T) {
	p := ToolPolicy{DefaultAllow: true, Escalate: map[string]bool{"bash": true}}
	fake := &fakeRequester{err: context.Canceled}
	hook := PermissionHook(p, &Escalator{Requester: fake, AspectID: "plumb"})

	in := bridle.BeforeToolCallCtx{Call: bridle.ToolCall{Name: "bash", Args: []byte(`{"command":"id"}`)}}
	out, _, err := hook(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deny {
		t.Fatal("escalation transport error must fail-safe to deny")
	}
	if !strings.Contains(out.Err, "escalation failed") {
		t.Errorf("Err=%q, want it to contain 'escalation failed'", out.Err)
	}
}

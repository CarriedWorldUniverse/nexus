package frames

import (
	"encoding/json"
	"testing"
)

func TestCWBPayloadsRoundTrip(t *testing.T) {
	req, err := NewRequest(KindCWBRequest, CWBRequestPayload{
		Pillar: "herald", Method: "GET", Path: "/api/me", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var rp CWBRequestPayload
	if err := PayloadAs(req, &rp); err != nil {
		t.Fatal(err)
	}
	if rp.Pillar != "herald" || rp.Method != "GET" || rp.Path != "/api/me" {
		t.Fatalf("rp=%+v", rp)
	}
	resp, err := NewResponse(KindCWBResponse, req.ID, CWBResponsePayload{Status: 200, Body: []byte(`{"id":"a1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var sp CWBResponsePayload
	if err := PayloadAs(resp, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Status != 200 || string(sp.Body) != `{"id":"a1"}` {
		t.Fatalf("sp=%+v", sp)
	}
}

func TestRunGetResultRoundTrip(t *testing.T) {
	p := RunGetResultPayload{
		Run: RunPayload{RunID: "run-a", Ticket: "NEX-1", Status: "running"},
		Timeline: []TimelineItemPayload{
			{Kind: "chat", At: 1, Chat: &ChatItemPayload{MsgID: 5, From: "shadow", Content: "!dispatch ..."}},
			{Kind: "activity", At: 2, Activity: &ActivityItemPayload{Type: "turn", Text: "thinking"}},
		},
	}
	b, _ := json.Marshal(p)
	var back RunGetResultPayload
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Run.RunID != "run-a" || len(back.Timeline) != 2 || back.Timeline[1].Activity.Type != "turn" {
		t.Fatalf("round-trip: %+v", back)
	}
}

// The read loop drops unknown kinds (forward-compat per spec §5.3), so
// forgetting IsKnown registration silently breaks a new frame. Pin the
// spawn kinds (NEX-571).
func TestSpawnKindsAreKnown(t *testing.T) {
	if !IsKnown(KindSpawnRequest) {
		t.Error("spawn.request must be registered in IsKnown or the read loop drops it")
	}
	if !IsKnown(KindSpawnResult) {
		t.Error("spawn.result must be registered in IsKnown or the read loop drops it")
	}
}

func TestSpawnPayloadsRoundTrip(t *testing.T) {
	req, err := NewRequest(KindSpawnRequest, SpawnRequestPayload{
		Brief: "summarize the runner package", Count: 2, Thread: "NEX-571",
	})
	if err != nil {
		t.Fatal(err)
	}
	var rp SpawnRequestPayload
	if err := PayloadAs(req, &rp); err != nil {
		t.Fatal(err)
	}
	if rp.Brief != "summarize the runner package" || rp.Count != 2 || rp.Thread != "NEX-571" {
		t.Fatalf("rp=%+v", rp)
	}
	resp, err := NewResponse(KindSpawnResult, req.ID, SpawnResultPayload{
		Hands: []SpawnHandle{{RunID: "run-1", Name: "plumb.sub-1"}, {Name: "plumb.sub-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sp SpawnResultPayload
	if err := PayloadAs(resp, &sp); err != nil {
		t.Fatal(err)
	}
	if len(sp.Hands) != 2 || sp.Hands[0].RunID != "run-1" || sp.Hands[1].Name != "plumb.sub-2" {
		t.Fatalf("sp=%+v", sp)
	}
	if sp.Hands[1].RunID != "" {
		t.Fatalf("queued hand should round-trip an empty RunID, got %q", sp.Hands[1].RunID)
	}
}

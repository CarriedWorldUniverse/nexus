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

package frames

import "testing"

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

package main

import (
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func conveneCallReq(args map[string]any) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "convene_close"
	req.Params.Arguments = args
	return req
}

func TestConveneClosePayloadFromArgs(t *testing.T) {
	p, err := conveneClosePayloadFromArgs(conveneCallReq(map[string]any{
		"convene_id": "cv-1", "status": "converged", "summary_msg_id": 8115,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if p.ConveneID != "cv-1" || p.Status != "converged" || p.SummaryMsgID != 8115 {
		t.Fatalf("payload = %+v", p)
	}

	if _, err := conveneClosePayloadFromArgs(conveneCallReq(map[string]any{"status": "converged"})); err == nil {
		t.Fatal("missing convene_id must error")
	}
	if _, err := conveneClosePayloadFromArgs(conveneCallReq(map[string]any{"convene_id": "cv-1", "status": "done"})); err == nil {
		t.Fatal("invalid status must error")
	}
	// summary_msg_id optional.
	if p, err := conveneClosePayloadFromArgs(conveneCallReq(map[string]any{"convene_id": "cv-1", "status": "abandoned"})); err != nil || p.SummaryMsgID != 0 {
		t.Fatalf("optional summary: p=%+v err=%v", p, err)
	}
}

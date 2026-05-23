package funnel

import (
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/bridle"
)

func TestTriageContractFor_NoTriageTool(t *testing.T) {
	inbox := []bridle.InboxItem{{MsgID: 7, From: "operator", Content: "hi"}}
	tools := []bridle.ToolDef{{Name: ToolNameSendChat}}
	if got := triageContractFor(inbox, tools); got != "" {
		t.Errorf("expected empty contract when triage tool absent; got %q", got)
	}
}

func TestTriageContractFor_NoMsgIDs(t *testing.T) {
	inbox := []bridle.InboxItem{{MsgID: 0, From: "system", Content: "synthetic"}}
	tools := []bridle.ToolDef{{Name: ToolNameTriage}}
	if got := triageContractFor(inbox, tools); got != "" {
		t.Errorf("expected empty contract when no inbox item carries a msg_id; got %q", got)
	}
}

func TestTriageContractFor_EmitsContract(t *testing.T) {
	inbox := []bridle.InboxItem{
		{MsgID: 17, From: "operator", Content: "first"},
		{MsgID: 0, From: "system", Content: "synthetic — should be skipped"},
		{MsgID: 23, From: "harrow", Content: "second"},
	}
	tools := []bridle.ToolDef{{Name: ToolNameTriage}, {Name: ToolNameSendChat}}
	got := triageContractFor(inbox, tools)
	if got == "" {
		t.Fatal("expected non-empty contract")
	}
	if !strings.Contains(got, "## Triage requirement") {
		t.Errorf("contract missing header; got %q", got)
	}
	if !strings.Contains(got, "msg_ids requiring triage this turn: 17, 23") {
		t.Errorf("contract missing msg_id list (or includes synthetic id 0); got %q", got)
	}
}

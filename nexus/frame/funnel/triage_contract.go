package funnel

import (
	"strconv"
	"strings"

	"github.com/CarriedWorldUniverse/bridle"
)

// triageContractFor returns the triage requirement prompt block the
// model must see when the funnel registered the triage tool and the
// inbox carries at least one chat msg_id.
//
// Lived in bridle's lowerRequest until 2026-05-23. Moved here because
// the contract is funnel policy (see 2026-05-10-funnel-triage-contract.md
// + ToolNameTriage in comms.go), not harness mechanism — bridle's own
// contract ("send_comms is just a tool the funnel supplies — bridle
// has no special case") was being violated.
//
// Returns empty string when:
//   - the triage tool is not in the configured tool set (e.g.
//     claudecode-backed funnels run with nil Tools because the
//     subprocess owns its tool surface natively — telling the model to
//     call triage() when triage isn't callable just makes it loop), or
//   - no inbox item has a chat msg_id (synthetic/internal items don't
//     participate in triage).
func triageContractFor(inbox []bridle.InboxItem, tools []bridle.ToolDef) string {
	hasTriage := false
	for _, t := range tools {
		if t.Name == ToolNameTriage {
			hasTriage = true
			break
		}
	}
	if !hasTriage {
		return ""
	}
	var ids []int64
	for _, item := range inbox {
		if item.MsgID > 0 {
			ids = append(ids, item.MsgID)
		}
	}
	if len(ids) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Triage requirement\n")
	sb.WriteString("You MUST call triage(msg_id, decision, reason) once for EVERY chat msg_id above before this turn ends.\n")
	sb.WriteString("  - decision=\"reply\" if you used send_chat to address that msg_id (cite it via reply_to or in-content reference)\n")
	sb.WriteString("  - decision=\"skip\" with a reason for any message you intentionally do not reply to\n")
	sb.WriteString("Skip reasons: addressed_to_other, acknowledgement_only, out_of_scope, duplicate, noise, or a freeform sentence.\n")
	sb.WriteString("msg_ids requiring triage this turn: ")
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	sb.WriteString("\n")
	return sb.String()
}

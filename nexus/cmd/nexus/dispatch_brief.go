// Dispatch-brief construction for the Keel queue manager (NEX-262).
//
// The Keel queue-manager loop polls the ledger for ready tickets and
// hands them to assignee aspects via this brief. Pre-NEX-262, the
// brief carried only Key/Summary/Priority/Status/DefinitionOfDone —
// aspects routinely re-derived context already on the ticket because
// Description/Type/ParentKey/Reporter weren't surfaced.
//
// This file isolates the formatter so it can be unit-tested without
// spinning up the whole queue-manager loop.

package main

import (
	"fmt"
	"strings"

	"github.com/CarriedWorldUniverse/ledger"
)

// buildDispatchBrief formats the chat-content payload the queue-manager
// hands to an assignee aspect. The shape mimics a human-typed chat
// message so the receiving aspect's funnel parses it like any other
// inbound work item:
//
//   @<assignee> [TICKET <key>] <summary>
//
//   Type: <type>
//   Priority: <priority>
//   Status: <status>
//   Parent: <parent>           (omitted when empty)
//   Reporter: <reporter>       (omitted when empty)
//
//   Description:
//   <description>              (whole section omitted when empty)
//
//   Definition of Done:
//   <dod>
//
// Caller is responsible for filtering out tickets without an assignee
// or DoD before invoking — this formatter assumes both are present.
func buildDispatchBrief(issue *ledger.Issue) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "@%s [TICKET %s] %s\n", issue.AssigneeAspect, issue.Key, issue.Summary)
	sb.WriteString("\n")
	if issue.Type != "" {
		fmt.Fprintf(&sb, "Type: %s\n", issue.Type)
	}
	fmt.Fprintf(&sb, "Priority: %s\n", issue.Priority)
	fmt.Fprintf(&sb, "Status: %s\n", issue.Status)
	if issue.ParentKey != "" {
		fmt.Fprintf(&sb, "Parent: %s\n", issue.ParentKey)
	}
	if issue.Reporter != "" {
		fmt.Fprintf(&sb, "Reporter: %s\n", issue.Reporter)
	}
	if strings.TrimSpace(issue.Description) != "" {
		fmt.Fprintf(&sb, "\nDescription:\n%s\n", issue.Description)
	}
	fmt.Fprintf(&sb, "\nDefinition of Done:\n%s", issue.DefinitionOfDone)
	return sb.String()
}

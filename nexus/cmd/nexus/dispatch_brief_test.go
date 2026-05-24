package main

import (
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/ledger"
)

func TestBuildDispatchBrief_FullyPopulated(t *testing.T) {
	issue := &ledger.Issue{
		Key:              "NEX-999",
		Type:             "Story",
		Status:           "Ready",
		Summary:          "Build the thing",
		Description:      "Long body explaining what the thing is.",
		Priority:         "High",
		AssigneeAspect:   "anvil",
		Reporter:         "shadow",
		ParentKey:        "NEX-100",
		DefinitionOfDone: "- Thing exists\n- Tests pass",
	}
	got := buildDispatchBrief(issue)

	// All fields appear, each on its own labeled line where applicable.
	for _, want := range []string{
		"@anvil [TICKET NEX-999] Build the thing",
		"Type: Story",
		"Priority: High",
		"Status: Ready",
		"Parent: NEX-100",
		"Reporter: shadow",
		"Description:\nLong body explaining what the thing is.",
		"Definition of Done:\n- Thing exists\n- Tests pass",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestBuildDispatchBrief_OmitsEmptyOptionalFields(t *testing.T) {
	issue := &ledger.Issue{
		Key:              "NEX-1",
		Status:           "Ready",
		Summary:          "Minimal ticket",
		Priority:         "Medium",
		AssigneeAspect:   "keel",
		DefinitionOfDone: "Done.",
		// Type, Description, ParentKey, Reporter all empty.
	}
	got := buildDispatchBrief(issue)

	// Required lines still render.
	for _, want := range []string{
		"@keel [TICKET NEX-1] Minimal ticket",
		"Priority: Medium",
		"Status: Ready",
		"Definition of Done:\nDone.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief missing required %q\n--- got ---\n%s", want, got)
		}
	}
	// Empty optional fields must NOT produce dangling labels.
	for _, unwanted := range []string{
		"Type:",
		"Parent:",
		"Reporter:",
		"Description:",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("brief contains unexpected empty-field label %q\n--- got ---\n%s", unwanted, got)
		}
	}
}

func TestBuildDispatchBrief_WhitespaceOnlyDescriptionTreatedAsEmpty(t *testing.T) {
	issue := &ledger.Issue{
		Key:              "NEX-2",
		Status:           "Ready",
		Summary:          "Trim test",
		Priority:         "Low",
		AssigneeAspect:   "harrow",
		Description:      "   \n\t  \n",
		DefinitionOfDone: "x",
	}
	got := buildDispatchBrief(issue)
	if strings.Contains(got, "Description:") {
		t.Errorf("whitespace-only description should be omitted\n--- got ---\n%s", got)
	}
}

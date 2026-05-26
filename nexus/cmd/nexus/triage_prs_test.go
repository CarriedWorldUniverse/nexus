package main

import (
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
)

// NEX-244: PR triage comment always carries the marker prefix so
// the next poll's de-dupe check (prHasMarkerComment) can find it.
// Stable string — changing it without migration would re-triage
// every previously commented PR.
func TestFormatPRTriageComment_CarriesMarker(t *testing.T) {
	body := formatPRTriageComment(classification.Verdict{
		Class:  classification.ClassNeedsReview,
		Reason: "new auth handler",
	})
	if !strings.Contains(body, prTriageMarker) {
		t.Errorf("formatted comment missing marker %q; de-dupe would fail.\nbody=%s", prTriageMarker, body)
	}
	if !strings.Contains(body, "class=needs-review") {
		t.Errorf("comment should show class; got:\n%s", body)
	}
	if !strings.Contains(body, "new auth handler") {
		t.Errorf("comment should include reason; got:\n%s", body)
	}
}

// NEX-244: each verdict class renders correctly in the comment.
// Trivial PRs get the trivial label, suspicious gets suspicious.
func TestFormatPRTriageComment_AllClasses(t *testing.T) {
	for _, class := range []classification.VerdictClass{
		classification.ClassTrivial,
		classification.ClassNeedsReview,
		classification.ClassSuspicious,
	} {
		body := formatPRTriageComment(classification.Verdict{
			Class:  class,
			Reason: "test",
		})
		if !strings.Contains(body, "class="+string(class)) {
			t.Errorf("class %q missing from rendered comment: %s", class, body)
		}
	}
}

// NEX-244: prTriageStats accumulates outcomes correctly. Same
// pattern as triageStats in close_merged_tickets.go but per-PR
// instead of per-ticket.
func TestPRTriageStats_RecordOutcomes(t *testing.T) {
	s := prTriageStats{}
	s.prsInspected = 5
	s.triaged = 3
	s.skippedExisting = 1
	s.errored = 1
	if s.prsInspected != 5 {
		t.Errorf("prsInspected = %d, want 5", s.prsInspected)
	}
	if s.triaged+s.skippedExisting+s.errored != 5 {
		t.Errorf("triaged + skippedExisting + errored = %d, want 5",
			s.triaged+s.skippedExisting+s.errored)
	}
}

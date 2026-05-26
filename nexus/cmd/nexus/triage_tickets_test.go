package main

import (
	"strings"
	"testing"

	"github.com/CarriedWorldUniverse/nexus/nexus/classification"
)

// NEX-247 Slice 2: triage comment always carries the marker prefix
// so the next poll's de-dupe check can find it. Stable string —
// changing it without migration would re-triage every previously
// commented ticket.
func TestFormatTriageComment_CarriesMarker(t *testing.T) {
	body := formatTriageComment(classification.TicketTriageVerdict{
		AssigneeAspect: "keel",
		Confidence:     "high",
		Reason:         "broker change",
	})
	if !strings.Contains(body, triageMarker) {
		t.Errorf("formatted comment missing marker %q; de-dupe would fail.\nbody=%s", triageMarker, body)
	}
	if !strings.Contains(body, "aspect=keel") {
		t.Errorf("comment should show aspect routing; got:\n%s", body)
	}
	if !strings.Contains(body, "confidence: high") {
		t.Errorf("comment should show confidence; got:\n%s", body)
	}
	if !strings.Contains(body, "broker change") {
		t.Errorf("comment should include reason; got:\n%s", body)
	}
}

// NEX-247 Slice 2: team-routing verdict renders team= prefix (not aspect=).
func TestFormatTriageComment_TeamRouting(t *testing.T) {
	body := formatTriageComment(classification.TicketTriageVerdict{
		AssigneeTeam: "oss-nexus-dev",
		Confidence:   "low",
		Reason:       "ambiguous",
	})
	if !strings.Contains(body, "team=oss-nexus-dev") {
		t.Errorf("team verdict should render team= prefix; got:\n%s", body)
	}
	if strings.Contains(body, "aspect=") {
		t.Errorf("team verdict should NOT contain aspect= prefix; got:\n%s", body)
	}
}

// NEX-247 Slice 2: triageStats records each outcome in the right
// bucket + the operator summary line reflects the count accurately.
func TestTriageStats_RecordOutcomes(t *testing.T) {
	s := triageStats{}
	for i := 0; i < 3; i++ {
		s.record(outcomeTriaged)
	}
	for i := 0; i < 2; i++ {
		s.record(outcomeSkippedExisting)
	}
	s.record(outcomeErrored)

	if s.processed != 6 {
		t.Errorf("processed = %d, want 6", s.processed)
	}
	if s.triaged != 3 {
		t.Errorf("triaged = %d, want 3", s.triaged)
	}
	if s.skippedExisting != 2 {
		t.Errorf("skippedExisting = %d, want 2", s.skippedExisting)
	}
	if s.errored != 1 {
		t.Errorf("errored = %d, want 1", s.errored)
	}
}

// NEX-247 Slice 2: adfWalk extracts plain text from typical Jira
// ADF responses (paragraph + text node tree). Used both for the
// description sent to the classifier AND for de-dupe-checking
// existing comments.
func TestADFWalk_ParagraphsToText(t *testing.T) {
	adf := map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []any{
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "First line."},
				},
			},
			map[string]any{
				"type": "paragraph",
				"content": []any{
					map[string]any{"type": "text", "text": "Second line."},
				},
			},
		},
	}
	got := adfWalk(adf)
	if !strings.Contains(got, "First line.") || !strings.Contains(got, "Second line.") {
		t.Errorf("text extraction lost content; got %q", got)
	}
}

// NEX-247 Slice 2: adfWalk handles nil + empty maps gracefully —
// some Jira fields (description on freshly-created tickets) arrive
// as null. Don't panic.
func TestADFWalk_HandlesNilAndEmpty(t *testing.T) {
	if got := adfWalk(nil); got != "" {
		t.Errorf("nil adf should produce empty string; got %q", got)
	}
	if got := adfWalk(map[string]any{}); got != "" {
		t.Errorf("empty adf should produce empty string; got %q", got)
	}
}

// NEX-247 Slice 2: adfFromPlain produces a structurally valid ADF
// document with paragraph nodes per line. Jira rejects malformed
// ADF with a 400, so this is a real wire-shape requirement.
func TestADFFromPlain_StructuralShape(t *testing.T) {
	doc := adfFromPlain("line one\n\nline three")
	if doc["type"] != "doc" {
		t.Errorf("root type = %v, want doc", doc["type"])
	}
	if v, _ := doc["version"]; v != 1 {
		t.Errorf("version = %v, want 1", v)
	}
	content, ok := doc["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content not a slice of maps; got %T", doc["content"])
	}
	if len(content) != 3 { // "line one", empty line, "line three"
		t.Errorf("content blocks = %d, want 3 (incl. blank line as empty paragraph)", len(content))
	}
	for _, block := range content {
		if block["type"] != "paragraph" {
			t.Errorf("block type = %v, want paragraph", block["type"])
		}
	}
}

// NEX-247 Slice 2: a comment we just posted (via formatTriageComment)
// must round-trip correctly through adfFromPlain → adfWalk so the
// next poll's de-dupe sees the marker. Pin the round-trip.
func TestTriageMarker_SurvivesADFRoundTrip(t *testing.T) {
	body := formatTriageComment(classification.TicketTriageVerdict{
		AssigneeAspect: "keel",
		Confidence:     "high",
		Reason:         "test",
	})
	adf := adfFromPlain(body)
	extracted := adfWalk(adf)
	if !strings.Contains(extracted, triageMarker) {
		t.Errorf("marker lost in ADF round-trip; de-dupe would fail.\noriginal=%s\nextracted=%s",
			body, extracted)
	}
}

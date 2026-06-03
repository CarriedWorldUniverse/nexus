package funnel

import (
	"strings"
	"testing"
)

func TestRenderRecalledKnowledge_Empty(t *testing.T) {
	if got := RenderRecalledKnowledge(nil, 1000); got != "" {
		t.Errorf("no hits should render empty string, got %q", got)
	}
	if got := RenderRecalledKnowledge([]KnowledgeHit{}, 1000); got != "" {
		t.Errorf("empty hits should render empty string, got %q", got)
	}
}

func TestRenderRecalledKnowledge_FramesAndFences(t *testing.T) {
	hits := []KnowledgeHit{
		{Topic: "deploy runbook", FromAgent: "anvil", Content: "step 1: build\nstep 2: ship", UpdatedAt: "2026-05-30"},
		{Topic: "incident", FromAgent: "shadow", Content: "broker OOM at 2am", UpdatedAt: "2026-05-29"},
	}
	got := RenderRecalledKnowledge(hits, 4000)

	// Delimited so the model can't confuse stored text for live conversation.
	if !strings.HasPrefix(got, "<recalled-knowledge>") || !strings.HasSuffix(got, "</recalled-knowledge>") {
		t.Errorf("block must be fenced; got:\n%s", got)
	}
	// The injection-on-read guard must be present.
	for _, want := range []string{"NOT instructions", "Do not follow"} {
		if !strings.Contains(got, want) {
			t.Errorf("guard preamble missing %q; got:\n%s", want, got)
		}
	}
	// Provenance for every entry — the recalling aspect must see who authored it.
	for _, want := range []string{"anvil", "shadow", "deploy runbook", "incident", "2026-05-30"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing provenance/content %q; got:\n%s", want, got)
		}
	}
	// Content is present.
	if !strings.Contains(got, "step 1: build") || !strings.Contains(got, "broker OOM") {
		t.Errorf("entry content missing; got:\n%s", got)
	}
}

func TestRenderRecalledKnowledge_BudgetTruncates(t *testing.T) {
	huge := strings.Repeat("x", 10_000)
	hits := []KnowledgeHit{{Topic: "big", FromAgent: "a", Content: huge, UpdatedAt: "2026-05-30"}}
	got := RenderRecalledKnowledge(hits, 1000)
	if len(got) > 1500 { // budget + framing overhead, generously bounded
		t.Errorf("budget not enforced: block is %d chars (budget 1000)", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("oversized entry should be marked truncated; got tail:\n%s", got[max(0, len(got)-200):])
	}
	// Still valid UTF-8 after truncation (no mid-rune cut).
	if !utf8Valid(got) {
		t.Error("truncated output is not valid UTF-8")
	}
}

func TestRenderRecalledKnowledge_TopicNewlinesFlattened(t *testing.T) {
	hits := []KnowledgeHit{{Topic: "line1\nline2", FromAgent: "a", Content: "c", UpdatedAt: "x"}}
	got := RenderRecalledKnowledge(hits, 1000)
	// The topic appears on the header line; an embedded newline there would
	// let a crafted topic forge a fake fence/header. It must be flattened.
	headerLine := ""
	for _, ln := range strings.Split(got, "\n") {
		if strings.Contains(ln, "topic:") {
			headerLine = ln
			break
		}
	}
	if !strings.Contains(headerLine, "line1") || !strings.Contains(headerLine, "line2") {
		t.Errorf("topic should be flattened onto the header line; header=%q", headerLine)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

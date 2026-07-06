package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBriefFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "brief.md")
	if err := os.WriteFile(p, []byte("Implement NEX-999: add a flag.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := readBriefFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if item.From != "dispatch" {
		t.Errorf("From = %q, want dispatch", item.From)
	}
	if item.Content == "" || item.Content[:9] != "Implement" {
		t.Errorf("Content = %q, want the brief text", item.Content)
	}
}

func TestReadBriefFile_Missing(t *testing.T) {
	if _, err := readBriefFile("/no/such/brief"); err == nil {
		t.Fatal("expected error for missing brief file")
	}
}

// TestWithBranchInstruction (NET-46 live evidence): instructing the model up
// front which branch to use is the cheap half of the fix — tolerating a
// nonstandard branch in prExists is the other half.
func TestWithBranchInstruction(t *testing.T) {
	got := withBranchInstruction("Implement NEX-999.", "org/repo", "builder/NEX-999")
	if got == "Implement NEX-999." {
		t.Fatal("expected branch instruction to be appended")
	}
	if !strings.Contains(got, "builder/NEX-999") {
		t.Errorf("content missing branch name: %q", got)
	}
	if !strings.HasPrefix(got, "Implement NEX-999.") {
		t.Errorf("original content not preserved: %q", got)
	}
}

func TestWithBranchInstruction_RepoLessUnchanged(t *testing.T) {
	if got := withBranchInstruction("hello", "", "builder/NEX-1"); got != "hello" {
		t.Errorf("repo-less content should pass through unchanged, got %q", got)
	}
	if got := withBranchInstruction("hello", "org/repo", ""); got != "hello" {
		t.Errorf("branch-less content should pass through unchanged, got %q", got)
	}
}

package main

import (
	"os"
	"path/filepath"
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

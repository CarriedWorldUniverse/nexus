package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPendingFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pending")
	if pendingExists(p) {
		t.Fatal("fresh dir: no pending")
	}
	if err := setPending(p, true); err != nil {
		t.Fatal(err)
	}
	if !pendingExists(p) {
		t.Fatal("pending should exist after set true")
	}
	if err := setPending(p, false); err != nil {
		t.Fatal(err)
	}
	if pendingExists(p) {
		t.Fatal("pending should be cleared")
	}
	_ = os.RemoveAll(dir)
}

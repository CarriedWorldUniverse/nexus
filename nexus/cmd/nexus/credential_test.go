package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Safer-secrets follow-up to NEX-303: readBundle resolves the
// credential bundle from exactly one of three sources. Inline mode
// is retained for non-secret bundles + back-compat; file/stdin are
// the safe paths for real secrets (no shell history / no ps output).

// inline + file + stdin all unset -> error.
func TestReadBundle_NoneSet_Errors(t *testing.T) {
	_, err := readBundle("", "", false)
	if err == nil {
		t.Fatal("expected error when no bundle source set")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error should mention exactly-one; got %v", err)
	}
}

// More than one source set -> error (mutually exclusive).
func TestReadBundle_MultipleSet_Errors(t *testing.T) {
	cases := []struct {
		name             string
		inline, filePath string
		stdin            bool
	}{
		{"inline + file", `{}`, "/tmp/x", false},
		{"inline + stdin", `{}`, "", true},
		{"file + stdin", "", "/tmp/x", true},
		{"all three", `{}`, "/tmp/x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := readBundle(c.inline, c.filePath, c.stdin)
			if err == nil {
				t.Errorf("expected error when multiple bundle sources set")
			}
		})
	}
}

// Inline mode returns the JSON verbatim (preserves back-compat).
func TestReadBundle_Inline(t *testing.T) {
	want := `{"key":"sk-test"}`
	got, err := readBundle(want, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

// File mode reads the JSON from disk — the secret never crosses the
// process arg boundary. Operator-supplied 0600 file (or process
// substitution / pass show >(tee) variants).
func TestReadBundle_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	want := `{"atlassian_token":"super-secret-token","atlassian_email":"x@y.z","atlassian_subdomain":"acme"}`
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readBundle("", path, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

// Missing file -> useful error message that mentions the path so
// operator can spot a typo without guessing.
func TestReadBundle_File_NotFound(t *testing.T) {
	_, err := readBundle("", "/nonexistent/path/bundle.json", false)
	if err == nil {
		t.Fatal("expected error on missing file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/path/bundle.json") {
		t.Errorf("error should mention the file path for operator; got %v", err)
	}
}

// Stdin mode: redirect os.Stdin to a pipe carrying the JSON, then
// invoke readBundle. Verifies the io.ReadAll(os.Stdin) path works
// end-to-end + composability with `pass show ... | nexus credential set`.
func TestReadBundle_Stdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })

	want := `{"key":"sk-via-stdin"}`
	go func() {
		_, _ = w.WriteString(want)
		_ = w.Close()
	}()

	got, err := readBundle("", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

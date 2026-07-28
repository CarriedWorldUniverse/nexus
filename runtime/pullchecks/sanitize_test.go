package pullchecks

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain literal passes through", "pr-exists", "pr-exists"},
		{"newline stripped", "pr\nexists", "prexists"},
		{"tab stripped", "pr\texists", "prexists"},
		{"ESC (ANSI) stripped", "pr\x1b[31mexists", "pr[31mexists"},
		{"CR stripped", "pr\rexists", "prexists"},
		{"spaces preserved (not control)", "pr exists ok", "pr exists ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeName(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeNameCapRespected(t *testing.T) {
	in := strings.Repeat("n", 500)
	got := SanitizeName(in)
	if len(got) > maxNameBytes {
		t.Fatalf("SanitizeName length = %d, want <= %d", len(got), maxNameBytes)
	}
	if len(got) >= maxNameBytes {
		t.Fatalf("SanitizeName length = %d, want strictly under the server cap (margin) — got no margin", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("SanitizeName produced invalid UTF-8")
	}
}

func TestSanitizeNameStripsNonPrintable(t *testing.T) {
	// cairn's stricter name rule rejects ANY non-printable rune, a superset
	// of control characters — e.g. a Unicode zero-width space, which is not
	// unicode.IsPrint despite not being a control character either.
	in := "ci" + string(rune(0x200B)) + "check"
	got := SanitizeName(in)
	if strings.ContainsFunc(got, func(r rune) bool { return !unicode.IsPrint(r) }) {
		t.Errorf("SanitizeName(%q) = %q still contains a non-printable rune", in, got)
	}
}

func TestSanitizeSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text passes through", "acceptance criteria met: all checks green", "acceptance criteria met: all checks green"},
		{"newline stripped (multi-line summary)", "line one\nline two", "line oneline two"},
		{"CRLF stripped", "a\r\nb", "ab"},
		{"punctuation and spaces preserved", "3/5 tests passed, 2 skipped.", "3/5 tests passed, 2 skipped."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeSummary(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSummaryCapRespected(t *testing.T) {
	in := strings.Repeat("s", 20000)
	got := SanitizeSummary(in)
	if len(got) > maxSummaryBytes {
		t.Fatalf("SanitizeSummary length = %d, want <= %d", len(got), maxSummaryBytes)
	}
	if len(got) >= maxSummaryBytes {
		t.Fatalf("SanitizeSummary length = %d, want strictly under the server cap (margin)", len(got))
	}
}

func TestSanitizeEvidenceURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain url passes through", "https://cairn.example/org/repo/pull/1", "https://cairn.example/org/repo/pull/1"},
		{"newline stripped", "https://x/\nevil", "https://x/evil"},
		{"ESC stripped", "https://x/\x1b[0m", "https://x/[0m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeEvidenceURL(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeEvidenceURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeEvidenceURLCapRespected(t *testing.T) {
	in := "https://x/" + strings.Repeat("u", 5000)
	got := SanitizeEvidenceURL(in)
	if len(got) > maxEvidenceURLBytes {
		t.Fatalf("SanitizeEvidenceURL length = %d, want <= %d", len(got), maxEvidenceURLBytes)
	}
	if len(got) >= maxEvidenceURLBytes {
		t.Fatalf("SanitizeEvidenceURL length = %d, want strictly under the server cap (margin)", len(got))
	}
}

func TestTruncateUTF8NeverSplitsRune(t *testing.T) {
	// A multibyte rune ("é" is 2 bytes in UTF-8) sitting right at the
	// truncation boundary must not be split into invalid UTF-8.
	in := strings.Repeat("a", 9) + "é" // byte 10-11 is the 2-byte rune
	got := truncateUTF8(in, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateUTF8(%q, 10) = %q is not valid UTF-8", in, got)
	}
	if len(got) > 10 {
		t.Fatalf("truncateUTF8 length = %d, want <= 10", len(got))
	}
}

func TestTruncateUTF8ShortStringUnchanged(t *testing.T) {
	if got := truncateUTF8("short", 100); got != "short" {
		t.Errorf("truncateUTF8 = %q, want unchanged %q", got, "short")
	}
}

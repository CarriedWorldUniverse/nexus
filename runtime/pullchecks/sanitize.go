package pullchecks

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Field caps mirror cairn-server's RecordPullCheck validation exactly (see
// cairn internal/grpcapi/pull.go's maxCheckNameBytes/maxCheckSummaryBytes/
// maxCheckEvidenceURLBytes) — the server rejects anything over these with
// codes.InvalidArgument.
const (
	maxNameBytes        = 128
	maxSummaryBytes     = 8192
	maxEvidenceURLBytes = 2048

	// sanitizeMargin is subtracted from each server-side cap before this
	// client truncates, so a boundary miscount here (multi-byte rune,
	// off-by-one) can never round-trip back up to the server's exact cap and
	// trip its InvalidArgument check. The margin is intentionally small —
	// this is a safety buffer, not a meaningfully tighter limit.
	sanitizeMargin = 8
)

// stripControl removes every Unicode control rune (tab, newline, CR, ESC,
// etc. — unicode.IsControl) from s. Plain spaces are not control characters
// and are left alone. Mirrors cairn-server's hasControlRune check, which
// gates RecordPullCheck's summary/evidence_url fields.
func stripControl(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stripNonPrintable removes every rune that is not unicode.IsPrint —
// cairn-server's stricter check-name rule (a superset of stripControl:
// non-printable also excludes control characters). Mirrors
// hasControlOrNonPrintableRune, which gates RecordPullCheck's name field.
func stripNonPrintable(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// truncateUTF8 truncates s to at most maxBytes bytes without splitting a
// multi-byte rune in half (which would otherwise leave invalid UTF-8 at the
// tail).
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}

// SanitizeName makes s safe for RecordPullCheckRequest.Name: strips every
// non-printable rune (cairn-server rejects any with codes.InvalidArgument —
// see hasControlOrNonPrintableRune) and truncates to the server's 128-byte
// cap, minus sanitizeMargin. The broker's gate names are small fixed
// literals ("pr-exists", "pr-substantial", "acceptance-judge",
// "test-evidence"); this exists so the recorder NEVER trips the server's
// validation regardless of what a future caller passes.
func SanitizeName(s string) string {
	return truncateUTF8(stripNonPrintable(s), maxNameBytes-sanitizeMargin)
}

// SanitizeSummary makes s safe for RecordPullCheckRequest.Summary: strips
// control characters (see hasControlRune) and truncates to the server's
// 8192-byte cap, minus sanitizeMargin. Ordinary printable text, including
// spaces and punctuation, passes through unchanged.
func SanitizeSummary(s string) string {
	return truncateUTF8(stripControl(s), maxSummaryBytes-sanitizeMargin)
}

// SanitizeEvidenceURL makes s safe for RecordPullCheckRequest.EvidenceUrl:
// strips control characters and truncates to the server's 2048-byte cap,
// minus sanitizeMargin.
func SanitizeEvidenceURL(s string) string {
	return truncateUTF8(stripControl(s), maxEvidenceURLBytes-sanitizeMargin)
}

package dispatch

import "testing"

// TestSelectPRURLByTicket (NET-46 live evidence): anvil-builder committed to
// its own branch (anvil/workers-json-flag) and opened a real PR (#413) that
// the conventional builder/<ticket> head-branch check missed entirely.
// selectPRURLByTicket is the pure fallback matcher — searches by ticket ID
// in the head branch name or title.
func TestSelectPRURLByTicket(t *testing.T) {
	out := []byte(`[
		{"number":413,"headRefName":"anvil/workers-json-flag","title":"NET-46: add workers json flag","url":"https://github.com/org/repo/pull/413"},
		{"number":9,"headRefName":"builder/OTHER-1","title":"unrelated","url":"https://github.com/org/repo/pull/9"}
	]`)

	url, err := selectPRURLByTicket(out, "NET-46")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if url != "https://github.com/org/repo/pull/413" {
		t.Fatalf("url = %q, want pull/413", url)
	}

	url, err = selectPRURLByTicket(out, "NEX-999")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if url != "" {
		t.Fatalf("url = %q, want empty (no match)", url)
	}
}

func TestSelectPRURLByTicketBadJSON(t *testing.T) {
	if _, err := selectPRURLByTicket([]byte("not json"), "NET-46"); err == nil {
		t.Fatal("expected parse error")
	}
}

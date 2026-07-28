package broker

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's typefaces are self-hosted, which only works if they are
// actually inside the embedded FS the broker serves. A missing embed shows
// up in a browser as a silent fallback to system fonts — exactly the
// failure mode self-hosting was meant to remove — so assert it here.
func TestDashboardFontsAreEmbedded(t *testing.T) {
	var woff2, licences int
	err := fs.WalkDir(dashboardFS, "static/dashboard/fonts", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(p, ".woff2"):
			woff2++
		case strings.HasPrefix(d.Name(), "OFL-"):
			licences++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fonts directory is not embedded: %v", err)
	}
	if woff2 != 26 {
		t.Errorf("embedded woff2 files = %d, want 26", woff2)
	}
	if licences != 3 {
		t.Errorf("embedded OFL licences = %d, want 3 (redistribution requires them)", licences)
	}

	// And the stylesheet that points at them must ship too, with no remote
	// import left behind.
	css, err := fs.ReadFile(dashboardFS, "static/dashboard/css/fonts.css")
	if err != nil {
		t.Fatalf("css/fonts.css not embedded: %v", err)
	}
	if !strings.Contains(string(css), "@font-face") {
		t.Error("css/fonts.css carries no @font-face rules")
	}
	tokens, err := fs.ReadFile(dashboardFS, "static/dashboard/css/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tokens), "fonts.googleapis.com") {
		t.Error("tokens.css still imports fonts from a remote host")
	}
}

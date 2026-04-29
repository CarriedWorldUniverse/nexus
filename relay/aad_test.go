package relay

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestMakeAADConcatenatesNoSeparator(t *testing.T) {
	pathID := "nxc_pGSqjw9XR86lzR8XTry2wkFGkvrvISTgkNGlGnuYmCo"
	msgID := "01979b0a-c0de-7eef-a000-000000000001"
	got := MakeAAD(pathID, msgID)
	want := pathID + msgID
	if string(got) != want {
		t.Errorf("MakeAAD drift\ngot:  %q\nwant: %q", string(got), want)
	}
	if len(got) != len(pathID)+len(msgID) {
		t.Errorf("MakeAAD length wrong: %d, want %d (no separator, no padding)", len(got), len(pathID)+len(msgID))
	}
}

func TestMakeAADStartsWithPathID(t *testing.T) {
	got := MakeAAD("nxc_abc", "01abc-def")
	if !strings.HasPrefix(string(got), "nxc_") {
		t.Errorf("MakeAAD must start with path_id (`nxc_` prefix), got: %q", string(got))
	}
}

// TestMakeAADMatchesPublishedVector ensures our impl matches the
// discovery doc's published example. If MakeAAD ever drifts from the
// hex bytes the well-known doc publishes, external implementers
// validating against our published vector would fail to interop.
//
// Hex constant copied from internal/discovery/example_aead.go's
// computeAEADExample output. If that example changes, this test
// should fail and force a coordinated update.
func TestMakeAADMatchesPublishedVector(t *testing.T) {
	pathID := "nxc_pGSqjw9XR86lzR8XTry2wkFGkvrvISTgkNGlGnuYmCo"
	msgID := "01979b0a-c0de-7eef-a000-000000000001"
	wantHex := "6e78635f704753716a7739585238366c7a52385854727932776b46476b767276495354676b4e476c476e75596d436f30313937396230612d633064652d376565662d613030302d303030303030303030303031"
	got := MakeAAD(pathID, msgID)
	if hex.EncodeToString(got) != wantHex {
		t.Errorf("MakeAAD diverges from published vector\ngot:  %s\nwant: %s\nIf this changed intentionally, update internal/discovery/example_aead.go and the published well-known doc, then update this constant.",
			hex.EncodeToString(got), wantHex)
	}
}

package docregister

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveDocRegister exercises the full register — CreateDoc, Revise,
// SubmitForApproval, ApproveWithChanges (new cairn version), Supersede —
// against a real checked-out cairn line, mirroring the discipline of
// nexus/workgraph's TestLiveWorkGraph: skip cleanly unless explicitly opted
// in.
//
// Gated on DOCREGISTER_E2E_CAIRN_REPO_DIR: a local working directory that is
// a git checkout of a real cairn-hosted line (e.g. `cairn express
// docregister-e2e-fixture` against a scratch cairn line, or any existing
// checkout you don't mind this test committing scratch docs into — it does
// NOT push, so the sovereign line is untouched unless you push yourself).
// See README.md's "Live-verify path" for the full recipe.
func TestLiveDocRegister(t *testing.T) {
	repoDir := os.Getenv("DOCREGISTER_E2E_CAIRN_REPO_DIR")
	if repoDir == "" {
		t.Skip("set DOCREGISTER_E2E_CAIRN_REPO_DIR (a git checkout of a real cairn line) to run the live docregister e2e")
	}

	content := &GitCairnContent{RepoDir: repoDir}
	reg := &Register{Store: newTestStore(t), Content: content}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := reg.CreateDoc(ctx, KindSpec, "docregister e2e fixture", "wi-e2e", "# e2e v1\n")
	if err != nil {
		t.Fatalf("CreateDoc: %v", err)
	}
	doc, err := reg.GetDoc(ctx, id)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	if doc.CairnRef == "" {
		t.Fatal("cairn_ref not set after CreateDoc")
	}

	if err := reg.SubmitForApproval(ctx, id); err != nil {
		t.Fatalf("SubmitForApproval: %v", err)
	}
	if err := reg.ApproveWithChanges(ctx, id, "operator-e2e", "# e2e v2 (operator edit)\n", "e2e tightened wording"); err != nil {
		t.Fatalf("ApproveWithChanges: %v", err)
	}
	after, err := reg.GetDoc(ctx, id)
	if err != nil {
		t.Fatalf("GetDoc after approve-with-changes: %v", err)
	}
	if after.CairnRef == doc.CairnRef {
		t.Fatal("cairn_ref unchanged — ApproveWithChanges must commit a new cairn version")
	}
	got, err := reg.GetContent(ctx, id)
	if err != nil {
		t.Fatalf("GetContent: %v", err)
	}
	if got != "# e2e v2 (operator edit)\n" {
		t.Fatalf("content = %q, want the operator-edited body", got)
	}

	if err := reg.Supersede(ctx, id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Logf("live docregister e2e OK: doc=%s cairn_ref=%s (left uncommitted-to-remote in %s; push yourself if you want it kept)", id, after.CairnRef, repoDir)
}

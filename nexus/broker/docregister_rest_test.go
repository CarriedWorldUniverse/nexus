package broker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/CarriedWorldUniverse/nexus/nexus/docregister"
	"github.com/CarriedWorldUniverse/nexus/nexus/roster"
)

// docregisterTestRig wires a Broker with the document register configured,
// mirroring adminWorkersTestRig (admin_workers_test.go): one admin token,
// one non-admin peer token, against the same broker.
type docregisterTestRig struct {
	url        string
	adminToken string
	peerToken  string
}

// fakeDocContent is an in-memory CairnContent for broker-layer tests — the
// broker doesn't need to exercise real git plumbing (that's
// nexus/docregister's job); it needs to exercise routing + auth tiers.
type fakeDocContent struct {
	seq  int
	docs map[string]string
}

func newFakeDocContent() *fakeDocContent {
	return &fakeDocContent{docs: make(map[string]string)}
}

func (f *fakeDocContent) Commit(ctx context.Context, docID string, kind docregister.Kind, content string) (string, error) {
	f.seq++
	ref := docID + "@fake-" + string(rune('0'+f.seq))
	f.docs[ref] = content
	return ref, nil
}

func (f *fakeDocContent) Fetch(ctx context.Context, ref string) (string, error) {
	return f.docs[ref], nil
}

func newDocregisterTestRig(t *testing.T) *docregisterTestRig {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := docregister.NewSQLStore(db)

	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	if err := tokens.mintInMemory("peer", false); err != nil {
		t.Fatal(err)
	}

	r := roster.New()
	b := New(Config{
		Tokens: tokens,
		Admin:  &AdminCallbacks{},
		DocRegister: &docregister.Register{
			Store:   store,
			Content: newFakeDocContent(),
		},
	}, r)

	mux := http.NewServeMux()
	b.registerDocRegisterWorkbench(mux)
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &docregisterTestRig{
		url:        srv.URL,
		adminToken: tokens.TokenForAgent("frame"),
		peerToken:  tokens.TokenForAgent("peer"),
	}
}

func (rig *docregisterTestRig) do(t *testing.T, method, path, token string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, rig.url+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (rig *docregisterTestRig) createDoc(t *testing.T, workItemID string) string {
	t.Helper()
	resp := rig.do(t, "POST", "/api/docs", rig.peerToken, docCreateBody{
		Kind: "spec", Title: "test spec", WorkItemID: workItemID, MDContent: "# body",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d; want 201", resp.StatusCode)
	}
	var doc docPayload
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.ID
}

func TestDocRegister_WorkbenchReachableByNonAdmin(t *testing.T) {
	rig := newDocregisterTestRig(t)
	id := rig.createDoc(t, "wi-1")
	if id == "" {
		t.Fatal("expected a doc id")
	}

	resp := rig.do(t, "GET", "/api/docs/"+id, rig.peerToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; want 200", resp.StatusCode)
	}
	var doc docPayload
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "draft" {
		t.Fatalf("status = %q, want draft", doc.Status)
	}

	// list
	resp2 := rig.do(t, "GET", "/api/docs?kind=spec", rig.peerToken, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d; want 200", resp2.StatusCode)
	}
	var list docsListResponse
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Docs) != 1 {
		t.Fatalf("list = %d docs, want 1", len(list.Docs))
	}

	// submit
	resp3 := rig.do(t, "POST", "/api/docs/"+id+"/submit", rig.peerToken, nil)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d; want 200", resp3.StatusCode)
	}
}

func TestDocRegister_VerdictEndpointsRejectNonAdmin(t *testing.T) {
	rig := newDocregisterTestRig(t)
	id := rig.createDoc(t, "wi-2")

	resp := rig.do(t, "POST", "/api/docs/"+id+"/submit", rig.peerToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d; want 200", resp.StatusCode)
	}

	cases := []struct {
		path string
		body any
	}{
		{"/api/admin/docs/" + id + "/approve", docVerdictBody{By: "peer"}},
		{"/api/admin/docs/" + id + "/approve-with-changes", docVerdictBody{By: "peer", MDContent: "x"}},
		{"/api/admin/docs/" + id + "/reject", docVerdictBody{By: "peer"}},
	}
	for _, c := range cases {
		resp := rig.do(t, "POST", c.path, rig.peerToken, c.body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s with peer token: status = %d; want 403", c.path, resp.StatusCode)
		}
	}

	// No token at all is also rejected (auth middleware, not just admin check).
	resp = rig.do(t, "POST", "/api/admin/docs/"+id+"/approve", "", docVerdictBody{By: "x"})
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("approve with no token succeeded; want rejected")
	}

	// Admin token succeeds.
	resp = rig.do(t, "POST", "/api/admin/docs/"+id+"/approve", rig.adminToken, docVerdictBody{By: "operator"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve with admin token: status = %d; want 200", resp.StatusCode)
	}
	var doc docPayload
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "approved" {
		t.Fatalf("status = %q, want approved", doc.Status)
	}
}

func TestDocRegister_ApproveWithChanges_ViaREST(t *testing.T) {
	rig := newDocregisterTestRig(t)
	id := rig.createDoc(t, "wi-3")
	resp := rig.do(t, "POST", "/api/docs/"+id+"/submit", rig.peerToken, nil)
	resp.Body.Close()

	resp = rig.do(t, "POST", "/api/admin/docs/"+id+"/approve-with-changes", rig.adminToken, docVerdictBody{
		By: "operator", MDContent: "# edited body", Comments: "tightened",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve-with-changes status = %d; want 200", resp.StatusCode)
	}
	var doc docPayload
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "approved_with_changes" {
		t.Fatalf("status = %q, want approved_with_changes", doc.Status)
	}
	if doc.Version != 2 {
		t.Fatalf("version = %d, want 2", doc.Version)
	}

	resp2 := rig.do(t, "GET", "/api/docs/"+id+"/content", rig.peerToken, nil)
	defer resp2.Body.Close()
	var content docContentResponse
	if err := json.NewDecoder(resp2.Body).Decode(&content); err != nil {
		t.Fatal(err)
	}
	if content.Content != "# edited body" {
		t.Fatalf("content = %q, want edited body", content.Content)
	}
}

func TestDocRegister_NotConfigured_RoutesAbsent(t *testing.T) {
	tokens := NewTokenStore()
	if err := tokens.mintInMemory("frame", true); err != nil {
		t.Fatal(err)
	}
	r := roster.New()
	b := New(Config{Tokens: tokens, Admin: &AdminCallbacks{}}, r) // no DocRegister

	mux := http.NewServeMux()
	b.registerDocRegisterWorkbench(mux)
	b.registerAdmin(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/docs", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.TokenForAgent("frame"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 when DocRegister isn't configured", resp.StatusCode)
	}
}

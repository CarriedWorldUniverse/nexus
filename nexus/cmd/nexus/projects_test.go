package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CarriedWorldUniverse/ledger"
	"github.com/CarriedWorldUniverse/nexus/nexus/issuesrest"
)

func TestProjectsHandlerListsActiveProjects(t *testing.T) {
	ctx := context.Background()
	svc, err := ledger.New(ctx, ledger.Config{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	defer svc.Close()

	createProject(t, svc, ledger.Project{Key: "NEX", Name: "Nexus"})
	createProject(t, svc, ledger.Project{Key: "OLD", Name: "Archived", Archived: true})

	req := httptest.NewRequest(http.MethodGet, issuesrest.ProjectsPath, nil)
	rec := httptest.NewRecorder()

	projectsHandler(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []projectResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(projects) = %d, want 1: %#v", len(got), got)
	}
	if got[0].Key != "NEX" || got[0].Name != "Nexus" || got[0].Archived {
		t.Fatalf("project = %#v, want active NEX project", got[0])
	}
}

func TestProjectsHandlerCanIncludeArchivedProjects(t *testing.T) {
	ctx := context.Background()
	svc, err := ledger.New(ctx, ledger.Config{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	defer svc.Close()

	createProject(t, svc, ledger.Project{Key: "NEX", Name: "Nexus"})
	createProject(t, svc, ledger.Project{Key: "OLD", Name: "Archived", Archived: true})

	req := httptest.NewRequest(http.MethodGet, issuesrest.ProjectsPath+"?include_archived=true", nil)
	rec := httptest.NewRecorder()

	projectsHandler(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got []projectResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(projects) = %d, want 2: %#v", len(got), got)
	}
	if got[0].Key != "NEX" || got[1].Key != "OLD" || !got[1].Archived {
		t.Fatalf("projects = %#v, want active then archived projects", got)
	}
}

func TestProjectsHandlerReturnsEmptyArrayWhenNoProjects(t *testing.T) {
	svc, err := ledger.New(context.Background(), ledger.Config{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	defer svc.Close()

	req := httptest.NewRequest(http.MethodGet, issuesrest.ProjectsPath, nil)
	rec := httptest.NewRecorder()

	projectsHandler(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("body = %q, want empty JSON array", got)
	}
}

func TestProjectsHandlerRejectsNonGET(t *testing.T) {
	svc, err := ledger.New(context.Background(), ledger.Config{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	defer svc.Close()

	req := httptest.NewRequest(http.MethodPost, issuesrest.ProjectsPath, nil)
	rec := httptest.NewRecorder()

	projectsHandler(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func createProject(t *testing.T, svc *ledger.Service, p ledger.Project) {
	t.Helper()
	if err := svc.CreateProject(context.Background(), p); err != nil {
		t.Fatalf("CreateProject(%s): %v", p.Key, err)
	}
}

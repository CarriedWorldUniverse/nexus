package main

import (
	"encoding/json"
	"net/http"

	"github.com/CarriedWorldUniverse/ledger"
	"github.com/CarriedWorldUniverse/nexus/nexus/issuesrest"
)

type projectResponse struct {
	Key          string `json:"key"`
	Organisation string `json:"organisation"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	DefaultTeam  string `json:"default_team"`
	Archived     bool   `json:"archived"`
}

func projectsHandler(svc *ledger.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != issuesrest.ProjectsPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		projects, err := svc.ListProjects(r.Context(), r.URL.Query().Get("include_archived") == "true")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		out := make([]projectResponse, 0, len(projects))
		for _, p := range projects {
			out = append(out, projectResponse{
				Key:          p.Key,
				Organisation: p.Organisation,
				Name:         p.Name,
				Description:  p.Description,
				DefaultTeam:  p.DefaultTeam,
				Archived:     p.Archived,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

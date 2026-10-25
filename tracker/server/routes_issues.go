package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerIssueRoutes registers the read routes: GET /v1/issues/{id},
// POST /v1/issues:show-many and POST /v1/issues:list.
func (s *Server) registerIssueRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/issues/{id}", s.getIssue)
	mux.HandleFunc("POST /v1/issues:show-many", s.showMany)
	mux.HandleFunc("POST /v1/issues:list", s.listIssues)
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found, err := s.cfg.Store.GetIssues(r.Context(), []string{id})
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	is, ok := found[id]
	if !ok {
		writeError(w, api.NotFound(id), s.cfg.Log)
		return
	}
	writeJSON(w, http.StatusOK, is)
}

func (s *Server) showMany(w http.ResponseWriter, r *http.Request) {
	var req api.ShowManyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	found, err := s.cfg.Store.GetIssues(r.Context(), req.IDs)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if found == nil {
		found = map[string]api.Issue{}
	}
	writeJSON(w, http.StatusOK, api.ShowManyResponse{Issues: found})
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	var f api.ListFilter
	if err := decodeJSON(r, &f); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	page, err := s.cfg.Store.ListIssues(r.Context(), f)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if page.Issues == nil {
		page.Issues = []api.Issue{}
	}
	writeJSON(w, http.StatusOK, page)
}

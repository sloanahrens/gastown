package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerIssueRoutes registers the read routes: GET /v1/issues/{id},
// POST /v1/issues:show-many, POST /v1/issues:list, POST /v1/issues:count and
// POST /v1/assigned.
func (s *Server) registerIssueRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/issues/{id}", s.getIssue)
	mux.HandleFunc("POST /v1/issues:show-many", s.showMany)
	mux.HandleFunc("POST /v1/issues:list", s.listIssues)
	mux.HandleFunc("POST /v1/issues:count", s.countIssues)
	mux.HandleFunc("POST /v1/assigned", s.assigned)
}

// assigned answers several assignees' issues in one call: the per-seat read
// the dispatch tick used to pay for once per status, per seat.
func (s *Server) assigned(w http.ResponseWriter, r *http.Request) {
	var req api.AssignedRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	page, err := s.cfg.Store.Assigned(r.Context(), req)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if page.Assigned == nil {
		page.Assigned = map[string][]api.Issue{}
	}
	writeJSON(w, http.StatusOK, page)
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

// countIssues answers how many issues a filter matches, the whole match: it is
// the read the wisp reaper sizes its work by without paging every row.
func (s *Server) countIssues(w http.ResponseWriter, r *http.Request) {
	var req api.CountRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	n, err := s.cfg.Store.CountIssues(r.Context(), req.Filter)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	writeJSON(w, http.StatusOK, api.CountResponse{Count: n})
}

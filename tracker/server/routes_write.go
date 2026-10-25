package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerWriteRoutes registers the write routes: POST /v1/issues,
// PATCH /v1/issues/{id}, POST /v1/issues:close and POST /v1/issues:delete.
// Every write is attributed to the actor of the request's token; an actor
// named in a body is ignored.
func (s *Server) registerWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/issues", s.createIssue)
	mux.HandleFunc("PATCH /v1/issues/{id}", s.updateIssue)
	mux.HandleFunc("POST /v1/issues:close", s.closeIssues)
	mux.HandleFunc("POST /v1/issues:delete", s.deleteIssues)
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	var p api.CreateParams
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	p.Actor = api.ActorFrom(r.Context())
	is, err := s.cfg.Store.CreateIssue(r.Context(), p)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	writeJSON(w, http.StatusCreated, is)
}

func (s *Server) updateIssue(w http.ResponseWriter, r *http.Request) {
	var req api.UpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	req.Patch.Actor = api.ActorFrom(r.Context())
	if err := s.cfg.Store.UpdateIssue(r.Context(), r.PathValue("id"), req.Patch, req.Guard); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) closeIssues(w http.ResponseWriter, r *http.Request) {
	var req api.CloseRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := s.cfg.Store.CloseIssues(r.Context(), req.IDs, req.Reason, req.Force); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteIssues(w http.ResponseWriter, r *http.Request) {
	var req api.DeleteRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := s.cfg.Store.DeleteIssues(r.Context(), req.IDs); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

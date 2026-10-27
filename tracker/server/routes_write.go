package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerWriteRoutes registers the write routes: POST /v1/issues,
// PATCH /v1/issues/{id}, POST /v1/issues:close, POST /v1/issues:delete,
// POST /v1/graphs (a set of issues and their links, all or none), POST
// /v1/batch (ordered operations, all or none), and the assignment verbs
// POST /v1/issues/{id}/claim, /release and /transfer.
// Every write is attributed to the actor of the request's token; an actor
// named in a body is ignored. A batch comment is the exception: it carries no
// author field, so a batch body that names one is refused before the store.
func (s *Server) registerWriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/issues", s.createIssue)
	mux.HandleFunc("PATCH /v1/issues/{id}", s.updateIssue)
	mux.HandleFunc("POST /v1/issues:close", s.closeIssues)
	mux.HandleFunc("POST /v1/issues:delete", s.deleteIssues)
	mux.HandleFunc("POST /v1/graphs", s.createGraph)
	mux.HandleFunc("POST /v1/issues/{id}/claim", s.claimIssue)
	mux.HandleFunc("POST /v1/issues/{id}/release", s.releaseIssue)
	mux.HandleFunc("POST /v1/issues/{id}/transfer", s.transferIssue)
	mux.HandleFunc("POST /v1/batch", s.batch)
}

// claimIssue takes the issue for the token's actor; the request has no body,
// because the holder is always the caller.
func (s *Server) claimIssue(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.Claim(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// releaseIssue returns the issue the token's actor holds to open, unassigned.
func (s *Server) releaseIssue(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.Release(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// transferIssue hands the issue the token's actor holds to another actor. The
// call is refused unless the caller holds it, so a body names only the new
// holder; the body is checked here, so a malformed assignee or a close in
// disguise is a 400 before the store sees it.
func (s *Server) transferIssue(w http.ResponseWriter, r *http.Request) {
	var req api.TransferRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := s.cfg.Store.Transfer(r.Context(), r.PathValue("id"), req.Status, req.Assignee); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// batch answers one result per operation, in the order asked for. A batch
// that fails answers no results, and an operation's api.Error names its index
// instead.
func (s *Server) batch(w http.ResponseWriter, r *http.Request) {
	var req api.BatchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	res, err := s.cfg.Store.Batch(r.Context(), req)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if res.Results == nil {
		res.Results = []api.BatchOperationResult{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) createGraph(w http.ResponseWriter, r *http.Request) {
	var req api.GraphRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	res, err := s.cfg.Store.CreateGraph(r.Context(), req)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if res.IDs == nil {
		res.IDs = map[string]string{}
	}
	writeJSON(w, http.StatusCreated, res)
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

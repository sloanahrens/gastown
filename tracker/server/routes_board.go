package server

import (
	"context"
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerBoardRoutes registers POST /v1/ready, POST /v1/blocked and
// POST /v1/issues:children-of, plus POST /v1/board/dispatch. They are POST so the filter travels in a body;
// none of them writes.
func (s *Server) registerBoardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/ready", func(w http.ResponseWriter, r *http.Request) { s.boardPage(w, r, s.cfg.Store.Ready) })
	mux.HandleFunc("POST /v1/blocked", func(w http.ResponseWriter, r *http.Request) { s.boardPage(w, r, s.cfg.Store.Blocked) })
	mux.HandleFunc("POST /v1/issues:children-of", s.childrenOf)
	mux.HandleFunc("POST /v1/board/dispatch", s.dispatchBoard)
}

// dispatchBoard answers the whole spec dispatch tick read in one snapshot.
func (s *Server) dispatchBoard(w http.ResponseWriter, r *http.Request) {
	var req api.DispatchBoardRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	resp, err := s.cfg.Store.DispatchBoard(r.Context(), req)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if resp.Rigs == nil {
		resp.Rigs = map[string]api.RigBoard{}
	}
	if resp.Probes == nil {
		resp.Probes = map[string]api.Probe{}
	}
	if resp.Errors == nil {
		resp.Errors = map[string]string{}
	}
	for rig, b := range resp.Rigs {
		if b.Ready == nil {
			b.Ready = []api.Issue{}
		}
		if b.Held == nil {
			b.Held = []api.HeldBead{}
		}
		if b.Children == nil {
			b.Children = map[string][]api.ChildStatus{}
		}
		resp.Rigs[rig] = b
	}
	writeJSON(w, http.StatusOK, resp)
}

// boardPage reads a ReadyFilter, runs query and answers with the page.
func (s *Server) boardPage(w http.ResponseWriter, r *http.Request, query func(ctx context.Context, f api.ReadyFilter) (api.IssuePage, error)) {
	var f api.ReadyFilter
	if err := decodeJSON(r, &f); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	page, err := query(r.Context(), f)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if page.Issues == nil {
		page.Issues = []api.Issue{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) childrenOf(w http.ResponseWriter, r *http.Request) {
	var req api.ChildrenOfRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	kids, err := s.cfg.Store.ChildrenOf(r.Context(), req.Parents)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if kids == nil {
		kids = map[string][]api.Issue{}
	}
	writeJSON(w, http.StatusOK, api.ChildrenOfResponse{Children: kids})
}

package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerLinkRoutes registers the comment and dependency routes under
// /v1/issues/{id}.
func (s *Server) registerLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/issues/{id}/comments", s.addComment)
	mux.HandleFunc("GET /v1/issues/{id}/comments", s.listComments)
	mux.HandleFunc("POST /v1/issues/{id}/dependencies", s.addDependency)
	mux.HandleFunc("DELETE /v1/issues/{id}/dependencies/{dependsOn}", s.removeDependency)
	mux.HandleFunc("GET /v1/issues/{id}/dependencies", s.listDependencies)
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	var req api.CommentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	author := req.Author
	if author == "" {
		author = api.ActorFrom(r.Context())
	}
	if err := s.cfg.Store.AddComment(r.Context(), r.PathValue("id"), author, req.Text); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	comments, err := s.cfg.Store.Comments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if comments == nil {
		comments = []api.Comment{}
	}
	writeJSON(w, http.StatusOK, api.CommentsResponse{Comments: comments})
}

func (s *Server) addDependency(w http.ResponseWriter, r *http.Request) {
	var req api.DependencyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := s.cfg.Store.AddDependency(r.Context(), r.PathValue("id"), req.DependsOn, req.Type); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeDependency(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Store.RemoveDependency(r.Context(), r.PathValue("id"), r.PathValue("dependsOn")); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listDependencies(w http.ResponseWriter, r *http.Request) {
	deps, err := s.cfg.Store.Dependencies(r.Context(), r.PathValue("id"), r.URL.Query().Get("type"))
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if deps == nil {
		deps = []api.IssueDep{}
	}
	writeJSON(w, http.StatusOK, api.DependenciesResponse{Dependencies: deps})
}

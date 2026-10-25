package server

import "net/http"

// registerLinkRoutes registers the comment and dependency routes under /v1/issues/{id}.
// It is filled in by its own bead; the stub keeps parallel beads out of each other's files.
func (s *Server) registerLinkRoutes(mux *http.ServeMux) {}

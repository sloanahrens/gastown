package server

import "net/http"

// registerIssueRoutes registers the read routes: GET /v1/issues/{id}, POST /v1/issues:show-many, POST /v1/issues:list.
// It is filled in by its own bead; the stub keeps parallel beads out of each other's files.
func (s *Server) registerIssueRoutes(mux *http.ServeMux) {}

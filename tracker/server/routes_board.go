package server

import "net/http"

// registerBoardRoutes registers GET /v1/ready, GET /v1/blocked and POST /v1/issues:children-of.
// It is filled in by its own bead; the stub keeps parallel beads out of each other's files.
func (s *Server) registerBoardRoutes(mux *http.ServeMux) {}

package server

import "net/http"

// registerWriteRoutes registers the write routes: POST /v1/issues, PATCH /v1/issues/{id}, POST /v1/issues:close, POST /v1/issues:delete.
// It is filled in by its own bead; the stub keeps parallel beads out of each other's files.
func (s *Server) registerWriteRoutes(mux *http.ServeMux) {}

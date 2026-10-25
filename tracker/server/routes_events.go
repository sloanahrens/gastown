package server

import "net/http"

// registerEventRoutes registers GET /v1/events.
// It is filled in by its own bead; the stub keeps parallel beads out of each other's files.
func (s *Server) registerEventRoutes(mux *http.ServeMux) {}

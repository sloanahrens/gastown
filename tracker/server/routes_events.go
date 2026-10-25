package server

import (
	"net/http"
	"strconv"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerEventRoutes registers GET /v1/events?after=<seq>&limit=<n>.
func (s *Server) registerEventRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/events", s.eventsAfter)
}

func (s *Server) eventsAfter(w http.ResponseWriter, r *http.Request) {
	after, err := queryInt(r, "after", 64)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	limit, err := queryInt(r, "limit", 32)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	page, err := s.cfg.Store.EventsAfter(r.Context(), after, int(limit))
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if page.Events == nil {
		page.Events = []api.Event{}
	}
	writeJSON(w, http.StatusOK, page)
}

// queryInt reads a non-negative integer query parameter that fits in bits
// bits. Absent means 0; a negative, non-numeric or too large value is
// api.Invalid.
func queryInt(r *http.Request, name string, bits int) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, bits)
	if err != nil || n < 0 {
		return 0, api.Invalid("%s must be a non-negative integer that fits in %d bits", name, bits)
	}
	return n, nil
}

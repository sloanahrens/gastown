package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerAdminRoutes registers the maintenance reads and the configuration
// routes: GET /v1/stats?rig=, GET /v1/config/{key} and PUT /v1/config/{key}.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("GET /v1/config/{key}", s.configGet)
	mux.HandleFunc("PUT /v1/config/{key}", s.configSet)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.cfg.Store.Stats(r.Context(), r.URL.Query().Get("rig"))
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if st.ByStatus == nil {
		st.ByStatus = map[string]int{}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) configGet(w http.ResponseWriter, r *http.Request) {
	v, ok, err := s.cfg.Store.ConfigGet(r.Context(), r.PathValue("key"))
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	writeJSON(w, http.StatusOK, api.ConfigValue{Value: v, Set: ok})
}

func (s *Server) configSet(w http.ResponseWriter, r *http.Request) {
	var req api.ConfigSetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := s.cfg.Store.ConfigSet(r.Context(), r.PathValue("key"), req.Value); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

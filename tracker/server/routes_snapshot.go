package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerSnapshotRoutes registers GET /v1/snapshot/town?cursor=&limit=: one
// transaction's page of the town, with exact counts over the whole set.
func (s *Server) registerSnapshotRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/snapshot/town", s.townSnapshot)
}

func (s *Server) townSnapshot(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 32)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	snap, err := s.cfg.Store.TownSnapshot(r.Context(), api.SnapshotRequest{
		Limit:  int(limit),
		Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	// The page and the counts are collections: empty, never null.
	if snap.Issues == nil {
		snap.Issues = []api.SnapshotIssue{}
	}
	if snap.Counts.ByStatus == nil {
		snap.Counts.ByStatus = map[string]int{}
	}
	if snap.Counts.ByRig == nil {
		snap.Counts.ByRig = map[string]int{}
	}
	if snap.Counts.BlockedRigs == nil {
		snap.Counts.BlockedRigs = map[string]int{}
	}
	writeJSON(w, http.StatusOK, snap)
}

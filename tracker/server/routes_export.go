package server

import (
	"encoding/json"
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// registerExportRoutes registers POST /v1/export: one rig's issues, wisps
// included, then their labels, dependencies and comments, as JSONL.
func (s *Server) registerExportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/export", s.exportRig)
}

// exportRig writes one record per line as the store reads them. The status
// goes out with the first record, so a failure before it is an ordinary error
// answer; a failure after it is the stream's last line, an error record.
func (s *Server) exportRig(w http.ResponseWriter, r *http.Request) {
	var req api.ExportRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if req.Rig == "" {
		writeError(w, api.Invalid("rig is required"), s.cfg.Log)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)
	started := false
	// Each record is flushed as it is read, so the client sees it while the
	// store is still reading the next one. A client that went away cancels
	// r's context, which ends the read; the flush's error adds nothing to that.
	emit := func(rec api.ExportRecord) error {
		if !started {
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		_ = rc.Flush()
		return nil
	}
	err := s.cfg.Store.Export(r.Context(), req.Rig, emit)
	if err == nil {
		// A rig with no records is still a successful, empty export.
		if !started {
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	if !started {
		writeError(w, err, s.cfg.Log)
		return
	}
	// The status is out, so the failure is reported in the stream. When the
	// failure was the client going away, this line has nowhere to go.
	_ = enc.Encode(api.ExportRecord{Type: api.RecordError, Error: apiErrorOf(err, s.cfg.Log)})
}

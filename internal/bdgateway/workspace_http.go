package bdgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ServeHTTP is the gateway's PUT /v1/workspace handler. G6 mounts it with the
// class check in front (only a daemon or operator token may push) and the auth
// middleware; this method only decodes, validates and stores. Every response,
// refusal included, carries the live tree's hash in WorkspaceHashHeader, so a
// caller whose push was refused learns which workspace its next bd call reads.
func (s *WorkspaceStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		s.writeJSON(w, http.StatusMethodNotAllowed, ErrorPayload{
			Code:    "method_not_allowed",
			Message: "the workspace takes PUT only",
		})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWorkspaceDocumentBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, ErrorPayload{
				Code:    "workspace_document_too_large",
				Message: fmt.Sprintf("the document is over %d bytes", MaxWorkspaceDocumentBytes),
			})
			return
		}
		s.writeJSON(w, http.StatusBadRequest, ErrorPayload{
			Code:    "workspace_document_unreadable",
			Message: fmt.Sprintf("reading the document: %v", err),
		})
		return
	}
	var doc WorkspaceDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorPayload{
			Code:    "workspace_document_malformed",
			Message: fmt.Sprintf("the body is not a workspace document: %v", err),
		})
		return
	}
	manifest, err := s.Put(doc)
	if err != nil {
		status, code := workspaceRefusal(err)
		s.writeJSON(w, status, ErrorPayload{Code: code, Message: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, workspacePushResult{ManifestSHA: manifest})
}

// AddWorkspaceHeader sets the live tree's manifest hash on h. G6 adds it to
// every gateway response, not only the workspace's own, so the forwarder and
// the doctor can compare it with the files they believe current.
func (s *WorkspaceStore) AddWorkspaceHeader(h http.Header) {
	h.Set(WorkspaceHashHeader, s.Manifest())
}

// workspacePushResult is the 200 body: the hash of the tree the gateway now
// serves, which the document's manifest_sha already matched.
type workspacePushResult struct {
	ManifestSHA string `json:"manifest_sha"`
}

// workspaceRefusal maps a push refusal to its HTTP status and machine-readable
// code. An error this function does not know is a 500, never a 200: a store
// failure must not read as a successful push.
func workspaceRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, ErrWorkspacePath):
		return http.StatusBadRequest, "workspace_path_refused"
	case errors.Is(err, ErrWorkspaceFileName):
		return http.StatusBadRequest, "workspace_file_name_refused"
	case errors.Is(err, ErrWorkspaceFileTooLarge):
		return http.StatusRequestEntityTooLarge, "workspace_file_too_large"
	case errors.Is(err, ErrWorkspaceTooLarge):
		return http.StatusRequestEntityTooLarge, "workspace_too_large"
	case errors.Is(err, ErrWorkspaceNoFiles):
		return http.StatusBadRequest, "workspace_no_files"
	case errors.Is(err, ErrWorkspaceManifestMismatch):
		return http.StatusBadRequest, "workspace_manifest_mismatch"
	default:
		return http.StatusInternalServerError, "workspace_store_failed"
	}
}

// writeJSON answers with a JSON body and the live workspace hash. The status
// line goes out with the headers, so an encode or write failure after it has no
// second answer to give; the client sees a short body with its status.
func (s *WorkspaceStore) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	s.AddWorkspaceHeader(w.Header())
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

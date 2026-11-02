// Package consoleapi is the operator console's HTTP read surface over the
// admin plane's views: GET /workspaces lists every workspace and
// GET /workspaces/{id} shows one, each behind a staff bearer check. Responses
// are JSON and a method other than GET is refused.
//
// The Verifier the handler takes is defined here. An adapter to internal/signin's
// Staff verifier is a later bead, as is the admin-plane server that mounts this
// handler; nothing in this package opens a listener.
package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/steveyegge/gastown/internal/adminplane/views"
)

// ErrNotFound is what a Source returns, wrapped or not, when it holds no
// workspace with the requested id.
var ErrNotFound = errors.New("consoleapi: no such workspace")

// Verifier checks an operator's bearer token and returns the staff email it
// belongs to.
type Verifier interface {
	Verify(ctx context.Context, bearerToken string) (staffEmail string, err error)
}

// Handler serves the console's read views to a verified staff member.
type Handler struct {
	source   views.Source
	verifier Verifier
	mux      *http.ServeMux
}

// New returns a Handler that reads src and admits a request only when verifier
// accepts its bearer token.
func New(src views.Source, verifier Verifier) *Handler {
	h := &Handler{source: src, verifier: verifier, mux: http.NewServeMux()}
	h.mux.HandleFunc("/workspaces", h.list)
	h.mux.HandleFunc("/workspaces/", h.detail)
	return h
}

// ServeHTTP refuses a method other than GET before checking the token, then
// serves the routed view.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token, ok := bearerToken(r)
	if !ok || h.verifier == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if _, err := h.verifier.Verify(r.Context(), token); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// list serves the workspace list view.
func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	rows, err := views.List(h.source)
	if err != nil {
		internalError(w)
		return
	}
	writeJSON(w, rows)
}

// detail serves one workspace's detail view, answering 404 when the Source
// reports ErrNotFound for the requested id.
func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/workspaces/")
	if id == "" || strings.Contains(id, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	detail, err := views.Detail(h.source, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		internalError(w)
		return
	}
	writeJSON(w, detail)
}

// bearerToken returns the token of an Authorization: Bearer header. It refuses
// a missing header, another scheme, and an empty or multi-word token.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.Contains(token, " ") {
		return "", false
	}
	return token, true
}

// writeJSON writes v as the response body. It marshals before writing a status,
// so a value that cannot be encoded gets the internal error rather than a
// truncated success.
func writeJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		internalError(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// internalError answers 500 with the same fixed text for every failure, so no
// source or verifier error reaches the client.
func internalError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte("internal error"))
}

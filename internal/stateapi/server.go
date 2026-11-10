// Package stateapi serves the daemon's file-backed state over HTTP. Every route
// is GET-only; every route but /healthz needs a bearer token.
package stateapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// cursorFormat names the cursor layout a client passes back: a generation
// number and an offset, joined by a colon.
const cursorFormat = "gen:offset"

// apiVersion is the version of this read API, reported by /v1/info.
const apiVersion = 1

// route is one registered path. A public route skips the bearer check.
type route struct {
	h      http.Handler
	public bool
}

// Server is the state API's handler. Register routes with Handle before the
// server starts serving; the route table is not guarded against concurrent
// registration.
type Server struct {
	town   string
	token  func() string
	routes map[string]route
}

// NewServer returns a Server for town that admits a bearer request when it
// matches the value token returns. token is read on every request, so a
// rotated token takes effect without a restart. A nil token or an empty one
// refuses every request except /healthz.
func NewServer(town string, token func() string) *Server {
	s := &Server{town: town, token: token, routes: map[string]route{}}
	s.register("/healthz", http.HandlerFunc(healthz), true)
	s.Handle("/v1/info", http.HandlerFunc(s.info))
	return s
}

// Handle registers h at path behind the bearer check. It panics if path is
// already registered, since a second handler would silently shadow the first,
// or if h is nil, since the request would panic instead.
func (s *Server) Handle(path string, h http.Handler) {
	s.register(path, h, false)
}

// Routes returns the registered paths in sorted order.
func (s *Server) Routes() []string {
	paths := make([]string, 0, len(s.routes))
	for p := range s.routes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// ServeHTTP answers 401 for an unregistered path unless the request carries the
// configured token, then 404, so a caller without the token gets the same answer
// for every unregistered path. For a registered path it answers 405 for any
// method but GET, then 401 for a protected route without the configured token.
// It checks the method before the token there, so that refusal never depends on
// the token.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.routes[r.URL.Path]
	if !ok {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !rt.public && !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rt.h.ServeHTTP(w, r)
}

func (s *Server) register(path string, h http.Handler, public bool) {
	if _, dup := s.routes[path]; dup {
		panic("stateapi: route already registered: " + path)
	}
	if h == nil {
		panic("stateapi: nil handler for route: " + path)
	}
	s.routes[path] = route{h: h, public: public}
}

// authorized reports whether r carries the configured bearer token. The two
// digests are compared, not the raw values, so the comparison takes the same
// time whatever the lengths of the token and the presented value.
func (s *Server) authorized(r *http.Request) bool {
	want := ""
	if s.token != nil {
		want = s.token()
	}
	if want == "" {
		return false
	}
	got, ok := bearerToken(r)
	if !ok {
		return false
	}
	wantSum := sha256.Sum256([]byte(want))
	gotSum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(wantSum[:], gotSum[:]) == 1
}

// info serves the town name, the API version and the cursor format.
func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"town":   s.town,
		"api":    apiVersion,
		"cursor": cursorFormat,
	})
}

// healthz answers 200 so a probe can check the process without a token.
func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// bearerToken returns the token of an Authorization: Bearer header. It refuses
// a missing header, another scheme, and a multi-word token. An empty token is
// returned as is, so authorized's empty-config check is the one that refuses it.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.Contains(token, " ") {
		return "", false
	}
	return token, true
}

// writeJSON writes v as a JSON body under status. Every value passed here is a
// map of strings and an int, which encoding/json cannot refuse, so the encoder
// runs after the status is sent and a write error has no caller to report to.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the {"error": text} body every refusal uses.
func writeError(w http.ResponseWriter, status int, text string) {
	writeJSON(w, status, map[string]string{"error": text})
}

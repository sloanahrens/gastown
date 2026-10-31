// Package server serves the /v1 HTTP API of trackerd: probes, bearer-token
// auth, schema-level checks and typed errors, over a store.Store. Routes for
// the Store's operations land with their beads; this package owns the
// cross-cutting rules every route inherits.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Authenticator resolves a bearer token to the actor it was issued to, or
// returns api.ErrUnknownToken for a token it does not know, has revoked, or
// has expired (the server answers 401 for it).
type Authenticator interface {
	Identify(ctx context.Context, token string) (api.Identity, error)
}

// Health reports whether the database is usable: Ping that it answers,
// WriteProbe that it accepts writes (a read-only replica answers Ping).
type Health interface {
	Ping(ctx context.Context) error
	WriteProbe(ctx context.Context) error
}

// Logger is the one method the server needs from a logger.
type Logger interface {
	Printf(format string, args ...any)
}

// Config is what New needs. SchemaLevel is the level this binary was built
// for and the database was migrated to; clients must match it.
type Config struct {
	// Store is what the /v1 routes read and write.
	Store       store.Store
	Auth        Authenticator
	Health      Health
	SchemaLevel int
	Log         Logger
	// ExportTimeout bounds one export, the time its read holds one snapshot
	// open while a slow client reads; default 30 min.
	ExportTimeout time.Duration
	// Reap is the wisp age policy the purge route and the reaper loop apply;
	// zero ages take the defaults.
	Reap store.ReapPolicy
	// Now is the clock the policy measures from; nil means the wall clock.
	Now func() time.Time
}

// Server is trackerd's HTTP handler.
type Server struct {
	cfg Config
	mux *http.ServeMux
	// routes is the /v1 mux, behind the schema gate and authentication.
	routes  *http.ServeMux
	metrics *metrics
}

// readyTimeout bounds the readiness probe so a stalled database reads as not
// ready instead of hanging the prober.
const readyTimeout = 2 * time.Second

// defaultExportTimeout bounds one export when Config.ExportTimeout is zero.
const defaultExportTimeout = 30 * time.Minute

// New returns a Server. Auth and Health are required.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = discardLog{}
	}
	if cfg.ExportTimeout == 0 {
		cfg.ExportTimeout = defaultExportTimeout
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), metrics: newMetrics(cfg.Log)}
	// Probes carry no token: the daemon and launchd call them.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	// The scrape is unauthenticated like the probes, on the same loopback-only
	// listener, and is not itself instrumented.
	s.mux.HandleFunc("GET /metrics", s.metrics.serve)
	s.mux.HandleFunc("GET /readyz", s.readyz)
	s.mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"service": "trackerd", "schema_level": cfg.SchemaLevel})
	})

	v1 := http.NewServeMux()
	s.routes = v1
	v1.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"actor": ActorFrom(r.Context())})
	})
	s.registerIssueRoutes(v1)
	s.registerWriteRoutes(v1)
	s.registerLinkRoutes(v1)
	s.registerBoardRoutes(v1)
	s.registerSnapshotRoutes(v1)
	s.registerEventRoutes(v1)
	s.registerExportRoutes(v1)
	s.registerAdminRoutes(v1)
	// Instrumentation sits inside authentication because it reads the actor
	// from the resolved identity, and outside the schema gate so a refused
	// request is still counted under the route it asked for.
	s.mux.Handle("/v1/", s.authenticated(s.metrics.instrument(v1, s.schemaGate(v1))))
	return s
}

// Handler is the server's http.Handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	// The probe is unauthenticated, so its body says what failed and never
	// why: the cause (a host, a user, a SQL state) goes to the log.
	if err := s.cfg.Health.Ping(ctx); err != nil {
		s.cfg.Log.Printf("readyz: ping failed: %v", err)
		writeAPIError(w, http.StatusServiceUnavailable, api.Unavailable("database does not answer"))
		return
	}
	if err := s.cfg.Health.WriteProbe(ctx); err != nil {
		s.cfg.Log.Printf("readyz: write probe failed: %v", err)
		writeAPIError(w, http.StatusServiceUnavailable, api.Unavailable("database does not accept writes"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "schema_level": s.cfg.SchemaLevel})
}

// schemaGate refuses a /v1 request whose schema level is missing, malformed
// or different from the server's, before any route sees it. It runs after
// authentication, so only a caller with a token learns the server's level here.
func (s *Server) schemaGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(api.SchemaHeader)
		if raw == "" {
			writeAPIError(w, http.StatusBadRequest, api.Invalid("the %s header is required", api.SchemaHeader))
			return
		}
		got, err := strconv.Atoi(raw)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, api.Invalid("the %s header must be an integer, got %q", api.SchemaHeader, raw))
			return
		}
		if got != s.cfg.SchemaLevel {
			writeAPIError(w, http.StatusConflict, api.Conflict("schema level skew: client %d, server %d", got, s.cfg.SchemaLevel))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ActorFrom is the actor the request's token was issued to, or "" outside an
// authenticated route. Store writes read it with api.ActorFrom on the same
// context.
func ActorFrom(ctx context.Context) string { return api.ActorFrom(ctx) }

// authenticated resolves the bearer token to an actor and puts it on the
// request context. A missing, malformed or unknown token is a 401.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, api.Invalid("a bearer token is required"))
			return
		}
		id, err := s.cfg.Auth.Identify(r.Context(), token)
		if errors.Is(err, api.ErrUnknownToken) {
			writeAPIError(w, http.StatusUnauthorized, api.Invalid("unknown or expired bearer token"))
			return
		}
		if err != nil {
			writeError(w, err, s.cfg.Log)
			return
		}
		next.ServeHTTP(w, r.WithContext(api.WithIdentity(r.Context(), id)))
	})
}

// bearerToken extracts the token from an Authorization header; the scheme is
// case-insensitive and the token must be non-empty.
func bearerToken(h string) (string, bool) {
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// RequireLoopback checks that addr is host:port with a literal loopback IP.
// trackerd binds nowhere else: a name like "localhost" can resolve to
// anything, and an empty host means every interface.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("server: listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("server: listen address %q is not a loopback IP; trackerd binds 127.0.0.1 or [::1] only", addr)
	}
	return nil
}

type discardLog struct{}

func (discardLog) Printf(string, ...any) {}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, e *api.Error) { writeJSON(w, status, e) }

// writeError answers err. An api.Error keeps its kind and status; anything
// else is an internal failure: the cause goes to the log and the client gets
// a generic unavailable, so no connection string or SQL detail leaks.
func writeError(w http.ResponseWriter, err error, log Logger) {
	ae := apiErrorOf(err, log)
	writeAPIError(w, ae.HTTPStatus(), ae)
}

// apiErrorOf is the api.Error that answers err. Anything that is not one is
// logged and becomes the generic unavailable.
func apiErrorOf(err error, log Logger) *api.Error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae
	}
	log.Printf("internal error: %v", err)
	return api.Unavailable("internal error; see the server log")
}

// requireAdmin lets only an admin-scope token through to next. A seat token
// gets a 403 naming the scope, not a 401: it is a valid token that may not do
// this. A context with no scope (not authenticated) is refused too.
func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if api.ScopeFrom(r.Context()) != api.ScopeAdmin {
			writeAPIError(w, http.StatusForbidden, api.Forbidden("this needs an admin token"))
			return
		}
		next(w, r)
	}
}

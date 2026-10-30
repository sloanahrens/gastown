package server

import (
	"context"
	"net/http"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Authorization: any authenticated token may read configuration and statistics;
// writing configuration, issuing a token, revoking one and purging wisps need an
// admin-scope token (a seat token gets a 403). Admin tokens are held by the
// daemon and operators, never by a seat.
//
// registerAdminRoutes registers the maintenance reads and the configuration
// routes: GET /v1/stats?rig=, GET /v1/config/{key} and PUT /v1/config/{key};
// the token routes POST /v1/tokens (issue) and POST /v1/tokens:revoke, so
// a caller can hand a seat its token without holding the database password;
// and POST /v1/wisps:purge, which deletes the expired wisps among the ids named.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("GET /v1/config/{key}", s.configGet)
	mux.HandleFunc("PUT /v1/config/{key}", requireAdmin(s.configSet))
	mux.HandleFunc("POST /v1/tokens", requireAdmin(s.issueToken))
	mux.HandleFunc("POST /v1/wisps:purge", requireAdmin(s.purgeWisps))
	mux.HandleFunc("POST /v1/tokens:revoke", requireAdmin(s.revokeTokens))
}

// issueToken creates a token for the actor and scope the body names and
// answers it once: the token is in no log line and no metric, so the response
// body is the only copy the tracker makes in the clear. The one line the log
// does get names the admin who issued it, the target actor and the scope, so
// a seat's token can be traced back to the caller that handed it out.
func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	var req api.IssueTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	scope := req.ScopeOrSeat()
	token, err := s.cfg.Store.IssueTokenScoped(r.Context(), req.Actor, scope, nil)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	s.cfg.Log.Printf("token issue: admin=%q target=%q scope=%q", api.ActorFrom(r.Context()), req.Actor, scope)
	writeJSON(w, http.StatusCreated, api.IssuedToken{Actor: req.Actor, Scope: scope, Token: token})
}

// revokeTokens ends every live token of the actor the body names, or the one
// token it names by id: an actor with none is a success answering zero, while
// an id that names none is a not_found, an exact reference that missed. A
// revoke by actor logs one line naming the admin, the target and how many
// tokens it ended, zero included, so an attempt on a tokenless actor stays on
// record; a revoke by id logs one naming the id, and a failed one logs nothing
// but the error.
func (s *Server) revokeTokens(w http.ResponseWriter, r *http.Request) {
	var req api.RevokeTokensRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if req.Actor != "" {
		n, err := s.cfg.Store.RevokeActorTokens(r.Context(), req.Actor)
		if err != nil {
			writeError(w, err, s.cfg.Log)
			return
		}
		s.cfg.Log.Printf("token revoke: admin=%q target=%q revoked=%d", api.ActorFrom(r.Context()), req.Actor, n)
		writeJSON(w, http.StatusOK, api.RevokeTokensResult{Revoked: n})
		return
	}
	ok, err := s.cfg.Store.RevokeTokenID(r.Context(), req.TokenID)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if !ok {
		writeError(w, api.NotFound("token "+req.TokenID), s.cfg.Log)
		return
	}
	// %q on every value: a token id is request text nothing checks for a
	// newline, and an unquoted one would forge a line of its own.
	s.cfg.Log.Printf("token revoke: admin=%q token_id=%q", api.ActorFrom(r.Context()), req.TokenID)
	writeJSON(w, http.StatusOK, api.RevokeTokensResult{Revoked: 1})
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

// reapPassTimeout bounds one reaper pass; a pass that outlives it is cut off
// and the next tick starts a fresh one.
const reapPassTimeout = 10 * time.Minute

// purgeWisps deletes the expired wisps among the ids the body names, under the
// wisp policy. Each id the policy keeps is reported with its reason, so a caller
// can tell a refusal from a purge.
func (s *Server) purgeWisps(w http.ResponseWriter, r *http.Request) {
	var req api.PurgeWispsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	res, err := store.PurgeWisps(r.Context(), s.cfg.Store, req.IDs, s.now(), s.cfg.Reap)
	if err != nil {
		writeError(w, err, s.cfg.Log)
		return
	}
	s.cfg.Log.Printf("wisp purge: admin=%q purged=%d kept=%d", api.ActorFrom(r.Context()), len(res.Purged), len(res.Kept))
	writeJSON(w, http.StatusOK, res)
}

// StartReaper runs the wisp policy as a background pass every interval until ctx
// ends, logging each pass's counts. The returned channel closes once the loop has
// stopped. An interval of zero or less starts nothing and returns a closed
// channel.
func (s *Server) StartReaper(ctx context.Context, every time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if every <= 0 {
		close(done)
		return done
	}
	ticker := time.NewTicker(every)
	go func() {
		defer close(done)
		defer ticker.Stop()
		s.reapLoop(ctx, ticker.C)
	}()
	return done
}

// reapLoop runs one pass for each tick until ctx ends. A failed pass is logged
// and the loop goes on: the next tick is the retry.
func (s *Server) reapLoop(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		s.reapOnce(ctx)
	}
}

func (s *Server) reapOnce(ctx context.Context) {
	passCtx, cancel := context.WithTimeout(ctx, reapPassTimeout)
	defer cancel()
	res, err := store.ReapPass(passCtx, s.cfg.Store, s.now(), s.cfg.Reap)
	if err != nil {
		s.cfg.Log.Printf("reaper pass: failed: %v", err)
		return
	}
	s.cfg.Log.Printf("reaper pass: closed=%d purged=%d", res.Closed, res.Purged)
}

// now is the clock the policy measures from.
func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

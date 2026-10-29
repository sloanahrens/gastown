package server

import (
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// Authorization: any authenticated token may read configuration and statistics;
// writing configuration, issuing a token and revoking one need an admin-scope
// token (a seat token gets a 403). Admin tokens are held by the daemon and
// operators, never by a seat.
//
// registerAdminRoutes registers the maintenance reads and the configuration
// routes: GET /v1/stats?rig=, GET /v1/config/{key} and PUT /v1/config/{key};
// and the token routes POST /v1/tokens (issue) and POST /v1/tokens:revoke, so
// a caller can hand a seat its token without holding the database password.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/stats", s.stats)
	mux.HandleFunc("GET /v1/config/{key}", s.configGet)
	mux.HandleFunc("PUT /v1/config/{key}", requireAdmin(s.configSet))
	mux.HandleFunc("POST /v1/tokens", requireAdmin(s.issueToken))
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
// revoke that ends a token logs one line naming the admin, the target and how
// many tokens it ended; a failed one logs nothing but the error.
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

package bdgateway

import "net/http"

// NewPushHandler returns the handler a second, push-only listener serves: the
// workspace push and the counters, and no other endpoint.
func NewPushHandler(g *Gateway) http.Handler {
	return &pushHandler{g: g}
}

// pushHandler routes the two repair routes without consulting Ready, so a pod
// whose workspace volume is fresh or lost can still be pushed a tree (gt-9vg8a4).
type pushHandler struct {
	g *Gateway
}

// ServeHTTP routes PUT /v1/workspace and GET /v1/stats to the gateway's own
// handlers, answers every other path 404, and adds the workspace hash header as
// ServeHTTP does. Authentication and the class checks stay the gateway's, so the
// push needs ClassWrite and the stats read ClassRead; because the exec route is
// not reachable here, the listener needs no token class of its own. A method
// either route does not take keeps the gateway's own 405.
func (h *pushHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.g == nil {
		refuse(w, http.StatusInternalServerError, codeInternal, "the push handler has no gateway")
		return
	}
	if h.g.workspaces != nil {
		h.g.workspaces.AddWorkspaceHeader(w.Header())
	}
	switch r.URL.Path {
	case workspacePath:
		h.g.serveWorkspace(w, r)
	case statsPath:
		h.g.serveStats(w, r)
	default:
		refuse(w, http.StatusNotFound, codeNotFound, "no such endpoint")
	}
}

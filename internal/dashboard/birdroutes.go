package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The Reports page's two read-only routes: the birds' rows as JSON, and one
// bird's own page served from the runtime directory beside its summary.
//
// A bird writes its page as HTML, and the dashboard would otherwise serve it
// from its own origin under its own policy, where a tag in the page could read
// /api/state. So the page is served inert: a policy of its own, with no script
// source and a sandbox directive that gives the page an opaque origin, in place
// of the dashboard's own policy for this route alone.
const (
	birdPagePrefix = "/birds/"
	birdPageCSP    = "default-src 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src data:; sandbox"
)

// serveBirds returns the birds' rows for the Reports page's list. A hub with no
// reader draws an empty list rather than a failure, the way every unread part
// of the page is empty, and the list encodes as [] rather than null.
func (h *Hub) serveBirds(w http.ResponseWriter, r *http.Request) {
	rows := []BirdReport{}
	if h.cfg.Birds != nil {
		rows = h.cfg.Birds()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// serveBirdPage serves one bird's page: daily.html, or the daily.prev.html
// before it. The path must have exactly two parts, and the reader checks each
// against the name a bird writes and reads the file — Lstatting it, capping it
// and screening it — so the route never builds a path from the URL and nothing
// else in the reports directory can be asked for. Anything else — a page that
// is not there, a name of another shape, or a hub with no birds directory —
// answers 404; a page that looks like it holds a secret answers 403 and its
// body says only that.
func (h *Hub) serveBirdPage(w http.ResponseWriter, r *http.Request) {
	bird, file, ok := birdPageParts(r.URL.EscapedPath())
	if !ok || h.cfg.BirdsDir == "" {
		http.NotFound(w, r)
		return
	}
	page, err := NewBirdReportsReader(h.cfg.BirdsDir).ReadPage(bird, file)
	switch {
	case errors.Is(err, errBirdPageSecret):
		http.Error(w, "withheld", http.StatusForbidden)
		return
	case err != nil:
		http.NotFound(w, r)
		return
	}
	// This replaces the dashboard's policy that guard set for the request: the
	// page is a bird's own HTML and runs nothing. guard's Cache-Control and
	// nosniff are the route's too, and are left as it set them.
	w.Header().Set("Content-Security-Policy", birdPageCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// birdPathsAsWritten sends a bird path that the mux would clean to the bird
// route as it was written. The mux cleans a "." or ".." segment and a doubled
// separator out of a path before matching it, and answers a redirect to the
// cleaned path; a bird path answers such a request 404 instead, because it
// names no page, and a redirect would send a fetch or a frame to a path this
// dashboard does not serve.
func (h *Hub) birdPathsAsWritten(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if birdPathNeedsCleaning(r.URL.EscapedPath()) {
			h.serveBirdPage(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// birdPathNeedsCleaning reports whether the mux would clean escapedPath before
// matching it: a "." or ".." segment, or a separator with nothing after it.
// Only bird paths are considered; every other path keeps the mux's behavior.
func birdPathNeedsCleaning(escapedPath string) bool {
	rest, ok := strings.CutPrefix(escapedPath, birdPagePrefix)
	if !ok {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// birdPageParts splits a bird path as written into its two parts, and says
// whether it has exactly two. The escaped path is what is read: a percent
// escape is how a separator could hide from the two-part test, and neither name
// a bird writes can hold one, so an escaped part is refused where it stands
// rather than decoded into a name. The parts themselves are checked by the
// reader that reads them, which is the one place that knows the names a bird
// writes.
func birdPageParts(escapedPath string) (bird, file string, ok bool) {
	rest, ok := strings.CutPrefix(escapedPath, birdPagePrefix)
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

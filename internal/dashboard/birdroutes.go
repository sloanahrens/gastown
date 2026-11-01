package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
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
// is not there, a name of another shape, or a birds directory that is not an
// absolute path — answers 404; a page that looks like it holds a secret answers
// 403 and its body says only that. The directory must be absolute because a
// relative one would resolve against the dashboard's own working directory,
// which is not the birds' runtime directory.
func (h *Hub) serveBirdPage(w http.ResponseWriter, r *http.Request) {
	bird, file, ok := birdPageParts(r.URL.EscapedPath())
	if !ok || !filepath.IsAbs(h.cfg.BirdsDir) {
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

// birdPathsToTheRoute answers every request that names the bird route before
// the mux matches it. A bird path answers 404 to anything that is not exactly
// one bird's page, and the mux would not: it cleans a "." or ".." segment and a
// doubled separator out of the path and answers a redirect to the cleaned path,
// which would send a fetch or a frame to a path this dashboard does not serve.
// Which form the mux cleans — the escaped path or the decoded one — is its
// business, so a request is answered here when either form names the route, and
// the mux never sees a bird path at all.
func (h *Hub) birdPathsToTheRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if birdPathNamed(r.URL) {
			h.serveBirdPage(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// birdPathNamed reports whether u names the bird route or a path under it, as
// written or as decoded. A "/birds" with no slash is the route's own entry and
// names no page, but it is caught here too, so the mux does not answer its
// redirect to the trailing slash.
func birdPathNamed(u *url.URL) bool {
	route := strings.TrimSuffix(birdPagePrefix, "/")
	for _, path := range []string{u.EscapedPath(), u.Path} {
		if path == route || strings.HasPrefix(path, birdPagePrefix) {
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

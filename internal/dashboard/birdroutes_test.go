package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// birdRouteCSP is the policy the bird page route must carry, spelled out here
// rather than taken from the route, so a directive the route drops is red.
const birdRouteCSP = "default-src 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src data:; sandbox"

// birdRouteGet sends one GET to a hub's handler over loopback, the way a page
// or a frame for one of the birds' reports does.
func birdRouteGet(t *testing.T, h *Hub, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// birdRoutesServer is a hub whose bird routes read root.
func birdRoutesServer(root string) *Hub {
	return NewHub(Config{BirdsDir: root})
}

// A fresh page and the page before it are served as the bird wrote them, under
// a policy of their own: no script, an opaque origin, and none of the
// dashboard's own policy, which would otherwise let the page read /api/state
// from this origin.
func TestBirdRoutesServeThePageInert(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pages := map[string]string{
		"daily.html":      "<!doctype html><html><body><p>dunlin's day</p></body></html>",
		"daily.prev.html": "<!doctype html><html><body><p>dunlin's day before</p></body></html>",
	}
	writeBird(t, root, "dunlin", birdWrites{page: pages["daily.html"], prev: pages["daily.prev.html"]})
	h := birdRoutesServer(root)

	for file, page := range pages {
		rec := birdRouteGet(t, h, "/birds/dunlin/"+file)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200 (body %q)", file, rec.Code, rec.Body.String())
		}
		for header, want := range map[string]string{
			"Content-Security-Policy": birdRouteCSP,
			"X-Content-Type-Options":  "nosniff",
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           "no-store",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("GET %s: %s %q, want %q", file, header, got, want)
			}
		}
		if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "default-src 'self'") {
			t.Errorf("GET %s: the page carries the dashboard's own policy: %q", file, csp)
		}
		if got := rec.Body.String(); got != page {
			t.Errorf("GET %s: body %q, want the page as written", file, got)
		}
	}
}

// Everything that is not /birds/<bird>/daily.html, daily.prev.html or a
// topic page <slug>.html answers 404, and the guard still answers 405 to
// a write and 403 to a Host that is not this machine.
func TestBirdRoutesRefuseEverythingElse(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBird(t, root, "dunlin", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("dunlin", birdReportsNow, "title", "summary")),
		page: "<p>a page</p>",
	})
	// A file of another name in the same reports directory, and a directory of
	// another spelling of the same bird: each is refused by the name check, not
	// by being absent.
	if err := os.WriteFile(filepath.Join(root, "dunlin", "reports", "other.txt"), []byte("<p>another file</p>"), 0o644); err != nil {
		t.Fatalf("write the file of another name: %v", err)
	}
	writeBird(t, root, "Dunlin", birdWrites{page: "<p>a page under another spelling</p>"})
	h := birdRoutesServer(root)

	for _, path := range []string{
		"/birds",
		"/birds/",
		"/birds/../x",
		"/birds/./daily.html",
		"/birds/%2e%2e/daily.html",
		"/birds/dunlin/daily.html/",
		"/birds/dunlin/other.txt",
		"/birds/dunlin/Other.html",
		"/birds/dunlin/a.b.html",
		"/birds/Dunlin/daily.html",
		"/birds/dunlin/daily.json",
		"/birds/nosuchbird/daily.html",
		"/birds/dunlin%2Fdaily.html",
		"/birds//daily.html",
	} {
		if rec := birdRouteGet(t, h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}

	post := httptest.NewRequest(http.MethodPost, "/birds/dunlin/daily.html", nil)
	post.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, post)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rec.Code)
	}

	hostile := httptest.NewRequest(http.MethodGet, "/birds/dunlin/daily.html", nil)
	hostile.Host = "evil.example.com"
	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, hostile)
	if rec.Code != http.StatusForbidden {
		t.Errorf("hostile Host: status %d, want 403", rec.Code)
	}
}

// A birds directory that is not an absolute path serves nothing, even when the
// path as written reaches a real page from the working directory: a relative
// directory resolves against the dashboard's own working directory, which is
// not the birds' runtime directory, so the route refuses before it reads.
func TestBirdRoutesRefuseADirectoryThatIsNotAbsolute(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBird(t, root, "dunlin", birdWrites{page: "<p>a page behind a relative path</p>"})
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(wd, root)
	if err != nil {
		t.Fatalf("rel from the working directory to the birds' root: %v", err)
	}
	if filepath.IsAbs(rel) {
		t.Fatalf("%q is absolute, so the case under test is not built", rel)
	}

	for _, dir := range []string{"", rel} {
		h := NewHub(Config{BirdsDir: dir})
		if rec := birdRouteGet(t, h, "/birds/dunlin/daily.html"); rec.Code != http.StatusNotFound {
			t.Errorf("BirdsDir %q: status %d, want 404 (body %q)", dir, rec.Code, rec.Body.String())
		}
	}
}

// A page that is a link, or is larger than the cap, is refused: the reader
// Lstats the file so it is never followed out of the reports directory, and the
// route's body never carries the text the link pointed at. A reports directory
// that is a link is not that bird's output either.
func TestBirdRoutesRefuseASymlinkAndAnOversizePage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const marker = "text outside the bird's reports directory"
	outside := filepath.Join(t.TempDir(), "outside.html")
	if err := os.WriteFile(outside, []byte(marker), 0o644); err != nil {
		t.Fatalf("write the file outside the reports directory: %v", err)
	}

	writeBird(t, root, "linked", birdWrites{})
	if err := os.Symlink(outside, filepath.Join(root, "linked", "reports", "daily.html")); err != nil {
		t.Fatalf("symlink the page: %v", err)
	}

	writeBird(t, root, "big", birdWrites{})
	if err := os.WriteFile(filepath.Join(root, "big", "reports", "daily.html"), make([]byte, birdPageMaxBytes+1), 0o644); err != nil {
		t.Fatalf("write the oversize page: %v", err)
	}

	linkedReports := filepath.Join(t.TempDir(), "reports")
	if err := os.Mkdir(linkedReports, 0o755); err != nil {
		t.Fatalf("mkdir the linked reports directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(linkedReports, "daily.html"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write the page behind the linked reports directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "linkdir"), 0o755); err != nil {
		t.Fatalf("mkdir the bird: %v", err)
	}
	if err := os.Symlink(linkedReports, filepath.Join(root, "linkdir", "reports")); err != nil {
		t.Fatalf("symlink the reports directory: %v", err)
	}

	linkedBird := filepath.Join(t.TempDir(), "linkdir")
	if err := os.MkdirAll(filepath.Join(linkedBird, "reports"), 0o755); err != nil {
		t.Fatalf("mkdir the bird behind the linked bird directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(linkedBird, "reports", "daily.html"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write the page behind the linked bird directory: %v", err)
	}
	if err := os.Symlink(linkedBird, filepath.Join(root, "linkedbird")); err != nil {
		t.Fatalf("symlink the bird directory: %v", err)
	}

	h := birdRoutesServer(root)
	for _, path := range []string{
		"/birds/linked/daily.html",
		"/birds/big/daily.html",
		"/birds/linkdir/daily.html",
		"/birds/linkedbird/daily.html",
	} {
		rec := birdRouteGet(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, marker) {
			t.Errorf("GET %s: body carries the text outside the reports directory", path)
		}
	}
}

// A page that looks like it holds a secret is withheld, and neither its body
// nor its error names any of the text. The same reader lists the birds as JSON,
// and a hub with no reader lists none.
func TestBirdRoutesWithholdASecretPage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	secret := "sk-" + strings.Repeat("a", 19) + "9"
	writeBird(t, root, "dunlin", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("dunlin", birdReportsNow, "dunlin's title", "dunlin's summary")),
		page: "<!doctype html><html><body><p>token " + secret + "</p></body></html>",
	})
	reader := NewBirdReportsReader(root)
	h := NewHub(Config{
		BirdsDir: root,
		Birds:    func() []BirdReport { return reader.Read(birdReportsNow) },
	})

	rec := birdRouteGet(t, h, "/birds/dunlin/daily.html")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, secret) {
		t.Errorf("the withheld response carries the secret")
	}
	if body := rec.Body.String(); !strings.Contains(body, "withheld") {
		t.Errorf("body %q, want it to say withheld", body)
	}

	rec = birdRouteGet(t, h, "/api/birds")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/birds: status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/api/birds: Content-Type %q, want application/json", ct)
	}
	var rows []BirdReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode /api/birds: %v (body %q)", err, rec.Body.String())
	}
	if len(rows) != 1 || rows[0].Bird != "dunlin" {
		t.Fatalf("/api/birds rows %+v, want the one bird the reader lists", rows)
	}
	if !strings.Contains(rec.Body.String(), `"bird":"dunlin"`) {
		t.Errorf("/api/birds body %q, want the row keyed by bird", rec.Body.String())
	}

	// A hub whose reader is not wired lists no birds, rather than null.
	rec = birdRouteGet(t, NewHub(Config{}), "/api/birds")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("/api/birds with no reader: body %q, want []", got)
	}
}

// A bird's topic pages are served by the same sandboxed route: a regular
// <slug>.html directly inside its reports directory. A link, a name outside the
// slug shape, daily.json and a dot segment are refused (gt-ylswr).
func TestBirdTopicPageRoute(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBird(t, root, "dunlin", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("dunlin", birdReportsNow, "title", "summary")),
		page: "<p>daily</p>",
	})
	reports := filepath.Join(root, "dunlin", "reports")
	topic := "<!doctype html><title>The pool</title><p>topic</p>"
	for name, body := range map[string]string{
		"pool-cutover.html": topic,
		"a.html":            "<p>one letter</p>",
		"9lives.html":       "<p>starts with a digit</p>",
		"secret-target.txt": "outside the shape",
		"a.b.html":          "<p>a dot in the slug</p>",
		"Upper.html":        "<p>an upper-case slug</p>",
	} {
		if err := os.WriteFile(filepath.Join(reports, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Symlink(filepath.Join(reports, "pool-cutover.html"), filepath.Join(reports, "linked.html")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(reports, "dir.html"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := birdRoutesServer(root)

	for _, name := range []string{"pool-cutover.html", "a.html", "9lives.html"} {
		rec := birdRouteGet(t, h, "/birds/dunlin/"+name)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", name, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != birdRouteCSP {
			t.Errorf("GET %s: policy %q, want the sandbox policy", name, got)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s: lost the guard's nosniff or no-store headers", name)
		}
	}
	if got := birdRouteGet(t, h, "/birds/dunlin/pool-cutover.html").Body.String(); got != topic {
		t.Errorf("the topic page body is %q, want it as written", got)
	}

	long := strings.Repeat("a", 62) + ".html"
	for _, path := range []string{
		"/birds/dunlin/linked.html",
		"/birds/dunlin/dir.html",
		"/birds/dunlin/daily.json",
		"/birds/dunlin/secret-target.txt",
		"/birds/dunlin/-lead.html",
		"/birds/dunlin/Upper.html",
		"/birds/dunlin/a.b.html",
		"/birds/dunlin/.html",
		"/birds/dunlin/" + long,
		"/birds/dunlin/pool-cutover.html/",
		"/birds/dunlin/../daily.html",
		"/birds/dunlin/%2e%2e/pool-cutover.html",
		"/birds/dunlin/..%2fpool-cutover.html",
		"/birds/dunlin/missing.html",
		"/birds/nosuchbird/pool-cutover.html",
	} {
		if rec := birdRouteGet(t, h, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}
}

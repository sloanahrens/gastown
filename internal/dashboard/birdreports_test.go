package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// The reader's clock: every fixture below writes its sidecars against this
// instant, so an age and a stale verdict are the ones the test meant.
var birdReportsNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// birdSidecarFor is the sidecar a bird named name writes at at: the fields a
// row shows, with written_at in the RFC 3339 the reader parses.
func birdSidecarFor(name string, at time.Time, title, summary string) birdSidecar {
	return birdSidecar{Bird: name, WrittenAt: at.UTC().Format(time.RFC3339), Title: title, Summary: summary}
}

// birdSidecarJSON is that sidecar as the file holds it. The marshal is what
// makes a summary with control characters or a non-ASCII rune in it still a
// valid sidecar, rather than an escape the test wrote by hand.
func birdSidecarJSON(t *testing.T, s birdSidecar) string {
	t.Helper()

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	return string(b)
}

// birdWrites is the content of one bird's reports directory: the files named
// here are written, and a file named by the empty string is not written at all.
type birdWrites struct {
	json string
	page string
	prev string
}

// writeBird writes <root>/<name>/reports and the files in w. The report names
// are spelled out rather than taken from the reader's constants, so a reader
// that looks for another name is red here.
func writeBird(t *testing.T, root, name string, w birdWrites) {
	t.Helper()

	dir := filepath.Join(root, name, "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	for file, content := range map[string]string{
		"daily.json":      w.json,
		"daily.html":      w.page,
		"daily.prev.html": w.prev,
	} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s of %s: %v", file, name, err)
		}
	}
}

// birdSetMtime gives path the instant the test means for it, so a page's age is
// the one the fixture wrote rather than the moment the test happened to run.
func birdSetMtime(t *testing.T, path string, at time.Time) {
	t.Helper()

	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// birdNames is the rows' birds in the order the reader returned them.
func birdNames(rows []BirdReport) []string {
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Bird
	}
	return names
}

// birdByName is the rows keyed by the bird each row is about.
func birdByName(rows []BirdReport) map[string]BirdReport {
	byName := make(map[string]BirdReport, len(rows))
	for _, r := range rows {
		byName[r.Bird] = r
	}
	return byName
}

// A summary is stale once it is more than 26 hours old: 25h59m and exactly 26h
// are fresh, 26h01m is stale. The rows the operator should see first come
// first — a bird's own ask for attention, then the stale ones, then by name.
func TestBirdReportsReadsFreshAndStale(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	writeBird(t, root, "fresh-bird", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("fresh-bird", now.Add(-(25*time.Hour+59*time.Minute)), "fresh title", "fresh summary")),
	})
	writeBird(t, root, "edge-bird", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("edge-bird", now.Add(-26*time.Hour), "edge title", "edge summary")),
	})
	writeBird(t, root, "old-bird", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("old-bird", now.Add(-(26*time.Hour+time.Minute)), "old title", "old summary")),
	})
	opFresh := birdSidecarFor("zzz-op", now.Add(-time.Hour), "op title", "op summary")
	opFresh.NeedsOperator, opFresh.Decisions = true, 3
	writeBird(t, root, "zzz-op", birdWrites{json: birdSidecarJSON(t, opFresh)})
	opStale := birdSidecarFor("aaa-stale", now.Add(-30*time.Hour), "op title", "op summary")
	opStale.NeedsOperator = true
	writeBird(t, root, "aaa-stale", birdWrites{json: birdSidecarJSON(t, opStale)})

	rows := NewBirdReportsReader(root).Read(now)

	wantOrder := []string{"aaa-stale", "zzz-op", "old-bird", "edge-bird", "fresh-bird"}
	if got := birdNames(rows); !slices.Equal(got, wantOrder) {
		t.Errorf("rows in order %v, want %v", got, wantOrder)
	}

	byName := birdByName(rows)
	for name, want := range map[string]struct {
		state string
		stale bool
	}{
		"fresh-bird": {BirdReportFresh, false},
		"edge-bird":  {BirdReportFresh, false},
		"old-bird":   {BirdReportStale, true},
		"zzz-op":     {BirdReportFresh, false},
		"aaa-stale":  {BirdReportStale, true},
	} {
		if r := byName[name]; r.State != want.state || r.Stale != want.stale {
			t.Errorf("%s: state %q stale %v, want %q stale %v", name, r.State, r.Stale, want.state, want.stale)
		}
	}

	fresh := byName["fresh-bird"]
	if !fresh.Written.Equal(now.Add(-(25*time.Hour + 59*time.Minute))) {
		t.Errorf("fresh-bird written at %v, want the sidecar's written_at", fresh.Written)
	}
	if fresh.Title != "fresh title" || fresh.Summary != "fresh summary" {
		t.Errorf("fresh-bird row shows %q / %q, want the sidecar's title and summary", fresh.Title, fresh.Summary)
	}
	if fresh.HasPage || fresh.HasPrev || fresh.Note != "" {
		t.Errorf("fresh-bird wrote no page and is readable, yet has_page %v has_prev %v note %q",
			fresh.HasPage, fresh.HasPrev, fresh.Note)
	}

	op := byName["zzz-op"]
	if !op.NeedsOperator || op.Decisions != 3 {
		t.Errorf("zzz-op has needs_operator %v decisions %d, want the sidecar's true and 3",
			op.NeedsOperator, op.Decisions)
	}
}

// Only a real directory, named like a bird, whose reports directory holds a
// regular daily.json or daily.html is a row: every other entry under the
// runtime directory — the 70-odd that are not birds, a symlinked bird, a
// symlinked sidecar — is skipped, so the page lists birds and nothing else.
func TestBirdReportsSkipsDirsThatAreNotBirdsWithReports(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	// The one row this fixture is meant to produce.
	writeBird(t, root, "plain", birdWrites{json: birdSidecarJSON(t, birdSidecarFor("plain", now, "title", "summary"))})

	// Names no bird may have: uppercase, a dot, an underscore, a leading digit,
	// and one past the 32 characters a name may run to.
	for _, name := range []string{"UPPERCASE", "has.dot", "under_score", "9bird", strings.Repeat("a", 33)} {
		writeBird(t, root, name, birdWrites{json: birdSidecarJSON(t, birdSidecarFor(name, now, "title", "summary"))})
	}

	// A directory that is not a bird's: no reports directory at all, and one
	// whose reports directory holds neither file.
	if err := os.MkdirAll(filepath.Join(root, "bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty-reports", "reports"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A bird directory that is a symlink to a bird that does have a report, so
	// following it would add a row the runtime directory does not hold.
	if err := os.Symlink(filepath.Join(root, "plain"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	// A daily.json that is a symlink to another bird's sidecar, with no page of
	// its own: reading through it would put that bird's summary under this one.
	writeBird(t, root, "linked-sidecar", birdWrites{})
	if err := os.Symlink(
		filepath.Join(root, "plain", "reports", "daily.json"),
		filepath.Join(root, "linked-sidecar", "reports", "daily.json"),
	); err != nil {
		t.Fatal(err)
	}

	rows := NewBirdReportsReader(root).Read(now)
	if got := birdNames(rows); !slices.Equal(got, []string{"plain"}) {
		t.Fatalf("rows %v, want only plain", got)
	}
}

// Everything the reader could not use is a row in error whose note names what
// is wrong, so the operator can tell the bird what to fix. A page with no
// sidecar is still a row: the page is what the bird wrote it for, and its own
// mtime is the only instant there is.
func TestBirdReportsNamesWhatItCouldNotRead(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	pageAt := now.Add(-30 * time.Minute)
	root := t.TempDir()

	writeBird(t, root, "oversize", birdWrites{json: strings.Repeat(" ", 16<<10+1)})
	writeBird(t, root, "badjson", birdWrites{json: `{"bird":`})
	writeBird(t, root, "otherbird", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("someone-else", now, "title", "summary")),
	})
	writeBird(t, root, "ahead", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("ahead", now.Add(10*time.Minute), "title", "summary")),
	})
	writeBird(t, root, "badtime", birdWrites{
		json: birdSidecarJSON(t, birdSidecar{Bird: "badtime", WrittenAt: "yesterday at noon"}),
	})
	writeBird(t, root, "htmlonly", birdWrites{page: "<html>today</html>"})
	birdSetMtime(t, filepath.Join(root, "htmlonly", "reports", "daily.html"), pageAt)

	// A sidecar that is a symlink beside a real page: the page is a report, and
	// the file named daily.json is refused rather than followed.
	writeBird(t, root, "linked-sidecar", birdWrites{page: "<html>today</html>"})
	birdSetMtime(t, filepath.Join(root, "linked-sidecar", "reports", "daily.html"), pageAt)
	if err := os.Symlink(
		filepath.Join(root, "badjson", "reports", "daily.json"),
		filepath.Join(root, "linked-sidecar", "reports", "daily.json"),
	); err != nil {
		t.Fatal(err)
	}

	// Exactly five minutes ahead is not "in the future": a clock that is
	// together to the minute is a clock, an hour is a wrong written_at.
	writeBird(t, root, "edge-ahead", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("edge-ahead", now.Add(5*time.Minute), "title", "summary")),
	})

	rows := NewBirdReportsReader(root).Read(now)
	byName := birdByName(rows)

	for name, note := range map[string]string{
		"oversize":       "daily.json is larger than 16 KiB",
		"badjson":        "daily.json is not valid JSON",
		"otherbird":      "daily.json names a different bird",
		"ahead":          "written_at is in the future",
		"badtime":        "written_at is not an RFC 3339 instant",
		"htmlonly":       "no summary",
		"linked-sidecar": "daily.json could not be read",
	} {
		r, ok := byName[name]
		if !ok {
			t.Errorf("%s: no row, want one in error (%q)", name, note)
			continue
		}
		if r.State != BirdReportError || r.Note != note {
			t.Errorf("%s: state %q note %q, want %q note %q", name, r.State, r.Note, BirdReportError, note)
		}
		if r.Title != "" || r.Summary != "" {
			t.Errorf("%s: an unreadable row shows title %q summary %q, want neither", name, r.Title, r.Summary)
		}
	}

	if html := byName["htmlonly"]; !html.Written.Equal(pageAt) {
		t.Errorf("htmlonly written at %v, want its page's mtime %v", html.Written, pageAt)
	}
	if html := byName["htmlonly"]; !html.HasPage {
		t.Error("htmlonly has a daily.html the reader can serve, but has_page is false")
	}
	if ahead := byName["ahead"]; !ahead.Written.IsZero() {
		t.Errorf("ahead shows written %v, want no instant for a refused sidecar", ahead.Written)
	}
	if edge := byName["edge-ahead"]; edge.State != BirdReportFresh || edge.Note != "" {
		t.Errorf("edge-ahead: state %q note %q, want a fresh row", edge.State, edge.Note)
	}
}

// A runtime directory the reader cannot read is one row in error, never an
// empty page that reads as "no bird has reported".
func TestBirdReportsNamesAnUnreadableRuntimeDir(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()
	asFile := filepath.Join(root, "runtime-is-a-file")
	if err := os.WriteFile(asFile, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, dir := range map[string]string{
		"a file": filepath.Join(asFile),
		"absent": filepath.Join(root, "nowhere"),
	} {
		rows := NewBirdReportsReader(dir).Read(now)
		if len(rows) != 1 {
			t.Fatalf("%s runtime directory: %d rows, want one in error", name, len(rows))
		}
		if rows[0].State != BirdReportError || rows[0].Note != "the runtime directory could not be read" {
			t.Errorf("%s runtime directory: state %q note %q, want an error row", name, rows[0].State, rows[0].Note)
		}
	}
}

// The summary a row carries is what the page may draw: control characters gone,
// no longer than 1200 runes, and withheld whole — title and summary both empty,
// the note naming only the rule — when it looks like it holds a secret.
func TestBirdReportsCleansTheSummary(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()
	// Positive inputs are joined from pieces so that no secret-looking literal
	// sits in this file for the repository's own scanners to flag.
	secret := "password=" + strings.Repeat("k", 16)

	hostile := "line one\nline two\tindent\rcarriage\x07 bell\x1b[31m nul:\x00 rlo:‮right"
	writeBird(t, root, "ctrl", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("ctrl", now, "ctrl title", hostile)),
	})
	long := strings.Repeat("日", 1300)
	writeBird(t, root, "long", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("long", now, "long title", long)),
	})
	writeBird(t, root, "leaky", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("leaky", now, "leaky title", "the bird tried "+secret)),
	})
	writeBird(t, root, "leaky-title", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("leaky-title", now, secret, "a clean summary")),
	})

	rows := NewBirdReportsReader(root).Read(now)
	byName := birdByName(rows)

	ctrl := byName["ctrl"]
	if ctrl.State != BirdReportFresh {
		t.Fatalf("ctrl: state %q note %q, want a fresh row", ctrl.State, ctrl.Note)
	}
	for _, want := range []string{"line one\nline two\tindent", "carriage", "bell", "[31m", "rlo", "right"} {
		if !strings.Contains(ctrl.Summary, want) {
			t.Errorf("the summary lost %q: %q", want, ctrl.Summary)
		}
	}
	for _, r := range ctrl.Summary {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("the summary kept the invisible rune %U: %q", r, ctrl.Summary)
		}
	}

	cut := byName["long"].Summary
	if got := utf8.RuneCountInString(cut); got != 1200 {
		t.Errorf("the long summary is %d runes, want 1200", got)
	}
	if strings.Trim(cut, "日") != "" {
		t.Errorf("the long summary was cut mid-rune: %q", cut)
	}

	leak := byName["leaky"]
	if leak.State != BirdReportWithheld {
		t.Errorf("leaky: state %q note %q, want %q", leak.State, leak.Note, BirdReportWithheld)
	}
	if leak.Summary != "" || leak.Title != "" {
		t.Errorf("leaky kept title %q summary %q, want neither", leak.Title, leak.Summary)
	}
	if !strings.Contains(leak.Note, "credential-assignment") {
		t.Errorf("leaky: note %q does not name the rule that matched", leak.Note)
	}

	title := byName["leaky-title"]
	if title.State != BirdReportWithheld || title.Summary != "" || title.Title != "" {
		t.Errorf("leaky-title: state %q title %q summary %q, want withheld and empty",
			title.State, title.Title, title.Summary)
	}

	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	for _, held := range []string{strings.Repeat("k", 8), secret} {
		if strings.Contains(string(encoded), held) {
			t.Errorf("a withheld secret reached the JSON-encoded rows: %q", held)
		}
	}
}

// has_page and has_prev say whether the page and the page before it are there
// to serve: a regular file under a megabyte. A page an hour older than the
// summary it sits beside is still a row, with a note saying so.
func TestBirdReportsSaysWhichPagesAreThere(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	writeBird(t, root, "with-page", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("with-page", now.Add(-time.Hour), "title", "summary")),
		page: "<html>today</html>",
		prev: "<html>yesterday</html>",
	})
	birdSetMtime(t, filepath.Join(root, "with-page", "reports", "daily.html"), now.Add(-30*time.Minute))
	birdSetMtime(t, filepath.Join(root, "with-page", "reports", "daily.prev.html"), now.Add(-30*time.Hour))

	writeBird(t, root, "late-page", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("late-page", now.Add(-2*time.Hour), "title", "summary")),
		page: "<html>yesterday</html>",
	})
	birdSetMtime(t, filepath.Join(root, "late-page", "reports", "daily.html"), now.Add(-5*time.Hour))

	// One byte over the cap, and exactly at it: neither is a page to serve.
	writeBird(t, root, "big-page", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("big-page", now, "title", "summary")),
		page: strings.Repeat("x", 1<<20+1),
	})
	writeBird(t, root, "exact-page", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("exact-page", now, "title", "summary")),
		page: strings.Repeat("x", 1<<20),
	})

	rows := NewBirdReportsReader(root).Read(now)
	byName := birdByName(rows)

	page := byName["with-page"]
	if !page.HasPage || !page.HasPrev || page.Note != "" {
		t.Errorf("with-page: has_page %v has_prev %v note %q, want both pages and no note",
			page.HasPage, page.HasPrev, page.Note)
	}

	if late := byName["late-page"]; late.Note != "page older than its summary" {
		t.Errorf("late-page: note %q, want the page named as older than its summary", late.Note)
	}

	for _, name := range []string{"big-page", "exact-page"} {
		if r := byName[name]; r.HasPage || r.HasPrev {
			t.Errorf("%s: has_page %v has_prev %v, want a page only under a megabyte", name, r.HasPage, r.HasPrev)
		}
	}
}

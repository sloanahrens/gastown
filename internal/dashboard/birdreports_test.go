package dashboard

import (
	"encoding/json"
	"fmt"
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

// The reader marks the reporter WithHeadline names, so the page can draw the
// town's headline report first without carrying a reporter's name of its own
// (gt-5c4df). The other rows are not marked, and a reader given no name — or a
// name no bird directory carries — marks none.
func TestBirdReportsMarksTheHeadlineReporter(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()
	for _, name := range []string{"curlew", "dunlin", "heron"} {
		writeBird(t, root, name, birdWrites{
			json: birdSidecarJSON(t, birdSidecarFor(name, now.Add(-time.Hour), "title", "summary")),
		})
	}

	for _, c := range []struct {
		name     string
		headline string
		want     string
	}{
		{name: "the named reporter is the marked one", headline: "heron", want: "heron"},
		{name: "no headline marks none", headline: "", want: ""},
		{name: "a name no bird directory carries marks none", headline: "osprey", want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			rows := NewBirdReportsReader(root).WithHeadline(c.headline).Read(now)
			if len(rows) != 3 {
				t.Fatalf("the reader returned %d rows, want the three birds: %+v", len(rows), rows)
			}
			for _, r := range rows {
				if want := r.Bird == c.want; r.Headline != want {
					t.Errorf("%s: headline %v, want %v", r.Bird, r.Headline, want)
				}
			}
		})
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

// A real bird directory may still point its reports directory, or a page in
// it, at another bird's output. Neither is that bird's report: the reports
// directory must be a real directory and a page a regular file, so following
// either link would put another path's text on this page.
func TestBirdReportsRefusesSymlinkedReportsAndPages(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	// The bird whose output the links below point at.
	writeBird(t, root, "plain", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("plain", now, "plain title", "plain summary")),
		page: "<html>plain</html>",
		prev: "<html>plain yesterday</html>",
	})

	// A real bird directory whose reports directory is a link to plain's:
	// following it would show plain's report under this bird's name.
	if err := os.MkdirAll(filepath.Join(root, "linked-reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(root, "plain", "reports"),
		filepath.Join(root, "linked-reports", "reports"),
	); err != nil {
		t.Fatal(err)
	}

	// A bird with its own sidecar whose two pages are links to plain's: the
	// summary is this bird's, the pages are not, so neither is a page to serve.
	writeBird(t, root, "linked-pages", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("linked-pages", now, "title", "summary")),
	})
	for link, target := range map[string]string{
		"daily.html":      filepath.Join(root, "plain", "reports", "daily.html"),
		"daily.prev.html": filepath.Join(root, "plain", "reports", "daily.prev.html"),
	} {
		if err := os.Symlink(target, filepath.Join(root, "linked-pages", "reports", link)); err != nil {
			t.Fatal(err)
		}
	}

	// A bird whose only page is a link and that has no sidecar: it names no
	// report at all, because the page is not one the reader can serve.
	writeBird(t, root, "linked-page-only", birdWrites{})
	if err := os.Symlink(
		filepath.Join(root, "plain", "reports", "daily.html"),
		filepath.Join(root, "linked-page-only", "reports", "daily.html"),
	); err != nil {
		t.Fatal(err)
	}

	rows := NewBirdReportsReader(root).Read(now)
	if got, want := birdNames(rows), []string{"linked-pages", "plain"}; !slices.Equal(got, want) {
		t.Fatalf("rows %v, want %v", got, want)
	}

	linked := birdByName(rows)["linked-pages"]
	if linked.State != BirdReportFresh || linked.Summary != "summary" {
		t.Errorf("linked-pages: state %q summary %q, want its own fresh summary", linked.State, linked.Summary)
	}
	if linked.HasPage || linked.HasPrev {
		t.Errorf("linked-pages: has_page %v has_prev %v, want neither through a link",
			linked.HasPage, linked.HasPrev)
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

// A title goes to the page beside the summary, so it is cleaned the same way:
// every control character but a newline and a tab, and every invisible format
// character, is gone. A bird that writes an escape sequence or a bidi
// override into its title does not get it onto the page.
func TestBirdReportsCleansTheTitle(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	hostile := "title\x07 bell\x1b[31m red\x00 nul‮here"
	writeBird(t, root, "hostile-title", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("hostile-title", now, hostile, "clean summary")),
	})

	rows := NewBirdReportsReader(root).Read(now)
	row := birdByName(rows)["hostile-title"]
	if row.State != BirdReportFresh {
		t.Fatalf("hostile-title: state %q note %q, want a fresh row", row.State, row.Note)
	}
	if want := "title bell[31m red nulhere"; row.Title != want {
		t.Errorf("the title is %q, want %q with the control and format runes gone", row.Title, want)
	}
	for _, r := range row.Title {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Errorf("the title kept the invisible rune %U: %q", r, row.Title)
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

// decision_items is the reporter's own list of decisions to put on the one
// "Needs you" list, so it is decoded on its own: a missing, null or malformed
// array costs the row its list and never its summary, and a reporter that
// writes more than the cap has its list cut rather than the page grown.
func TestBirdReportsDecisionItems(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	// Nine items: only the first eight are read, and the two with control
	// characters in them are cleaned the way the summary is.
	asker := birdSidecarFor("asker", now, "title", "summary")
	asker.DecisionItems = json.RawMessage(`[
		{"question": "ship the darker palette?", "recommendation": "yes, ship it"},
		{"question": "question\u0007 bell \u001b[31m", "recommendation": "answer\rwith\rcontrol"},
		{"question": "item three", "recommendation": "rec three"},
		{"question": "item four", "recommendation": "rec four"},
		{"question": "item five", "recommendation": "rec five"},
		{"question": "item six", "recommendation": "rec six"},
		{"question": "item seven", "recommendation": "rec seven"},
		{"question": "item eight", "recommendation": "rec eight"},
		{"question": "item nine", "recommendation": "rec nine"}
	]`)
	writeBird(t, root, "asker", birdWrites{json: birdSidecarJSON(t, asker)})

	long := birdSidecarFor("long", now, "title", "summary")
	long.DecisionItems = json.RawMessage(`[{"question": "` + strings.Repeat("日", 300) + `", "recommendation": "rec"}]`)
	writeBird(t, root, "long", birdWrites{json: birdSidecarJSON(t, long)})

	// Every shape that is not the array of questions the writer promises: the
	// row keeps its summary and reads fresh, and carries no items.
	missing := birdSidecarFor("missing", now, "missing title", "missing summary")
	writeBird(t, root, "missing", birdWrites{json: birdSidecarJSON(t, missing)})

	nullItems := birdSidecarFor("null-items", now, "null title", "null summary")
	nullItems.DecisionItems = json.RawMessage(`null`)
	writeBird(t, root, "null-items", birdWrites{json: birdSidecarJSON(t, nullItems)})

	notArray := birdSidecarFor("not-array", now, "string title", "string summary")
	notArray.DecisionItems = json.RawMessage(`"two questions and an answer"`)
	writeBird(t, root, "not-array", birdWrites{json: birdSidecarJSON(t, notArray)})

	wrongType := birdSidecarFor("wrong-type", now, "wrong title", "wrong summary")
	wrongType.DecisionItems = json.RawMessage(`[{"question": 7, "recommendation": "rec"}]`)
	writeBird(t, root, "wrong-type", birdWrites{json: birdSidecarJSON(t, wrongType)})

	// An entry with nothing but control characters after cleaning is not a
	// question, so it is not an item; the entry beside it is.
	emptyQuestion := birdSidecarFor("empty-question", now, "empty title", "empty summary")
	emptyQuestion.DecisionItems = json.RawMessage(`[
		{"question": "\u0007\u0000", "recommendation": "orphan answer"},
		{"question": "kept?", "recommendation": ""}
	]`)
	writeBird(t, root, "empty-question", birdWrites{json: birdSidecarJSON(t, emptyQuestion)})

	// Entries with no question are not decisions, so they do not spend the
	// cap: eight of them ahead of two real questions still leave both shown.
	padded := birdSidecarFor("padded", now, "padded title", "padded summary")
	padded.DecisionItems = json.RawMessage(`[
		{"question": "\u0007", "recommendation": "one"},
		{"question": "\u0007", "recommendation": "two"},
		{"question": "\u0007", "recommendation": "three"},
		{"question": "\u0007", "recommendation": "four"},
		{"question": "\u0007", "recommendation": "five"},
		{"question": "\u0007", "recommendation": "six"},
		{"question": "\u0007", "recommendation": "seven"},
		{"question": "\u0007", "recommendation": "eight"},
		{"question": "the real question?", "recommendation": "yes"},
		{"question": "and the next?", "recommendation": "later"}
	]`)
	writeBird(t, root, "padded", birdWrites{json: birdSidecarJSON(t, padded)})

	rows := NewBirdReportsReader(root).Read(now)
	byName := birdByName(rows)

	got := byName["asker"].DecisionItems
	if len(got) != 8 {
		t.Fatalf("asker carried %d items, want the first 8", len(got))
	}
	if got[0].Question != "ship the darker palette?" || got[0].Recommendation != "yes, ship it" {
		t.Errorf("asker's first item is %+v, want the question and recommendation as written", got[0])
	}
	if got[7].Question != "item eight" {
		t.Errorf("asker's last item is %q, want item eight: the list was not cut at 8 in order", got[7].Question)
	}
	if got[1].Question != "question bell [31m" {
		t.Errorf("asker's second question is %q, want the control characters gone", got[1].Question)
	}
	if got[1].Recommendation != "answerwithcontrol" {
		t.Errorf("asker's second recommendation is %q, want the control characters gone", got[1].Recommendation)
	}

	cut := byName["long"].DecisionItems
	if len(cut) != 1 {
		t.Fatalf("long carried %d items, want 1", len(cut))
	}
	if n := utf8.RuneCountInString(cut[0].Question); n != 240 {
		t.Errorf("the long question is %d runes, want 240", n)
	}
	if strings.Trim(cut[0].Question, "日") != "" {
		t.Errorf("the long question was cut mid-rune: %q", cut[0].Question)
	}

	for _, name := range []string{"missing", "null-items", "not-array", "wrong-type"} {
		row := byName[name]
		// The two malformed shapes say so in the note (gt-tm1fa); the row is
		// still fresh and keeps its summary.
		wantNote := map[string]string{"not-array": "decision items could not be read", "wrong-type": "1 decision item could not be read"}[name]
		if row.State != BirdReportFresh || row.Note != wantNote {
			t.Errorf("%s: state %q note %q, want a fresh row with note %q and no items", name, row.State, row.Note, wantNote)
		}
		if row.Summary == "" {
			t.Errorf("%s: a sidecar that is not an items array cost the row its summary", name)
		}
		if len(row.DecisionItems) != 0 {
			t.Errorf("%s: carried %d items, want none", name, len(row.DecisionItems))
		}
	}

	kept := byName["empty-question"].DecisionItems
	if len(kept) != 1 || kept[0].Question != "kept?" || kept[0].Recommendation != "" {
		t.Errorf("empty-question carried %+v, want only the item with a question", kept)
	}

	paddedItems := byName["padded"].DecisionItems
	if len(paddedItems) != 2 || paddedItems[0].Question != "the real question?" || paddedItems[1].Question != "and the next?" {
		t.Errorf("padded carried %+v, want the two real questions past the entries that are not decisions", paddedItems)
	}
}

// One element of the wrong type costs the row that element and nothing else:
// the other questions are shown and the note counts the one that was skipped.
// A value that is not an array shows nothing and says so; a missing, null or
// empty list stays silent (gt-tm1fa).
func TestBirdDecisionItemsUnreadable(t *testing.T) {
	t.Parallel()

	good := `{"question": "q%d?", "recommendation": "r"}`
	seven := ""
	for i := 1; i <= 7; i++ {
		seven += fmt.Sprintf(good, i) + ","
	}
	cases := []struct {
		name      string
		raw       string
		wantItems int
		wantNote  string
	}{
		{"seven good and one wrong-typed", `[` + seven + `{"q": 5}]`, 7, "1 decision item could not be read"},
		{"a list-valued question", `[` + seven + `{"q": ["x"]}]`, 7, "1 decision item could not be read"},
		{"two bad elements", `[{"q": 5}, ` + seven + `"text"]`, 7, "2 decision items could not be read"},
		{"not an array", `{"question": "q?"}`, 0, "decision items could not be read"},
		{"a string", `"q?"`, 0, "decision items could not be read"},
		{"empty array", `[]`, 0, ""},
		{"null", `null`, 0, ""},
		{"missing", ``, 0, ""},
	}
	for _, c := range cases {
		items, _, note := birdDecisionItems(json.RawMessage(c.raw))
		if len(items) != c.wantItems || note != c.wantNote {
			t.Errorf("%s: %d items and note %q, want %d and %q", c.name, len(items), note, c.wantItems, c.wantNote)
		}
	}
}

// A decision item's question and recommendation are text a bird wrote, so each
// passes the screen the summary passes: a flagged item is dropped whole, its
// rule named on the row, and the row's own state left alone because the
// reporter's summary is still there to show.
func TestBirdReportsDecisionItemsWithheld(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()
	// Positive inputs are joined from pieces so that no secret-looking literal
	// sits in this file for the repository's own scanners to flag.
	secret := "password=" + strings.Repeat("k", 16)

	asker := birdSidecarFor("asker", now, "title", "summary")
	asker.DecisionItems = json.RawMessage(`[
		{"question": "deploy now?", "recommendation": "wait for green"},
		{"question": "what is the token?", "recommendation": "` + secret + `"},
		{"question": "which bird writes?", "recommendation": "heron"}
	]`)
	writeBird(t, root, "asker", birdWrites{json: birdSidecarJSON(t, asker)})

	// A withheld summary does not take the reporter's clean asks with it: the
	// items are screened on their own text.
	leaky := birdSidecarFor("leaky-summary", now, "leaky title", "the bird tried "+secret)
	leaky.DecisionItems = json.RawMessage(`[{"question": "retry the patrol?", "recommendation": "yes"}]`)
	writeBird(t, root, "leaky-summary", birdWrites{json: birdSidecarJSON(t, leaky)})

	rows := NewBirdReportsReader(root).Read(now)
	byName := birdByName(rows)

	row := byName["asker"]
	if row.State != BirdReportFresh || row.Note != "" {
		t.Errorf("asker: state %q note %q, want the row itself untouched", row.State, row.Note)
	}
	if len(row.DecisionItems) != 2 {
		t.Fatalf("asker carried %d items, want the two clean ones", len(row.DecisionItems))
	}
	if row.DecisionItems[0].Question != "deploy now?" || row.DecisionItems[1].Question != "which bird writes?" {
		t.Errorf("asker kept %+v, want the clean items in their own order", row.DecisionItems)
	}
	if !slices.Equal(row.DecisionItemsWithheld, []string{"credential-assignment"}) {
		t.Errorf("asker withheld %v, want the one rule that matched", row.DecisionItemsWithheld)
	}

	leakyRow := byName["leaky-summary"]
	if leakyRow.State != BirdReportWithheld || leakyRow.Summary != "" {
		t.Errorf("leaky-summary: state %q summary %q, want the summary withheld", leakyRow.State, leakyRow.Summary)
	}
	if len(leakyRow.DecisionItems) != 1 || leakyRow.DecisionItems[0].Question != "retry the patrol?" {
		t.Errorf("leaky-summary carried %+v, want its clean ask", leakyRow.DecisionItems)
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
	if !strings.Contains(string(encoded), `"decision_items_withheld":["credential-assignment"]`) {
		t.Errorf("the rows do not carry the rule that withheld an item: %s", encoded)
	}
}

// The reporters write decision_items as objects with the keys q and rec, so
// those are the spellings the list is read from: the short question becomes the
// row's question and the short recommendation its recommendation.
func TestBirdReportsDecisionItemShortKeys(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	asker := birdSidecarFor("asker", now, "title", "summary")
	asker.DecisionItems = json.RawMessage(`[
		{"q": "ship the darker palette?", "rec": "yes, ship it"},
		{"q": "and the rollback plan?", "rec": "the flag"}
	]`)
	writeBird(t, root, "asker", birdWrites{json: birdSidecarJSON(t, asker)})

	byName := birdByName(NewBirdReportsReader(root).Read(now))

	got := byName["asker"].DecisionItems
	if len(got) != 2 {
		t.Fatalf("asker carried %d items, want both short-key items", len(got))
	}
	if got[0].Question != "ship the darker palette?" || got[0].Recommendation != "yes, ship it" {
		t.Errorf("asker's first item is %+v, want the short keys read as question and recommendation", got[0])
	}
	if got[1].Question != "and the rollback plan?" || got[1].Recommendation != "the flag" {
		t.Errorf("asker's second item is %+v, want the short keys read as question and recommendation", got[1])
	}

	// The JSON the page reads keeps the long names whatever the sidecar wrote.
	encoded, err := json.Marshal(byName["asker"])
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if !strings.Contains(string(encoded), `"question":"ship the darker palette?"`) || !strings.Contains(string(encoded), `"recommendation":"yes, ship it"`) {
		t.Errorf("the row does not serve the item under the long names: %s", encoded)
	}
}

// Both spellings are read. An item written with the long keys is unchanged, an
// item that wrote both answers to the long one, and an item that wrote no
// question under either spelling is not a decision and is dropped.
func TestBirdReportsDecisionItemKeyPrecedence(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()

	asker := birdSidecarFor("asker", now, "title", "summary")
	asker.DecisionItems = json.RawMessage(`[
		{"question": "long question?", "recommendation": "long rec"},
		{"question": "the long one", "q": "the short one", "recommendation": "long rec", "rec": "short rec"},
		{"q": "short only?", "rec": "short rec"},
		{"rec": "an answer with no question"},
		{"q": "", "rec": "an empty short question"}
	]`)
	writeBird(t, root, "asker", birdWrites{json: birdSidecarJSON(t, asker)})

	byName := birdByName(NewBirdReportsReader(root).Read(now))

	got := byName["asker"].DecisionItems
	if len(got) != 3 {
		t.Fatalf("asker carried %d items, want the long-only, both-keys and short-only items", len(got))
	}
	if got[0].Question != "long question?" || got[0].Recommendation != "long rec" {
		t.Errorf("the long-key item is %+v, want its own text", got[0])
	}
	if got[1].Question != "the long one" || got[1].Recommendation != "long rec" {
		t.Errorf("the both-keys item is %+v, want the long spelling to win on both fields", got[1])
	}
	if got[2].Question != "short only?" || got[2].Recommendation != "short rec" {
		t.Errorf("the short-key item is %+v, want its own text", got[2])
	}
}

// Reading the short keys does not move the guards after them: a short-key item
// still spends the cap, is still cut to the rune limit, and a short-key
// recommendation that looks like a secret is still dropped and its rule named.
func TestBirdReportsDecisionItemShortKeysScreened(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()
	// Positive inputs are joined from pieces so that no secret-looking literal
	// sits in this file for the repository's own scanners to flag.
	secret := "password=" + strings.Repeat("k", 16)

	// Nine short-key items: the ninth is past the cap.
	capped := birdSidecarFor("capped", now, "title", "summary")
	capped.DecisionItems = json.RawMessage(`[
		{"q": "one", "rec": "r"},
		{"q": "two", "rec": "r"},
		{"q": "three", "rec": "r"},
		{"q": "four", "rec": "r"},
		{"q": "five", "rec": "r"},
		{"q": "six", "rec": "r"},
		{"q": "seven", "rec": "r"},
		{"q": "eight", "rec": "r"},
		{"q": "nine", "rec": "r"}
	]`)
	writeBird(t, root, "capped", birdWrites{json: birdSidecarJSON(t, capped)})

	long := birdSidecarFor("long", now, "title", "summary")
	long.DecisionItems = json.RawMessage(`[{"q": "` + strings.Repeat("日", 300) + `", "rec": "` + strings.Repeat("文", 300) + `"}]`)
	writeBird(t, root, "long", birdWrites{json: birdSidecarJSON(t, long)})

	leaky := birdSidecarFor("leaky", now, "title", "summary")
	leaky.DecisionItems = json.RawMessage(`[
		{"q": "what is the token?", "rec": "` + secret + `"},
		{"q": "and then?", "rec": "wait"}
	]`)
	writeBird(t, root, "leaky", birdWrites{json: birdSidecarJSON(t, leaky)})

	byName := birdByName(NewBirdReportsReader(root).Read(now))

	cappedItems := byName["capped"].DecisionItems
	if len(cappedItems) != 8 {
		t.Fatalf("capped carried %d items, want the first 8 short-key items", len(cappedItems))
	}
	if cappedItems[7].Question != "eight" {
		t.Errorf("capped's last item is %q, want eight: the list was not cut at 8 in order", cappedItems[7].Question)
	}

	cut := byName["long"].DecisionItems
	if len(cut) != 1 {
		t.Fatalf("long carried %d items, want 1", len(cut))
	}
	if n := utf8.RuneCountInString(cut[0].Question); n != 240 {
		t.Errorf("the long short-key question is %d runes, want 240", n)
	}
	if n := utf8.RuneCountInString(cut[0].Recommendation); n != 240 {
		t.Errorf("the long short-key recommendation is %d runes, want 240", n)
	}
	if strings.Trim(cut[0].Question, "日") != "" || strings.Trim(cut[0].Recommendation, "文") != "" {
		t.Errorf("a short-key field was cut mid-rune: %+v", cut[0])
	}

	leakyRow := byName["leaky"]
	if len(leakyRow.DecisionItems) != 1 || leakyRow.DecisionItems[0].Question != "and then?" {
		t.Errorf("leaky carried %+v, want only the clean short-key item", leakyRow.DecisionItems)
	}
	if !slices.Equal(leakyRow.DecisionItemsWithheld, []string{"credential-assignment"}) {
		t.Errorf("leaky withheld %v, want the one rule a short-key recommendation matched", leakyRow.DecisionItemsWithheld)
	}
}

// The bird's row lists its other pages: topic pages directly inside the reports
// directory, regular files only, by their own <title> (the name when there is
// none or the title looks like a secret), newest first, at most ten. The daily
// pages and files of another shape are not topic pages (gt-ylswr).
func TestBirdTopicPagesListed(t *testing.T) {
	t.Parallel()

	now := birdReportsNow
	root := t.TempDir()
	writeBird(t, root, "heron", birdWrites{
		json: birdSidecarJSON(t, birdSidecarFor("heron", now, "title", "summary")),
		page: "<title>Daily</title>",
		prev: "<title>Daily before</title>",
	})
	writeBird(t, root, "quiet", birdWrites{page: "<p>daily only</p>"})
	reports := filepath.Join(root, "heron", "reports")
	write := func(name, body string, age time.Duration) {
		t.Helper()
		path := filepath.Join(reports, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		birdSetMtime(t, path, now.Add(-age))
	}
	// Twelve topic pages, one an hour older than the last.
	for i := 0; i < 12; i++ {
		write(fmt.Sprintf("topic-%02d.html", i), fmt.Sprintf("<html><head><title>Topic %d &amp; more</title></head></html>", i), time.Duration(i+1)*time.Hour)
	}
	write("untitled.html", "<p>no title here</p>", 30*time.Minute)
	write("leaky.html", "<title>"+"password="+strings.Repeat("k", 16)+"</title>", 20*time.Minute)
	write("notes.txt", "<title>not a page</title>", time.Minute)
	write("Upper.html", "<title>not the shape</title>", time.Minute)
	if err := os.Symlink(filepath.Join(reports, "topic-00.html"), filepath.Join(reports, "linked.html")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(reports, "dir.html"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	byName := birdByName(NewBirdReportsReader(root).Read(now))
	pages := byName["heron"].Pages
	if len(pages) != 10 {
		t.Fatalf("heron lists %d pages, want the newest 10: %+v", len(pages), pages)
	}
	var names []string
	for _, p := range pages {
		names = append(names, p.Name)
	}
	want := []string{"leaky.html", "untitled.html", "topic-00.html", "topic-01.html", "topic-02.html", "topic-03.html", "topic-04.html", "topic-05.html", "topic-06.html", "topic-07.html"}
	if !slices.Equal(names, want) {
		t.Errorf("heron lists %v, want %v: newest first, no daily page, link, directory or other shape", names, want)
	}
	if pages[0].Title != "leaky.html" {
		t.Errorf("a title that looks like a secret is listed as %q, want the page's name", pages[0].Title)
	}
	if pages[1].Title != "untitled.html" {
		t.Errorf("a page with no title is listed as %q, want its name", pages[1].Title)
	}
	if pages[2].Title != "Topic 0 & more" {
		t.Errorf("a titled page is listed as %q, want its <title> with entities decoded", pages[2].Title)
	}
	if want := now.Add(-time.Hour).UTC(); !pages[2].MTime.Equal(want) {
		t.Errorf("topic-00 lists mtime %v, want %v", pages[2].MTime, want)
	}
	if got := byName["quiet"].Pages; len(got) != 0 {
		t.Errorf("a bird with only a daily page lists %+v, want none", got)
	}

	// The JSON carries them as name, title and mtime, and leaves the key out when
	// there are none.
	raw, err := json.Marshal(byName["heron"])
	if err != nil || !strings.Contains(string(raw), `"pages":[{"name":"leaky.html","title":"leaky.html","mtime":"`) {
		t.Errorf("heron's row encodes as %s (%v), want a pages array of name, title, mtime", raw, err)
	}
	raw, _ = json.Marshal(byName["quiet"])
	if strings.Contains(string(raw), `"pages"`) {
		t.Errorf("a row with no topic pages encodes as %s, want no pages key", raw)
	}
}

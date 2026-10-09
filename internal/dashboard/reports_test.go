package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
)

// The panel's clock: every fixture below is written against this instant, so an
// age and an overdue verdict are the ones the test meant.
var reportsNow = time.Date(2026, 10, 8, 15, 30, 0, 0, time.UTC)

// reportName is the name the overseer's tool gives the report it writes at at.
func reportName(at time.Time) string { return at.UTC().Format(reportsNameLayout) + reportsExt }

// reportStem is that name without its .md, which is what /api/report names a
// report by.
func reportStem(at time.Time) string { return at.UTC().Format(reportsNameLayout) }

// reportServer is a hub whose Report pane lists dir's reports, which is all the
// /api/report route reads to serve one of them.
func reportServer(t *testing.T, dir string) *Hub {
	t.Helper()

	now := reportsNow
	return NewHub(Config{
		Now:        func() time.Time { return now },
		Reports:    func() *Reports { return NewReportsReader(dir).Read(now) },
		ReportsDir: dir,
	})
}

// reportGet sends one GET for a report to a hub's handler, the way the page's
// fetch does, over loopback so the guard lets it through.
func reportGet(t *testing.T, h *Hub, query string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest("GET", "/api/report?"+query, nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// reportsFixture writes a reports directory holding name -> text and returns
// its path. A name that is not the overseer's is written too, which is how the
// latest.md copy is covered.
func reportsFixture(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The pane shows the newest write: its text, the instant its name carries, and
// the writes before it, newest first. A file whose name is not the overseer's —
// the latest.md copy the tool leaves for a human, and a name that is close to
// the shape but is not it — is not a report of its own.
func TestReportsReadsTheLatestWrite(t *testing.T) {
	t.Parallel()

	older, newest := reportsNow.Add(-time.Hour), reportsNow.Add(-time.Minute)
	dir := reportsFixture(t, map[string]string{
		reportName(older):  "the older report",
		reportName(newest): "the newest report",
		"latest.md":        "the copy the tool leaves for a human",
		"20261008T1529":    "a name with no extension",
	})

	r := NewReportsReader(dir).Read(reportsNow)
	if r.State != ReportsFresh || r.Note != "" {
		t.Fatalf("state %q note %q, want a fresh report and no note", r.State, r.Note)
	}
	if r.Overdue {
		t.Error("a report a minute old reads as overdue")
	}
	if r.Text != "the newest report" {
		t.Errorf("text %q, want the newest report's", r.Text)
	}
	if !r.Written.Equal(newest) {
		t.Errorf("written %v, want %v", r.Written, newest)
	}
	if want := []time.Time{newest, older}; !reflect.DeepEqual(r.Times, want) {
		t.Errorf("times %v, want %v", r.Times, want)
	}
}

// A latest report past its own hour reads overdue and one inside it does not:
// the boundary is what tells a late overseer from one that has simply not run
// yet. The reader leaves the note empty — the pane names an overdue report in
// its own words.
func TestReportsReadsALateReportAsOverdue(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		age     time.Duration
		overdue bool
		state   string
	}{
		{time.Minute, false, ReportsFresh},
		{reportsOverdue - time.Minute, false, ReportsFresh},
		{reportsOverdue, false, ReportsFresh},
		{reportsOverdue + time.Second, true, ReportsOverdue},
		{48 * time.Hour, true, ReportsOverdue},
	} {
		dir := reportsFixture(t, map[string]string{reportName(reportsNow.Add(-tc.age)): "the report"})
		r := NewReportsReader(dir).Read(reportsNow)
		if r.State != tc.state || r.Overdue != tc.overdue {
			t.Errorf("a report %v old: state %q overdue %v, want %q %v", tc.age, r.State, r.Overdue, tc.state, tc.overdue)
		}
		if r.Note != "" {
			t.Errorf("a report %v old: note %q, want none", tc.age, r.Note)
		}
		if r.Text != "the report" {
			t.Errorf("a report %v old: text %q", tc.age, r.Text)
		}
	}
}

// Every input the reader cannot use is a named state rather than a blank pane,
// and no two of them answer alike: a directory with no report in it says the
// overseer has written none, and a read that failed names its own failure, so a
// failed read can never be read as "no reports".
func TestReportsNamesEveryStateItCannotRead(t *testing.T) {
	t.Parallel()

	oversize := reportsFixture(t, map[string]string{
		reportName(reportsNow.Add(-time.Minute)): strings.Repeat("x", reportsMaxBytes+1),
	})
	gone := reportsFixture(t, map[string]string{reportName(reportsNow.Add(-time.Minute)): "the report"})
	if err := os.Remove(filepath.Join(gone, reportName(reportsNow.Add(-time.Minute)))); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(gone, "nowhere.md"), filepath.Join(gone, reportName(reportsNow.Add(-time.Minute)))); err != nil {
		t.Fatal(err)
	}
	notADir := filepath.Join(t.TempDir(), "reports")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	notes := map[string]string{}
	for _, tc := range []struct {
		name  string
		dir   string
		state string
		note  string
	}{
		{"no directory", filepath.Join(t.TempDir(), "gone"), ReportsMissing, reportsNoOverseer},
		{"an empty directory", t.TempDir(), ReportsMissing, reportsNoneYet},
		{"a path that is not a directory", notADir, ReportsError, reportsBadDir},
		{"a report over the cap", oversize, ReportsError, reportsOversize},
		{"a report that is not there", gone, ReportsError, reportsBadReport},
	} {
		r := NewReportsReader(tc.dir).Read(reportsNow)
		if r.State != tc.state || r.Note != tc.note {
			t.Errorf("%s: state %q note %q, want %q %q", tc.name, r.State, r.Note, tc.state, tc.note)
		}
		if r.Times == nil {
			t.Errorf("%s: the page's payload has a null list of times", tc.name)
		}
		if prev, ok := notes[tc.note]; ok {
			t.Errorf("%s answers %q, the same as %s", tc.name, tc.note, prev)
		}
		notes[tc.note] = tc.name
	}
}

// The report is another tool's text. Every control character but a newline and
// a tab is stripped — the page draws the text with pre-wrap, so those two are
// the ones that carry the report's shape — along with the invisible format
// characters. An HTML tag is left as the text it is, because the page writes
// the text with textContent and parses no markup.
func TestReportsStripsControlCharacters(t *testing.T) {
	t.Parallel()

	hostile := "line one\nline two\tindent\rcarriage\x07 bell\x1b[31m nul:\x00 rlo:‮right-to-left <b>bold</b>"
	dir := reportsFixture(t, map[string]string{reportName(reportsNow): hostile})

	r := NewReportsReader(dir).Read(reportsNow)
	if r.State != ReportsFresh {
		t.Fatalf("state %q note %q", r.State, r.Note)
	}
	for _, want := range []string{"line one\nline two\tindent", "carriage", "bell", "[31m", "rlo", "right-to-left", "<b>bold</b>"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("the text lost %q: %q", want, r.Text)
		}
	}
	for _, run := range r.Text {
		if run == '\n' || run == '\t' {
			continue
		}
		if unicode.IsControl(run) || unicode.Is(unicode.Cf, run) {
			t.Errorf("the report kept the invisible rune %U: %q", run, r.Text)
		}
	}
}

// The town keeps every report and the pane lists the newest day of them: the
// list is the newest twenty-four writes, newest first, and the report shown is
// the newest of those.
func TestReportsKeepsTheNewestTwentyFour(t *testing.T) {
	t.Parallel()

	files := map[string]string{}
	for i := 0; i < reportsKept+6; i++ {
		at := reportsNow.Add(-time.Duration(i) * time.Minute)
		files[reportName(at)] = "report " + at.Format(reportsNameLayout)
	}

	r := NewReportsReader(reportsFixture(t, files)).Read(reportsNow)
	if len(r.Times) != reportsKept {
		t.Fatalf("the list holds %d writes, want %d", len(r.Times), reportsKept)
	}
	if !r.Times[0].Equal(reportsNow) {
		t.Errorf("the list starts at %v, want the newest write", r.Times[0])
	}
	if !r.Times[reportsKept-1].Equal(reportsNow.Add(-time.Duration(reportsKept-1) * time.Minute)) {
		t.Errorf("the list ends at %v, want the twenty-fourth write", r.Times[reportsKept-1])
	}
	if !r.Written.Equal(reportsNow) || r.Text != "report "+reportsNow.Format(reportsNameLayout) {
		t.Errorf("the report shown is not the newest write: %v %q", r.Written, r.Text)
	}
}

// The reader only reads: a pass over the reports directory in every state
// leaves it exactly as it was, so nothing on this page can write into the
// overseer's own output.
func TestReportsReaderWritesNothing(t *testing.T) {
	t.Parallel()

	dir := reportsFixture(t, map[string]string{
		reportName(reportsNow.Add(-time.Minute)): "the report",
		"latest.md":                              "the copy",
	})
	before := cloudTree(t, dir)
	if len(before) != 2 {
		t.Fatalf("the fixture is not the two files it should be: %v", before)
	}
	r := NewReportsReader(dir)
	r.Read(reportsNow)                                            // fresh
	r.Read(reportsNow.Add(48 * time.Hour))                        // overdue
	r.Read(reportsNow)                                            // again, from a warm reader
	NewReportsReader(filepath.Join(dir, "gone")).Read(reportsNow) // no directory
	if after := cloudTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("a read changed the reports directory:\nbefore %v\nafter  %v", before, after)
	}
}

// The route the pane reads an earlier report through: a name the reader lists
// returns that report's text, the same field the pane draws, and nothing else.
func TestReportRouteReadsAListedReport(t *testing.T) {
	t.Parallel()

	older := reportsNow.Add(-time.Hour)
	dir := reportsFixture(t, map[string]string{
		reportName(reportsNow.Add(-time.Minute)): "the newest report",
		reportName(older):                        "the older report",
	})

	rec := reportGet(t, reportServer(t, dir), "name="+reportStem(older))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/report = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, `"text":"the older report"`) {
		t.Errorf("the route did not return the listed report's text: %s", got)
	}
}

// A name that is not the shape a report's name has is refused as a bad request,
// and one of the right shape that the reader does not list is refused as not
// found: neither reaches the directory, and no path is ever built from the
// query string. "../latest" is both cases at once, and the one an attacker
// tries first.
func TestReportRouteRefusesNamesThatAreNotListedReports(t *testing.T) {
	t.Parallel()

	dir := reportsFixture(t, map[string]string{reportName(reportsNow.Add(-time.Minute)): "the report"})
	h := reportServer(t, dir)

	for _, tc := range []struct {
		name string
		code int
	}{
		{"../latest", http.StatusBadRequest},
		{"latest", http.StatusBadRequest},
		{"20261008T153000.md", http.StatusBadRequest},                     // a name, not the timestamp
		{"20261008T15300Z", http.StatusBadRequest},                        // a digit short of the shape
		{"2026100T153000Z", http.StatusBadRequest},                        // a digit short in the date
		{reportStem(reportsNow.Add(-2 * time.Hour)), http.StatusNotFound}, // right shape, not listed
	} {
		rec := reportGet(t, h, "name="+url.QueryEscape(tc.name))
		if rec.Code != tc.code {
			t.Errorf("name %q = %d, want %d", tc.name, rec.Code, tc.code)
		}
	}
}

// A symlink named like a report is refused wherever a report is read: the
// latest the pane shows, and an earlier one the route is asked for. The target
// here is a real file, so following the link would have read text that is not a
// report — the case the reader's Lstat is there for.
func TestReportsRefuseASymlinkNamedLikeAReport(t *testing.T) {
	t.Parallel()

	at := reportsNow.Add(-time.Minute)
	dir := reportsFixture(t, map[string]string{"elsewhere.md": "not a report"})
	if err := os.Symlink(filepath.Join(dir, "elsewhere.md"), filepath.Join(dir, reportName(at))); err != nil {
		t.Fatal(err)
	}

	r := NewReportsReader(dir).Read(reportsNow)
	if r.State != ReportsError || r.Note != reportsBadReport {
		t.Errorf("a symlinked latest: state %q note %q, want %q %q", r.State, r.Note, ReportsError, reportsBadReport)
	}

	if rec := reportGet(t, reportServer(t, dir), "name="+reportStem(at)); rec.Code == http.StatusOK {
		t.Errorf("the route read a symlink named like a report: %d %s", rec.Code, rec.Body.String())
	}
}

// The route is a read like every other on this page: the guard refuses a write
// before the handler ever sees it.
func TestReportRouteRefusesWritesThroughTheGuard(t *testing.T) {
	t.Parallel()

	dir := reportsFixture(t, map[string]string{reportName(reportsNow.Add(-time.Minute)): "the report"})
	req := httptest.NewRequest("POST", "/api/report?name="+reportStem(reportsNow.Add(-time.Minute)), nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	reportServer(t, dir).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/report = %d, want the guard to refuse it", rec.Code)
	}
}

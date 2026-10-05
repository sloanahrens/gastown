package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// fakeFailingBeads is the bead store the watch files through, holding the
// repair beads a test gives it and recording what the watch writes. List
// honors the requested status filter the way bd does, so a test proves the
// watch asks for the statuses it needs and not merely that it reads a bead.
type fakeFailingBeads struct {
	stored   []*beads.Issue
	listErr  error
	created  []beads.CreateOptions
	comments []string
}

func (f *fakeFailingBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*beads.Issue
	for _, is := range f.stored {
		if statusMatches(opts.Status, is.Status) {
			out = append(out, is)
		}
	}
	return out, nil
}

// statusMatches is bd's --status filter: "all" (and the empty filter, which
// the query path also reads as all) keeps everything, else the comma-joined
// list must name the status.
func statusMatches(filter, status string) bool {
	if filter == "" || filter == "all" {
		return true
	}
	for _, want := range strings.Split(filter, ",") {
		if strings.TrimSpace(want) == status {
			return true
		}
	}
	return false
}

func (f *fakeFailingBeads) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	f.created = append(f.created, opts)
	is := &beads.Issue{ID: fmt.Sprintf("flaky-%d", len(f.created)), Title: opts.Title}
	return is, nil
}

func (f *fakeFailingBeads) AddComment(id, text string) error {
	f.comments = append(f.comments, id+": "+text)
	return nil
}

// alert is one escalation the watch raised, by fingerprint.
type alert struct {
	key, message string
}

// watch is a ciTestWatch over a throwaway ledger with the clock pinned, so a
// test's repeat is a fact and not a race.
type watch struct {
	*ciTestWatch
	beads  *fakeFailingBeads
	alerts []alert
	now    time.Time
}

func newWatch(t *testing.T) *watch {
	t.Helper()
	w := &watch{
		beads: &fakeFailingBeads{},
		now:   time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
	}
	w.ciTestWatch = &ciTestWatch{
		Rig:      "gastown",
		Ledger:   filepath.Join(t.TempDir(), "ci-failures.jsonl"),
		Beads:    w.beads,
		Escalate: w.escalate,
		Now:      func() time.Time { return w.now },
	}
	return w
}

func (w *watch) escalate(key, message string) {
	w.alerts = append(w.alerts, alert{key: key, message: message})
}

// fail records one red candidate for bead naming the given tests.
func (w *watch) fail(bead string, tests ...land.TestFailure) {
	w.ciTestWatch.Record(land.CIFailure{Bead: bead, Tests: tests})
}

func test(pkg, name string) land.TestFailure {
	return land.TestFailure{Package: pkg, Test: name}
}

func TestCITestWatchEscalatesATestFailingOnTwoBeads(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	if len(w.alerts) != 0 || len(w.beads.created) != 0 {
		t.Fatalf("first failure escalated (%v) or filed (%v), want neither", w.alerts, w.beads.created)
	}
	w.fail("gt-two", test("github.com/x/a", "TestAlpha"))
	if len(w.alerts) != 1 {
		t.Fatalf("alerts = %v, want the one escalation for the second bead", w.alerts)
	}
	wantKey := "failing-test:gastown:github.com/x/a:TestAlpha"
	if w.alerts[0].key != wantKey {
		t.Errorf("alert key = %q, want %q", w.alerts[0].key, wantKey)
	}
	for _, beadID := range []string{"gt-one", "gt-two"} {
		if !strings.Contains(w.alerts[0].message, beadID) {
			t.Errorf("alert message %q does not name %s", w.alerts[0].message, beadID)
		}
	}
	if len(w.beads.created) != 1 {
		t.Fatalf("created = %v, want one repair bead", w.beads.created)
	}
	if got, want := w.beads.created[0].Title, "failing test (gastown): github.com/x/a TestAlpha"; got != want {
		t.Errorf("bead title = %q, want %q", got, want)
	}
	if w.beads.created[0].Priority != 1 {
		t.Errorf("bead priority = %d, want 1", w.beads.created[0].Priority)
	}
	if len(w.beads.created[0].Labels) != 1 || w.beads.created[0].Labels[0] != LabelFlakyTest {
		t.Errorf("bead labels = %v, want [%s]", w.beads.created[0].Labels, LabelFlakyTest)
	}
}

func TestCITestWatchIgnoresOneBeadFailingTwice(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	if len(w.alerts) != 0 {
		t.Errorf("alerts = %v, want none: one bead failing twice is not the rule", w.alerts)
	}
	if len(w.beads.created) != 0 {
		t.Errorf("created = %v, want no repair bead", w.beads.created)
	}
}

func TestCITestWatchFilesOneBeadPerTest(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	w.fail("gt-two", test("github.com/x/a", "TestAlpha"))
	w.beads.stored = append(w.beads.stored, &beads.Issue{
		ID: "gt-flaky", Title: "failing test (gastown): github.com/x/a TestAlpha",
		Status: string(beads.StatusOpen),
	})
	w.now = w.now.Add(time.Hour)
	w.fail("gt-three", test("github.com/x/a", "TestAlpha"))
	if len(w.beads.created) != 1 {
		t.Errorf("created = %v, want the one bead filed for the test", w.beads.created)
	}
	if len(w.beads.comments) != 1 {
		t.Fatalf("comments = %v, want the repeat recorded on the open bead", w.beads.comments)
	}
	if !strings.HasPrefix(w.beads.comments[0], "gt-flaky: ") || !strings.Contains(w.beads.comments[0], "gt-three") {
		t.Errorf("comment = %q, want gt-flaky and the new bead", w.beads.comments[0])
	}
	if len(w.alerts) != 2 {
		t.Errorf("alerts = %d, want the key re-raised on the repeat so it upserts", len(w.alerts))
	}
}

func TestCITestWatchFindsTheRepairBeadInAnyNonClosedStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []beads.IssueStatus{
		beads.StatusOpen,
		beads.StatusInProgress,
		beads.StatusBlocked,
		beads.IssueStatusHooked,
		beads.StatusDeferred,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			w := newWatch(t)
			w.beads.stored = append(w.beads.stored, &beads.Issue{
				ID:     "gt-repair",
				Title:  failingTestTitle("gastown", "github.com/x/a", "TestAlpha"),
				Status: string(status),
			})
			w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
			w.fail("gt-two", test("github.com/x/a", "TestAlpha"))
			if len(w.beads.created) != 0 {
				t.Errorf("created = %v, want no second bead beside the %s one", w.beads.created, status)
			}
			if len(w.beads.comments) != 1 {
				t.Fatalf("comments = %v, want the repeat recorded on the %s bead", w.beads.comments, status)
			}
			if !strings.HasPrefix(w.beads.comments[0], "gt-repair: ") {
				t.Errorf("comment = %q, want it on the %s bead gt-repair", w.beads.comments[0], status)
			}
		})
	}
}

func TestCITestWatchFilesBesideAClosedRepairBead(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.beads.stored = append(w.beads.stored, &beads.Issue{
		ID:     "gt-old",
		Title:  failingTestTitle("gastown", "github.com/x/a", "TestAlpha"),
		Status: string(beads.StatusClosed),
	})
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	w.fail("gt-two", test("github.com/x/a", "TestAlpha"))
	if len(w.beads.created) != 1 {
		t.Fatalf("created = %v, want a fresh bead: a closed one does not stand in for the repair", w.beads.created)
	}
	if len(w.beads.comments) != 0 {
		t.Errorf("comments = %v, want none on the closed bead", w.beads.comments)
	}
}

func TestCITestWatchCountsNothingUnparsed(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.ciTestWatch.Record(land.CIFailure{Bead: "gt-one"})
	w.ciTestWatch.Record(land.CIFailure{Bead: "gt-two"})
	if len(w.alerts) != 0 || len(w.beads.created) != 0 {
		t.Fatalf("unparsed tails escalated (%v) or filed (%v), want neither", w.alerts, w.beads.created)
	}
	recs := readLedger(t, w.ciTestWatch.Ledger)
	if len(recs) != 2 {
		t.Fatalf("ledger = %v, want the two unparsed failures recorded", recs)
	}
	for _, r := range recs {
		if !r.Unparsed || r.Package != "" || r.Test != "" {
			t.Errorf("record = %+v, want it marked unparsed with no test", r)
		}
	}
}

func TestCITestWatchIgnoresAFailurePastTheWindow(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	w.now = w.now.Add(ciFailureWindow + time.Minute)
	w.fail("gt-two", test("github.com/x/a", "TestAlpha"))
	if len(w.alerts) != 0 || len(w.beads.created) != 0 {
		t.Fatalf("a failure past %s counted: alerts=%v created=%v", ciFailureWindow, w.alerts, w.beads.created)
	}
}

func TestCITestWatchSeparatesTestsAndRigs(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"), test("github.com/x/b", "TestBeta"))
	if len(w.alerts) != 0 {
		t.Fatalf("one bead escalated: %v", w.alerts)
	}
	w.fail("gt-two", test("github.com/x/b", "TestBeta"))
	if len(w.alerts) != 1 {
		t.Fatalf("alerts = %v, want only TestBeta's", w.alerts)
	}
	if !strings.Contains(w.alerts[0].key, "github.com/x/b:TestBeta") {
		t.Errorf("alert key = %q, want it keyed on the failing test", w.alerts[0].key)
	}
	w.fail("gt-three", test("github.com/x/a", "TestAlpha"))
	if len(w.alerts) != 2 {
		t.Errorf("alerts = %v, want TestAlpha's now too", w.alerts)
	}
}

func TestCITestWatchCountsARepeatWithinOneRig(t *testing.T) {
	t.Parallel()
	one := newWatch(t)
	other := &ciTestWatch{
		Rig:      "otherrig",
		Ledger:   one.Ledger,
		Beads:    one.beads,
		Escalate: one.escalate,
		Now:      func() time.Time { return one.now },
	}
	one.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	other.Record(land.CIFailure{Bead: "gt-two", Tests: []land.TestFailure{test("github.com/x/a", "TestAlpha")}})
	if len(one.alerts) != 0 {
		t.Errorf("alerts = %v, want none: another rig's package is a different test", one.alerts)
	}
	one.fail("gt-three", test("github.com/x/a", "TestAlpha"))
	if len(one.alerts) != 1 {
		t.Errorf("alerts = %v, want the repeat within the rig counted", one.alerts)
	}
}

func TestCITestWatchRecordsTheParsedFailure(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	recs := readLedger(t, w.ciTestWatch.Ledger)
	if len(recs) != 1 {
		t.Fatalf("ledger = %v, want the one failure", recs)
	}
	r := recs[0]
	if r.Rig != "gastown" || r.Bead != "gt-one" || r.Package != "github.com/x/a" || r.Test != "TestAlpha" {
		t.Errorf("record = %+v, want rig, bead, package and test", r)
	}
	if !r.Time.Equal(w.now) {
		t.Errorf("record time = %s, want %s", r.Time, w.now)
	}
}

func TestCITestWatchPrunesTheLedger(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.fail("gt-old", test("github.com/x/a", "TestAlpha"))
	w.now = w.now.Add(ciFailureRetention + time.Hour)
	w.fail("gt-new", test("github.com/x/b", "TestBeta"))
	recs := readLedger(t, w.ciTestWatch.Ledger)
	if len(recs) != 1 || recs[0].Bead != "gt-new" {
		t.Errorf("ledger = %v, want only the record inside the retention", recs)
	}
}

func TestCITestWatchSurvivesAFailedBeadRead(t *testing.T) {
	t.Parallel()
	w := newWatch(t)
	w.beads.listErr = errors.New("dolt down")
	w.fail("gt-one", test("github.com/x/a", "TestAlpha"))
	w.fail("gt-two", test("github.com/x/a", "TestAlpha"))
	if len(w.beads.created) != 1 {
		t.Errorf("created = %v, want a repair bead filed despite the failed list", w.beads.created)
	}
}

func readLedger(t *testing.T, path string) []ciFailure {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading ledger: %v", err)
	}
	var out []ciFailure
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r ciFailure
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("ledger line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

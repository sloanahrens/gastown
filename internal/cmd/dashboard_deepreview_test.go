package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
)

// The panel's clock: every fixture below is written against this instant.
var deepReviewNow = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

const deepReviewInterval = "24h"

// fakeDeepReview answers a dashDeepReviewReader from fixtures. slice, risk and
// issues hold the beads the reader lists under each schedule's own label:
// issues is the legacy schedule's runs, which most of these cases use.
type fakeDeepReview struct {
	slice       []*beads.Issue
	risk        []*beads.Issue
	issues      []*beads.Issue
	listErr     error
	comments    map[string][]beads.Comment
	commentsErr error
	patrols     *config.PatrolsConfig
	patrolsErr  error
	townRoot    string
	reads       []string
	// labels are the labels the reader listed, in order.
	labels []string
}

// scheduledPatrols is a patrols config carrying the review's schedules, each at
// one interval.
func scheduledPatrols(enabled bool, interval string) *config.PatrolsConfig {
	entries := make([]config.ScheduledSlingEntry, 0, len(deepReviewPaths))
	for _, path := range deepReviewPaths {
		entries = append(entries, config.ScheduledSlingEntry{Name: path.name, Rig: deepReviewRig, IntervalStr: interval})
	}
	return &config.PatrolsConfig{ScheduledSlings: &config.ScheduledSlingsConfig{Enabled: enabled, Entries: entries}}
}

func newFakeDeepReview(t *testing.T) *fakeDeepReview {
	t.Helper()
	return &fakeDeepReview{townRoot: t.TempDir(), patrols: scheduledPatrols(true, deepReviewInterval)}
}

func (f *fakeDeepReview) reader() *dashDeepReviewReader {
	return &dashDeepReviewReader{
		townRoot: f.townRoot,
		list: func(label string) ([]*beads.Issue, error) {
			f.labels = append(f.labels, label)
			if f.listErr != nil {
				return nil, f.listErr
			}
			// The label is the wire name the patrol files runs under, so the
			// fake serves it by name rather than by the paths variable: a
			// reader that lists any other label gets an error, not an empty
			// list that would hide the mistake.
			switch label {
			case "scheduled:" + deepReviewPaths[0].name:
				return f.slice, nil
			case "scheduled:" + deepReviewPaths[1].name:
				return f.risk, nil
			case "scheduled:" + deepReviewPaths[2].name:
				return f.issues, nil
			}
			return nil, fmt.Errorf("the reader listed an unknown label %q", label)
		},
		comments: func(id string) ([]beads.Comment, error) {
			f.reads = append(f.reads, id)
			if f.commentsErr != nil {
				return nil, f.commentsErr
			}
			return f.comments[id], nil
		},
		patrols: func() (*config.PatrolsConfig, error) { return f.patrols, f.patrolsErr },
	}
}

// runBead is a closed deep-review run created hoursAgo hours before the
// fixture clock that took tookMin minutes.
func runBead(id string, hoursAgo, tookMin int, notes string) *beads.Issue {
	created := deepReviewNow.Add(-time.Duration(hoursAgo) * time.Hour)
	return &beads.Issue{
		ID:        id,
		Status:    "closed",
		CreatedAt: created.Format(time.RFC3339),
		ClosedAt:  created.Add(time.Duration(tookMin) * time.Minute).Format(time.RFC3339),
		Notes:     notes,
	}
}

func openRunBead(id string, hoursAgo int) *beads.Issue {
	b := runBead(id, hoursAgo, 0, "")
	b.Status, b.ClosedAt = "open", ""
	return b
}

const goodNotes = "finding: a.go:F:x gt-bead1\n" +
	"finding: b.go:G:y digest-only\n" +
	"finding: c.go:H:z gt-bead2\n" +
	"queued: d.go:I:w P2 d.go:10 a claim\n" +
	"cursor: abc def"

func runIDs(rows []dashboard.DeepReviewRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// nextTexts is the panel's next-run lines keyed by the schedule's name, which
// makes the one nameless line a town with no schedule carries an entry of its
// own under the empty name.
func nextTexts(next []dashboard.DeepReviewNext) map[string]string {
	out := make(map[string]string, len(next))
	for _, n := range next {
		out[n.Name] = n.Text
	}
	return out
}

const (
	sliceSchedule = "deep-review-slice"
	riskSchedule  = "deep-review-risk"
)

// The panel's query names every status and no priority filter: ListOptions'
// zero value is Priority 0, which bd reads as "P0 only", so a run at any other
// priority would vanish from a pane that asked that way. The label is the
// schedule's, so the reader lists once per schedule: a run bead carries one
// label, and asking for three at once would find none of them.
func TestDeepReviewListAsksForEveryRunOfEverySchedule(t *testing.T) {
	t.Parallel()

	o := deepReviewListOptions
	if o.Label != "" || o.Status != "all" || o.Priority != -1 || o.Limit != 0 {
		t.Errorf("the panel's query is %+v", o)
	}
	f := newFakeDeepReview(t)
	f.reader().read(deepReviewNow)
	want := []string{"scheduled:" + sliceSchedule, "scheduled:" + riskSchedule, "scheduled:" + deepReviewSchedule}
	if !reflect.DeepEqual(f.labels, want) {
		t.Errorf("the panel listed %v, want %v", f.labels, want)
	}
}

// The three schedules' runs are one list on the pane, newest first, and each
// row says which schedule filed it.
func TestDeepReviewRowsAreTheNewestTenWithTheirKind(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	// Twelve closed runs, four per schedule, listed oldest first so the sort
	// has work to do. i%3 picks the schedule: every third run is a slice, the
	// next a risk, the rest legacy.
	for i := 12; i >= 1; i-- {
		run := runBead(fmt.Sprintf("gt-r%02d", i), i*24, 30, goodNotes)
		switch i % 3 {
		case 0:
			f.slice = append(f.slice, run)
		case 1:
			f.risk = append(f.risk, run)
		default:
			f.issues = append(f.issues, run)
		}
	}
	f.comments = map[string][]beads.Comment{
		"gt-r01": {{Text: "the digest, never drawn"}},
		"gt-r02": {{Text: "a"}, {Text: "b"}},
	}

	got := f.reader().read(deepReviewNow)

	want := []string{"gt-r01", "gt-r02", "gt-r03", "gt-r04", "gt-r05", "gt-r06", "gt-r07", "gt-r08", "gt-r09", "gt-r10"}
	if ids := runIDs(got.Rows); !reflect.DeepEqual(ids, want) {
		t.Fatalf("rows = %v, want %v", ids, want)
	}
	for _, tc := range []struct{ id, kind string }{
		{"gt-r01", dashboard.DeepReviewKindRisk},
		{"gt-r02", dashboard.DeepReviewKindLegacy},
		{"gt-r03", dashboard.DeepReviewKindSlice},
	} {
		if row := rowFor(t, got.Rows, tc.id); row.Kind != tc.kind {
			t.Errorf("%s drew kind %q, want %q", tc.id, row.Kind, tc.kind)
		}
	}
	r := got.Rows[0]
	if r.Status != dashboard.DeepReviewClosed || r.DurationSec == nil || *r.DurationSec != 30*60 {
		t.Errorf("status/duration = %q/%v, want closed/1800", r.Status, r.DurationSec)
	}
	if r.Findings != 3 || r.Filed != 2 || r.Queued != 1 || r.Unparsed {
		t.Errorf("counts = findings %d filed %d queued %d unparsed %v, want 3/2/1/false", r.Findings, r.Filed, r.Queued, r.Unparsed)
	}
	if r.Digest != "gt-r01 · 1 comment" || got.Rows[1].Digest != "gt-r02 · 2 comments" || got.Rows[2].Digest != "" {
		t.Errorf("digests = %q, %q, %q", r.Digest, got.Rows[1].Digest, got.Rows[2].Digest)
	}
	// The digest's text is read for its count and goes no further.
	if raw := fmt.Sprintf("%+v", got); strings.Contains(raw, "never drawn") {
		t.Errorf("the payload carries a comment's text: %s", raw)
	}
	if len(f.reads) != 10 {
		t.Errorf("comments read for %d beads, want the 10 drawn", len(f.reads))
	}
	if got.Unavailable || got.Paused != "" {
		t.Errorf("unavailable %v paused %q", got.Unavailable, got.Paused)
	}
}

func rowFor(t *testing.T, rows []dashboard.DeepReviewRow, id string) dashboard.DeepReviewRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row for %s in %v", id, runIDs(rows))
	return dashboard.DeepReviewRow{}
}

// Every configured schedule has a run of its own to draw from: a schedule with
// none is due now, and one schedule's runs say nothing about another's.
func TestDeepReviewEmpty(t *testing.T) {
	t.Parallel()

	got := newFakeDeepReview(t).reader().read(deepReviewNow)
	if got.Unavailable || got.Rows == nil || len(got.Rows) != 0 {
		t.Errorf("an empty list read as %+v, want an empty, available panel", got)
	}
	for name, text := range nextTexts(got.Next) {
		if text != deepReviewDueNow {
			t.Errorf("a schedule with no run yet is %q for %s, want due now", text, name)
		}
	}
	if len(got.Next) != len(deepReviewPaths) {
		t.Errorf("next = %+v, want one line per configured schedule", got.Next)
	}
}

func TestDeepReviewOneOpenRun(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.issues = []*beads.Issue{openRunBead("gt-live", 1), runBead("gt-old", 48, 20, "")}
	got := f.reader().read(deepReviewNow)

	// The open run is the legacy schedule's, so only its line reads running:
	// the other schedules never filed a run.
	texts := nextTexts(got.Next)
	if texts[deepReviewSchedule] != deepReviewRunning {
		t.Errorf("next for the legacy schedule = %q, want running", texts[deepReviewSchedule])
	}
	if texts[sliceSchedule] != deepReviewDueNow || texts[riskSchedule] != deepReviewDueNow {
		t.Errorf("next = %+v, want the schedules with no run due now", got.Next)
	}
	r := got.Rows[0]
	if r.ID != "gt-live" || r.Status != dashboard.DeepReviewOpen || r.DurationSec != nil {
		t.Errorf("open row = %+v, want open with no duration", r)
	}
}

// A run's bead the patrol closed because the sling failed is not a run: it
// neither counts as the last one nor draws as one, and it does not hold its
// schedule's next line back.
func TestDeepReviewFailedSlingBeadIsIgnored(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	failed := runBead("gt-failed", 1, 0, "")
	failed.CloseReason = "scheduled_slings-sling-failed: gt sling failed; the patrol retries on the next tick"
	f.issues = []*beads.Issue{failed, runBead("gt-real", 10, 15, goodNotes)}

	got := f.reader().read(deepReviewNow)
	if ids := runIDs(got.Rows); !reflect.DeepEqual(ids, []string{"gt-real"}) {
		t.Errorf("rows = %v, want only the real run", ids)
	}
	// Ten hours into a 24h interval: due in 14h, not 23h from the failed bead.
	if text := nextTexts(got.Next)[deepReviewSchedule]; text != "in 14h 0m" {
		t.Errorf("next = %q, want in 14h 0m", text)
	}

	// A failed-sling bead that is still open is not "running" either: the
	// patrol closes it, but the decision ignores only closed ones, so an open
	// one blocks — the same answer decideScheduledSling gives.
	open := runBead("gt-open", 1, 0, "")
	open.Status = "open"
	open.CloseReason = failed.CloseReason
	if next := deepReviewNextDue([]*beads.Issue{open}, 24*time.Hour, deepReviewNow); next != "running" {
		t.Errorf("an open bead with a failure reason is %q, want running", next)
	}
}

// deepReviewNextDue is decideScheduledSling's decision spelled as the panel's
// line: any open bead is running, the newest creation plus the interval is the
// due time, and a time that has passed is due now.
func TestDeepReviewNextDueMirrorsTheDaemonsDecision(t *testing.T) {
	t.Parallel()

	week := 7 * 24 * time.Hour
	for _, tc := range []struct {
		name     string
		issues   []*beads.Issue
		interval time.Duration
		want     string
	}{
		{"none", nil, week, "due now"},
		{"open wins over recency", []*beads.Issue{runBead("a", 1, 5, ""), openRunBead("b", 100)}, week, "running"},
		{"recent", []*beads.Issue{runBead("a", 24, 5, "")}, week, "in 6d 0h 0m"},
		{"newest counts", []*beads.Issue{runBead("a", 200, 5, ""), runBead("b", 24, 5, "")}, week, "in 6d 0h 0m"},
		{"exactly due", []*beads.Issue{runBead("a", 168, 5, "")}, week, "due now"},
		{"past", []*beads.Issue{runBead("a", 300, 5, "")}, week, "due now"},
		{"nil entries are skipped", []*beads.Issue{nil, runBead("a", 300, 5, "")}, week, "due now"},
	} {
		if got := deepReviewNextDue(tc.issues, tc.interval, deepReviewNow); got != tc.want {
			t.Errorf("%s: next = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A schedule whose gate decides whether a run happens has no countdown to draw:
// the interval no longer says when the next run is, so its line says the gate
// does. That is the risk path's whole point — it runs when there is something
// to review, not every interval (gt-zzdge).
func TestDeepReviewGatedScheduleDrawsItsGate(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	scheduledSling(f.patrols, riskSchedule).GateCommand = "gt deep-review gate --mode risk"
	f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, "")}

	texts := nextTexts(f.reader().read(deepReviewNow).Next)
	if texts[riskSchedule] != deepReviewGated {
		t.Errorf("the gated schedule drew %q, want %q", texts[riskSchedule], deepReviewGated)
	}
	// The schedules without a gate keep their countdowns: five hours into the
	// legacy schedule's 24h interval is due in 19h, and the slice schedule has
	// no run at all.
	if texts[deepReviewSchedule] != "in 19h 0m" {
		t.Errorf("the legacy schedule drew %q, want in 19h 0m", texts[deepReviewSchedule])
	}
	if texts[sliceSchedule] != deepReviewDueNow {
		t.Errorf("the ungated slice schedule drew %q, want due now", texts[sliceSchedule])
	}

	// An open run is running whatever the gate says: the gate let that run
	// through, and it is the one the pane is watching.
	f.risk = []*beads.Issue{openRunBead("gt-live", 1)}
	if texts := nextTexts(f.reader().read(deepReviewNow).Next); texts[riskSchedule] != deepReviewRunning {
		t.Errorf("a gated schedule with an open run drew %q, want running", texts[riskSchedule])
	}

	// A blank gate is no gate: the whitespace is a config that says nothing.
	scheduledSling(f.patrols, riskSchedule).GateCommand = "  "
	f.risk = nil
	if texts := nextTexts(f.reader().read(deepReviewNow).Next); texts[riskSchedule] != deepReviewDueNow {
		t.Errorf("a blank gate drew %q, want the schedule's own due time", texts[riskSchedule])
	}
}

// A town with no schedule configured, or a patrols config that cannot be read,
// has the one nameless line the pane drew before the split; the runs still show.
func TestDeepReviewNoScheduleConfigured(t *testing.T) {
	t.Parallel()

	for name, set := range map[string]func(*fakeDeepReview){
		"disabled":   func(f *fakeDeepReview) { f.patrols = scheduledPatrols(false, deepReviewInterval) },
		"no patrols": func(f *fakeDeepReview) { f.patrols = nil },
		"no scheduled_slings": func(f *fakeDeepReview) {
			f.patrols = &config.PatrolsConfig{}
		},
		"another entry": func(f *fakeDeepReview) {
			f.patrols = scheduledPatrols(true, deepReviewInterval)
			f.patrols.ScheduledSlings.Entries = []config.ScheduledSlingEntry{{Name: "doc-audit", Rig: deepReviewRig, IntervalStr: deepReviewInterval}}
		},
		"config unreadable": func(f *fakeDeepReview) { f.patrolsErr = errors.New("bad json") },
	} {
		f := newFakeDeepReview(t)
		set(f)
		f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, goodNotes)}
		got := f.reader().read(deepReviewNow)
		if want := []dashboard.DeepReviewNext{{Text: deepReviewNotScheduled}}; !reflect.DeepEqual(got.Next, want) {
			t.Errorf("%s: next = %+v, want the one nameless line", name, got.Next)
		}
		if got.Unavailable || len(got.Rows) != 1 {
			t.Errorf("%s: the rows are %+v, want the run still shown", name, got)
		}
	}
}

// A schedule the config carries but whose interval is unusable is not scheduled,
// and the schedules beside it still draw their own lines.
func TestDeepReviewScheduleWithNoUsableInterval(t *testing.T) {
	t.Parallel()

	for _, interval := range []string{"soon", "0s", "-1h", ""} {
		f := newFakeDeepReview(t)
		f.patrols = scheduledPatrols(true, interval)
		f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, goodNotes)}
		got := f.reader().read(deepReviewNow)
		if len(got.Next) != len(deepReviewPaths) {
			t.Fatalf("interval %q: next = %+v, want one line per configured schedule", interval, got.Next)
		}
		for _, n := range got.Next {
			if n.Text != deepReviewNotScheduled {
				t.Errorf("interval %q: %s drew %q, want not scheduled", interval, n.Name, n.Text)
			}
		}
		if got.Unavailable || len(got.Rows) != 1 {
			t.Errorf("interval %q: the rows are %+v, want the run still shown", interval, got)
		}
	}
}

// A schedule the config no longer carries has no line, and the ones it does
// carry keep theirs: the panel reads what the town is configured to run.
func TestDeepReviewScheduleMissingFromTheConfig(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.patrols.ScheduledSlings.Entries = []config.ScheduledSlingEntry{
		{Name: sliceSchedule, Rig: deepReviewRig, IntervalStr: deepReviewInterval},
		{Name: deepReviewSchedule, Rig: deepReviewRig, IntervalStr: deepReviewInterval},
	}
	got := f.reader().read(deepReviewNow)
	if len(got.Next) != 2 {
		t.Fatalf("next = %+v, want the two configured schedules", got.Next)
	}
	for _, n := range got.Next {
		if n.Name == riskSchedule {
			t.Errorf("next = %+v, want no line for the schedule the config dropped", got.Next)
		}
	}
}

func TestDeepReviewHeld(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		file string
		rig  string
		want string
	}{
		"hold file":  {file: dispatch.HoldFileName, want: "operator dispatch hold present"},
		"town ESTOP": {file: estop.FileName, want: "town ESTOP active"},
		"rig ESTOP":  {file: estop.RigFileName(deepReviewRig), want: "rig ESTOP active"},
	} {
		f := newFakeDeepReview(t)
		f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, "")}
		if err := os.WriteFile(filepath.Join(f.townRoot, tc.file), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := f.reader().read(deepReviewNow)
		if !strings.Contains(got.Paused, tc.want) {
			t.Errorf("%s: paused = %q, want it to say %q", name, got.Paused, tc.want)
		}
		if len(got.Rows) != 1 {
			t.Errorf("%s: a held town hides its rows: %+v", name, got.Rows)
		}
	}

	// Standby sets both the hold file and the ESTOP; one reason is drawn.
	f := newFakeDeepReview(t)
	for _, name := range []string{dispatch.HoldFileName, estop.FileName} {
		if err := os.WriteFile(filepath.Join(f.townRoot, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reader().read(deepReviewNow); got.Paused == "" || strings.Contains(got.Paused, "\n") {
		t.Errorf("standby paused = %q, want one line", got.Paused)
	}
}

// One schedule's list failing leaves the panel with a partial picture of the
// review, which is worse than saying the picture could not be read.
func TestDeepReviewListErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.listErr = errors.New("bd: dolt down")
	f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, "")}
	got := f.reader().read(deepReviewNow)
	if !got.Unavailable || len(got.Rows) != 0 || len(got.Next) != 0 {
		t.Errorf("a failed list read as %+v, want unavailable and nothing else", got)
	}
}

func TestDeepReviewCommentsErrorKeepsTheRow(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, goodNotes)}
	f.commentsErr = errors.New("bd: timeout")
	got := f.reader().read(deepReviewNow)
	if got.Unavailable || len(got.Rows) != 1 || got.Rows[0].Findings != 3 {
		t.Fatalf("read = %+v, want the row kept", got)
	}
	if !strings.Contains(got.Rows[0].Digest, "unreadable") {
		t.Errorf("digest = %q, want it to say the comments were unreadable", got.Rows[0].Digest)
	}
}

// Notes are text an agent wrote: a malformed, binary or oversized one never
// panics and never costs more than the caps.
func TestDeepReviewMalformedNotes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                    string
		notes                   string
		findings, filed, queued int
		unparsed                bool
	}{
		{"empty", "", 0, 0, 0, false},
		{"prose only", "worked on it\nfinding something later", 0, 0, 0, false},
		{"finding without fields", "finding:", 0, 0, 0, true},
		{"finding with one field", "finding: a.go:F:x", 0, 0, 0, true},
		{"queued without fields", "queued: a.go:F:x P2", 0, 0, 0, true},
		{"cursor without fields", "cursor: abc", 0, 0, 0, true},
		{"good lines survive a bad one", "finding: a b\nfinding: x\nqueued: a P1 f:1 c", 1, 1, 1, true},
		{"digest-only is not filed", "finding: a digest-only", 1, 0, 0, false},
		{"indented and spaced", "  finding:   a    gt-x  ", 1, 1, 0, false},
		{"binary", "\x00\x01\xff\xfe finding: a b\n\x00\x00", 0, 0, 0, false},
		{"invalid utf8 after a prefix", "finding: \xff\xfe \xfd", 1, 1, 0, false},
		{"crlf", "finding: a gt-x\r\nqueued: a P1 f:1 c\r\n", 1, 1, 1, false},
	} {
		row := dashboard.DeepReviewRow{}
		deepReviewCountNotes(&row, tc.notes)
		if row.Findings != tc.findings || row.Filed != tc.filed || row.Queued != tc.queued || row.Unparsed != tc.unparsed {
			t.Errorf("%s: findings %d filed %d queued %d unparsed %v, want %d/%d/%d/%v",
				tc.name, row.Findings, row.Filed, row.Queued, row.Unparsed, tc.findings, tc.filed, tc.queued, tc.unparsed)
		}
	}
}

func TestDeepReviewOversizedNotes(t *testing.T) {
	t.Parallel()

	// One finding at the top, then more than the 64 KiB cap of filler: the
	// finding counts, the cut marks the row, and a line past 512 bytes is read
	// only as far as the cap.
	big := "finding: a gt-x\n" + strings.Repeat("x", deepReviewNotesMax) + "\nfinding: late gt-y"
	row := dashboard.DeepReviewRow{}
	deepReviewCountNotes(&row, big)
	if row.Findings != 1 || !row.Unparsed {
		t.Errorf("oversized notes: findings %d unparsed %v, want 1/true", row.Findings, row.Unparsed)
	}

	// A long line whose fields sit inside the cap still parses; one whose
	// fields start past it does not exist to the reader.
	long := "finding: a gt-x " + strings.Repeat("y", 4*deepReviewLineMax)
	row = dashboard.DeepReviewRow{}
	deepReviewCountNotes(&row, long)
	if row.Findings != 1 || row.Filed != 1 || row.Unparsed {
		t.Errorf("long line: %+v, want one filed finding", row)
	}
	late := "finding: a " + strings.Repeat(" ", 2*deepReviewLineMax) + "gt-x"
	row = dashboard.DeepReviewRow{}
	deepReviewCountNotes(&row, late)
	if row.Findings != 0 || !row.Unparsed {
		t.Errorf("a line whose fields lie past the cap: %+v, want unparsed", row)
	}

	// Many short lines stay inside the byte cap.
	many := strings.Repeat("finding: a gt-x\n", deepReviewNotesMax)
	row = dashboard.DeepReviewRow{}
	deepReviewCountNotes(&row, many)
	if row.Findings > deepReviewNotesMax/len("finding: a gt-x\n") || !row.Unparsed {
		t.Errorf("many lines: findings %d unparsed %v", row.Findings, row.Unparsed)
	}
}

func TestDeepReviewRowFromAMalformedBeadDoesNotPanic(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.issues = []*beads.Issue{
		nil,
		{ID: "gt-bare"},
		{ID: "gt-badtimes", Status: "closed", CreatedAt: "yesterday", ClosedAt: "later", Notes: "\x00\xff"},
		{ID: "gt-backwards", Status: "closed", CreatedAt: deepReviewNow.Format(time.RFC3339), ClosedAt: deepReviewNow.Add(-time.Hour).Format(time.RFC3339)},
		{ID: "gt-<b>\x1b[31mhtml</b>", Status: "closed", CreatedAt: deepReviewNow.Add(-time.Hour).Format(time.RFC3339), ClosedAt: deepReviewNow.Format(time.RFC3339)},
	}
	got := f.reader().read(deepReviewNow)
	if len(got.Rows) != 4 {
		t.Fatalf("rows = %v, want the four non-nil beads", runIDs(got.Rows))
	}
	for _, r := range got.Rows {
		if strings.ContainsAny(r.ID, "\x1b\n") {
			t.Errorf("row id %q carries a control character", r.ID)
		}
		if r.DurationSec != nil && r.ID != "gt-<b> [31mhtml</b>" {
			t.Errorf("%s: duration %d from times that do not support one", r.ID, *r.DurationSec)
		}
	}
	// A bead with no creation time sorts after every one with one.
	if last := got.Rows[len(got.Rows)-1].ID; last != "gt-bare" && last != "gt-badtimes" {
		t.Errorf("the last row is %q, want one of the beads with no usable time", last)
	}
}

func TestDeepReviewStringsAreCapped(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.issues = []*beads.Issue{runBead("gt-"+strings.Repeat("é", 5000), 1, 1, "")}
	f.comments = map[string][]beads.Comment{f.issues[0].ID: {{Text: "digest"}}}
	got := f.reader().read(deepReviewNow)
	if n := len([]rune(got.Rows[0].ID)); n != deepReviewFieldMax {
		t.Errorf("id is %d runes, want the cap %d", n, deepReviewFieldMax)
	}
	if !strings.HasSuffix(got.Rows[0].Digest, " · 1 comment") || len([]rune(got.Rows[0].Digest)) > deepReviewFieldMax+20 {
		t.Errorf("digest is %d runes, want the capped id and the count", len([]rune(got.Rows[0].Digest)))
	}
}

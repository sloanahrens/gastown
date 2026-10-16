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

// The block's clock: every fixture below is written against this instant.
var deepReviewNow = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

const deepReviewInterval = "24h"

const (
	sliceSchedule = "deep-review-slice"
	riskSchedule  = "deep-review-risk"
	// auditSchedule is a schedule configured beside the review's pair, which is
	// what the block's old fixed list of names dropped (gt-uv08f).
	auditSchedule = "agentlab"
)

// defaultSchedules is the town's config in most of these cases: the review's
// two paths and the single-schedule label its older runs carry.
var defaultSchedules = []string{sliceSchedule, riskSchedule, deepReviewSchedule}

// fakeDeepReview answers a dashDeepReviewReader from fixtures. runs holds the
// beads each schedule's own label lists, which is the wire name the patrol
// files a run under: a reader that lists another label gets an error rather
// than an empty list that would hide the mistake.
type fakeDeepReview struct {
	runs        map[string][]*beads.Issue
	listErr     error
	comments    map[string][]beads.Comment
	commentsErr error
	patrols     *config.PatrolsConfig
	patrolsErr  error
	townRoot    string
	// reads are the bead ids the reader asked for comments about, in order.
	reads []string
	// labels are the labels the reader listed, in order.
	labels []string
}

// scheduledPatrols is a patrols config carrying the schedules named, each at
// one interval.
func scheduledPatrols(enabled bool, interval string, names ...string) *config.PatrolsConfig {
	entries := make([]config.ScheduledSlingEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, config.ScheduledSlingEntry{Name: name, Rig: deepReviewRig, IntervalStr: interval})
	}
	return &config.PatrolsConfig{ScheduledSlings: &config.ScheduledSlingsConfig{Enabled: enabled, Entries: entries}}
}

func newFakeDeepReview(t *testing.T) *fakeDeepReview {
	t.Helper()
	f := &fakeDeepReview{
		townRoot: t.TempDir(),
		runs:     map[string][]*beads.Issue{},
		patrols:  scheduledPatrols(true, deepReviewInterval, defaultSchedules...),
	}
	for _, name := range defaultSchedules {
		f.runs[deepReviewLabel(name)] = nil
	}
	return f
}

// set appends runs to one schedule's label, registering the label so a reader
// that lists it does not read as a mistake.
func (f *fakeDeepReview) set(name string, issues ...*beads.Issue) {
	label := deepReviewLabel(name)
	f.runs[label] = append(f.runs[label], issues...)
}

func (f *fakeDeepReview) reader() *dashDeepReviewReader {
	return &dashDeepReviewReader{
		townRoot: f.townRoot,
		list: func(label string) ([]*beads.Issue, error) {
			f.labels = append(f.labels, label)
			if f.listErr != nil {
				return nil, f.listErr
			}
			issues, ok := f.runs[label]
			if !ok {
				return nil, fmt.Errorf("the reader listed an unknown label %q", label)
			}
			return issues, nil
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

// nextTexts is the block's next-run cells keyed by the schedule's name.
func nextTexts(schedules []dashboard.DeepReviewSchedule) map[string]string {
	out := make(map[string]string, len(schedules))
	for _, s := range schedules {
		out[s.Name] = s.Next
	}
	return out
}

// scheduleFor is one row of the block by the name it carries.
func scheduleFor(t *testing.T, schedules []dashboard.DeepReviewSchedule, name string) dashboard.DeepReviewSchedule {
	t.Helper()
	for _, s := range schedules {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no row for %s in %+v", name, schedules)
	return dashboard.DeepReviewSchedule{}
}

// The block reads the town's own schedule list: one list per entry of
// daemon.patrols.scheduled_slings, so a schedule configured beside the review's
// pair is a row the moment it exists, and the list the config drops is a list
// the reader stops making (gt-uv08f).
func TestDeepReviewListsEveryConfiguredSchedule(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.patrols = scheduledPatrols(true, deepReviewInterval, sliceSchedule, auditSchedule)
	f.set(auditSchedule, runBead("gt-a1", 5, 10, goodNotes))
	got := f.reader().read(deepReviewNow)

	want := []string{"scheduled:" + sliceSchedule, "scheduled:" + auditSchedule}
	if !reflect.DeepEqual(f.labels, want) {
		t.Errorf("the block listed %v, want one list per configured schedule %v", f.labels, want)
	}
	if names := scheduleNames(got.Schedules); !reflect.DeepEqual(names, []string{sliceSchedule, auditSchedule}) {
		t.Errorf("the block drew %v, want the configured schedules in the config's order", names)
	}
	audit := scheduleFor(t, got.Schedules, auditSchedule)
	if audit.Rig != deepReviewRig {
		t.Errorf("the %s row carries rig %q, want the entry's own", auditSchedule, audit.Rig)
	}
	if audit.Last == nil || audit.Last.ID != "gt-a1" || audit.Last.Findings != 3 || audit.Last.Filed != 2 {
		t.Errorf("the %s row's last run is %+v, want gt-a1 with 3 findings and 2 filed", auditSchedule, audit.Last)
	}
	if last := scheduleFor(t, got.Schedules, sliceSchedule).Last; last != nil {
		t.Errorf("a schedule that filed no run carries %+v, want none", last)
	}
}

func scheduleNames(schedules []dashboard.DeepReviewSchedule) []string {
	out := make([]string, 0, len(schedules))
	for _, s := range schedules {
		out = append(out, s.Name)
	}
	return out
}

// The payload's runs are the review's newest across every schedule, newest
// first, which is the list the Usage panel joins its own records to by bead id.
func TestDeepReviewRunsAreTheNewestTenAcrossEverySchedule(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	// Twelve closed runs, four per schedule, listed oldest first so the sort
	// has work to do. i%3 picks the schedule: every third run is a slice, the
	// next a risk, the rest legacy.
	for i := 12; i >= 1; i-- {
		run := runBead(fmt.Sprintf("gt-r%02d", i), i*24, 30, goodNotes)
		switch i % 3 {
		case 0:
			f.set(sliceSchedule, run)
		case 1:
			f.set(riskSchedule, run)
		default:
			f.set(deepReviewSchedule, run)
		}
	}
	f.comments = map[string][]beads.Comment{
		"gt-r01": {{Text: "the digest, never drawn"}},
		"gt-r02": {{Text: "a"}, {Text: "b"}},
	}

	got := f.reader().read(deepReviewNow)

	want := []string{"gt-r01", "gt-r02", "gt-r03", "gt-r04", "gt-r05", "gt-r06", "gt-r07", "gt-r08", "gt-r09", "gt-r10"}
	if ids := runIDs(got.Runs); !reflect.DeepEqual(ids, want) {
		t.Fatalf("runs = %v, want %v", ids, want)
	}
	r := got.Runs[0]
	if r.Status != dashboard.DeepReviewClosed || r.Findings != 3 || r.Filed != 2 || r.Unparsed {
		t.Errorf("the newest run is %+v, want a closed run of 3 findings, 2 filed, parsed", r)
	}
	if r.Digest != "gt-r01 · 1 comment" || got.Runs[1].Digest != "gt-r02 · 2 comments" || got.Runs[2].Digest != "" {
		t.Errorf("digests = %q, %q, %q", r.Digest, got.Runs[1].Digest, got.Runs[2].Digest)
	}
	// Every schedule's newest run is the one the block's row draws, and it is
	// the same record the runs list carries: gt-r03, the newest of the slice's
	// four, with the counts its notes give it.
	if last := scheduleFor(t, got.Schedules, sliceSchedule).Last; last == nil || last.ID != "gt-r03" || last.Findings != 3 || last.Filed != 2 {
		t.Errorf("the slice row's last run is %+v, want gt-r03 with 3 findings and 2 filed", last)
	}
	// The digest's text is read for its count and goes no further.
	if raw := fmt.Sprintf("%+v", got); strings.Contains(raw, "never drawn") {
		t.Errorf("the payload carries a comment's text: %s", raw)
	}
	// One read per bead the payload draws: the ten runs are the three rows' last
	// runs as well, so the comment count of each is read once.
	if len(f.reads) != 10 {
		t.Errorf("comments read for %d beads, want the 10 drawn", len(f.reads))
	}
	if got.Unavailable || got.Paused != "" {
		t.Errorf("unavailable %v paused %q", got.Unavailable, got.Paused)
	}
}

func runIDs(runs []dashboard.DeepReviewRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// A schedule's own newest run is drawn even when it is older than every run of
// the review's ten newest: the block's rows are the schedules', and the list
// the Usage panel joins is the ten.
func TestDeepReviewDrawsASchedulesRunOutsideTheNewestTen(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.patrols = scheduledPatrols(true, deepReviewInterval, sliceSchedule, auditSchedule)
	for i := 12; i >= 1; i-- {
		f.set(sliceSchedule, runBead(fmt.Sprintf("gt-r%02d", i), i, 30, goodNotes))
	}
	f.set(auditSchedule, runBead("gt-old", 500, 10, goodNotes))
	f.comments = map[string][]beads.Comment{"gt-old": {{Text: "digest"}}}

	got := f.reader().read(deepReviewNow)
	if ids := runIDs(got.Runs); len(ids) != 10 || ids[len(ids)-1] != "gt-r10" {
		t.Errorf("runs = %v, want the slice schedule's ten newest", ids)
	}
	audit := scheduleFor(t, got.Schedules, auditSchedule)
	if audit.Last == nil || audit.Last.ID != "gt-old" {
		t.Fatalf("the %s row's last run is %+v, want its own gt-old", auditSchedule, audit.Last)
	}
	if audit.Last.Digest != "gt-old · 1 comment" {
		t.Errorf("the row's digest is %q, want it read for its own run", audit.Last.Digest)
	}
	// The run is not one of the ten, so its comments are read for the row alone:
	// ten runs and the one row that drew an older bead.
	if len(f.reads) != 11 {
		t.Errorf("comments read for %d beads, want the ten runs and the row's own", len(f.reads))
	}
}

// Every configured schedule has a run of its own to draw from: a schedule with
// none is due now and carries no last run, and one schedule's runs say nothing
// about another's.
func TestDeepReviewEmpty(t *testing.T) {
	t.Parallel()

	got := newFakeDeepReview(t).reader().read(deepReviewNow)
	if got.Unavailable || got.Runs == nil || len(got.Runs) != 0 {
		t.Errorf("an empty list read as %+v, want an empty, available payload", got)
	}
	for name, text := range nextTexts(got.Schedules) {
		if text != deepReviewDueNow {
			t.Errorf("a schedule with no run yet is %q for %s, want due now", text, name)
		}
	}
	if len(got.Schedules) != len(defaultSchedules) {
		t.Errorf("schedules = %+v, want one row per configured schedule", got.Schedules)
	}
	for _, s := range got.Schedules {
		if s.Last != nil {
			t.Errorf("%s carries a last run of %+v, want none", s.Name, s.Last)
		}
	}
}

func TestDeepReviewOneOpenRun(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.set(deepReviewSchedule, openRunBead("gt-live", 1), runBead("gt-old", 48, 20, ""))
	got := f.reader().read(deepReviewNow)

	// The open run is the legacy schedule's, so only its row reads running: the
	// other schedules never filed a run.
	texts := nextTexts(got.Schedules)
	if texts[deepReviewSchedule] != deepReviewRunning {
		t.Errorf("next for the legacy schedule = %q, want running", texts[deepReviewSchedule])
	}
	if texts[sliceSchedule] != deepReviewDueNow || texts[riskSchedule] != deepReviewDueNow {
		t.Errorf("schedules = %+v, want the schedules with no run due now", got.Schedules)
	}
	last := scheduleFor(t, got.Schedules, deepReviewSchedule).Last
	if last == nil || last.ID != "gt-live" || last.Status != dashboard.DeepReviewOpen {
		t.Errorf("the legacy row's last run is %+v, want the open gt-live", last)
	}
	if first := got.Runs[0]; first.ID != "gt-live" || first.Status != dashboard.DeepReviewOpen {
		t.Errorf("runs[0] = %+v, want the open run", first)
	}
}

// A run's bead the patrol closed because the sling failed is not a run: it
// neither draws as a schedule's last one nor counts toward the review's newest,
// and it does not hold its schedule's next cell back.
func TestDeepReviewFailedSlingBeadIsIgnored(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	failed := runBead("gt-failed", 1, 0, "")
	failed.CloseReason = "scheduled_slings-sling-failed: gt sling failed; the patrol retries on the next tick"
	f.set(deepReviewSchedule, failed, runBead("gt-real", 10, 15, goodNotes))

	got := f.reader().read(deepReviewNow)
	if ids := runIDs(got.Runs); !reflect.DeepEqual(ids, []string{"gt-real"}) {
		t.Errorf("runs = %v, want only the real run", ids)
	}
	if last := scheduleFor(t, got.Schedules, deepReviewSchedule).Last; last == nil || last.ID != "gt-real" {
		t.Errorf("the row's last run is %+v, want the real run", last)
	}
	// Ten hours into a 24h interval: due in 14h, not 23h from the failed bead.
	if text := nextTexts(got.Schedules)[deepReviewSchedule]; text != "in 14h 0m" {
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

// deepReviewNextDue is decideScheduledSling's decision spelled as the block's
// cell: any open bead is running, the newest creation plus the interval is the
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
// the interval no longer says when the next run is, so its cell says the gate
// does. That is the risk path's whole point — it runs when there is something
// to review, not every interval (gt-zzdge).
func TestDeepReviewGatedScheduleDrawsItsGate(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	scheduledSling(f.patrols, riskSchedule).GateCommand = "gt deep-review gate --mode risk"
	f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, ""))

	texts := nextTexts(f.reader().read(deepReviewNow).Schedules)
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
	// through, and it is the one the block is watching.
	f.set(riskSchedule, openRunBead("gt-live", 1))
	if texts := nextTexts(f.reader().read(deepReviewNow).Schedules); texts[riskSchedule] != deepReviewRunning {
		t.Errorf("a gated schedule with an open run drew %q, want running", texts[riskSchedule])
	}

	// A blank gate is no gate: the whitespace is a config that says nothing.
	scheduledSling(f.patrols, riskSchedule).GateCommand = "  "
	f.runs[deepReviewLabel(riskSchedule)] = nil
	if texts := nextTexts(f.reader().read(deepReviewNow).Schedules); texts[riskSchedule] != deepReviewDueNow {
		t.Errorf("a blank gate drew %q, want the schedule's own due time", texts[riskSchedule])
	}
}

// A town that configures no schedule draws no row — there is no entry to draw
// one from — but the review's runs are still listed, by the names the review is
// known to run as: those runs are its history, and the Usage panel joins its
// own records to them by bead id, so the list must not empty with the config
// (gt-uv08f).
func TestDeepReviewNoScheduleConfigured(t *testing.T) {
	t.Parallel()

	for name, set := range map[string]func(*fakeDeepReview){
		"no patrols":          func(f *fakeDeepReview) { f.patrols = nil },
		"no scheduled_slings": func(f *fakeDeepReview) { f.patrols = &config.PatrolsConfig{} },
		"no entries":          func(f *fakeDeepReview) { f.patrols = scheduledPatrols(true, deepReviewInterval) },
	} {
		f := newFakeDeepReview(t)
		set(f)
		f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, goodNotes))
		got := f.reader().read(deepReviewNow)
		if len(got.Schedules) != 0 {
			t.Errorf("%s: drew %+v, want no row for a schedule no config carries", name, got.Schedules)
		}
		if got.Unavailable {
			t.Errorf("%s: a town with no schedule read as unavailable", name)
		}
		if len(got.Runs) != 1 || got.Runs[0].ID != "gt-r1" {
			t.Errorf("%s: runs = %+v, want the run the town filed", name, got.Runs)
		}
	}
}

// A patrol that is configured and switched off is not a town without one: the
// schedules the config carries still draw their rows, and each says the patrol
// is off rather than counting down to a run the daemon will not dispatch. The
// runs it filed before it was switched off are still the review's history
// (gt-uv08f).
func TestDeepReviewDisabledPatrolDrawsItsSchedules(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.patrols = scheduledPatrols(false, deepReviewInterval, defaultSchedules...)
	f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, goodNotes))
	got := f.reader().read(deepReviewNow)
	if got.Unavailable {
		t.Fatalf("a disabled patrol read as unavailable: %+v", got)
	}
	if len(got.Schedules) != len(defaultSchedules) {
		t.Fatalf("schedules = %+v, want one row per configured schedule", got.Schedules)
	}
	for _, s := range got.Schedules {
		if s.Next != deepReviewPatrolOff {
			t.Errorf("%s drew %q, want the patrol's own word", s.Name, s.Next)
		}
	}
	if last := scheduleFor(t, got.Schedules, deepReviewSchedule).Last; last == nil || last.ID != "gt-r1" {
		t.Errorf("the disabled patrol's row carries %+v, want its newest run", last)
	}
	if len(got.Runs) != 1 || got.Runs[0].ID != "gt-r1" {
		t.Errorf("runs = %+v, want the run filed before the patrol was switched off", got.Runs)
	}
}

// A patrols config that is there and does not parse is not a town with no
// schedules: one is a daemon.json to fix and the other is a town that never ran
// the review, so the block names the read that failed (gt-uv08f).
func TestDeepReviewUnreadableConfigIsUnavailable(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.patrolsErr = errors.New("bad json")
	got := f.reader().read(deepReviewNow)
	if !got.Unavailable || len(got.Runs) != 0 || len(got.Schedules) != 0 {
		t.Errorf("an unreadable patrols config read as %+v, want unavailable and nothing else", got)
	}
	if len(f.labels) != 0 {
		t.Errorf("the reader listed %v for a config it could not read", f.labels)
	}
}

// A schedule the config carries but whose interval is unusable is not
// scheduled, and the schedules beside it still draw their own cells.
func TestDeepReviewScheduleWithNoUsableInterval(t *testing.T) {
	t.Parallel()

	for _, interval := range []string{"soon", "0s", "-1h", ""} {
		f := newFakeDeepReview(t)
		f.patrols = scheduledPatrols(true, interval, defaultSchedules...)
		f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, goodNotes))
		got := f.reader().read(deepReviewNow)
		if len(got.Schedules) != len(defaultSchedules) {
			t.Fatalf("interval %q: schedules = %+v, want one row per configured schedule", interval, got.Schedules)
		}
		for _, s := range got.Schedules {
			if s.Next != deepReviewNotScheduled {
				t.Errorf("interval %q: %s drew %q, want not scheduled", interval, s.Name, s.Next)
			}
		}
		// The run is still the schedule's last one: an interval the town cannot
		// use says when the next run is, not what the last one did.
		if last := scheduleFor(t, got.Schedules, deepReviewSchedule).Last; last == nil || last.Findings != 3 {
			t.Errorf("interval %q: the row's last run is %+v, want gt-r1", interval, last)
		}
	}
}

func TestDeepReviewHeld(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		file string
		want string
	}{
		"hold file":  {file: dispatch.HoldFileName, want: "operator dispatch hold present"},
		"town ESTOP": {file: estop.FileName, want: "town ESTOP active"},
		"rig ESTOP":  {file: estop.RigFileName(deepReviewRig), want: "rig ESTOP active"},
	} {
		f := newFakeDeepReview(t)
		f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, ""))
		if err := os.WriteFile(filepath.Join(f.townRoot, tc.file), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got := f.reader().read(deepReviewNow)
		if !strings.Contains(got.Paused, tc.want) {
			t.Errorf("%s: paused = %q, want it to say %q", name, got.Paused, tc.want)
		}
		// A held town starts no run, and says so beside rows that are still the
		// schedules' own facts.
		if len(got.Schedules) != len(defaultSchedules) {
			t.Errorf("%s: a held town hides its rows: %+v", name, got.Schedules)
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

// One schedule's list failing leaves the block with a partial picture of the
// review, which is worse than saying the picture could not be read.
func TestDeepReviewListErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.listErr = errors.New("bd: dolt down")
	f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, ""))
	got := f.reader().read(deepReviewNow)
	if !got.Unavailable || len(got.Runs) != 0 || len(got.Schedules) != 0 {
		t.Errorf("a failed list read as %+v, want unavailable and nothing else", got)
	}
}

func TestDeepReviewCommentsErrorKeepsTheRow(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.set(deepReviewSchedule, runBead("gt-r1", 5, 10, goodNotes))
	f.commentsErr = errors.New("bd: timeout")
	got := f.reader().read(deepReviewNow)
	if got.Unavailable || len(got.Runs) != 1 || got.Runs[0].Findings != 3 {
		t.Fatalf("read = %+v, want the row kept", got)
	}
	if !strings.Contains(got.Runs[0].Digest, "unreadable") {
		t.Errorf("digest = %q, want it to say the comments were unreadable", got.Runs[0].Digest)
	}
	if last := scheduleFor(t, got.Schedules, deepReviewSchedule).Last; last == nil || !strings.Contains(last.Digest, "unreadable") {
		t.Errorf("the row's last run is %+v, want the unreadable digest named there too", last)
	}
}

// Notes are text an agent wrote: a malformed, binary or oversized one never
// panics and never costs more than the caps.
func TestDeepReviewMalformedNotes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		notes           string
		findings, filed int
		unparsed        bool
	}{
		{"empty", "", 0, 0, false},
		{"prose only", "worked on it\nfinding something later", 0, 0, false},
		{"finding without fields", "finding:", 0, 0, true},
		{"finding with one field", "finding: a.go:F:x", 0, 0, true},
		{"queued without fields", "queued: a.go:F:x P2", 0, 0, true},
		{"cursor without fields", "cursor: abc", 0, 0, true},
		{"good lines survive a bad one", "finding: a b\nfinding: x\nqueued: a P1 f:1 c", 1, 1, true},
		{"digest-only is not filed", "finding: a digest-only", 1, 0, false},
		{"indented and spaced", "  finding:   a    gt-x  ", 1, 1, false},
		{"binary", "\x00\x01\xff\xfe finding: a b\n\x00\x00", 0, 0, false},
		{"invalid utf8 after a prefix", "finding: \xff\xfe \xfd", 1, 1, false},
		{"crlf", "finding: a gt-x\r\nqueued: a P1 f:1 c\r\n", 1, 1, false},
	} {
		run := dashboard.DeepReviewRun{}
		deepReviewCountNotes(&run, tc.notes)
		if run.Findings != tc.findings || run.Filed != tc.filed || run.Unparsed != tc.unparsed {
			t.Errorf("%s: findings %d filed %d unparsed %v, want %d/%d/%v",
				tc.name, run.Findings, run.Filed, run.Unparsed, tc.findings, tc.filed, tc.unparsed)
		}
	}
}

func TestDeepReviewOversizedNotes(t *testing.T) {
	t.Parallel()

	// One finding at the top, then more than the 64 KiB cap of filler: the
	// finding counts, the cut marks the run, and a line past 512 bytes is read
	// only as far as the cap.
	big := "finding: a gt-x\n" + strings.Repeat("x", deepReviewNotesMax) + "\nfinding: late gt-y"
	run := dashboard.DeepReviewRun{}
	deepReviewCountNotes(&run, big)
	if run.Findings != 1 || !run.Unparsed {
		t.Errorf("oversized notes: findings %d unparsed %v, want 1/true", run.Findings, run.Unparsed)
	}

	// A long line whose fields sit inside the cap still parses; one whose
	// fields start past it does not exist to the reader.
	long := "finding: a gt-x " + strings.Repeat("y", 4*deepReviewLineMax)
	run = dashboard.DeepReviewRun{}
	deepReviewCountNotes(&run, long)
	if run.Findings != 1 || run.Filed != 1 || run.Unparsed {
		t.Errorf("long line: %+v, want one filed finding", run)
	}
	late := "finding: a " + strings.Repeat(" ", 2*deepReviewLineMax) + "gt-x"
	run = dashboard.DeepReviewRun{}
	deepReviewCountNotes(&run, late)
	if run.Findings != 0 || !run.Unparsed {
		t.Errorf("a line whose fields lie past the cap: %+v, want unparsed", run)
	}

	// Many short lines stay inside the byte cap.
	many := strings.Repeat("finding: a gt-x\n", deepReviewNotesMax)
	run = dashboard.DeepReviewRun{}
	deepReviewCountNotes(&run, many)
	if run.Findings > deepReviewNotesMax/len("finding: a gt-x\n") || !run.Unparsed {
		t.Errorf("many lines: findings %d unparsed %v", run.Findings, run.Unparsed)
	}
}

func TestDeepReviewRunFromAMalformedBeadDoesNotPanic(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.set(deepReviewSchedule,
		nil,
		&beads.Issue{ID: "gt-bare"},
		&beads.Issue{ID: "gt-badtimes", Status: "closed", CreatedAt: "yesterday", ClosedAt: "later", Notes: "\x00\xff"},
		&beads.Issue{ID: "gt-backwards", Status: "closed", CreatedAt: deepReviewNow.Format(time.RFC3339), ClosedAt: deepReviewNow.Add(-time.Hour).Format(time.RFC3339)},
		&beads.Issue{ID: "gt-<b>\x1b[31mhtml</b>", Status: "closed", CreatedAt: deepReviewNow.Add(-time.Hour).Format(time.RFC3339), ClosedAt: deepReviewNow.Format(time.RFC3339)},
	)
	got := f.reader().read(deepReviewNow)
	if len(got.Runs) != 4 {
		t.Fatalf("runs = %v, want the four non-nil beads", runIDs(got.Runs))
	}
	for _, r := range got.Runs {
		if strings.ContainsAny(r.ID, "\x1b\n") {
			t.Errorf("run id %q carries a control character", r.ID)
		}
	}
	// A bead with no creation time sorts after every one with one, so a bad
	// stamp cannot read as the newest run.
	if last := got.Runs[len(got.Runs)-1].ID; last != "gt-bare" && last != "gt-badtimes" {
		t.Errorf("the last run is %q, want one of the beads with no usable time", last)
	}
	// The schedule's own last run is the first of the same list, malformed
	// times and all: the row and the runs list are one reading of the beads.
	if last := scheduleFor(t, got.Schedules, deepReviewSchedule).Last; last == nil || last.ID != got.Runs[0].ID {
		t.Errorf("the row's last run is %+v, want the newest run %s", last, got.Runs[0].ID)
	}
}

func TestDeepReviewStringsAreCapped(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.set(deepReviewSchedule, runBead("gt-"+strings.Repeat("é", 5000), 1, 1, ""))
	f.comments = map[string][]beads.Comment{f.runs[deepReviewLabel(deepReviewSchedule)][0].ID: {{Text: "digest"}}}
	got := f.reader().read(deepReviewNow)
	if n := len([]rune(got.Runs[0].ID)); n != deepReviewFieldMax {
		t.Errorf("id is %d runes, want the cap %d", n, deepReviewFieldMax)
	}
	if !strings.HasSuffix(got.Runs[0].Digest, " · 1 comment") || len([]rune(got.Runs[0].Digest)) > deepReviewFieldMax+20 {
		t.Errorf("digest is %d runes, want the capped id and the count", len([]rune(got.Runs[0].Digest)))
	}
}

// A schedule's name and rig are the town's config text, so both are drawn
// through the same cap and scrub as every other string the block takes from
// outside this binary.
func TestDeepReviewScheduleNameIsCapped(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	name := "sched-" + strings.Repeat("é", 5000)
	f.patrols = scheduledPatrols(true, deepReviewInterval, name)
	f.set(name, runBead("gt-r1", 5, 10, ""))
	got := f.reader().read(deepReviewNow)
	if len(got.Schedules) != 1 {
		t.Fatalf("schedules = %+v, want the one configured schedule", got.Schedules)
	}
	if n := len([]rune(got.Schedules[0].Name)); n != deepReviewFieldMax {
		t.Errorf("name is %d runes, want the cap %d", n, deepReviewFieldMax)
	}
}

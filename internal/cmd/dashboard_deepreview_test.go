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

// fakeDeepReview answers a dashDeepReviewReader from fixtures.
type fakeDeepReview struct {
	issues      []*beads.Issue
	listErr     error
	comments    map[string][]beads.Comment
	commentsErr error
	patrols     *config.PatrolsConfig
	patrolsErr  error
	townRoot    string
	reads       []string
}

// scheduled is a patrols config carrying the deep-review schedule.
func scheduledPatrols(enabled bool, interval string) *config.PatrolsConfig {
	return &config.PatrolsConfig{ScheduledSlings: &config.ScheduledSlingsConfig{
		Enabled: enabled,
		Entries: []config.ScheduledSlingEntry{{Name: deepReviewSchedule, Rig: deepReviewRig, IntervalStr: interval}},
	}}
}

func newFakeDeepReview(t *testing.T) *fakeDeepReview {
	t.Helper()
	return &fakeDeepReview{townRoot: t.TempDir(), patrols: scheduledPatrols(true, deepReviewInterval)}
}

func (f *fakeDeepReview) reader() *dashDeepReviewReader {
	return &dashDeepReviewReader{
		townRoot: f.townRoot,
		list:     func() ([]*beads.Issue, error) { return f.issues, f.listErr },
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

// The panel's query names the label, every status, and no priority filter:
// ListOptions' zero value is Priority 0, which bd reads as "P0 only", so a run
// at any other priority would vanish from a pane that asked that way.
func TestDeepReviewListAsksForEveryRun(t *testing.T) {
	t.Parallel()

	o := deepReviewListOptions
	if o.Label != "scheduled:deep-review" || o.Status != "all" || o.Priority != -1 || o.Limit != 0 {
		t.Errorf("the panel's query is %+v", o)
	}
}

func TestDeepReviewRowsAreTheNewestTen(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	// Twelve closed runs, listed oldest first so the sort has work to do.
	for i := 12; i >= 1; i-- {
		f.issues = append(f.issues, runBead(fmt.Sprintf("gt-r%02d", i), i*24, 30, goodNotes))
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

func TestDeepReviewEmpty(t *testing.T) {
	t.Parallel()

	got := newFakeDeepReview(t).reader().read(deepReviewNow)
	if got.Unavailable || got.Rows == nil || len(got.Rows) != 0 {
		t.Errorf("an empty list read as %+v, want an empty, available panel", got)
	}
	if got.Next != "due now" {
		t.Errorf("a schedule with no run yet is %q, want due now", got.Next)
	}
}

func TestDeepReviewOneOpenRun(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.issues = []*beads.Issue{openRunBead("gt-live", 1), runBead("gt-old", 48, 20, "")}
	got := f.reader().read(deepReviewNow)

	if got.Next != "running" {
		t.Errorf("next = %q, want running", got.Next)
	}
	r := got.Rows[0]
	if r.ID != "gt-live" || r.Status != dashboard.DeepReviewOpen || r.DurationSec != nil {
		t.Errorf("open row = %+v, want open with no duration", r)
	}
}

// A run's bead the patrol closed because the sling failed is not a run: it
// neither counts as the last one nor draws as one.
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
	if got.Next != "in 14h 0m" {
		t.Errorf("next = %q, want in 14h 0m", got.Next)
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

func TestDeepReviewScheduleThatCannotBeRead(t *testing.T) {
	t.Parallel()

	for name, set := range map[string]func(*fakeDeepReview){
		"bad interval":      func(f *fakeDeepReview) { f.patrols = scheduledPatrols(true, "soon") },
		"zero interval":     func(f *fakeDeepReview) { f.patrols = scheduledPatrols(true, "0s") },
		"negative interval": func(f *fakeDeepReview) { f.patrols = scheduledPatrols(true, "-1h") },
		"empty interval":    func(f *fakeDeepReview) { f.patrols = scheduledPatrols(true, "") },
		"disabled":          func(f *fakeDeepReview) { f.patrols = scheduledPatrols(false, "24h") },
		"no patrols":        func(f *fakeDeepReview) { f.patrols = nil },
		"no scheduled_slings": func(f *fakeDeepReview) {
			f.patrols = &config.PatrolsConfig{}
		},
		"another entry": func(f *fakeDeepReview) {
			f.patrols = scheduledPatrols(true, "24h")
			f.patrols.ScheduledSlings.Entries[0].Name = "doc-audit"
		},
		"config unreadable": func(f *fakeDeepReview) { f.patrolsErr = errors.New("bad json") },
	} {
		f := newFakeDeepReview(t)
		set(f)
		f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, goodNotes)}
		got := f.reader().read(deepReviewNow)
		if got.Next != "not scheduled" {
			t.Errorf("%s: next = %q, want not scheduled", name, got.Next)
		}
		if got.Unavailable || len(got.Rows) != 1 {
			t.Errorf("%s: the rows are %+v, want the run still shown", name, got)
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

func TestDeepReviewListErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	f := newFakeDeepReview(t)
	f.listErr = errors.New("bd: dolt down")
	f.issues = []*beads.Issue{runBead("gt-r1", 5, 10, "")}
	got := f.reader().read(deepReviewNow)
	if !got.Unavailable || len(got.Rows) != 0 || got.Next != "" {
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

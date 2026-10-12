package cmd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// The Deep review panel: when the scheduled deep review is next due, or why the
// town holds it, and the last ten runs with what each found. Every run is a
// bead the daemon's scheduled_slings patrol files under the label
// "scheduled:deep-review" in the gastown rig's own database, so that is where
// the reader lists; the town database holds none of them.
//
// A read that fails is a named state rather than an empty list, because "no
// deep review has run" and "the runs could not be read" are different things to
// tell the operator. The panel only reads: it never files, closes or pauses a
// run, and it never draws a comment's text.

const (
	// deepReviewRig is the rig the deep review runs in.
	deepReviewRig = "gastown"
	// deepReviewLabel is the label the patrol files each run under:
	// "scheduled:" plus the schedule's name.
	deepReviewLabel = "scheduled:" + deepReviewSchedule

	// dashDeepReviewRows is how many runs the panel draws.
	dashDeepReviewRows = 10

	// deepReviewNotesMax and deepReviewLineMax bound what a run's notes cost
	// the reader. Notes are text the run's agent wrote through gt bead note;
	// a runaway or binary one must not cost the dashboard more than this.
	deepReviewNotesMax = 64 << 10
	deepReviewLineMax  = 512

	// deepReviewFieldMax caps a string the panel draws, in runes.
	deepReviewFieldMax = 200

	// deepReviewDigestOnly is the second token of a finding: line the run filed
	// no bead for.
	deepReviewDigestOnly = "digest-only"

	// The notes lines a run writes, and how many tokens follow the label.
	// finding: <fingerprint> <bead id | digest-only>
	// queued:  <fingerprint> <P1|P2> <file:line> <claim>
	// cursor:  <reviewed_until> <rotation_cursor>
	deepReviewFindingLine = "finding:"
	deepReviewQueuedLine  = "queued:"
	deepReviewCursorLine  = "cursor:"
)

// deepReviewListOptions is the query the panel asks for: every run, closed ones
// included, at any priority. Priority is the no-filter sentinel -1 rather than
// the zero value, which bd reads as a P0-only filter.
var deepReviewListOptions = beads.ListOptions{
	Label:    deepReviewLabel,
	Status:   "all",
	Priority: -1,
	Limit:    0,
}

// dashDeepReviewReader reads the deep review's runs. list, comments and
// patrols are injectable so the row building is testable without a database or
// a town config.
type dashDeepReviewReader struct {
	townRoot string
	list     func() ([]*beads.Issue, error)
	comments func(id string) ([]beads.Comment, error)
	patrols  func() (*config.PatrolsConfig, error)
}

func newDashDeepReviewReader(townRoot string) *dashDeepReviewReader {
	// The rig's own database: a reader that listed from the town's would see
	// none of the runs. Resolve its directory first, as a rig whose .beads is a
	// redirect keeps its beads elsewhere.
	b := beads.New(beads.ResolveBeadsDir(filepath.Join(townRoot, deepReviewRig)))
	return &dashDeepReviewReader{
		townRoot: townRoot,
		list:     func() ([]*beads.Issue, error) { return b.List(deepReviewListOptions) },
		comments: b.Comments,
		patrols:  func() (*config.PatrolsConfig, error) { return loadDashPatrols(townRoot) },
	}
}

// read is the panel's whole reading. A list that fails is an unavailable panel;
// a digest whose comments cannot be counted is a row that says so, since the
// runs themselves were read.
func (r *dashDeepReviewReader) read(now time.Time) *dashboard.DeepReview {
	issues, err := r.list()
	if err != nil {
		return &dashboard.DeepReview{Unavailable: true}
	}

	out := &dashboard.DeepReview{
		Next:   r.next(issues, now),
		Paused: deepReviewField(dispatch.RigHold(r.townRoot, deepReviewRig)),
		Rows:   []dashboard.DeepReviewRow{},
	}

	runs := deepReviewRuns(issues)
	if len(runs) > dashDeepReviewRows {
		runs = runs[:dashDeepReviewRows]
	}
	for _, issue := range runs {
		row := deepReviewRow(issue)
		row.Digest = r.digest(issue.ID, row.ID)
		out.Rows = append(out.Rows, row)
	}
	return out
}

// deepReviewRuns are the listed beads that were runs, newest first. A bead the
// patrol closed because its sling failed is not one: no run happened, and a row
// of zeros would read as a clean review. A bead with no creation time that
// parses sorts after every one that has one.
func deepReviewRuns(issues []*beads.Issue) []*beads.Issue {
	runs := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue != nil && !deepReviewSlingFailed(issue) {
			runs = append(runs, issue)
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := beads.ParseIssueTime(runs[i].CreatedAt), beads.ParseIssueTime(runs[j].CreatedAt)
		switch {
		case a.IsZero() != b.IsZero():
			return b.IsZero()
		case a.IsZero():
			return false
		}
		return a.After(b)
	})
	return runs
}

// deepReviewSlingFailureMarker prefixes the close reason the scheduled_slings
// patrol writes on a run bead whose sling failed (internal/daemon's
// scheduledSlingFailureMarker, which this package may not import).
const deepReviewSlingFailureMarker = "scheduled_slings-sling-failed"

func deepReviewSlingFailed(issue *beads.Issue) bool {
	return issue.Status == "closed" && strings.HasPrefix(issue.CloseReason, deepReviewSlingFailureMarker)
}

// next is the panel's "next run" line: the schedule's interval from the town's
// config and the due logic of decideScheduledSling over the listed beads. A
// schedule that is missing, disabled or has no usable interval has no next run,
// and the rows still show.
func (r *dashDeepReviewReader) next(issues []*beads.Issue, now time.Time) string {
	patrols, err := r.patrols()
	if err != nil {
		return deepReviewNotScheduled
	}
	entry := scheduledSling(patrols, deepReviewSchedule)
	if entry == nil || !patrols.ScheduledSlings.Enabled {
		return deepReviewNotScheduled
	}
	interval, err := time.ParseDuration(entry.IntervalStr)
	if err != nil || interval <= 0 {
		return deepReviewNotScheduled
	}
	return deepReviewNextDue(issues, interval, now)
}

const (
	deepReviewNotScheduled = "not scheduled"
	deepReviewRunning      = "running"
	deepReviewDueNow       = "due now"
)

// deepReviewNextDue mirrors decideScheduledSling in internal/daemon, which this
// package may not import, and answers when it would next dispatch: a bead that
// is not closed means a run is in flight, whatever its age; failed-sling beads
// are ignored; otherwise the newest run's creation time plus the interval, and
// "due now" once that has passed. A schedule with no run yet is due now.
func deepReviewNextDue(issues []*beads.Issue, interval time.Duration, now time.Time) string {
	var newest time.Time
	for _, issue := range issues {
		if issue == nil || deepReviewSlingFailed(issue) {
			continue
		}
		if issue.Status != "closed" {
			return deepReviewRunning
		}
		if at := beads.ParseIssueTime(issue.CreatedAt); at.After(newest) {
			newest = at
		}
	}
	if newest.IsZero() {
		return deepReviewDueNow
	}
	left := newest.Add(interval).Sub(now)
	if left <= 0 {
		return deepReviewDueNow
	}
	return "in " + formatDuration(left.Truncate(time.Minute))
}

// digest names a run's digest: the bead's id and the comments it carries. The
// digest is the run's last comment, and its text is never read into the page.
// The comments are read by the bead's own id; label is the id as drawn.
func (r *dashDeepReviewReader) digest(id, label string) string {
	comments, err := r.comments(id)
	switch {
	case err != nil:
		return label + " · comments unreadable"
	case len(comments) == 0:
		return ""
	case len(comments) == 1:
		return label + " · 1 comment"
	}
	return fmt.Sprintf("%s · %d comments", label, len(comments))
}

// deepReviewField is a string the panel draws: one line, control and invisible
// format characters gone, capped at deepReviewFieldMax runes.
func deepReviewField(s string) string {
	return capRunes(questionField(s), deepReviewFieldMax)
}

// deepReviewRow is one run bead as the panel draws it, apart from its digest.
func deepReviewRow(issue *beads.Issue) dashboard.DeepReviewRow {
	row := dashboard.DeepReviewRow{
		ID:     deepReviewField(issue.ID),
		Status: dashboard.DeepReviewOpen,
	}
	if issue.Status == "closed" {
		row.Status = dashboard.DeepReviewClosed
		row.DurationSec = deepReviewDurationSec(issue)
	}
	deepReviewCountNotes(&row, issue.Notes)
	return row
}

// deepReviewDurationSec is closed_at minus created_at, nil when either does
// not parse or the close precedes the creation: a duration the bead cannot
// support is none, not zero.
func deepReviewDurationSec(issue *beads.Issue) *int64 {
	created, closed := beads.ParseIssueTime(issue.CreatedAt), beads.ParseIssueTime(issue.ClosedAt)
	if created.IsZero() || closed.IsZero() || closed.Before(created) {
		return nil
	}
	sec := int64(closed.Sub(created) / time.Second)
	return &sec
}

// deepReviewCountNotes reads a run's notes into the row's counts. The notes are
// whatever the run's agent appended, so nothing here may trust them: at most
// deepReviewNotesMax bytes are read, at most deepReviewLineMax bytes of a line,
// and a line that names one of the run's records but lacks its fields is
// skipped and marks the row unparsed. So does a notes field that was cut at the
// cap, as its counts may be short. Text that is not a record line, binary
// included, is ignored.
func deepReviewCountNotes(row *dashboard.DeepReviewRow, notes string) {
	if len(notes) > deepReviewNotesMax {
		notes = notes[:deepReviewNotesMax]
		row.Unparsed = true
	}
	for _, line := range strings.Split(notes, "\n") {
		if len(line) > deepReviewLineMax {
			line = line[:deepReviewLineMax]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		args := fields[1:]
		switch fields[0] {
		case deepReviewFindingLine:
			if len(args) < 2 {
				row.Unparsed = true
				continue
			}
			row.Findings++
			if args[1] != deepReviewDigestOnly {
				row.Filed++
			}
		case deepReviewQueuedLine:
			if len(args) < 4 {
				row.Unparsed = true
				continue
			}
			row.Queued++
		case deepReviewCursorLine:
			if len(args) < 2 {
				row.Unparsed = true
			}
		}
	}
}

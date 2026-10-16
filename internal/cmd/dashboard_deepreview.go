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
	"github.com/steveyegge/gastown/internal/question"
)

// The deep review's block of the Overseer panel: one row per entry of the
// town's daemon.patrols.scheduled_slings — the schedule's name, when it is next
// due and the newest run it filed, with what that run found and how many beads
// it filed — and the review's newest runs, which the Usage panel joins its own
// records to. Every run is a bead the daemon's scheduled_slings patrol files
// under the label "scheduled:" plus its schedule's name, in the gastown rig's
// own database, so that is where the reader lists; the town database holds none
// of them.
//
// The block reads the town's schedule list rather than a set of names baked
// into this binary: the review's slice and risk paths were once the whole list,
// and a schedule configured beside them was a schedule the page could not see
// (gt-uv08f). Its names are the config's own text, so each is drawn through
// deepReviewField.
//
// A read that fails is a named state rather than an empty list, because "no
// deep review has run" and "the runs could not be read" are different things to
// tell the operator. The block only reads: it never files, closes or pauses a
// run, and it never draws a comment's text.

const (
	// deepReviewRig is the rig the deep review runs in.
	deepReviewRig = "gastown"

	// dashDeepReviewRows is how many of the review's newest runs the payload
	// carries for the Usage panel to join its records to.
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

// deepReviewLabel is the label a schedule's runs carry.
func deepReviewLabel(name string) string { return "scheduled:" + name }

// deepReviewListOptions is the query the panel asks for, once per schedule:
// every run, closed ones included, at any priority. Priority is the no-filter
// sentinel -1 rather than the zero value, which bd reads as a P0-only filter.
// The label is the schedule's, so the reader sets it per list.
var deepReviewListOptions = beads.ListOptions{
	Status:   "all",
	Priority: -1,
	Limit:    0,
}

// dashDeepReviewReader reads the deep review's runs. list, comments and
// patrols are injectable so the row building is testable without a database or
// a town config.
type dashDeepReviewReader struct {
	townRoot string
	list     func(label string) ([]*beads.Issue, error)
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
		list: func(label string) ([]*beads.Issue, error) {
			options := deepReviewListOptions
			options.Label = label
			return b.List(options)
		},
		comments: b.Comments,
		patrols:  func() (*config.PatrolsConfig, error) { return loadDashPatrols(townRoot) },
	}
}

// read is the block's whole reading: one line per entry of the town's
// scheduled_slings config, with that schedule's next-run text and the newest
// run it filed, then the review's newest runs. A list that fails is an
// unavailable block, and so is a patrols config that is there and does not
// parse: the block says which it is rather than reading as a town that
// configures no schedule. A digest whose comments cannot be counted is a run
// that says so, since the runs themselves were read.
func (r *dashDeepReviewReader) read(now time.Time) *dashboard.DeepReview {
	out := &dashboard.DeepReview{
		Schedules: []dashboard.DeepReviewSchedule{},
		Runs:      []dashboard.DeepReviewRun{},
		Paused:    deepReviewField(dispatch.RigHold(r.townRoot, deepReviewRig)),
	}
	// The block reads the town's own schedule list rather than a set of names
	// baked into this binary: a schedule added to the config — a documentation
	// audit beside the review's slice and risk paths — is a row the moment it
	// exists, and one the config drops is a row that goes (gt-uv08f).
	patrols, err := r.patrols()
	if err != nil {
		// A config that is there and does not parse is not a town with no
		// schedules: one is a daemon.json to fix, the other a town that never
		// ran the review, and the block must not read alike for both (gt-uv08f).
		return &dashboard.DeepReview{Unavailable: true}
	}
	var slings *config.ScheduledSlingsConfig
	if patrols != nil {
		slings = patrols.ScheduledSlings
	}
	// The schedules the block reads runs from: the config's own entries, and —
	// when the town configures none, because there is no config or the patrol
	// carries no entries — the names this binary knows the review has run as.
	// The runs are the review's history whatever the config now says, and the
	// Usage panel joins its own records to them by bead id, so a town that
	// configures no schedule must not read as a review with no runs (gt-uv08f).
	// Only the config's own entries draw rows: a name the config does not
	// configure is not a schedule to draw a next run for.
	entries, drawRows := []config.ScheduledSlingEntry(nil), true
	if slings != nil {
		entries = slings.Entries
	}
	if len(entries) == 0 {
		entries, drawRows = knownDeepReviewEntries(), false
	}
	// The patrol off leaves every configured schedule unrun: the rows stay,
	// since the town configures them, and each says the patrol is off rather
	// than counting down to a run the daemon will not dispatch.
	enabled := slings != nil && slings.Enabled
	digests := map[string]string{}
	digestFor := func(issue *beads.Issue, label string) string {
		if text, ok := digests[issue.ID]; ok {
			return text
		}
		text := r.digest(issue.ID, label)
		digests[issue.ID] = text
		return text
	}

	// One list per schedule: bd's label filter keeps beads carrying the label,
	// so asking once for every schedule would list only a bead carrying all of
	// them, which is none of them. A schedule whose list fails leaves the block
	// with a partial picture of the review, which is worse than saying the
	// picture could not be read.
	listed := make([]*beads.Issue, 0, len(entries))
	for _, entry := range entries {
		issues, err := r.list(deepReviewLabel(entry.Name))
		if err != nil {
			return &dashboard.DeepReview{Unavailable: true}
		}
		listed = append(listed, issues...)
		if !drawRows {
			continue
		}

		schedule := dashboard.DeepReviewSchedule{
			Name: deepReviewField(entry.Name),
			Rig:  deepReviewField(entry.Rig),
			Next: deepReviewNotScheduled,
		}
		if !enabled {
			schedule.Next = deepReviewPatrolOff
		} else if interval, err := time.ParseDuration(entry.IntervalStr); err == nil && interval > 0 {
			schedule.Next = deepReviewScheduleNext(&entry, issues, interval, now)
		}
		if runs := deepReviewRuns(issues); len(runs) > 0 {
			last := deepReviewRow(runs[0])
			last.Digest = digestFor(runs[0], last.ID)
			schedule.Last = &last
		}
		out.Schedules = append(out.Schedules, schedule)
	}

	runs := deepReviewRuns(listed)
	if len(runs) > dashDeepReviewRows {
		runs = runs[:dashDeepReviewRows]
	}
	for _, issue := range runs {
		run := deepReviewRow(issue)
		run.Digest = digestFor(issue, run.ID)
		out.Runs = append(out.Runs, run)
	}
	return out
}

// deepReviewRuns are the listed beads that are runs, newest first. A bead the
// patrol closed because its sling failed is not one: no run happened, and a row
// of zeros would read as a clean review. A bead with no creation time that
// parses sorts after every one that has one.
func deepReviewRuns(listed []*beads.Issue) []*beads.Issue {
	runs := make([]*beads.Issue, 0, len(listed))
	for _, issue := range listed {
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

// deepReviewScheduleNext is one schedule's next-run text. An open run is
// "running" whatever the schedule's gate says: the gate let that run through,
// and it is what the panel is watching. A schedule whose gate decides when it
// runs draws the gate in place of the countdown the interval would give, since
// that interval no longer says when the next run happens.
func deepReviewScheduleNext(entry *config.ScheduledSlingEntry, issues []*beads.Issue, interval time.Duration, now time.Time) string {
	due := deepReviewNextDue(issues, interval, now)
	if due == deepReviewRunning || strings.TrimSpace(entry.GateCommand) == "" {
		return due
	}
	return deepReviewGated
}

const (
	deepReviewNotScheduled = "not scheduled"
	deepReviewRunning      = "running"
	deepReviewDueNow       = "due now"
	// deepReviewGated is the next-run text of a schedule with a gate_command:
	// the gate decides whether an interval's run happens at all.
	deepReviewGated = "gated: runs when needed"
	// deepReviewPatrolOff is the next-run text of every schedule in a town whose
	// scheduled_slings patrol is disabled: the schedules are still configured,
	// so they still draw, but nothing the interval says will be dispatched.
	deepReviewPatrolOff = "patrol disabled"
)

// knownDeepReviewEntries are the schedules the review is known to run as, for
// the town whose config lists none: the slice and risk paths and the legacy
// name the runs of both carry. They feed the runs listing alone — a town that
// configures no schedule draws no row for one.
func knownDeepReviewEntries() []config.ScheduledSlingEntry {
	entries := make([]config.ScheduledSlingEntry, 0, len(deepReviewModelSchedules))
	for _, name := range deepReviewModelSchedules {
		entries = append(entries, config.ScheduledSlingEntry{Name: name, Rig: deepReviewRig})
	}
	return entries
}

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
	return question.CapRunes(question.Field(s), deepReviewFieldMax)
}

// deepReviewRow is one run bead as the blocks draw it, apart from its digest.
func deepReviewRow(issue *beads.Issue) dashboard.DeepReviewRun {
	row := dashboard.DeepReviewRun{
		ID:     deepReviewField(issue.ID),
		Status: dashboard.DeepReviewOpen,
	}
	if issue.Status == "closed" {
		row.Status = dashboard.DeepReviewClosed
	}
	deepReviewCountNotes(&row, issue.Notes)
	return row
}

// deepReviewCountNotes reads a run's notes into the run's counts. The notes are
// whatever the run's agent appended, so nothing here may trust them: at most
// deepReviewNotesMax bytes are read, at most deepReviewLineMax bytes of a line,
// and a line that names one of the run's records but lacks its fields is
// skipped and marks the run unparsed. So does a notes field that was cut at the
// cap, as its counts may be short. Text that is not a record line, binary
// included, is ignored.
func deepReviewCountNotes(row *dashboard.DeepReviewRun, notes string) {
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
			// The run's queued findings are counted by the run itself, not by
			// this block: the line is read only for whether it is whole, which
			// is what Unparsed tells the reader.
			if len(args) < 4 {
				row.Unparsed = true
			}
		case deepReviewCursorLine:
			if len(args) < 2 {
				row.Unparsed = true
			}
		}
	}
}

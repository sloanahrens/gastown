// Package submission is the advisory battery on work waiting to land. A pass
// takes the gastown submissions since the last pass, asks Jev one battery about
// each, and appends one line per submission to a log that
// `gt jev submission --recent` prints. It never comments on a bead, files an
// attention item, blocks a landing or writes under .runtime/landings: the log
// is the only thing it writes.
//
// A submission comes from the first-sight record the landing worker wrote, so
// it is asked about whether or not it has landed. A record with no commits is
// read from a bare clone the pass owns, at ClonePath, so the pass never runs
// git in a town worktree and never contends with a landing for the rig's
// repository.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the
// submissions, the git reads and the Jev client, so a test runs a pass with
// fakes.
// Open wires the production versions.
package submission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

const (
	// RigName is the rig whose submissions a pass reads.
	RigName = "gastown"

	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-submission"

	// maxSectionRunes caps the bead's goal and acceptance quoted in the state.
	maxSectionRunes = 1500

	// maxNotesRunes caps the MR notes quoted in the state.
	maxNotesRunes = 4000

	// maxSubjects caps the commit subjects listed in the state.
	maxSubjects = 20

	// maxFiles caps the changed files listed in the state.
	maxFiles = 80

	// agentVar is the attached_vars key the dispatcher stamps with the seat's
	// agent.
	agentVar = "agent"
)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Submissions lists what a pass may ask about from cursor time t on: the
// first-sight records the landing worker wrote since then, and each waiting
// work bead that has no record. A pass only reads them.
type Submissions interface {
	Since(t time.Time) ([]Entry, error)
}

// Entry is one submission a pass can ask about.
type Entry struct {
	// Issue is the work bead. The state reads its title, goal, acceptance and
	// agent from it.
	Issue *beads.Issue
	// Work is the landing request: the branch, head, target and submission
	// time the line records.
	Work land.Work
	// Seen is when the pass orders the entry: a record's first sight, or for a
	// waiting bead with no record, its submission time.
	Seen time.Time
	// Subjects and Numstat are what the landing worker read from its clone at
	// first sight. Nil Subjects means the record holds none, and the pass reads
	// its own clone instead.
	Subjects []string
	Numstat  string
}

// Git reads a submission's commits and diff stat from the bare clone the pass
// owns, for an entry whose record holds no commits. Every command it runs runs
// in that clone, never in a town worktree.
type Git interface {
	// Sync creates the clone on the first run and fetches refs/heads/* into
	// it. It runs only when an entry needs the clone, and its failure stops
	// the pass and leaves the cursor unmoved.
	Sync(ctx context.Context) error
	// CommitSubjects returns the subjects on target..head, at most limit.
	CommitSubjects(ctx context.Context, target, head string, limit int) ([]string, error)
	// Numstat returns `git diff --numstat` for target..head.
	Numstat(ctx context.Context, target, head string) (string, error)
}

// Pass is one run of the submission battery, with every dependency injected.
type Pass struct {
	Asker       Asker
	Submissions Submissions
	Git         Git

	// LogPath is the log every answered submission appends to.
	LogPath string
	// CursorPath remembers the last submission a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The
	// pass asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Warnings receives a line for each submission the pass skips, naming the
	// cause. Nil drops them.
	Warnings io.Writer
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of submissions that got a log line this pass.
	Asked int
	// Skipped is the number of submissions passed over with no line: the
	// clone could not read their commits or diff, or Jev returned an unusable
	// response for them. An answer built on a diff the battery never saw would
	// judge a change nobody described, so no line is written, and one such
	// submission does not hold up the ones behind it.
	Skipped int
	// Repeated is the number of entries passed over because the log already
	// held a line for their bead and head at the current question version. One
	// submission reaches a pass twice when it is asked from its waiting bead
	// before the landing worker has written its first-sight record, and the
	// record is listed again by a later pass (gt-v419b).
	Repeated int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The submissions it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// ErrFetch means the bare clone could not be fetched, so the pass skipped
// everything: the submissions behind the failure wait for the next pass and
// the cursor does not move.
var ErrFetch = errors.New("the bare clone could not be fetched")

// Run does one pass. Entries are taken oldest first, and each is logged and the
// cursor advanced before the next, so a pass stopped partway never asks about
// a submission twice. An entry the log already holds a line for at the current
// question version is passed over instead of asked, and counted as Repeated: a
// submission listed under two seen times is asked once (gt-v419b). An entry
// whose Ask fails for any reason but an unusable response stops the pass with
// that error and is asked again by the next one. Run returns ErrBusy when
// another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	cur, ok, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		return sum, err
	}
	if !ok {
		sum.Started = true
		return sum, battery.WriteCursor(p.CursorPath, battery.Cursor{Time: p.now()})
	}

	all, err := p.Submissions.Since(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing submissions since %s: %w", cur.Time.Format(time.RFC3339), err)
	}
	// The log is read at the first entry that needs deciding, not here, so a
	// pass with nothing waiting does not read it (gt-ipgpu).
	var asked map[string]bool
	synced := false
	fetch := func(ctx context.Context) error {
		if synced {
			return nil
		}
		if err := p.Git.Sync(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrFetch, err)
		}
		synced = true
		return nil
	}
	for _, e := range pending(all, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		// Before the first entry is decided, and so before anything is asked:
		// without the lines already there, a repeated submission cannot be told
		// from a new one.
		if asked == nil {
			asked, err = askedIn(p.LogPath)
			if err != nil {
				return sum, fmt.Errorf("reading the submission log: %w", err)
			}
		}
		key := land.SubmissionKey(e.Issue.ID, e.Work.Head)
		if asked[key] {
			sum.Repeated++
		} else {
			allowed, err := p.Allows()
			if err != nil {
				return sum, fmt.Errorf("reading today's spend: %w", err)
			}
			if !allowed {
				sum.CapReached = true
				break
			}
			line, err := p.ask(ctx, e, fetch)
			switch {
			case errors.Is(err, errUnreadable), errors.Is(err, errNoCommits), errors.Is(err, jev.ErrBadResponse):
				if p.Warnings != nil {
					fmt.Fprintf(p.Warnings, "jev submission: skipping %s at %s: %v\n", e.Issue.ID, e.Work.Head, err)
				}
				sum.Skipped++
			case err != nil:
				return sum, err
			default:
				if err := battery.Append(p.LogPath, line); err != nil {
					return sum, err
				}
				sum.Asked++
				asked[key] = true
			}
		}
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: e.Seen, ID: e.Issue.ID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// askedIn returns the bead-and-head keys the log at path holds a line for at
// the current question version. A line from an older version answered a
// different question set, so it does not name this battery's ask and does not
// count.
func askedIn(path string) (map[string]bool, error) {
	lines, err := ReadRecent(path, math.MaxInt)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(lines))
	for _, l := range lines {
		if l.QuestionVersion == Version {
			keys[land.SubmissionKey(l.Bead, l.Head)] = true
		}
	}
	return keys, nil
}

// pending keeps the entries after the cursor, oldest first, and drops an entry
// with no issue or no place in the order: a bead whose submission time is
// zero is not asked. Ties on the seen time break by bead ID, so the cursor
// orders entries completely.
func pending(all []Entry, cur battery.Cursor) []Entry {
	var out []Entry
	for _, e := range all {
		if e.Issue == nil || e.Seen.IsZero() || !after(cur, e.Seen, e.Issue.ID) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Seen.Equal(out[j].Seen) {
			return out[i].Seen.Before(out[j].Seen)
		}
		return out[i].Issue.ID < out[j].Issue.ID
	})
	return out
}

// after reports whether an entry seen at seen for bead sorts after the cursor.
func after(cur battery.Cursor, seen time.Time, bead string) bool {
	if seen.Equal(cur.Time) {
		return bead > cur.ID
	}
	return seen.After(cur.Time)
}

// errUnreadable means a submission's commits or diff could not be read, so
// the pass has no state to ask about. The read is not retried: one such
// submission must not hold up the ones behind it. The returned error wraps it
// with the cause, which the pass logs.
var errUnreadable = errors.New("submission commits or diff unreadable")

// errNoCommits means the clone read target..head and found no commits on it:
// the head is already reachable from target, so the submission landed. The
// pass skips it like an unreadable one, but its own cause keeps the log from
// reporting a landed submission as a read failure.
var errNoCommits = errors.New("no commits on the submission's range")

// ask builds the state for one entry, asks the battery and returns the line.
// The commits and stat come from the entry's record when it holds subjects;
// otherwise they come from the bare clone, which fetch syncs on first use.
// Commits or a diff the clone cannot read return errUnreadable, and the pass
// skips the entry. An entry with no commits on target..head returns
// errNoCommits and is skipped too: its head is already reachable from target,
// so it landed, and its diff against target is the change target has made
// since, not its own. A failure that ctx's end
// caused returns ctx's error, so the next pass asks again.
func (p *Pass) ask(ctx context.Context, e Entry, fetch func(context.Context) error) (Line, error) {
	subjects, numstat := e.Subjects, e.Numstat
	if len(subjects) == 0 {
		if err := fetch(ctx); err != nil {
			return Line{}, err
		}
		var err error
		subjects, numstat, err = p.readClone(ctx, e.Work)
		if err != nil {
			return Line{}, err
		}
	}

	got, err := p.Asker.Ask(ctx, buildState(e.Issue, subjects, parseNumstat(numstat)), Questions())
	if err != nil {
		return Line{}, err
	}
	return Line{
		TS:              p.now(),
		Bead:            e.Issue.ID,
		Branch:          e.Work.Branch,
		Head:            e.Work.Head,
		Target:          e.Work.Target,
		Submitted:       e.Work.Submitted,
		Agent:           agentOf(e.Issue),
		Answers:         answersFrom(got),
		QuestionVersion: Version,
		Model:           got.Model,
	}, nil
}

// readClone reads a submission's commits and stat from the bare clone. Its
// errors are errUnreadable wrapping the cause, or errNoCommits for an empty
// range, except a ctx end, which is ctx's error.
func (p *Pass) readClone(ctx context.Context, w land.Work) ([]string, string, error) {
	subjects, err := p.Git.CommitSubjects(ctx, w.Target, w.Head, maxSubjects)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, "", cerr
		}
		return nil, "", fmt.Errorf("%w: reading the commits: %w", errUnreadable, err)
	}
	if len(subjects) == 0 {
		return nil, "", fmt.Errorf("%w: %s..%s is empty, so the head already landed", errNoCommits, w.Target, w.Head)
	}
	out, err := p.Git.Numstat(ctx, w.Target, w.Head)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, "", cerr
		}
		return nil, "", fmt.Errorf("%w: reading the diff: %w", errUnreadable, err)
	}
	return subjects, out, nil
}

// fileChange is one changed file and its line counts. A binary file counts 0.
type fileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// parseNumstat reads `git diff --numstat` output into paths with counts,
// capped at maxFiles. Numstat output holds no hunk text.
func parseNumstat(out string) []fileChange {
	var files []fileChange
	for _, l := range strings.Split(out, "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		// A binary file reports "-" for both counts, which Atoi fails on and
		// leaves 0.
		added, _ := strconv.Atoi(parts[0])
		removed, _ := strconv.Atoi(parts[1])
		files = append(files, fileChange{Path: parts[2], Added: added, Removed: removed})
		if len(files) == maxFiles {
			break
		}
	}
	return files
}

// buildState returns the state for one submission: one "field: value" line
// per field, each value JSON-quoted so the model reads it as data. It holds
// the bead's title, its Goal, Acceptance and MR notes, the commit subjects and
// the changed paths with counts, and never diff content.
func buildState(issue *beads.Issue, subjects []string, files []fileChange) string {
	sections := specdispatch.Sections(beads.StripAttachmentFields(issue.Description))
	if subjects == nil {
		subjects = []string{}
	}
	if len(subjects) > maxSubjects {
		subjects = subjects[:maxSubjects]
	}
	if files == nil {
		files = []fileChange{}
	}
	var b strings.Builder
	field := func(name string, v any) { fmt.Fprintf(&b, "%s: %s\n", name, quote(v)) }
	field("bead_title", truncate(issue.Title, maxSectionRunes))
	field("bead_goal", truncate(sections["goal"], maxSectionRunes))
	field("bead_acceptance", truncate(land.CriteriaText(issue), maxSectionRunes))
	field("mr_notes", mrNotes(issue.Notes))
	field("commit_subjects", subjects)
	field("files_changed", files)
	return b.String()
}

// mrNotes is the MR notes of the live submission: the notes from its READY TO
// LAND block on, which carry whatever the polecat and the landing wrote after
// it. Earlier rejections are the previous attempts' notes and are left out.
func mrNotes(notes string) string {
	if idx := strings.LastIndex(notes, land.ReadyNoteMarker+"\n"); idx >= 0 {
		notes = notes[idx:]
	}
	return truncate(strings.TrimSpace(notes), maxNotesRunes)
}

// agentOf is the agent the bead names: the agent its agent: label pins, else
// the agent attached var, else empty. The label is read the way the dispatcher
// reads it (PinnedAgents), so the record names the agent the dispatcher seats.
// A bead that wears several agent: labels, or a blank one, names none, as the
// dispatcher seats no such bead (ChooseSeat).
func agentOf(b *beads.Issue) string {
	if pinned := (specdispatch.Spec{Labels: b.Labels}).PinnedAgents(); len(pinned) > 0 {
		if len(pinned) > 1 {
			return ""
		}
		return pinned[0]
	}
	if fields := beads.ParseAttachmentFields(b); fields != nil {
		for _, v := range fields.AttachedVars {
			if name, ok := strings.CutPrefix(v, agentVar+"="); ok {
				return strings.TrimSpace(name)
			}
		}
	}
	return ""
}

// quote returns v as JSON on one line. It leaves <, > and & as they are, which
// the default encoder would turn into \u escapes.
func quote(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Every value passed is a string, a number or a slice of plain structs.
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

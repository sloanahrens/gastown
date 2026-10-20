// Package submission is the advisory battery on work waiting to land. A pass
// takes the gastown work beads submitted since the last pass, asks Jev one
// battery about each, and appends one line per submission to a log that
// `gt jev submission --recent` prints. It never comments on a bead, files an
// attention item, blocks a landing or writes under .runtime/landings: the log
// is the only thing it writes.
//
// The pass reads a bare clone it owns, at ClonePath, so it never runs git in a
// town worktree and never contends with a landing for the rig's repository.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the bead
// list, the git reads and the Jev client, so a test runs a pass with fakes.
// Open wires the production versions.
package submission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// maxSubjects caps the commit subjects listed in the state.
	maxSubjects = 20

	// maxFiles caps the changed files listed in the state.
	maxFiles = 80

	// agentLabelPrefix starts the label that pins a bead to one agent, and
	// agentVar is the attached_vars key the dispatcher stamps with the seat's
	// agent.
	agentLabelPrefix = specdispatch.AgentLabelPrefix
	agentVar         = "agent"
)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Submissions lists the work beads that carry a submission since t: those
// still waiting to land, and those closed since t. A pass only reads them.
type Submissions interface {
	Since(t time.Time) ([]*beads.Issue, error)
}

// Git reads a submission's commits and diff stat from the bare clone the pass
// owns. Every command it runs runs in that clone, never in a town worktree.
type Git interface {
	// Sync creates the clone on the first run and fetches refs/heads/* into
	// it. Its failure skips the pass and leaves the cursor unmoved.
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

// Run does one pass. Submissions are taken oldest first, and each is logged
// and the cursor advanced before the next, so a pass stopped partway never
// asks about a submission twice. A submission whose Ask fails for any reason
// but an unusable response stops the pass with that error and is asked again
// by the next one. Run returns ErrBusy when another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	if err := p.Git.Sync(ctx); err != nil {
		return sum, fmt.Errorf("%w: %w", ErrFetch, err)
	}

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
	for _, sub := range pending(all, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, sub)
		switch {
		case errors.Is(err, errUnreadable), errors.Is(err, jev.ErrBadResponse):
			sum.Skipped++
		case err != nil:
			return sum, err
		default:
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			sum.Asked++
		}
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: sub.work.Submitted, ID: sub.issue.ID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// submission is one work bead and the landing request its notes carry.
type submission struct {
	issue *beads.Issue
	work  land.Work
}

// pending keeps the submissions after the cursor, oldest first. A bead whose
// READY TO LAND block carries no Submitted stamp has no place in the order,
// so it is not asked: every block gt done writes carries one. Ties on the
// submission time break by bead ID, so the cursor orders submissions
// completely.
func pending(all []*beads.Issue, cur battery.Cursor) []submission {
	var out []submission
	for _, issue := range all {
		if issue == nil {
			continue
		}
		w, ok := land.ParseReadyNote(issue.Notes)
		if !ok || w.Submitted.IsZero() || !after(cur, w.Submitted, issue.ID) {
			continue
		}
		out = append(out, submission{issue: issue, work: w})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].work.Submitted.Equal(out[j].work.Submitted) {
			return out[i].work.Submitted.Before(out[j].work.Submitted)
		}
		return out[i].issue.ID < out[j].issue.ID
	})
	return out
}

// after reports whether a submission at submitted for bead sorts after the
// cursor.
func after(cur battery.Cursor, submitted time.Time, bead string) bool {
	if submitted.Equal(cur.Time) {
		return bead > cur.ID
	}
	return submitted.After(cur.Time)
}

// errUnreadable means a submission's commits or diff could not be read, so
// the pass has no state to ask about. The read is not retried: one such
// submission must not hold up the ones behind it.
var errUnreadable = errors.New("submission commits or diff unreadable")

// ask builds the state for one submission, asks the battery and returns the
// line. Commits or a diff the clone cannot read return errUnreadable, and the
// pass skips the submission. So does a submission with no commits on
// target..head: its head is already reachable from target, so it landed, and
// its diff against target is the change target has made since, not its own.
// A failure that ctx's end caused returns ctx's error, so the next pass asks
// again.
func (p *Pass) ask(ctx context.Context, sub submission) (Line, error) {
	subjects, err := p.Git.CommitSubjects(ctx, sub.work.Target, sub.work.Head, maxSubjects)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		return Line{}, errUnreadable
	}
	if len(subjects) == 0 {
		return Line{}, errUnreadable
	}
	out, err := p.Git.Numstat(ctx, sub.work.Target, sub.work.Head)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		return Line{}, errUnreadable
	}

	got, err := p.Asker.Ask(ctx, buildState(sub.issue, subjects, parseNumstat(out)), Questions())
	if err != nil {
		return Line{}, err
	}
	return Line{
		TS:              p.now(),
		Bead:            sub.issue.ID,
		Branch:          sub.work.Branch,
		Head:            sub.work.Head,
		Target:          sub.work.Target,
		Submitted:       sub.work.Submitted,
		Agent:           agentOf(sub.issue),
		Answers:         answersFrom(got),
		QuestionVersion: Version,
		Model:           got.Model,
	}, nil
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
// the bead's title, its Goal and Acceptance sections, the commit subjects and
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
	field("commit_subjects", subjects)
	field("files_changed", files)
	return b.String()
}

// agentOf is the agent the bead names: the first agent: label if it has one,
// else the agent attached var, else empty.
func agentOf(b *beads.Issue) string {
	for _, l := range b.Labels {
		if name, ok := strings.CutPrefix(l, agentLabelPrefix); ok && name != "" {
			return name
		}
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

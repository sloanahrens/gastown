// Package rework is the refused-review cause battery (gt-gop4g). When a
// landing's om review refuses the work, this pass asks Jev two things about the
// refusal: why it failed, and whether the landing will need a rework commit
// before it lands. One line per refused review is appended to the log, and
// nothing else is written.
//
// A refusal is the review log's own record: an om verdict the landing refused
// the work on, either request_changes or an approval whose findings blocked it.
// The findings are read from the work bead's MERGE REJECTION block for that
// attempt, and the changed files from a numstat diff of the refused head
// against its target, so the question is put to the verdict the landing was
// actually refused on.
//
// The pass never changes a landing: it reads the review log, the beads and the
// rig's repository, and writes only its log and cursor. The caller supplies
// those reads and the Jev client, so a test runs a pass on fixtures. Open wires
// the production versions.
package rework

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-rework"

	// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap
	// is town-wide, as for every Jev battery.
	DefaultMaxUSDPerDay = 1.0

	// maxFindingsRunes caps the findings the state quotes.
	maxFindingsRunes = 3000

	// maxFiles caps the changed files listed in the state.
	maxFiles = 60

	// Version identifies the question set. It is written to every log line, so
	// an answer can be told from one produced by different wording. Bump it
	// when a question id, its text, its options or a level changes.
	Version = 1
)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Reviews lists the town's om verdicts at or after t, oldest first.
// *land.ReviewsFile satisfies it.
type Reviews interface {
	Since(t time.Time) ([]land.ReviewRecord, error)
}

// Rigs reads the rig a refused landing belongs to: the work bead's notes, and
// the files the refused submission changed.
type Rigs interface {
	// Notes returns the work bead's notes. A bead that does not exist is
	// beads.ErrNotFound.
	Notes(rig, bead string) (string, error)
	// Numstat returns `git diff --numstat target...head` in the rig's
	// repository: the files the refused head changed against its target.
	Numstat(ctx context.Context, rig, target, head string) (string, error)
}

// Pass is one run of the rework battery, with every dependency injected.
type Pass struct {
	Asker   Asker
	Reviews Reviews
	Rigs    Rigs

	// LogPath is the log every answered refusal appends to.
	LogPath string
	// CursorPath remembers the last refusal a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The
	// pass asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Warnf reports a read the pass worked around. Nil drops it.
	Warnf func(format string, args ...any)
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of refusals that got a log line this pass.
	Asked int
	// Skipped is the number of refusals passed over with no line: the bead or
	// its rejection block could not be read, so there is no verdict to judge,
	// or Jev returned an unusable response for them. Skipping means one bad
	// read cannot hold up the refusals behind it.
	Skipped int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The refusals it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time, so the
	// town's past refusals are not billed.
	Started bool
}

// Run does one pass. Refusals are taken oldest first, and each is logged and
// the cursor advanced before the next, so a pass stopped partway never asks
// about a refusal twice. A refusal whose Ask fails for any reason but an
// unusable response stops the pass with that error and is asked again by the
// next one. Run returns ErrBusy when another pass holds the run lock.
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

	all, err := p.Reviews.Since(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing the om reviews since %s: %w", cur.Time.Format(time.RFC3339), err)
	}
	for _, rec := range pending(all, cur) {
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
		line, err := p.ask(ctx, rec)
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
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: rec.At, ID: failID(rec)}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// refused reports whether rec is an om review the landing refused the work on.
// A request_changes verdict is a refusal with or without the record's flag,
// and an approval carrying the flag was refused on its findings alone
// (gt-vu18r).
func refused(rec land.ReviewRecord) bool {
	return rec.Rejected || rec.Verdict == land.VerdictRequestChanges
}

// failID names one refused review: its bead and attempt, so two attempts of
// one bead are two items.
func failID(rec land.ReviewRecord) string {
	return rec.BeadID + "#" + strconv.Itoa(rec.Attempt)
}

// pending keeps the refused reviews after the cursor, oldest first. Ties on
// the review time break by ID, so the cursor orders refusals completely.
func pending(all []land.ReviewRecord, cur battery.Cursor) []land.ReviewRecord {
	var out []land.ReviewRecord
	for _, r := range all {
		if !refused(r) || r.At.IsZero() || !after(cur, r.At, failID(r)) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return failID(out[i]) < failID(out[j])
	})
	return out
}

// after reports whether a review at at for id sorts after the cursor.
func after(cur battery.Cursor, at time.Time, id string) bool {
	if at.Equal(cur.Time) {
		return id > cur.ID
	}
	return at.After(cur.Time)
}

// errUnreadable means a refusal's bead path could not be read: no notes, or no
// MERGE REJECTION block for the attempt, so there is no verdict to judge. The
// read is not retried, so one bad bead cannot hold up the refusals behind it.
var errUnreadable = errors.New("refused review's bead or rejection block unreadable")

// ask reads one refusal and asks Jev about it. A bead that does not exist, or
// whose notes hold no rejection block for the attempt, returns errUnreadable
// and the pass skips the refusal. Any other bead read error is infrastructure,
// such as a Dolt outage: it is returned wrapped, so the pass stops before the
// cursor moves and the next pass asks again. A diff that fails leaves the files
// out of the state and the question is asked on the findings alone: the files
// are a hint, and a refusal is always worth its line.
func (p *Pass) ask(ctx context.Context, rec land.ReviewRecord) (Line, error) {
	notes, err := p.Rigs.Notes(rec.Rig, rec.BeadID)
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return Line{}, fmt.Errorf("reading %s's notes: %w", rec.BeadID, err)
	}
	if err != nil {
		return Line{}, errUnreadable
	}
	rej, ok := rejectionForAttempt(notes, rec.Attempt)
	if !ok {
		return Line{}, errUnreadable
	}
	files, err := p.files(ctx, rec.Rig, rej.Target, rej.Head)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		p.warnf("%s: reading the refused submission's files: %v; asking about the findings alone", rec.BeadID, err)
	}

	got, err := p.Asker.Ask(ctx, buildState(rec, rej, files), Questions())
	if err != nil {
		return Line{}, err
	}
	if _, ok := got.Choice[QCause]; !ok {
		return Line{}, fmt.Errorf("%w: no %s answer", jev.ErrBadResponse, QCause)
	}
	return Line{
		TS:              p.now(),
		Refused:         rec.At,
		Rig:             rec.Rig,
		Bead:            rec.BeadID,
		Attempt:         rec.Attempt,
		Head:            rej.Head,
		Verdict:         rec.Verdict,
		Score:           rec.Score,
		Answers:         answersFrom(got),
		Model:           got.Model,
		QuestionVersion: Version,
	}, nil
}

// files reads the changed files of the refused head. A rejection that names no
// target or head has none to read, which is not an error.
func (p *Pass) files(ctx context.Context, rig, target, head string) ([]fileChange, error) {
	if target == "" || head == "" {
		return nil, nil
	}
	out, err := p.Rigs.Numstat(ctx, rig, target, head)
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *Pass) warnf(format string, args ...any) {
	if p.Warnf != nil {
		p.Warnf(format, args...)
	}
}

// rejectionHeaderRE finds the header line of each MERGE REJECTION block, so the
// notes can be cut into one block per attempt.
var rejectionHeaderRE = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(land.MergeRejectionNoteMarker) + ` \(attempt [0-9]+\): `)

// rejectionForAttempt returns the MERGE REJECTION block that opened attempt in
// notes. land's parser reads the last block of the text it is given, so each
// block is cut out up to the next header before it is parsed.
func rejectionForAttempt(notes string, attempt int) (land.RejectionNote, bool) {
	starts := rejectionHeaderRE.FindAllStringIndex(notes, -1)
	var found land.RejectionNote
	ok := false
	for i, s := range starts {
		end := len(notes)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		rej, parsed := land.ParseRejectionNote(notes[s[0]:end])
		if parsed && rej.Attempt == attempt {
			found, ok = rej, true
		}
	}
	return found, ok
}

// fileChange is one changed file and its line counts. A binary file counts 0.
type fileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// parseNumstat reads `git diff --numstat` output into paths with counts, capped
// at maxFiles. Numstat output holds no hunk text.
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

// state is what Jev reads about one refused review. Every value is quoted JSON
// data, and the instructions say so.
type state struct {
	Rig      string       `json:"rig"`
	Bead     string       `json:"bead"`
	Attempt  int          `json:"attempt"`
	Verdict  string       `json:"verdict"`
	Score    float64      `json:"score"`
	Findings string       `json:"findings"`
	Files    []fileChange `json:"files"`
}

// buildState returns the state for one refusal: om's verdict and score as the
// review log recorded them, the rejection's findings, and the files the refused
// head changed. The findings are cut to maxFindingsRunes from the start.
func buildState(rec land.ReviewRecord, rej land.RejectionNote, files []fileChange) state {
	if files == nil {
		files = []fileChange{}
	}
	return state{
		Rig:      rec.Rig,
		Bead:     rec.BeadID,
		Attempt:  rec.Attempt,
		Verdict:  rec.Verdict,
		Score:    rec.Score,
		Findings: truncate(formatFindings(rej.Findings), maxFindingsRunes),
		Files:    files,
	}
}

// formatFindings renders one line per finding: its severity, where it is, and
// what it says. A finding with no severity or no place still names what it
// says.
func formatFindings(findings []land.Finding) string {
	var b strings.Builder
	for _, f := range findings {
		var parts []string
		if sev := strings.TrimSpace(f.Severity); sev != "" {
			parts = append(parts, sev)
		}
		where := strings.TrimSpace(f.Path)
		if f.Line > 0 {
			where += ":" + strconv.Itoa(f.Line)
		}
		if where != "" {
			parts = append(parts, where)
		}
		line := strings.Join(parts, " ")
		if title := strings.TrimSpace(f.Title); title != "" {
			if line != "" {
				line += " — "
			}
			line += title
		}
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

// answersFrom reads the cause answer out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	cause := got.Choice[QCause]
	return map[string]Answer{
		QCause: {Choice: cause.Choice, Probabilities: cause.Probabilities},
	}
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

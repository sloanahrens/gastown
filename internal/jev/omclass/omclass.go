// Package omclass is the lesson-class battery (gt-rpuu4s). The weekly lessons
// pass counts om's findings into classes by keyword (internal/omlessons). This
// battery asks Jev, for each new finding, which of those classes it belongs to,
// and logs the answer beside the keyword class, so the scorecard can say how
// often Jev agrees with the keywords and where it finds a class they miss.
//
// The battery acts on nothing. It reads the review log and writes only its log
// and cursor; no answer feeds back into a landing, a class table or a bead.
//
// The caller supplies the reads and the Jev client, so a test runs a pass on
// fixtures. Open wires the production versions.
package omclass

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-omclass"

	// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap
	// is town-wide, as for every Jev battery.
	DefaultMaxUSDPerDay = 1.0

	// MaxFindingsPerRun caps the findings one pass takes, asked or skipped. The
	// findings past it wait for the next pass.
	MaxFindingsPerRun = 300

	// MaxAskAttempts bounds the asks one finding gets. A finding whose ask fails
	// this many passes running is logged with a Failure and no answer, and the
	// cursor moves past it, so one bad finding costs a bounded number of passes.
	MaxAskAttempts = 3

	// Version identifies the question set. It is written to every log line, so
	// an answer can be told from one produced by different wording. Bump it
	// when a question id, its text, its options or a level changes.
	Version = 1
)

// ErrBusy means another pass holds the run lock: a manual pass and the
// daemon's pass share one cursor, so they take turns.
var ErrBusy = errors.New("another jev omclass pass is running")

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Reviews lists the town's om review records at or after t, oldest first.
// *land.ReviewsFile satisfies it.
type Reviews interface {
	Since(t time.Time) ([]land.ReviewRecord, error)
}

// Pass is one run of the battery, with every dependency injected.
type Pass struct {
	Asker   Asker
	Reviews Reviews

	// LogPath is the log every asked finding appends to.
	LogPath string
	// CursorPath remembers the last finding a pass took.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of findings that got a log line this pass.
	Asked int
	// Skipped is the number of findings taken with no line: a finding with no
	// title or claim has nothing for Jev to read, and Jev returned no usable
	// choice for the others. The cursor moves past a skipped finding.
	Skipped int
	// Failed is the number of findings the pass gave up on after MaxAskAttempts
	// failed asks. Each has a line with a Failure and no answer.
	Failed int
	// CapReached is true when the pass stopped because the day's cap was spent.
	CapReached bool
	// Capped is true when the pass stopped at MaxFindingsPerRun with findings
	// still waiting.
	Capped bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time, so the
	// town's past findings are not billed.
	Started bool
}

// Line is one line of the omclass log: what Jev answered about one finding,
// beside the keyword class the table gives it. It holds ids, times and answers,
// never the finding's text.
type Line struct {
	// TS is when the pass asked about the finding.
	TS time.Time `json:"ts"`
	// At is when the review log recorded the review the finding came from.
	At time.Time `json:"at"`
	// Rig, Bead and Attempt name the review the finding belongs to, as the
	// review log counts it.
	Rig     string `json:"rig"`
	Bead    string `json:"bead"`
	Attempt int    `json:"attempt"`
	// Finding is the finding's index in its review's finding text.
	Finding int `json:"finding"`
	// Keyword is the class the table's keywords give the finding: a class id,
	// or none when no class matches.
	Keyword string `json:"keyword"`
	// Answers maps each question id to its answer.
	Answers map[string]Answer `json:"answers"`
	// Failure is set on a line for a finding the pass gave up on after
	// MaxAskAttempts failed asks. It names the kind of failure, never the reply
	// text, and such a line has no answer.
	Failure string `json:"failure,omitempty"`
	// Model is the versioned Jev model that answered.
	Model string `json:"model"`
	// QuestionVersion is Version when the line was logged.
	QuestionVersion int `json:"question_version"`
}

// Answer is Jev's answer to one choice question.
type Answer struct {
	// Choice is the option Jev picked.
	Choice string `json:"choice,omitempty"`
	// Probabilities maps every option of the choice to its probability.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// LogPath is the omclass log, one JSON line per asked finding.
func LogPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "omclass.jsonl")
}

// CursorPath is the file that remembers the last finding a pass took.
func CursorPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "omclass.cursor.json")
}

// Keyword returns the class id the keyword table gives f, or NoneLabel when no
// row matches it. It reads the table through omlessons.Classify, the function
// the lessons command counts with, so the label and the count agree.
func Keyword(f land.ReviewFindingText) string {
	id := omlessons.Classify(f)
	if id == omlessons.Unclassified {
		return NoneLabel
	}
	return id
}

// item is one finding the pass may take: its review, its place in the review's
// finding text, and the cursor id that names it.
type item struct {
	at      time.Time
	id      string
	rec     land.ReviewRecord
	index   int
	finding land.ReviewFindingText
}

// cursorState is what a pass saves: the cursor, and how many asks of the finding
// just past it have failed. Failing names that finding and Tries counts its
// failed asks. A cursor that moves on drops both.
type cursorState struct {
	battery.Cursor
	Failing string `json:"failing,omitempty"`
	Tries   int    `json:"tries,omitempty"`
}

// triesOf returns how many asks of finding id have failed so far.
func (s cursorState) triesOf(id string) int {
	if s.Failing != id {
		return 0
	}
	return s.Tries
}

// Run does one pass. Findings are taken oldest first, and each is logged and the
// cursor advanced before the next, so a pass stopped partway never asks about a
// finding twice. A finding whose Ask fails with no reply stops the pass with
// that error, and the next pass asks about it again, up to MaxAskAttempts; the
// attempt that reaches the bound logs the finding with a Failure and moves past
// it. A stopped run does not count against the finding. Run returns ErrBusy when
// another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	var st cursorState
	ok, err := battery.ReadState(p.CursorPath, &st)
	if err != nil {
		return sum, err
	}
	if !ok {
		sum.Started = true
		return sum, p.advance(battery.Cursor{Time: p.now()})
	}

	all, err := p.Reviews.Since(st.Time)
	if err != nil {
		return sum, fmt.Errorf("listing the om reviews since %s: %w", st.Time.Format(time.RFC3339), err)
	}
	for _, it := range pending(all, st.Cursor) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if sum.Asked+sum.Skipped+sum.Failed >= MaxFindingsPerRun {
			sum.Capped = true
			break
		}
		next := battery.Cursor{Time: it.at, ID: it.id}
		if strings.TrimSpace(it.finding.Title) == "" && strings.TrimSpace(it.finding.Claim) == "" {
			sum.Skipped++
			if err := p.advance(next); err != nil {
				return sum, err
			}
			st = cursorState{Cursor: next}
			continue
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, it)
		switch {
		case errors.Is(err, jev.ErrBadResponse):
			sum.Skipped++
		case err != nil && ctx.Err() != nil:
			// The pass was stopped, by the E-stop or its run budget. That is not
			// the finding failing, so the count is left alone.
			return sum, err
		case err != nil:
			tries := st.triesOf(it.id) + 1
			if tries < MaxAskAttempts {
				// Keep the count on the cursor and stop: the next pass asks again.
				if serr := battery.WriteState(p.CursorPath, cursorState{Cursor: st.Cursor, Failing: it.id, Tries: tries}); serr != nil {
					return sum, serr
				}
				return sum, fmt.Errorf("asking about %s: %w", it.id, err)
			}
			if err := battery.Append(p.LogPath, it.failedLine(p.now(), err)); err != nil {
				return sum, err
			}
			sum.Failed++
		default:
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			sum.Asked++
		}
		if err := p.advance(next); err != nil {
			return sum, err
		}
		st = cursorState{Cursor: next}
	}
	return sum, nil
}

// advance saves the cursor at c, with no failed asks counted against the
// finding after it.
func (p *Pass) advance(c battery.Cursor) error {
	return battery.WriteState(p.CursorPath, cursorState{Cursor: c})
}

// findingID names one finding: its bead, its attempt and its index in the
// review's finding text. The index is zero-padded, so the ids of one review
// sort in index order and the cursor orders them completely.
func findingID(rec land.ReviewRecord, index int) string {
	return fmt.Sprintf("%s#%d/%04d", rec.BeadID, rec.Attempt, index)
}

// pending keeps the findings after the cursor, oldest first. A queue line is the
// follow-up feed's record, not a verdict, so the pass skips it.
//
// The pass assumes the log is in At order: a verdict whose At falls before the
// cursor is taken as read. The landing worker stamps At just before it appends,
// and two landings can interleave between those two steps, so a verdict stamped
// before the cursor and appended after the pass that moved it is skipped. The
// window is short but not zero. The cursor has no sequence number to key on,
// because ReviewRecord carries none.
func pending(all []land.ReviewRecord, cur battery.Cursor) []item {
	var out []item
	for _, rec := range all {
		if rec.Queue != nil || rec.At.IsZero() {
			continue
		}
		for i, f := range rec.FindingText {
			it := item{at: rec.At, id: findingID(rec, i), rec: rec, index: i, finding: f}
			if !after(cur, it.at, it.id) {
				continue
			}
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		return out[i].id < out[j].id
	})
	return out
}

// after reports whether a finding at at with id sorts after the cursor.
func after(cur battery.Cursor, at time.Time, id string) bool {
	if at.Equal(cur.Time) {
		return id > cur.ID
	}
	return at.After(cur.Time)
}

// ask asks Jev which class one finding belongs to, and builds its log line.
// A reply with no choice is jev.ErrBadResponse, so the pass skips the finding
// rather than log an answer it does not have.
func (p *Pass) ask(ctx context.Context, it item) (Line, error) {
	got, err := p.Asker.Ask(ctx, buildState(it), Questions())
	if err != nil {
		return Line{}, err
	}
	ans, ok := got.Choice[QRuleClass]
	if !ok || ans.Choice == "" {
		return Line{}, fmt.Errorf("%w: no %s answer", jev.ErrBadResponse, QRuleClass)
	}
	l := it.line(p.now())
	l.Answers = map[string]Answer{QRuleClass: {Choice: ans.Choice, Probabilities: ans.Probabilities}}
	l.Model = got.Model
	return l, nil
}

// line is the log line for it before any answer: the ids and keyword of the
// finding, and the time the pass wrote it.
func (it item) line(ts time.Time) Line {
	return Line{
		TS:              ts,
		At:              it.at,
		Rig:             it.rec.Rig,
		Bead:            it.rec.BeadID,
		Attempt:         it.rec.Attempt,
		Finding:         it.index,
		Keyword:         Keyword(it.finding),
		QuestionVersion: Version,
	}
}

// failedLine is the log line for a finding the pass gave up on: the line before
// any answer, with the kind of failure and no answer.
func (it item) failedLine(ts time.Time, err error) Line {
	l := it.line(ts)
	l.Failure = failureKind(err)
	return l
}

// failureKind names why Jev gave no reply, without the reply's own text. A
// server's error body can quote anything, and the log holds no captured output.
func failureKind(err error) string {
	var se *jev.StatusError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("jev answered status %d", se.Status)
	case errors.Is(err, jev.ErrTimeout):
		return "jev timed out"
	default:
		return "jev call failed"
	}
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// state is what Jev reads about one finding: the file it names and its text.
// Every value is quoted JSON data, and the instructions say so.
type state struct {
	Rig     string `json:"rig"`
	Bead    string `json:"bead"`
	Attempt int    `json:"attempt"`
	File    string `json:"file"`
	Title   string `json:"title"`
	Claim   string `json:"claim"`
}

func buildState(it item) state {
	return state{
		Rig:     it.rec.Rig,
		Bead:    it.rec.BeadID,
		Attempt: it.rec.Attempt,
		File:    it.finding.File,
		Title:   it.finding.Title,
		Claim:   it.finding.Claim,
	}
}

// lockRun takes the pass's run lock without waiting, and returns the release.
// The lock sits beside the cursor, so every pass over one cursor contends on
// it, in this process and in others.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(cursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}

// Package failcause is the failing-test cause battery (gt-ugrv0). When a
// landing's candidate gate goes red on a Go test, this battery reads the
// gate's failure excerpt and asks Jev two things about each failing test: will
// it fail again on another landing within fourteen days, and what cause fits
// the excerpt. One line per failing test goes to the log, and nothing else is
// written.
//
// The battery never changes the gate's verdict. Shadow wraps the rig's
// candidate gate, returns the gate's result untouched, and judges the red
// verdict beside it. A failing test is never retried, skipped or excused: no
// answer here feeds back into the landing.
//
// The pass does I/O only through its log. The caller supplies the Jev asker and
// the spend check, so a test runs a pass with an httptest Jev and a fixture
// excerpt. Open wires the production versions.
package failcause

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	// Caller names the battery's Jev calls in the usage ledger.
	Caller = "jev-failcause"

	// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap
	// is town-wide, as for every Jev battery.
	DefaultMaxUSDPerDay = 1.0

	// maxExcerptRunes caps the failure excerpt Jev reads, cut from the start.
	maxExcerptRunes = 3000

	// defaultAskTimeout bounds the one Jev call a failing test makes.
	defaultAskTimeout = 2 * time.Minute

	// Version identifies the question set. It is written to every log line, so
	// an answer can be told from one produced by different wording. Bump it
	// when a question id, its text, its options or a level changes.
	Version = 1
)

// ErrCapReached reports that the day's Jev spend left no room for another ask,
// so the failing tests after the stop were not judged.
var ErrCapReached = errors.New("the day's Jev spend cap was reached")

// ErrSpendUnreadable reports that the day's Jev spend could not be read, so no
// failing test was judged: an unreadable cap cannot be spent against.
var ErrSpendUnreadable = errors.New("the day's Jev spend could not be read")

// judgeSlots lets one judgement ask Jev at a time in the process. Every
// judgement reads the day's spend and then spends against it, so two at once
// can both read a cap under budget and both cross it. A red verdict that
// arrives while another is being judged waits its turn on a goroutine holding
// nothing but its own Landing, so the paid calls are what the slot bounds; the
// landing never waits, because the judging is off its goroutine either way
// (gt-xlv27).
var judgeSlots = make(chan struct{}, 1)

// Landing is a red candidate verdict the battery judges: the bead whose
// candidate it gated, the rig, and the gate's failure excerpt.
type Landing struct {
	Bead string
	Rig  string
	// Tail is the excerpt the landing already extracted from the gate's job
	// log (land.CandidateResult.Tail).
	Tail string
}

// Pass is the battery with every dependency injected.
type Pass struct {
	Asker Asker

	// LogPath is the log every failing test appends its line to.
	LogPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// AskTimeout bounds each Jev call. Zero means defaultAskTimeout.
	AskTimeout time.Duration

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Judge asks Jev about each Go test the landing's excerpt names as failing,
// appending one line per test and leaving the tests after a spend stop
// unjudged and unwritten, which ErrCapReached or ErrSpendUnreadable reports. An
// excerpt naming no test asks and writes nothing, and a Jev call with no usable
// reply ends the judging with an error and no line for that test, so an outage
// leaves no record behind and the next red verdict is judged afresh.
func (p *Pass) Judge(ctx context.Context, l Landing) error {
	tests := land.ParseTestFailures(l.Tail)
	if len(tests) == 0 {
		return nil
	}
	now := p.now()
	excerpt := truncate(l.Tail, maxExcerptRunes)
	for _, t := range tests {
		allowed, err := p.Allows()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrSpendUnreadable, err)
		}
		if !allowed {
			return ErrCapReached
		}
		got, err := p.ask(ctx, state{
			Rig:     l.Rig,
			Bead:    l.Bead,
			Package: t.Package,
			Test:    t.Test,
			Excerpt: excerpt,
		})
		if err != nil {
			return fmt.Errorf("asking about %s %s: %w", t.Package, t.Test, err)
		}
		line := Line{
			TS:              now,
			Rig:             l.Rig,
			Bead:            l.Bead,
			Package:         t.Package,
			Test:            t.Test,
			Answers:         answersFrom(got),
			Model:           got.Model,
			QuestionVersion: Version,
		}
		if err := battery.Append(p.LogPath, line); err != nil {
			return fmt.Errorf("writing the failcause log: %w", err)
		}
	}
	return nil
}

func (p *Pass) ask(ctx context.Context, s state) (*jev.Answers, error) {
	askCtx, cancel := context.WithTimeout(ctx, p.askTimeout())
	defer cancel()
	got, err := p.Asker.Ask(askCtx, s, Questions())
	if err != nil {
		return nil, err
	}
	if _, ok := got.Noul[QWillRecur]; !ok {
		return nil, fmt.Errorf("%w: no %s answer", jev.ErrBadResponse, QWillRecur)
	}
	if _, ok := got.Choice[QCause]; !ok {
		return nil, fmt.Errorf("%w: no %s answer", jev.ErrBadResponse, QCause)
	}
	return got, nil
}

func (p *Pass) askTimeout() time.Duration {
	if p.AskTimeout == 0 {
		return defaultAskTimeout
	}
	return p.AskTimeout
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// state is what Jev reads about one failing test. Every value is quoted JSON
// data, and the instructions say so.
type state struct {
	Rig     string `json:"rig"`
	Bead    string `json:"bead"`
	Package string `json:"package"`
	Test    string `json:"test"`
	Excerpt string `json:"excerpt"`
}

// answersFrom reads the two answers out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	willRecur := got.Noul[QWillRecur]
	cause := got.Choice[QCause]
	return map[string]Answer{
		QWillRecur: {Noul: &willRecur},
		QCause:     {Choice: cause.Choice, Probabilities: cause.Probabilities},
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

// Shadow is the rig's candidate gate with the battery beside it. Run returns
// the gate's result exactly as the gate produced it. A red verdict with an
// excerpt is handed to the battery, which runs in the background, one
// judgement at a time, unless Async is set to run it inline.
type Shadow struct {
	// Gate is the rig's candidate gate, already wrapped by any battery that
	// sits beside this one.
	Gate land.Candidate
	// Pass is the battery.
	Pass *Pass
	// Rig is the rig the landing belongs to.
	Rig string
	// Logf reports what the battery could not do, a failed judgement or a spend
	// stop. It never changes the landing.
	Logf func(format string, args ...any)
	// Async runs the judging. Nil runs it on its own goroutine, so the landing
	// does not wait on Jev; a test sets it to run inline.
	Async func(call func())
}

// Run is the gate's run with the battery beside it.
func (s Shadow) Run(ctx context.Context, wt land.Repo, dir string, w land.Work, head string) land.CandidateResult {
	res := s.Gate.Run(ctx, wt, dir, w, head)
	if res.State != land.CandidateFailed || res.Tail == "" {
		return res
	}
	l := Landing{Bead: w.BeadID, Rig: s.Rig, Tail: res.Tail}
	// The landing's own context ends when the landing does, and the judging
	// may outlive it, so it runs on a context that cannot be canceled.
	judgeCtx := context.WithoutCancel(ctx)
	s.run(func() {
		err := s.Pass.Judge(judgeCtx, l)
		switch {
		case err == nil:
		case errors.Is(err, ErrCapReached):
			s.logf("%s: %v, so the remaining failing tests were not judged", w.BeadID, err)
		case errors.Is(err, ErrSpendUnreadable):
			s.logf("%s: %v, so no failing test was judged", w.BeadID, err)
		default:
			s.logf("%s: the failing-test cause was not logged: %v", w.BeadID, err)
		}
	})
	return res
}

// Discard is the gate's own, so a landing that ends without merging still
// deletes its candidate branch.
func (s Shadow) Discard(ctx context.Context, w land.Work, res land.CandidateResult) {
	s.Gate.Discard(ctx, w, res)
}

// run hands the judging to Async, or starts it on a goroutine holding the one
// judge slot. Async owns the scheduling, which is how a test runs the judging
// inline.
func (s Shadow) run(call func()) {
	if s.Async != nil {
		s.Async(call)
		return
	}
	go func() {
		judgeSlots <- struct{}{}
		defer func() { <-judgeSlots }()
		call()
	}()
}

func (s Shadow) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf("jev_failcause: "+format, args...)
	}
}

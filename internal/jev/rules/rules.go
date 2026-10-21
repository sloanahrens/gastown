// Package rules is the standing-rules battery: an advisory pass, like the
// thrash battery. A pass takes each submission gt done made for landing since
// the last pass, reads the tail of the session that submitted it, asks Jev
// whether that session broke a standing rule and which rule, and appends one
// line per submission to a log. It never edits, labels, holds, blocks or
// punishes a seat or a bead: the log is the only thing it writes.
//
// The transcript is read only through the progress battery's containment-
// checked reader (progress.TranscriptTail). A transcript that reader refuses
// gets no line at all, so nothing outside the town is ever logged. The line
// holds the bead, the polecat, the time, a SHA-256 of the excerpt and the
// answers. No excerpt text reaches the log.
//
// The scorecard adapter in internal/jev/scorecard labels each line afterwards
// from the guard records the town already keeps. A rule with no guard record
// stays unlabeled.
//
// The pass owns no I/O beyond its log and its cursor. The caller supplies the
// Jev client, the submission list, the worktree lookup and the spend, so a test
// runs a pass with fakes and no network. Open wires the production versions.
package rules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/progress"
)

const (
	// Caller names the pass's Jev calls in the usage ledger, so its spend can be
	// told apart from the other jobs that call Jev.
	Caller = "jev-rules"

	// excerptRunes caps the transcript tail Jev reads.
	excerptRunes = 4000
)

// ErrBusy means another pass holds the run lock.
var ErrBusy = errors.New("another jev rules pass is running")

// errRefused means the containment check refused a session's transcript, so
// the session gets no line.
var errRefused = errors.New("the containment check refused the transcript")

// errNoTranscript means the seat has no worktree, or no transcript in it.
var errNoTranscript = errors.New("no transcript for the seat")

// Asker asks one battery. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Done is one submission gt done made for landing: the bead, its rig, the
// polecat that submitted it and when.
type Done struct {
	Rig     string
	Bead    string
	Polecat string
	// SubmittedAt is the Submitted stamp of the bead's live READY TO LAND block.
	SubmittedAt time.Time
}

// Lister lists the submissions a pass considers. A pass only reads them.
type Lister interface {
	// Done lists the submissions made at or after t, including those whose
	// beads closed since t, so a submission that landed between two passes is
	// still asked about.
	Done(t time.Time) ([]Done, error)
}

// State is what Jev reads: the excerpt of the session's transcript, nothing
// else.
type State struct {
	Excerpt string `json:"excerpt"`
}

// Pass is one run of the rules battery, with every dependency injected.
type Pass struct {
	Asker  Asker
	Lister Lister

	// Locate returns the worktree of the polecat that submitted a bead, or ""
	// when none is found. A submission with no worktree gets a line with a
	// reason.
	Locate func(rig, polecat string) string
	// Latest returns the newest transcript of a worktree's seat. A pass reads it
	// at pass time, not at submission time, so a seat respawned in the gap
	// between the two may yield the newer session's transcript.
	Latest func(worktree string) (string, error)
	// TownRoot bounds the transcripts the progress reader will open.
	TownRoot string

	// LogPath is the rules log every asked session appends to.
	LogPath string
	// CursorPath remembers the last submission a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of submissions that got a line with answers.
	Asked int
	// Unanswered is the number that got a line with a reason and no answers.
	Unanswered int
	// Refused is the number whose transcript the containment check refused.
	// They get no line.
	Refused int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The submissions it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// Run does one pass. Submissions are taken oldest first, and each is logged
// and the cursor advanced before the next, so a pass stopped partway never
// asks about a submission twice. A submission whose ask was cut short by ctx,
// or that failed because Jev was out of reach, is neither logged nor passed:
// the next pass asks it again.
//
// Run returns ErrBusy when another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := battery.Lock(p.CursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return sum, ErrBusy
	}
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

	all, err := p.Lister.Done(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing submissions: %w", err)
	}
	for _, d := range afterCursor(all, cur) {
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

		line, err := p.ask(ctx, d)
		switch {
		case errors.Is(err, errRefused):
			sum.Refused++
		case err != nil:
			return sum, fmt.Errorf("asking about %s: %w", d.Bead, err)
		default:
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			if line.Answers != nil {
				sum.Asked++
			} else {
				sum.Unanswered++
			}
		}
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: d.SubmittedAt, ID: d.Bead}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// ask reads one submission's transcript excerpt and asks the battery about it.
// A transcript the containment check refuses returns errRefused, and the
// submission gets no line. A failure about the session itself returns a line
// with a reason and no error. Only a context that ended, or Jev out of reach,
// returns an error, so the pass stops with the cursor before this submission.
func (p *Pass) ask(ctx context.Context, d Done) (Line, error) {
	line := Line{
		TS:              p.now(),
		Rig:             d.Rig,
		Bead:            d.Bead,
		Polecat:         d.Polecat,
		DoneAt:          d.SubmittedAt,
		QuestionVersion: Version,
	}

	excerpt, err := p.excerpt(d)
	switch {
	case errors.Is(err, progress.ErrOutsideTown):
		return Line{}, errRefused
	case errors.Is(err, errNoTranscript):
		line.Reason = ReasonNoTranscript
		return line, nil
	case err != nil:
		line.Reason = ReasonTranscriptUnreadable
		return line, nil
	case strings.TrimSpace(excerpt) == "":
		line.Reason = ReasonNoTranscript
		return line, nil
	}
	sum := sha256.Sum256([]byte(excerpt))
	line.ExcerptSHA256 = hex.EncodeToString(sum[:])

	got, err := p.Asker.Ask(ctx, State{Excerpt: excerpt}, Questions())
	switch {
	case err == nil:
		line.Answers = answersFrom(got)
		line.Model = got.Model
	case ctx.Err() != nil:
		return line, ctx.Err()
	case errors.Is(err, jev.ErrBadResponse):
		line.Reason = ReasonBadResponse
	default:
		return line, err
	}
	return line, nil
}

// excerpt returns the tail of d's session transcript. A seat with no worktree or
// no transcript returns errNoTranscript; the containment check's refusal comes
// back as progress.ErrOutsideTown.
func (p *Pass) excerpt(d Done) (string, error) {
	worktree := p.Locate(d.Rig, d.Polecat)
	if worktree == "" {
		return "", errNoTranscript
	}
	path, err := p.Latest(worktree)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNoTranscript, err)
	}
	return progress.TranscriptTail(p.TownRoot, worktree, path, excerptRunes)
}

// answersFrom reads the battery's answers out of one Ask result.
func answersFrom(got *jev.Answers) map[string]Answer {
	out := make(map[string]Answer, 2)
	if p, ok := got.Noul[QBrokeStandingRule]; ok {
		out[QBrokeStandingRule] = Answer{Noul: &p}
	}
	if c, ok := got.Choice[QRuleKind]; ok {
		out[QRuleKind] = Answer{Choice: c.Choice, Probabilities: c.Probabilities}
	}
	return out
}

// afterCursor returns the submissions that sort after the cursor, oldest first.
// Ties on the stamp break by bead ID, the same order the cursor was written in.
func afterCursor(all []Done, cur battery.Cursor) []Done {
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].SubmittedAt.Equal(all[j].SubmittedAt) {
			return all[i].SubmittedAt.Before(all[j].SubmittedAt)
		}
		return all[i].Bead < all[j].Bead
	})
	var out []Done
	for _, d := range all {
		if d.SubmittedAt.After(cur.Time) || (d.SubmittedAt.Equal(cur.Time) && d.Bead > cur.ID) {
			out = append(out, d)
		}
	}
	return out
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

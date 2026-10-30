// Package progress is the advisory battery on polecats the overrun guard has
// nudged. The guard nudges a seat once its bead runs past the nudge threshold
// and stops it at the stop threshold on heuristics alone. A pass takes each
// nudge queued since the last pass, reads the seat's transcript for the last
// fifteen minutes, asks Jev the progress question set about it, and appends
// one line to a log that the guard's stop decision never reads. It never
// nudges, stops, messages or comments on a seat, and it never changes a bead.
//
// The pass reads the transcripts and beads it is given and writes nothing but
// its log. The caller supplies the queue, the bead reads and the Jev client, so
// a test runs a pass with fakes. Open wires the production versions.
package progress

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-progress"

	// truncateRunes caps the bead's title and acceptance quoted in the state.
	truncateRunes = 1500
)

// Reasons a line carries instead of answers. They are fixed words, never an
// error message, so the log never holds a path or a transcript fragment.
const (
	ReasonNoTranscript         = "no_transcript"
	ReasonTranscriptUnreadable = "transcript_unreadable"
	ReasonOutsideTown          = "outside_town"
	ReasonBeadUnreadable       = "bead_unreadable"
	ReasonBadResponse          = "bad_response"
	ReasonAskFailed            = "ask_failed"
)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Nudge is one nudge the overrun guard delivered, queued for the pass. The
// guard records it at delivery, so the transcript is the one live at the
// nudge, not one found later.
type Nudge struct {
	Rig, Polecat, Bead string
	// Session is the seat's tmux session name.
	Session string
	// At is when the nudge was delivered.
	At time.Time
	// Transcript is the seat's most recent transcript, or "" when the lookup
	// found none.
	Transcript string
	// Worktree is the seat's worktree the transcript was found from. The pass
	// allows the Claude projects directories for it, and no other seat's.
	Worktree string
}

// MaxQueued bounds the queue. A pass that cannot run (no price, the cap spent,
// the patrol switched off mid-run) must not let nudges pile up in memory, so a
// nudge past the bound is dropped.
const MaxQueued = 256

// Queue holds the nudges the pass has not yet asked about. The guard pushes
// from its own tick and the pass drains on its own goroutine, so it is
// guarded. The zero value is an empty queue.
type Queue struct {
	mu    sync.Mutex
	items []Nudge
}

// Push queues n. It reports false when the queue is full and n was dropped.
func (q *Queue) Push(n Nudge) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= MaxQueued {
		return false
	}
	q.items = append(q.items, n)
	return true
}

// Drain returns every queued nudge, oldest first, and empties the queue.
func (q *Queue) Drain() []Nudge {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}

// Len is the number of queued nudges.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Settings is the operator's configuration for the pass, read from
// patrols.jev_progress.
type Settings struct {
	// MaxUSDPerDay is the daily cap. Zero takes DefaultMaxUSDPerDay.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens. It is required: the
	// ledger stores tokens, never dollars.
	USDPerBillionTokens float64

	// Scrub scrubs the state before it is sent: the patrol's scrub key
	// sets it (gt-aqczg).
	Scrub bool

	// ScrubCheck runs the shadow scrub check on a sample of requests. The
	// scrub_check key sets it (gt-jb20i).
	ScrubCheck bool
}

// DefaultMaxUSDPerDay is the daily cap when the config names none. The cap is
// town-wide: it counts every Jev caller in the usage ledger.
const DefaultMaxUSDPerDay = 1.0

// Pass is one run of the progress battery, with every dependency injected.
type Pass struct {
	Asker Asker
	// Queue holds the nudges to ask about. Run drains it.
	Queue *Queue
	// Issue reads a bead in a rig. A read failure logs the line with
	// ReasonBeadUnreadable.
	Issue func(rig, id string) (*beads.Issue, error)

	// TownRoot bounds the transcripts the pass reads, with the seat's Claude
	// projects directories: one outside both is never opened.
	TownRoot string
	// ProjectDirs lists the Claude projects directories a worktree's transcripts
	// can sit in. Nil means agentlog.ProjectDirsFor under TownRoot.
	ProjectDirs func(worktree string) ([]string, error)
	// LogPath is the log every asked nudge appends to.
	LogPath string

	// Allows says whether the day's Jev spend is still under the cap. The
	// pass asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of nudges that got a line with answers.
	Asked int
	// Unanswered is the number of nudges that got a line with null answers,
	// because the transcript or the bead could not be read or Jev failed.
	Unanswered int
	// Dropped is the number of nudges the pass did not reach: the cap was
	// spent, the context ended or the spend could not be read. A nudge is not
	// held for a later pass, because the seat has moved on by then.
	Dropped int
	// CapReached is true when the pass stopped because the day's cap was
	// spent.
	CapReached bool
}

// Run asks about every queued nudge, oldest first, and appends one line per
// nudge that is reached. A nudge whose transcript or bead cannot be read gets a
// line with null answers and a reason, and the pass goes on to the next. A
// failure that ends the context, or a spend that cannot be read, stops the
// pass and drops the nudges it did not reach.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	nudges := p.Queue.Drain()
	for i, n := range nudges {
		if err := ctx.Err(); err != nil {
			sum.Dropped += len(nudges) - i
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			sum.Dropped += len(nudges) - i
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			sum.Dropped += len(nudges) - i
			break
		}
		line, err := p.ask(ctx, n)
		if err != nil {
			sum.Dropped += len(nudges) - i
			return sum, err
		}
		// An append that fails ends the pass. Every later line would fail the
		// same way, so asking about them would spend Jev on answers that are
		// never logged.
		if err := battery.Append(p.LogPath, line); err != nil {
			sum.Dropped += len(nudges) - i
			return sum, fmt.Errorf("appending to the progress log: %w", err)
		}
		if line.Answers != nil {
			sum.Asked++
		} else {
			sum.Unanswered++
		}
	}
	return sum, nil
}

// ask builds the state for one nudge and asks the battery about it. Every
// failure that is about the nudge itself returns a line with a reason. Only a
// context that ended returns an error, so the pass stops.
func (p *Pass) ask(ctx context.Context, n Nudge) (Line, error) {
	now := p.now()
	line := Line{
		TS:              now,
		Rig:             n.Rig,
		Polecat:         n.Polecat,
		Bead:            n.Bead,
		Session:         n.Session,
		NudgeAt:         n.At,
		QuestionVersion: Version,
	}

	// The transcript is checked before the bead is read, so a nudge whose
	// transcript cannot be read never reaches the bead store.
	path, reason := p.transcriptPath(n)
	if reason != "" {
		line.Reason = reason
		return line, nil
	}
	events, clipped, err := readWindow(path, now.Add(-transcriptWindow))
	if err != nil {
		line.Reason = ReasonTranscriptUnreadable
		return line, nil
	}
	issue, err := p.Issue(n.Rig, n.Bead)
	if err != nil || issue == nil {
		line.Reason = ReasonBeadUnreadable
		return line, nil
	}

	got, err := p.Asker.Ask(ctx, buildState(issue, events, clipped), Questions())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return line, cerr
		}
		if errors.Is(err, jev.ErrBadResponse) {
			line.Reason = ReasonBadResponse
		} else {
			line.Reason = ReasonAskFailed
		}
		return line, nil
	}
	line.Answers = answersFrom(got)
	line.Model = got.Model
	return line, nil
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// transcriptPath returns the transcript's path with its symlinks resolved, or
// the reason a nudge cannot be read from it. The containment check runs on the
// resolved paths, both sides: a symlink can point out of an allowed root, and
// a root that is itself a symlink (macOS temp dirs, for one) must still match.
// A town root that cannot be resolved refuses every transcript.
func (p *Pass) transcriptPath(n Nudge) (string, string) {
	if n.Transcript == "" {
		return "", ReasonNoTranscript
	}
	resolved, err := filepath.EvalSymlinks(n.Transcript)
	if err != nil {
		return "", ReasonTranscriptUnreadable
	}
	roots, err := p.allowedRoots(n.Worktree)
	if err != nil || !withinAny(roots, resolved) {
		return "", ReasonOutsideTown
	}
	return resolved, ""
}

// allowedRoots returns the resolved directories a transcript may sit under: the
// town root, and the Claude projects directories the nudged seat's worktree
// searches. Another seat's projects directory is not among them. A projects
// directory that does not exist holds no transcript, so it is left out.
func (p *Pass) allowedRoots(worktree string) ([]string, error) {
	town, err := filepath.EvalSymlinks(p.TownRoot)
	if err != nil {
		return nil, err
	}
	roots := []string{town}
	if worktree == "" {
		return roots, nil
	}
	projects := p.ProjectDirs
	if projects == nil {
		projects = func(w string) ([]string, error) { return agentlog.ProjectDirsFor(p.TownRoot, w) }
	}
	dirs, err := projects(worktree)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			continue
		}
		roots = append(roots, r)
	}
	return roots, nil
}

// withinAny reports whether path lies under one of roots.
func withinAny(roots []string, path string) bool {
	for _, root := range roots {
		if within(root, path) {
			return true
		}
	}
	return false
}

// within reports whether path lies under root. Both are cleaned first, so a
// path that climbs out with .. is refused. It is lexical: callers pass
// resolved paths.
func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

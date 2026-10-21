// Package dispatch is the advisory dispatch battery. A pass takes the
// placements made since the last pass, including the ones already closed, asks
// Jev one battery of questions about each (will it land first try, will it fit
// twenty minutes, does it need a strong model, and so on), and appends one line
// per bead to a log. It never edits, labels or comments on a bead: the log is
// the only thing it writes, and nothing a polecat reads changes.
//
// The pass runs on its own, after the fact, reading beads the dispatcher has
// already placed, so it adds nothing to the dispatcher's tick or to a sling.
// Nothing in internal/specdispatch, internal/cmd/spec.go or the sling code
// refers to this package.
//
// The pass owns no I/O beyond its log, its cursor and its retry queue. The
// caller supplies the Jev client, the bead list and the spend, so a test can
// run a pass with fakes and no network. Open wires the production versions.
//
// A question Jev cannot answer is never dropped on its first failure. A
// failure the bead's own request caused is queued and asked again next pass,
// up to maxAttempts; a failure that means Jev was out of reach stops the pass
// where it is, so an outage costs no attempts and loses no placement.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

const (
	// RigName is the rig whose beads a pass reads.
	RigName = "gastown"

	// Caller names the pass's Jev calls in the usage ledger, so its spend can
	// be told apart from the other jobs that call Jev.
	Caller = "jev-dispatch"

	// maxSectionRunes caps each bead section quoted in the state.
	maxSectionRunes = 1500

	// maxTitleRunes caps the title quoted in the state.
	maxTitleRunes = 300

	// maxLabels and maxLabelRunes cap the labels quoted in the state.
	maxLabels     = 30
	maxLabelRunes = 100

	// maxRetryWait caps how long a 429's Retry-After can hold a pass.
	maxRetryWait = 30 * time.Second

	// agentLabelPrefix starts the label that pins a bead to one agent.
	agentLabelPrefix = specdispatch.AgentLabelPrefix

	// agentVar is the attached_vars key that names the seat's agent.
	agentVar = "agent"
)

// Asker asks one battery. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Lister lists the placements a pass considers. A pass only reads them.
type Lister interface {
	// Placed lists the beads placed since t: every bead whose status says a
	// worker holds it (in_progress or hooked), and every bead closed since t,
	// which is where a placement finishes when its work lands between two
	// passes. A bead with neither status is not a placement. It need not
	// filter by the attached_at stamp; the pass does.
	Placed(t time.Time) ([]*beads.Issue, error)
}

// Pass is one run of the dispatch battery, with every dependency injected.
type Pass struct {
	Asker  Asker
	Lister Lister

	// LogPath is the dispatch log every answer appends to.
	LogPath string
	// CursorPath remembers the last placement a pass asked about.
	CursorPath string
	// RetryPath remembers the placements whose question failed and how many
	// passes have failed to answer each, so the next pass asks them again.
	RetryPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Wait pauses before a retry after a 429. Nil means a timer that stops
	// early when ctx is done.
	Wait func(ctx context.Context, d time.Duration) error
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of beads that got a log line this pass.
	Asked int
	// Failed is the number of beads whose question failed this pass. They get
	// no line. Each is queued for the next pass unless it is Final.
	Failed int
	// Failures is why each of them failed, in the order the pass met them. The
	// log line holds no cause, so this is where a caller shows one.
	Failures []Failure
	// GivenUp is how many of the failures were their last: those beads had
	// already failed maxAttempts times and are not asked again.
	GivenUp int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The beads it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// Failure is one placement whose question failed this pass.
type Failure struct {
	// Bead is the placement's bead.
	Bead string
	// Attempts is how many passes have failed to get an answer for it, this
	// one included.
	Attempts int
	// Err is why the ask failed. It never reaches the dispatch log.
	Err error
	// Final is true when the pass has given up: the placement reached
	// maxAttempts failures and is not asked again.
	Final bool
}

// placement is one bead's placement on a seat: its bead, the stamp the
// dispatcher gave it, and, when an earlier pass already failed to ask about
// it, how many attempts have failed so far.
type placement struct {
	issue      *beads.Issue
	attachedAt time.Time
	// queued is true when the cursor has already passed this placement and it
	// waits in the retry queue. Asking it must not move the cursor.
	queued bool
	// attempts is how many passes have already failed to answer it.
	attempts int
}

// Run does one pass. Placements are taken in the order they were stamped, and
// each is logged and the cursor advanced before the next, so a pass that is
// stopped partway never asks about a placement twice. A bead whose question was
// cut short by ctx is neither logged nor passed: the next pass asks it again.
//
// A question that fails because of the bead's own request is queued in the
// retry file with its attempt count and asked again next pass, until it has
// failed maxAttempts times. A question that fails any other way means Jev could
// not be reached: the pass stops at that placement, which stays queued or, for
// a new placement, behind the unmoved cursor, and returns the cause.
//
// Run returns ErrBusy when another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	if p.RetryPath == "" {
		return sum, errors.New("the pass has no retry path")
	}
	release, err := lockRun(p.CursorPath)
	if err != nil {
		return sum, err
	}
	defer release()

	now := p.now()
	cur, ok, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		return sum, err
	}
	if !ok {
		sum.Started = true
		return sum, battery.WriteCursor(p.CursorPath, battery.Cursor{Time: now})
	}

	queue, err := readRetries(p.RetryPath)
	if err != nil {
		return sum, err
	}
	all, err := p.Lister.Placed(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing placed beads: %w", err)
	}
	todo, dropped := queuePlacements(all, cur, queue)
	if dropped {
		if err := writeRetries(p.RetryPath, queue); err != nil {
			return sum, err
		}
	}

	for _, pl := range todo {
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

		ans, err := p.choose(ctx, pl.issue)
		if cerr := ctx.Err(); cerr != nil {
			return sum, cerr
		}
		key := retryKey(pl.issue.ID, pl.attachedAt)

		switch {
		case err == nil:
			line := Line{
				TS:              p.now(),
				Bead:            pl.issue.ID,
				AttachedAt:      pl.attachedAt,
				Agent:           agentOf(pl.issue),
				Answers:         lineAnswers(ans),
				QuestionVersion: Version,
				Model:           ans.Model,
			}
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			sum.Asked++
			if pl.queued {
				delete(queue, key)
				if err := writeRetries(p.RetryPath, queue); err != nil {
					return sum, err
				}
			}
		case !beadFault(err):
			// Jev is out of reach. Stop here rather than spend an attempt on
			// every placement behind this one, and drop nothing: the cursor
			// still points before this placement, or its queue entry stands.
			return sum, fmt.Errorf("asking about %s: %w", pl.issue.ID, err)
		default:
			attempts := pl.attempts + 1
			f := Failure{Bead: pl.issue.ID, Attempts: attempts, Err: err}
			if attempts >= maxAttempts {
				delete(queue, key)
				f.Final = true
				sum.GivenUp++
			} else {
				queue[key] = attempts
			}
			sum.Failures = append(sum.Failures, f)
			sum.Failed++
			// The queue is saved before the cursor moves past the placement, so
			// a crash between the two re-asks it rather than dropping it.
			if err := writeRetries(p.RetryPath, queue); err != nil {
				return sum, err
			}
		}

		if !pl.queued {
			if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: pl.attachedAt, ID: pl.issue.ID}); err != nil {
				return sum, err
			}
		}
	}
	return sum, nil
}

// queuePlacements returns what this pass should ask about, oldest first: every
// placement after the cursor, and every queued placement the cursor already
// passed. It drops queue entries for beads the lister no longer calls a
// placement, and reports whether it did. Ties on the stamp break by ID, so the
// cursor orders placements completely.
func queuePlacements(all []*beads.Issue, cur battery.Cursor, queue retries) (todo []placement, dropped bool) {
	placed := make(map[string]*beads.Issue, len(all))
	for _, b := range all {
		if !placedStatus(b) {
			continue
		}
		if at := attachedAt(b); !at.IsZero() {
			placed[retryKey(b.ID, at)] = b
		}
	}
	for key := range queue {
		if _, ok := placed[key]; !ok {
			delete(queue, key)
			dropped = true
		}
	}
	for key, b := range placed {
		at := attachedAt(b)
		if after(cur, at, b.ID) {
			todo = append(todo, placement{issue: b, attachedAt: at})
			continue
		}
		// Behind the cursor, so a queue entry is the only reason to ask again.
		if attempts, ok := queue[key]; ok {
			todo = append(todo, placement{issue: b, attachedAt: at, queued: true, attempts: attempts})
		}
	}
	sort.Slice(todo, func(i, j int) bool {
		if !todo[i].attachedAt.Equal(todo[j].attachedAt) {
			return todo[i].attachedAt.Before(todo[j].attachedAt)
		}
		return todo[i].issue.ID < todo[j].issue.ID
	})
	return todo, dropped
}

// placedStatus reports whether a bead's status says it was placed:
// in_progress, hooked, which is the status a sling gives a bead it places, or
// closed, which is where a placement ends when its work lands between two
// passes.
func placedStatus(b *beads.Issue) bool {
	switch beads.IssueStatus(b.Status) {
	case beads.StatusInProgress, beads.IssueStatusHooked, beads.StatusClosed:
		return true
	}
	return false
}

// attachedAt is the bead's attached_at stamp, zero when it has none or it does
// not parse.
func attachedAt(b *beads.Issue) time.Time {
	fields := beads.ParseAttachmentFields(b)
	if fields == nil {
		return time.Time{}
	}
	return beads.ParseIssueTime(fields.AttachedAt)
}

// after reports whether a placement stamped at with id sorts after the cursor.
func after(cur battery.Cursor, at time.Time, id string) bool {
	if at.Equal(cur.Time) {
		return id > cur.ID
	}
	return at.After(cur.Time)
}

// agentOf is the agent the bead is for: the first agent: label if it has one,
// else the agent var the dispatcher stamped with the placement, else empty.
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

// choose asks the battery about one bead. A 429 gets one retry, after the
// delay the server asked for, capped at maxRetryWait.
func (p *Pass) choose(ctx context.Context, b *beads.Issue) (*jev.Answers, error) {
	state := buildState(b)
	qs := questions()
	ans, err := p.Asker.Ask(ctx, state, qs)
	if !errors.Is(err, jev.ErrRateLimited) {
		return ans, err
	}
	var wait time.Duration
	var rl *jev.RateLimitError
	if errors.As(err, &rl) {
		wait = min(rl.RetryAfter, maxRetryWait)
	}
	if err := p.wait(ctx, wait); err != nil {
		return nil, err
	}
	return p.Asker.Ask(ctx, state, qs)
}

// lineAnswers copies the battery's answers into the log's shape.
func lineAnswers(a *jev.Answers) Answers {
	return Answers{
		LandsFirstTry:      a.Noul[qLandsFirstTry],
		FitsTwentyMinutes:  a.Noul[qFitsTwentyMinutes],
		AcceptanceTickable: a.Noul[qAcceptanceTickable],
		NeedsStrongModel:   a.Noul[qNeedsStrongModel],
		TouchesDaemon:      a.Noul[qTouchesDaemon],
		Size: SizeAnswer{
			Score:      a.Score[qSize].Score,
			Confidence: a.Score[qSize].Confidence,
		},
	}
}

// buildState returns the state for one bead: one "name: value" line per field,
// each value a JSON string literal on one line, so the model reads every value
// as data. It holds the title, the Goal, Constraints, Acceptance and Size
// sections, the labels and the agent, and nothing else of the bead.
func buildState(b *beads.Issue) string {
	sections := specdispatch.Sections(beads.StripAttachmentFields(b.Description))
	acceptance := strings.TrimSpace(b.AcceptanceCriteria)
	if acceptance == "" {
		acceptance = sections["acceptance"]
	}
	labels := make([]string, 0, min(len(b.Labels), maxLabels))
	for _, l := range b.Labels {
		if len(labels) == maxLabels {
			break
		}
		labels = append(labels, quote(truncate(l, maxLabelRunes)))
	}

	var s strings.Builder
	fmt.Fprintf(&s, "title: %s\n", quote(truncate(b.Title, maxTitleRunes)))
	fmt.Fprintf(&s, "goal: %s\n", quote(truncate(sections["goal"], maxSectionRunes)))
	fmt.Fprintf(&s, "constraints: %s\n", quote(truncate(sections["constraints"], maxSectionRunes)))
	fmt.Fprintf(&s, "acceptance: %s\n", quote(truncate(acceptance, maxSectionRunes)))
	fmt.Fprintf(&s, "size: %s\n", quote(truncate(sections["size"], maxSectionRunes)))
	fmt.Fprintf(&s, "labels: [%s]\n", strings.Join(labels, ", "))
	fmt.Fprintf(&s, "agent: %s\n", quote(agentOf(b)))
	return s.String()
}

// quote returns s as a JSON string literal on one line. It leaves <, > and &
// as they are, which the default encoder would turn into \u escapes.
func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(s)
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

func (p *Pass) wait(ctx context.Context, d time.Duration) error {
	if p.Wait != nil {
		return p.Wait(ctx, d)
	}
	return waitFor(ctx, d)
}

// waitFor pauses for d, or until ctx is done.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

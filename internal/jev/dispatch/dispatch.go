// Package dispatch is the advisory dispatch battery. A pass takes the beads
// placed on a seat since the last pass, asks Jev one battery of questions about
// each (will it land first try, will it fit twenty minutes, does it need a
// strong model, and so on), and appends one line per bead to a log. It never
// edits, labels or comments on a bead: the log is the only thing it writes, and
// nothing a polecat reads changes.
//
// The pass runs on its own, after the fact, reading beads the dispatcher has
// already placed, so it adds nothing to the dispatcher's tick or to a sling.
// Nothing in internal/specdispatch, internal/cmd/spec.go or the sling code
// refers to this package.
//
// The pass owns no I/O beyond its log and cursor. The caller supplies the Jev
// client, the bead list and the spend, so a test can run a pass with fakes and
// no network. Open wires the production versions.
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

// Lister lists the beads that sit on a seat. A pass only reads them.
type Lister interface {
	// Placed lists every bead whose status says a worker holds it: in_progress
	// or hooked. It need not filter by time; the pass does.
	Placed() ([]*beads.Issue, error)
}

// Pass is one run of the dispatch battery, with every dependency injected.
type Pass struct {
	Asker  Asker
	Lister Lister

	// LogPath is the dispatch log every answer appends to.
	LogPath string
	// CursorPath remembers the last placement a pass asked about.
	CursorPath string

	// SpentToday reports the USD spent by every Jev caller since local
	// midnight.
	SpentToday func() (float64, error)
	// CapUSD is the daily cap: a pass asks nothing more once SpentToday
	// reaches it.
	CapUSD float64

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
	// Failed is the number of beads whose question failed. They get no line
	// and are not asked again: the cursor passes them.
	Failed int
	// CapReached is true when the pass stopped because the day's cap was
	// spent. The beads it did not reach wait for the next pass.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// placement is one bead's placement on a seat.
type placement struct {
	issue      *beads.Issue
	attachedAt time.Time
}

// Run does one pass. Placements are taken in the order they were stamped, and
// each is logged and the cursor advanced before the next, so a pass that is
// stopped partway never asks about a placement twice. A bead whose question was
// cut short by ctx is neither logged nor passed: the next pass asks it again.
// Run returns ErrBusy when another pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
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

	all, err := p.Lister.Placed()
	if err != nil {
		return sum, fmt.Errorf("listing placed beads: %w", err)
	}
	for _, pl := range pending(all, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		spent, err := p.SpentToday()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if spent >= p.CapUSD {
			sum.CapReached = true
			break
		}
		ans, err := p.choose(ctx, pl.issue)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return sum, cerr
			}
			sum.Failed++
		} else {
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
		}
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: pl.attachedAt, ID: pl.issue.ID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// pending keeps the placements after the cursor, oldest first. A bead counts
// when a worker holds it and its description carries an attached_at stamp.
// Ties on the stamp break by ID, so the cursor orders placements completely.
func pending(all []*beads.Issue, cur battery.Cursor) []placement {
	var out []placement
	for _, b := range all {
		if b.Status != "in_progress" && b.Status != beads.StatusHooked {
			continue
		}
		at := attachedAt(b)
		if at.IsZero() || !after(cur, at, b.ID) {
			continue
		}
		out = append(out, placement{issue: b, attachedAt: at})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].attachedAt.Equal(out[j].attachedAt) {
			return out[i].attachedAt.Before(out[j].attachedAt)
		}
		return out[i].issue.ID < out[j].issue.ID
	})
	return out
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

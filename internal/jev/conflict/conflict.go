// Package conflict is the conflict-risk battery. A pass takes the placements
// made since the last pass, pairs each one with the beads that were in flight
// when it was placed (up to five), asks Jev whether the two will touch the same
// files and the same function, and appends one line per pair to a log. It
// never edits, labels or comments on a bead: the log is the only thing it
// writes, and nothing the dispatcher does changes.
//
// The pass runs on its own, after the fact, reading the beads the dispatcher
// has already placed, so it adds nothing to the dispatcher's tick or to a sling.
// Nothing in internal/specdispatch, internal/cmd/spec.go or the sling code
// refers to this package.
//
// A pair is asked about once. Its key is saved after its line is appended, so a
// crash between the two can repeat a line but never asks a pair a second time
// in a later pass. A pair whose question fails because of the beads' own text
// is retried on a later pass, up to maxAttempts; any other failure means Jev
// could not be reached, and the pass stops where it is.
package conflict

import (
	"bytes"
	"context"
	"encoding/json"
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
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-conflict"

	// RigName is the rig whose beads a pass reads.
	RigName = "gastown"

	// maxInFlight caps how many in-flight beads a placement is paired with.
	maxInFlight = 5

	// maxAttempts is how many passes may fail to get an answer for one pair
	// before the pass gives up on it, as dispatch's maxAttempts does for a
	// placement.
	maxAttempts = 3

	// maxSectionRunes caps each bead section quoted in the state.
	maxSectionRunes = 1500

	// maxTitleRunes caps the title quoted in the state.
	maxTitleRunes = 300
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
	// passes. A bead with neither status is not a placement.
	Placed(t time.Time) ([]*beads.Issue, error)
}

// Pass is one run of the conflict battery, with every dependency injected.
type Pass struct {
	Asker  Asker
	Lister Lister

	// LogPath is the conflict log every answer appends to.
	LogPath string
	// CursorPath remembers the last placement a pass asked about.
	CursorPath string
	// PairsPath remembers the pairs a pass has asked about and the failures.
	PairsPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of pairs that got a log line this pass.
	Asked int
	// Failed is the number of pairs whose question failed this pass because of
	// the beads' own text. Each is asked again on a later pass, unless it is
	// one of GivenUp.
	Failed int
	// GivenUp is how many pairs reached maxAttempts failures and are not asked
	// again.
	GivenUp int
	// CapReached is true when the pass stopped because the day's cap was spent.
	CapReached bool
	// Started is true when the pass found no cursor and only set one, so it
	// asked about nothing. The first pass starts from its own time.
	Started bool
}

// placement is one bead's placement: the bead and the stamp the dispatcher
// gave it.
type placement struct {
	issue *beads.Issue
	at    time.Time
}

// key names the placement, so a pair's key holds both stamps.
func (p placement) key() string {
	return p.issue.ID + "@" + p.at.UTC().Format(time.RFC3339Nano)
}

// less orders placements by stamp, breaking ties by ID, so the cursor orders
// placements completely.
func (p placement) less(q placement) bool {
	if !p.at.Equal(q.at) {
		return p.at.Before(q.at)
	}
	return p.issue.ID < q.issue.ID
}

// Run does one pass. Placements are taken in the order they were stamped. Each
// one is paired with the in-flight beads at its stamp, and each new pair is
// asked and logged before the next. The cursor moves past a placement only
// once all its pairs are done, so a pass that stops partway asks about the
// rest next time and never repeats a pair.
//
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

	st, err := readPairs(p.PairsPath)
	if err != nil {
		return sum, err
	}
	if st.prune(cur.Time) {
		if err := battery.WriteState(p.PairsPath, st); err != nil {
			return sum, err
		}
	}
	all, err := p.Lister.Placed(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing placed beads: %w", err)
	}

	for _, pl := range placementsAfter(all, cur) {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		for _, other := range inFlightAt(pl, all) {
			key := pairKey(pl, other)
			if _, done := st.Done[key]; done {
				continue
			}
			allowed, err := p.Allows()
			if err != nil {
				return sum, fmt.Errorf("reading today's spend: %w", err)
			}
			if !allowed {
				sum.CapReached = true
				return sum, nil
			}

			ans, err := p.ask(ctx, pl.issue, other.issue)
			if cerr := ctx.Err(); cerr != nil {
				return sum, cerr
			}
			if err != nil {
				if !beadFault(err) {
					return sum, fmt.Errorf("asking about %s and %s: %w", pl.issue.ID, other.issue.ID, err)
				}
				sum.Failed++
				attempts := st.Attempts[key] + 1
				if attempts < maxAttempts {
					st.Attempts[key] = attempts
					if err := battery.WriteState(p.PairsPath, st); err != nil {
						return sum, err
					}
					// The cursor stays before this placement, so the next pass
					// reaches the pair again.
					return sum, nil
				}
				delete(st.Attempts, key)
				st.Done[key] = newer(pl, other)
				sum.GivenUp++
				if err := battery.WriteState(p.PairsPath, st); err != nil {
					return sum, err
				}
				continue
			}

			line := Line{
				TS:              p.now(),
				Bead:            pl.issue.ID,
				AttachedAt:      pl.at,
				Other:           other.issue.ID,
				OtherAttachedAt: other.at,
				Answers:         lineAnswers(ans),
				QuestionVersion: Version,
				Model:           ans.Model,
			}
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			sum.Asked++
			st.Done[key] = newer(pl, other)
			delete(st.Attempts, key)
			if err := battery.WriteState(p.PairsPath, st); err != nil {
				return sum, err
			}
		}
		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: pl.at, ID: pl.issue.ID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// placementsAfter returns the placements after the cursor, oldest first.
func placementsAfter(all []*beads.Issue, cur battery.Cursor) []placement {
	var out []placement
	for _, b := range all {
		if b == nil || !placedStatus(b) {
			continue
		}
		at := attachedAt(b)
		if at.IsZero() {
			continue
		}
		pl := placement{issue: b, at: at}
		if after(cur, pl) {
			out = append(out, pl)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

// inFlightAt returns up to maxInFlight beads that were in flight when pl was
// placed: placed before it, and still held now or closed after its stamp. The
// most recent placements come first.
func inFlightAt(pl placement, all []*beads.Issue) []placement {
	var out []placement
	for _, b := range all {
		if b == nil || b.ID == pl.issue.ID || !placedStatus(b) {
			continue
		}
		at := attachedAt(b)
		if at.IsZero() {
			continue
		}
		other := placement{issue: b, at: at}
		if !other.less(pl) {
			continue
		}
		if beads.IssueStatus(b.Status) == beads.StatusClosed && !beads.ParseIssueTime(b.ClosedAt).After(pl.at) {
			continue
		}
		out = append(out, other)
	}
	sort.Slice(out, func(i, j int) bool { return out[j].less(out[i]) })
	if len(out) > maxInFlight {
		out = out[:maxInFlight]
	}
	return out
}

// placedStatus reports whether a bead's status says it was placed: in_progress
// or hooked, the status a sling gives a bead it places, or closed, where a
// placement ends when its work lands between two passes.
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

// after reports whether a placement sorts after the cursor.
func after(cur battery.Cursor, pl placement) bool {
	if pl.at.Equal(cur.Time) {
		return pl.issue.ID > cur.ID
	}
	return pl.at.After(cur.Time)
}

// ask asks the battery about one pair.
func (p *Pass) ask(ctx context.Context, a, b *beads.Issue) (*jev.Answers, error) {
	return p.Asker.Ask(ctx, buildState(a, b), questions())
}

// lineAnswers copies the battery's answers into the log's shape.
func lineAnswers(a *jev.Answers) Answers {
	return Answers{
		SameFiles:    a.Noul[qSameFiles],
		SameFunction: a.Noul[qSameFunction],
		Risk: RiskAnswer{
			Score:      a.Score[qRisk].Score,
			Confidence: a.Score[qRisk].Confidence,
		},
	}
}

// buildState returns the state for one pair: the title, Goal, Constraints and
// Acceptance of each bead, one "name: value" line per field, each value a JSON
// string literal on one line, so the model reads every value as data.
func buildState(a, b *beads.Issue) string {
	var s strings.Builder
	writeBead(&s, "first", a)
	writeBead(&s, "second", b)
	return s.String()
}

// writeBead writes one bead's fields under prefix.
func writeBead(s *strings.Builder, prefix string, b *beads.Issue) {
	sections := specdispatch.Sections(beads.StripAttachmentFields(b.Description))
	acceptance := strings.TrimSpace(b.AcceptanceCriteria)
	if acceptance == "" {
		acceptance = sections["acceptance"]
	}
	fmt.Fprintf(s, "%s_title: %s\n", prefix, quote(truncate(b.Title, maxTitleRunes)))
	fmt.Fprintf(s, "%s_goal: %s\n", prefix, quote(truncate(sections["goal"], maxSectionRunes)))
	fmt.Fprintf(s, "%s_constraints: %s\n", prefix, quote(truncate(sections["constraints"], maxSectionRunes)))
	fmt.Fprintf(s, "%s_acceptance: %s\n", prefix, quote(truncate(acceptance, maxSectionRunes)))
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

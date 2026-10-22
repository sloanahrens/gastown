// Package thrash is the thrash battery: an advisory pass, like the dispatch
// battery. A pass takes the placements made since the last pass, including the
// ones already closed, asks Jev two questions about each (will the polecat be
// respawned at least once on this bead, and will the overrun guard stop it for
// running long), and appends one line per placement to a log. It never edits,
// labels, holds or splits a bead: the log is the only thing it writes.
//
// The battery is separate from the dispatch battery, so the dispatch scorecard
// is unchanged. The pass reads no label files. The scorecard adapter in
// internal/jev/scorecard labels each line afterwards from the bead's respawn
// counts and the overrun guard's records.
//
// The pass owns no I/O beyond its log and its cursor. The caller supplies the
// Jev client, the bead list and the spend, so a test can run a pass with fakes
// and no network. Open wires the production versions.
//
// A question Jev cannot answer for a reason the bead's own request caused is
// skipped and the cursor moves past the placement, so one bad bead never holds
// the pass. A failure that means Jev was out of reach stops the pass with the
// cursor before the placement, so an outage costs no placement.
package thrash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	Caller = "jev-thrash"

	// maxSectionRunes caps each bead section quoted in the state.
	maxSectionRunes = 1500

	// maxTitleRunes caps the title quoted in the state.
	maxTitleRunes = 300

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
	// worker holds it (in_progress or hooked), and every bead closed since t.
	// It need not filter by the attached_at stamp; the pass does.
	Placed(t time.Time) ([]*beads.Issue, error)
}

// ErrBusy means another pass holds the run lock.
var ErrBusy = errors.New("another jev thrash pass is running")

// Pass is one run of the thrash battery, with every dependency injected.
type Pass struct {
	Asker  Asker
	Lister Lister

	// LogPath is the thrash log every answer appends to.
	LogPath string
	// CursorPath remembers the last placement a pass asked about.
	CursorPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of beads that got a log line this pass.
	Asked int
	// Failed is the number of beads whose question failed this pass. They get
	// no line and the cursor moves past them: they are not asked again.
	Failed int
	// Failures is why each of them failed, in the order the pass met them.
	Failures []Failure
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
	// Err is why the ask failed. It never reaches the thrash log.
	Err error
}

// placement is one bead's placement on a seat, with the stamp the dispatcher
// gave it.
type placement struct {
	issue      *beads.Issue
	attachedAt time.Time
}

// Run does one pass. Placements are taken in the order they were stamped, and
// each is logged and the cursor advanced before the next, so a pass that is
// stopped partway never asks about a placement twice. A bead whose question was
// cut short by ctx is neither logged nor passed: the next pass asks it again.
//
// Run returns ErrBusy when another pass holds the run lock.
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

	all, err := p.Lister.Placed(cur.Time)
	if err != nil {
		return sum, fmt.Errorf("listing placed beads: %w", err)
	}
	for _, pl := range afterCursor(all, cur) {
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

		ans, err := p.Asker.Ask(ctx, buildState(pl.issue), questions())
		if cerr := ctx.Err(); cerr != nil {
			return sum, cerr
		}
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
		case !beadFault(err):
			// Jev is out of reach. Stop with the cursor still before this
			// placement, so the next pass asks about it.
			return sum, fmt.Errorf("asking about %s: %w", pl.issue.ID, err)
		default:
			sum.Failed++
			sum.Failures = append(sum.Failures, Failure{Bead: pl.issue.ID, Err: err})
		}

		if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: pl.attachedAt, ID: pl.issue.ID}); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// afterCursor returns the placements that sort after the cursor, oldest first.
// Placements without an attached_at stamp are skipped. Ties on the stamp break
// by ID, so the cursor orders placements completely.
func afterCursor(all []*beads.Issue, cur battery.Cursor) []placement {
	seen := make(map[string]bool, len(all))
	var todo []placement
	for _, b := range all {
		if b == nil || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		at := attachedAt(b)
		if at.IsZero() || !after(cur, at, b.ID) {
			continue
		}
		todo = append(todo, placement{issue: b, attachedAt: at})
	}
	sort.Slice(todo, func(i, j int) bool {
		if !todo[i].attachedAt.Equal(todo[j].attachedAt) {
			return todo[i].attachedAt.Before(todo[j].attachedAt)
		}
		return todo[i].issue.ID < todo[j].issue.ID
	})
	return todo
}

// after reports whether a placement stamped at with id sorts after the cursor.
func after(cur battery.Cursor, at time.Time, id string) bool {
	if at.Equal(cur.Time) {
		return id > cur.ID
	}
	return at.After(cur.Time)
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

// beadFault reports whether a failed ask was caused by the bead's own request,
// which a retry cannot fix: a 4xx other than an authorization failure, a
// rate-limit or an account refusal, or a response Jev could not parse. Anything
// else means Jev could not be reached or refused the account, which says
// nothing about the bead.
func beadFault(err error) bool {
	if errors.Is(err, jev.ErrRateLimited) {
		return false
	}
	var se *jev.StatusError
	if errors.As(err, &se) {
		return se.Status >= 400 && se.Status < 500 &&
			se.Status != http.StatusUnauthorized &&
			se.Status != http.StatusTooManyRequests &&
			!accountRefusal(se.Status)
	}
	return errors.Is(err, jev.ErrBadResponse)
}

// accountRefusal reports whether a 4xx answers for the account rather than for
// the request: 402 and 403 refuse every request the key makes, so dropping the
// placement for one would lose every placement reached while the refusal lasts.
func accountRefusal(status int) bool {
	return status == http.StatusPaymentRequired || status == http.StatusForbidden
}

// agentOf is the agent the bead is for: the agent its agent: label pins, else
// the agent var the dispatcher stamped with the placement, else empty. A bead
// that wears several agent: labels names none.
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

// buildState returns the state for one bead: one "name: value" line per field,
// each value a JSON string literal on one line, so the model reads every value
// as data. It holds the title, the Goal, Constraints, Acceptance and Size
// sections and the agent, and nothing else of the bead.
func buildState(b *beads.Issue) string {
	sections := specdispatch.Sections(beads.StripAttachmentFields(b.Description))
	acceptance := strings.TrimSpace(b.AcceptanceCriteria)
	if acceptance == "" {
		acceptance = sections["acceptance"]
	}

	var s strings.Builder
	fmt.Fprintf(&s, "title: %s\n", quote(truncate(b.Title, maxTitleRunes)))
	fmt.Fprintf(&s, "goal: %s\n", quote(truncate(sections["goal"], maxSectionRunes)))
	fmt.Fprintf(&s, "constraints: %s\n", quote(truncate(sections["constraints"], maxSectionRunes)))
	fmt.Fprintf(&s, "acceptance: %s\n", quote(truncate(acceptance, maxSectionRunes)))
	fmt.Fprintf(&s, "size: %s\n", quote(truncate(sections["size"], maxSectionRunes)))
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

// lineAnswers copies the battery's answers into the log's shape.
func lineAnswers(a *jev.Answers) Answers {
	return Answers{
		WillRespawn: a.Noul[qWillRespawn],
		NeedsSplit:  a.Noul[qNeedsSplit],
	}
}

// lockRun takes the pass's run lock without waiting, and returns the release.
// The lock sits beside the cursor, so every pass over one cursor contends on it.
func lockRun(cursorPath string) (func(), error) {
	release, err := battery.Lock(cursorPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

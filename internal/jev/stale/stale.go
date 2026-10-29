// Package stale is the advisory weekly read of open beads that nobody has
// touched for a fortnight. A pass takes each open bead untouched for 14 days
// in every rig it is given, asks Jev whether the bead is obsolete, superseded
// by a landed change, worth doing, in need of reshaping, and how its priority
// fits, and appends one line per bead to a log. It never edits, labels,
// comments on, closes or reprioritises a bead: the log is the only thing it
// writes.
//
// A bead is asked about at most once in 30 days, read from the log's own
// lines, so a second pass inside that window asks nothing. A call that got no
// reply does not count, so the next pass asks that bead again. The caller supplies
// the bead reads, the hm search and the Jev client, so a test runs a pass with
// fakes and no network. Open wires the production versions.
package stale

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-stale"

	// StaleAfter is how long a bead must go untouched before it is asked about.
	StaleAfter = 14 * 24 * time.Hour

	// AskAgain is how long a bead waits after it was asked about before a pass
	// asks again.
	AskAgain = 30 * 24 * time.Hour

	// LandedWindow is how far back the landed beads offered as candidates
	// reach.
	LandedWindow = 30 * 24 * time.Hour

	// MaxBeadsPerRun caps the beads one pass asks about, oldest first.
	MaxBeadsPerRun = 500

	// MaxLanded caps the landed beads offered per bead.
	MaxLanded = 10

	// maxTitleRunes and maxDescRunes cap the bead values quoted in the state.
	maxTitleRunes = 300
	maxDescRunes  = 600

	// hmTimeout bounds one hm query.
	hmTimeout = time.Minute
)

// skipLabels keep a bead out of the read: a bead a human owns or a handoff note
// is not work that can be obsolete. Each label is named by the constant that
// owns its spelling, so a rename reaches this read too. The plain person
// spellings have no label constant of their own; dispatch writes them by hand.
var skipLabels = []string{
	dispatch.HumanOnlyLabel,
	dispatch.OverseerOnlyLabel,
	dispatch.OperatorOnlyLabel,
	"handoff",
	"needs-human",
	"needs-overseer",
	"needs-operator",
	land.LabelNeedsHuman,
	land.LabelNeedsOverseer,
	land.LabelNeedsOperator,
}

// workTypes are the issue types the pass reads. Every other type (rig,
// molecule, agent, epic, wisp, and whatever the beads package marks
// infrastructure) is bookkeeping, not work a landed change could supersede. An
// allowlist keeps a type added later out of the read until it is named here.
var workTypes = []string{"task", "bug", "feature"}

// crewSeatMarker starts the second segment of a crew seat's own task bead ID
// ("<prefix>-crew-<name>").
const crewSeatMarker = "crew-"

// IsWorkType reports whether an issue type is one the pass reads: task, bug or
// feature. The scorecard asks it too, so it discards the lines an earlier pass
// logged for rig, molecule and other bookkeeping beads.
func IsWorkType(issueType string) bool {
	t := strings.ToLower(strings.TrimSpace(issueType))
	for _, w := range workTypes {
		if t == w {
			return true
		}
	}
	return false
}

// isCrewSeat reports whether a bead ID names a crew seat's task bead,
// "<prefix>-crew-<name>". Only a marker in a session position counts: a work
// bead whose ID merely contains "crew-" (a sub-ID like gt-fix-crew-1) is still
// read, as it is in gt handoff's session-name check.
func isCrewSeat(id string) bool {
	_, rest, ok := strings.Cut(id, "-")
	return ok && strings.HasPrefix(rest, crewSeatMarker) && len(rest) > len(crewSeatMarker)
}

// Asker asks one battery of questions about one state. *jev.Client satisfies
// it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Searcher returns the IDs of the beads in a rig that a text matches, best
// first. An error means the search could not run, and the pass offers the
// most recent landed beads instead.
type Searcher interface {
	Search(ctx context.Context, rig, text string) ([]string, error)
}

// Beads reads a rig's beads. A pass only reads them.
type Beads interface {
	// Open lists the open beads in a rig.
	Open(rig string) ([]*beads.Issue, error)
	// Landed lists the beads in a rig closed as landed at or after since.
	Landed(rig string, since time.Time) ([]*beads.Issue, error)
}

// Pass is one run of the stale read, with every dependency injected.
type Pass struct {
	Asker Asker
	// Searcher is nil when hm is not installed, and the pass offers the most
	// recent landed beads.
	Searcher Searcher
	Beads    Beads

	// Rigs are the rigs the pass reads, in the order the daemon serves them.
	Rigs []string

	// LogPath is the log every bead's answers append to. Its lock sits beside
	// it, so two passes never ask about the same beads at once.
	LogPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of beads that got a line with answers.
	Asked int
	// Unanswered is the number of beads that got a line with null answers.
	Unanswered int
	// Deferred is the number of eligible beads the pass did not reach: the
	// run's cap or the day's cap stopped it first. They wait for the next pass.
	Deferred int
	// CapReached is true when the pass stopped because the day's cap was spent.
	CapReached bool
	// UnreadRigs counts the rigs whose beads could not be read, and that the
	// pass skipped.
	UnreadRigs int
}

// candidate is one bead the pass will ask about, with the rig it lives in.
type candidate struct {
	rig   string
	issue *beads.Issue
	// updated is when the bead was last written, from its updated_at.
	updated time.Time
}

// Run does one pass. Beads are asked oldest first, and each line is appended
// before the next bead is asked, so a pass that is stopped partway never asks
// about a bead twice. A bead whose question was cut short by ctx is neither
// logged nor passed: the next pass asks it. Run returns ErrBusy when another
// pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.LogPath)
	if err != nil {
		return sum, err
	}
	defer release()

	now := p.now()
	asked, err := askedSince(p.LogPath, now.Add(-AskAgain))
	if err != nil {
		return sum, err
	}

	// A rig whose beads or landed beads cannot be read is skipped whole, so
	// its beads are never asked with a candidate list that is missing.
	var cands []candidate
	landed := map[string][]*beads.Issue{}
	for _, rig := range p.Rigs {
		open, err := p.Beads.Open(rig)
		if err != nil {
			sum.UnreadRigs++
			continue
		}
		var rigCands []candidate
		for _, b := range open {
			if c, ok := p.eligible(rig, b, now); ok && !asked[askKey(rig, b.ID)] {
				rigCands = append(rigCands, c)
			}
		}
		if len(rigCands) == 0 {
			continue
		}
		lands, err := p.Beads.Landed(rig, now.Add(-LandedWindow))
		if err != nil {
			sum.UnreadRigs++
			continue
		}
		landed[rig] = lands
		cands = append(cands, rigCands...)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].updated.Equal(cands[j].updated) {
			return cands[i].updated.Before(cands[j].updated)
		}
		return cands[i].rig+cands[i].issue.ID < cands[j].rig+cands[j].issue.ID
	})
	if len(cands) > MaxBeadsPerRun {
		sum.Deferred += len(cands) - MaxBeadsPerRun
		cands = cands[:MaxBeadsPerRun]
	}

	for i, c := range cands {
		if err := ctx.Err(); err != nil {
			sum.Deferred += len(cands) - i
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			sum.Deferred += len(cands) - i
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			sum.Deferred += len(cands) - i
			break
		}
		line, err := p.ask(ctx, c, landed[c.rig], now)
		if err != nil {
			sum.Deferred += len(cands) - i
			return sum, err
		}
		if err := battery.Append(p.LogPath, line); err != nil {
			sum.Deferred += len(cands) - i
			return sum, fmt.Errorf("writing the stale log: %w", err)
		}
		if line.Answers != nil {
			sum.Asked++
		} else {
			sum.Unanswered++
		}
	}
	return sum, nil
}

// eligible reports whether a bead is one the pass reads: an open work bead
// (task, bug or feature), not a wisp, not a crew seat's bead, not labeled for a
// human or a handoff, and untouched for StaleAfter.
func (p *Pass) eligible(rig string, b *beads.Issue, now time.Time) (candidate, bool) {
	if b == nil || b.Ephemeral || !IsWorkType(b.Type) || isCrewSeat(b.ID) || hasAny(b.Labels, skipLabels...) {
		return candidate{}, false
	}
	updated := beads.ParseIssueTime(b.UpdatedAt)
	if updated.IsZero() || now.Sub(updated) < StaleAfter {
		return candidate{}, false
	}
	return candidate{rig: rig, issue: b, updated: updated}, true
}

// ask builds the line one bead earns. A failed hm search falls back to the
// most recent landed beads; a failed Jev call logs null answers and a reason.
// Only a canceled ctx returns an error, so the bead is left for the next pass.
func (p *Pass) ask(ctx context.Context, c candidate, lands []*beads.Issue, now time.Time) (Line, error) {
	offered := p.offerLanded(ctx, c, lands)
	if err := ctx.Err(); err != nil {
		return Line{}, err
	}
	line := Line{
		TS:              now,
		Rig:             c.rig,
		Bead:            c.issue.ID,
		Type:            c.issue.Type,
		Priority:        c.issue.Priority,
		UpdatedAt:       c.updated,
		DescHash:        HashDescription(c.issue.Description),
		LandedOffered:   idsOf(offered),
		QuestionVersion: Version,
	}

	got, err := p.Asker.Ask(ctx, buildState(c, offered, now), Questions())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		if errors.Is(err, jev.ErrBadResponse) {
			line.Reason = ReasonBadResponse
		} else {
			line.Reason = ReasonAskFailed
		}
		return line, nil
	}
	line.Model = got.Model
	line.Answers = answersFrom(got)
	return line, nil
}

// offerLanded picks up to MaxLanded landed beads closest to the bead, in rank
// order when hm answers. When hm is absent, fails or matches no landed bead,
// the most recent landed beads in the rig are offered.
func (p *Pass) offerLanded(ctx context.Context, c candidate, lands []*beads.Issue) []*beads.Issue {
	if len(lands) == 0 {
		return nil
	}
	if p.Searcher != nil {
		ids, err := p.Searcher.Search(ctx, c.rig, searchText(c.issue))
		if err == nil {
			byID := make(map[string]*beads.Issue, len(lands))
			for _, l := range lands {
				byID[l.ID] = l
			}
			var out []*beads.Issue
			seen := make(map[string]bool, MaxLanded)
			for _, id := range ids {
				l, ok := byID[id]
				if !ok || seen[id] || len(out) == MaxLanded {
					continue
				}
				seen[id] = true
				out = append(out, l)
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	recent := append([]*beads.Issue(nil), lands...)
	sort.SliceStable(recent, func(i, j int) bool {
		return beads.ParseIssueTime(recent[i].ClosedAt).After(beads.ParseIssueTime(recent[j].ClosedAt))
	})
	if len(recent) > MaxLanded {
		recent = recent[:MaxLanded]
	}
	return recent
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// landedBead is one landed bead as the state quotes it.
type landedBead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// state is the bead and the landed beads the questions are asked about. Every
// value is data, quoted in JSON, and the instructions say so.
type state struct {
	Rig         string       `json:"rig"`
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Priority    int          `json:"priority"`
	Labels      []string     `json:"labels"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Acceptance  string       `json:"acceptance"`
	AgeDays     int          `json:"age_days"`
	IdleDays    int          `json:"idle_days"`
	Landed      []landedBead `json:"landed_beads"`
}

// buildState is the state for one bead.
func buildState(c candidate, lands []*beads.Issue, now time.Time) state {
	b := c.issue
	st := state{
		Rig:         c.rig,
		ID:          b.ID,
		Type:        b.Type,
		Priority:    b.Priority,
		Labels:      b.Labels,
		Title:       truncate(b.Title, maxTitleRunes),
		Description: truncate(b.Description, maxDescRunes),
		Acceptance:  truncate(land.CriteriaText(b), maxDescRunes),
		AgeDays:     daysBetween(beads.ParseIssueTime(b.CreatedAt), now),
		IdleDays:    daysBetween(c.updated, now),
		Landed:      []landedBead{},
	}
	for _, l := range lands {
		st.Landed = append(st.Landed, landedBead{ID: l.ID, Title: truncate(l.Title, maxTitleRunes)})
	}
	return st
}

// searchText is the text the hm search is run with: the bead's title and the
// start of its description.
func searchText(b *beads.Issue) string {
	s := truncate(b.Title, maxTitleRunes)
	if b.Description != "" {
		s += "\n" + truncate(b.Description, maxDescRunes)
	}
	return s
}

// askedSince is the set of beads a pass has logged at or after since, keyed by
// askKey. A line counts when Jev answered, or replied with something the pass
// could not read (bad_response), so a bad reply is not asked again on every
// pass. A line whose call got no reply (ask_failed) does not count: a transient
// outage must not hold a bead out for thirty days, so the next pass asks it.
func askedSince(logPath string, since time.Time) (map[string]bool, error) {
	var lines []Line
	if err := battery.ReadRecent(logPath, math.MaxInt, &lines); err != nil {
		return nil, fmt.Errorf("reading the stale log: %w", err)
	}
	out := make(map[string]bool)
	for _, l := range lines {
		if !l.TS.Before(since) && l.Reason != ReasonAskFailed {
			out[askKey(l.Rig, l.Bead)] = true
		}
	}
	return out, nil
}

func askKey(rig, id string) string {
	return rig + "/" + id
}

// hasAny reports whether labels holds any of want.
func hasAny(labels []string, want ...string) bool {
	for _, l := range labels {
		for _, w := range want {
			if l == w {
				return true
			}
		}
	}
	return false
}

func idsOf(list []*beads.Issue) []string {
	out := make([]string, 0, len(list))
	for _, b := range list {
		out = append(out, b.ID)
	}
	return out
}

// daysBetween is the whole days from then to now, zero when then is unknown.
func daysBetween(then, now time.Time) int {
	if then.IsZero() {
		return 0
	}
	return int(now.Sub(then) / (24 * time.Hour))
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

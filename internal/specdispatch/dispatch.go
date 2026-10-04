package specdispatch

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// HostSafetyPrompt is the instruction every spec dispatch carries (stored as
// the sling args, which the polecat reads with its work).
const HostSafetyPrompt = "HOST SAFETY (spec dispatcher): any test of install or uninstall MUST use a temporary " +
	"INSTALL_DIR (mktemp -d) and never the real ~/.local/bin. Never run make install, gt dolt cleanup, " +
	"or rm -rf outside your worktree on this host."

// excludedLabels is the pre-filter the ready board is read with (`bd ready
// --exclude-label`), so a bead the dispatcher would never take is not fetched
// at all. It is not the dispatcher's verdict: the verdict is Eligible's, on the
// full bead, and it reads every routing decision through the shared hold rule
// (dispatch.DispatchHoldFields). A hold this list does not name — the operator
// label, gt:needs-human, a ruling in prose — still stops the dispatcher there,
// and every label it does name Eligible would have refused anyway (gt-lxxo4).
var excludedLabels = []string{"gt:ready-to-land", "needs-human", "needs-mayor-review", DispatchFailedLabel}

// ExcludedLabels returns the labels that keep a bead from the dispatcher.
func ExcludedLabels() []string { return append([]string(nil), excludedLabels...) }

// DispatchTypes are the bead types the dispatcher fills a seat with: the work
// bead kinds. They are narrower than the ready board — a docs or chore bead is
// real work, but it is the mayor's patrol that surfaces those, not the seat
// filler (seat-refill's type whitelist, carried over with the plugin).
var dispatchTypes = []string{"task", "bug", "feature"}

// IsDispatchType reports whether t names a bead type the dispatcher takes,
// ignoring case and surrounding space.
func IsDispatchType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, d := range dispatchTypes {
		if t == d {
			return true
		}
	}
	return false
}

// ShapeGate values (polecat_pool.shape_gate, gt-cq5gb): what the dispatcher
// does with a candidate's shape lint before it spends a seat.
const (
	// ShapeGateOff runs no lint: shape is not a gate.
	ShapeGateOff = "off"
	// ShapeGateWarn holds a bead the lint refuses instead of slinging it: the
	// bead stays ready and wears one SHAPE comment, for the operator to shape
	// or waive (gt-f8ppx).
	ShapeGateWarn = "warn"
	// ShapeGateRefuse holds the bead like warn, and labels it needs-shape (or
	// needs-planning) so the operator's reshape queue names it.
	ShapeGateRefuse = "refuse"
)

// Eligible reports whether a ready bead is the dispatcher's to consider, and
// why not when it is not. It is the candidate filter: status open, unassigned,
// a work bead (not an epic, not a runtime family) of a dispatchable type
// (task/bug/feature), at or above maxPriority's floor, carrying none of the
// excluded labels, not submitted for landing, and asserting no hold on its own
// record (dispatch.DispatchHoldFields): a bead parked for a person or by a
// ruling is a skip here, not a sling the guard has to refuse every tick
// (gt-lxxo4). A bead mid-submission is a skip for the reason the label alone
// cannot give: gt done writes the READY TO LAND block before the
// gt:ready-to-land label, and the block is the half a lagging read keeps
// (gt-kr5xv). The
// label spec and type feature are retired and accepted-but-ignored (gt-mmsr2),
// so neither gates a candidate: the retired type name is not what admits a
// feature bead, the type whitelist is. The shape lint runs later, on the full
// bead.
//
// maxPriority is the operator's ceiling on a candidate's priority number
// (polecat_pool.max_priority, default 2): a bead numbered higher is backlog the
// operator keeps. A negative priority is an unscored bead, which is never
// dispatched.
//
// reserved names the labels this tick's seats reserve (Budget.ReservedLabels:
// polecat_pool.pro_label, "needs-pro" by default). The shared hold rule counts
// such a label as a hold, because a dispatcher with no seat for it must leave
// the bead alone — but this dispatcher's reserved seat is the path that label
// asks for, so a bead carrying one is routed there, not held (gt-lxxo4). Pass
// nil when there is no such seat: then the rule's hold stands.
func Eligible(s Spec, maxPriority int, reserved []string) (bool, string) {
	status := strings.ToLower(strings.TrimSpace(s.Status))
	switch {
	case status == "deferred":
		return false, "deferred"
	case status != "open":
		return false, "status " + status
	case strings.TrimSpace(s.Assignee) != "":
		return false, "assigned to " + s.Assignee
	}
	if why := NotWorkBead(s); why != "" {
		return false, "not a work bead: " + why
	}
	if !IsDispatchType(s.Type) {
		return false, "type " + strings.ToLower(strings.TrimSpace(s.Type))
	}
	for _, l := range excludedLabels {
		if s.HasLabel(l) {
			return false, "label " + l
		}
	}
	// A bead mid-submission: gt done writes the READY TO LAND block before the
	// gt:ready-to-land label (internal/done markReadyToLand), so a read that
	// caught the block but not the label would otherwise sling work the landing
	// worker already owns. The hold reads the record gt done writes first,
	// which is the one a lagging read still carries (gt-kr5xv).
	if s.SubmittedForLanding {
		return false, "submitted for landing"
	}
	// The shared hold rule, over the bead's own fields. A decision recorded
	// there — a routing label, gt:needs-human (the spelling internal/land
	// writes, and the one excludedLabels never carried), the operator's
	// reservation, or a MAYOR DESIGN DECISION / do-not-redispatch ruling in
	// design or notes — takes the bead off every automatic dispatch path, so
	// the dispatcher must not re-sling it (gt-lxxo4). Reading the rule here is
	// what keeps the dispatcher's draw and the sling guard's the same line.
	// labels drops the seat-reserved ones, so a needs-pro bead reaches the pro
	// seat instead of being held as the rule holds it for a dispatcher with no
	// such seat.
	labels := withoutLabels(s.Labels, reserved)
	if why := dispatch.DispatchHoldFields(s.Status, labels, s.Assignee, s.Design, s.Notes); why != "" {
		return false, "hold: " + why
	}
	if s.Priority < 0 || s.Priority > maxPriority {
		return false, fmt.Sprintf("priority P%d outside the ceiling P%d", s.Priority, maxPriority)
	}
	return true, ""
}

// withoutLabels returns labels with every entry of drop removed, matching as
// HasLabel does (case- and space-insensitive). The input is never modified.
func withoutLabels(labels, drop []string) []string {
	if len(drop) == 0 {
		return labels
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		keep := true
		for _, d := range drop {
			if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(d)) {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, l)
		}
	}
	return out
}

// Order sorts candidates deterministically: priority (P0 first), then
// created_at (oldest first), then id.
func Order(specs []Spec) {
	sort.SliceStable(specs, func(i, j int) bool {
		a, b := specs[i], specs[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		ta, tb := parseTime(a.CreatedAt), parseTime(b.CreatedAt)
		if !ta.Equal(tb) {
			// An unparseable time sorts after every parseable one.
			if ta.IsZero() {
				return false
			}
			if tb.IsZero() {
				return true
			}
			return ta.Before(tb)
		}
		return a.ID < b.ID
	})
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Seat is one capped agent the dispatcher may sling onto.
type Seat struct {
	Agent string
	Cap   int // live polecats allowed on this agent
	Live  int // live polecats (and in-flight claims) on this agent
	// DeadHooked counts how many of Live hold the seat with no tmux session:
	// their polecat crashed and is waiting for a supervised restart. They count
	// toward Cap, and naming them explains a full seat that shows fewer live
	// sessions than its number (gt-tldj4).
	DeadHooked int
	// Label, when set, reserves the seat for beads carrying it: a bead with
	// that label is this seat's alone, and a seat without one leaves it alone
	// (the pro seat's selector, polecat_pool.pro_label).
	Label string
}

// Free reports whether the seat has room.
func (s Seat) Free() bool { return s.Agent != "" && s.Live < s.Cap }

// takes reports whether the seat may take spec: a reserving seat takes only
// its own beads, and a plain seat leaves every reserved bead to the seat that
// reserved it.
func (s Seat) takes(spec Spec, b Budget) bool {
	if s.Label != "" {
		return spec.HasLabel(s.Label)
	}
	for _, other := range b.Seats {
		if other.Label != "" && spec.HasLabel(other.Label) {
			return false
		}
	}
	return true
}

// reason renders the seat for a report: "seat <agent> <live+1>/<cap>".
func (s Seat) reason() string {
	return fmt.Sprintf("seat %s %d/%d", s.Agent, s.Live+1, s.Cap)
}

// Budget is the seat picture the seat choice runs on.
type Budget struct {
	// Seats in preference order. A spec takes the first free seat.
	Seats []Seat
	// NewestSpawn and MinSpawnGap are the stagger: no dispatch while the
	// newest polecat is younger than the gap.
	NewestSpawn time.Time
	MinSpawnGap time.Duration
	Now         time.Time
}

// Picture renders the seats for a report: "claude-sonnet 1/2, ...". A seat with
// a dead-hooked occupant names it, so a full seat has a stated reason even when
// it shows fewer live sessions than its number (gt-tldj4).
func (b Budget) Picture() string {
	parts := make([]string, 0, len(b.Seats))
	for _, s := range b.Seats {
		p := fmt.Sprintf("%s %d/%d", s.Agent, s.Live, s.Cap)
		if s.DeadHooked > 0 {
			p += fmt.Sprintf(" (%d dead-hooked)", s.DeadHooked)
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no seats"
	}
	return strings.Join(parts, ", ")
}

// ReservedLabels returns the labels the seats reserve, in seat order and
// without repeats: a bead carrying one belongs to that seat alone, and so is
// routed rather than held when Eligible reads the shared hold rule over it.
func (b Budget) ReservedLabels() []string {
	var out []string
	for _, s := range b.Seats {
		if s.Label == "" {
			continue
		}
		dup := false
		for _, seen := range out {
			if strings.EqualFold(seen, s.Label) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s.Label)
		}
	}
	return out
}

// SetLive fills each seat's Live from a count per agent. It clears DeadHooked,
// which is a breakdown of Live and is set afterwards by SetDeadHooked.
func (b *Budget) SetLive(live map[string]int) {
	for i := range b.Seats {
		b.Seats[i].Live = live[b.Seats[i].Agent]
		b.Seats[i].DeadHooked = 0
	}
}

// SetDeadHooked fills each seat's dead-hooked count from a count per agent,
// after SetLive has set Live (gt-tldj4).
func (b *Budget) SetDeadHooked(dead map[string]int) {
	for i := range b.Seats {
		b.Seats[i].DeadHooked = dead[b.Seats[i].Agent]
	}
}

// Bump records one more polecat on agent, spawned at now.
func (b *Budget) Bump(agent string, now time.Time) {
	for i := range b.Seats {
		if b.Seats[i].Agent == agent {
			b.Seats[i].Live++
		}
	}
	b.NewestSpawn = now
}

// SeatChoice is the seat decision for one spec.
type SeatChoice struct {
	// Agent is the agent to sling with; empty when Skip.
	Agent string
	// Skip means no seat now; Reason says why. A skip is not a refusal: the
	// bead stays ready and the next tick asks again.
	Skip   bool
	Reason string
}

// ChooseSeat picks the first free seat that may take spec within the budget.
// It never exceeds a cap: a seat at its cap is not a seat. A seat reserved for
// a label (the pro seat) takes only beads carrying it, and a bead carrying a
// reserved label is left for that seat — a needs-pro bead is not slung onto the
// pool's plain seat. A seat existing without room for this bead is a skip, not
// a refusal: the bead stays ready and the next tick asks again.
func ChooseSeat(b Budget, spec Spec) SeatChoice {
	if b.MinSpawnGap > 0 && !b.NewestSpawn.IsZero() {
		if age := b.Now.Sub(b.NewestSpawn); age < b.MinSpawnGap {
			return SeatChoice{Skip: true, Reason: fmt.Sprintf("min_spawn_gap: newest polecat %s old (< %s)", age.Round(time.Second), b.MinSpawnGap)}
		}
	}
	free := false
	for _, seat := range b.Seats {
		if !seat.Free() {
			continue
		}
		free = true
		if seat.takes(spec, b) {
			return SeatChoice{Agent: seat.Agent, Reason: seat.reason()}
		}
	}
	if free {
		return SeatChoice{Skip: true, Reason: "no free seat takes this bead: " + b.Picture()}
	}
	return SeatChoice{Skip: true, Reason: "seats full: " + b.Picture()}
}

// Contention retry bounds for a dispatch Dolt aborted with Error 1213.
const (
	RetryAttempts   = 4
	RetryBackoffMin = 200 * time.Millisecond
	RetryBackoffMax = 3 * time.Second
)

// IsSerializationFailure reports whether err is a Dolt serialization failure
// (MySQL Error 1213 / SQLSTATE 40001). Only an abort qualifies: Dolt rolls the
// transaction back before reporting it, so repeating the dispatch cannot
// duplicate what the failed attempt wrote.
func IsSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{"Error 1213", "serialization failure", "try restarting transaction"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// RetryOnContention runs fn up to attempts times while it fails with a Dolt
// serialization failure, sleeping a jittered, doubling backoff between tries.
// Any other error, or success, returns at once. It returns the last error and
// the number of attempts made.
func RetryOnContention(attempts int, sleep func(time.Duration), jitter *rand.Rand, fn func() error) (int, error) {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = fn()
		if err == nil || !IsSerializationFailure(err) || attempt == attempts {
			return attempt, err
		}
		sleep(backoff(attempt, jitter))
	}
	return attempts, err
}

func backoff(attempt int, jitter *rand.Rand) time.Duration {
	d := RetryBackoffMin << (attempt - 1)
	if d > RetryBackoffMax || d <= 0 {
		d = RetryBackoffMax
	}
	if jitter != nil {
		// Full jitter over [d/2, d].
		d = d/2 + time.Duration(jitter.Int63n(int64(d/2)+1))
	}
	return d
}

// ErrRetriesExhausted wraps the final contention failure.
var ErrRetriesExhausted = errors.New("dispatch lost to Dolt contention on every attempt")

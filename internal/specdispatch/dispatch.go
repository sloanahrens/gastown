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

// CrewOnlyLabel marks a bead only a person lands: a person hand-labels it, and
// the dispatcher never slings it (gt-k1g3e).
const CrewOnlyLabel = "crew-only"

// excludedLabels are the labels that keep a candidate out of the dispatcher's
// queue. It is not the dispatcher's verdict: the verdict is Eligible's, on the
// full bead, and it reads every routing decision through the shared hold rule
// (dispatch.DispatchHoldFields). A hold this list does not name — the operator
// label, gt:needs-human, a ruling in prose — still stops the dispatcher there,
// and every label it does name Eligible would have refused anyway (gt-lxxo4).
//
// dispatch.HumanOnlyLabel is named here for the skip line rather than for the
// verdict: the shared rule holds it already, and a name in this list is what
// makes the refusal a board-carried line the tick prints (gt-me800).
var excludedLabels = []string{"gt:ready-to-land", "needs-human", DispatchFailedLabel, CrewOnlyLabel, dispatch.HumanOnlyLabel}

// ExcludedLabels returns the labels that keep a bead from the dispatcher.
func ExcludedLabels() []string { return append([]string(nil), excludedLabels...) }

// boardCarriedLabels are the excludedLabels whose beads the ready board must
// still carry. The tick reports the refusal of a bead wearing one, and it
// cannot report a bead the board never fetched: these are the labels whose
// refusal the operator reads in the tick log, the rest is a silent pre-filter
// (gt-q6zoo, gt-k1g3e). Every one of them is still refused by Eligible.
var boardCarriedLabels = []string{DispatchFailedLabel, CrewOnlyLabel, dispatch.HumanOnlyLabel}

// BoardCarriedLabels returns the excluded labels whose beads the ready board
// must still carry. Before adding one, check that the tick reports it: a
// label here whose refusal nothing reads is noise on every tick.
func BoardCarriedLabels() []string { return append([]string(nil), boardCarriedLabels...) }

// ReadyQueryLabels are the excluded labels the ready query filters server-side
// (`bd ready --exclude-label`): ExcludedLabels minus the board-carried ones, so
// a bead the dispatcher would never take is not fetched at all (gt-q6zoo,
// gt-k1g3e).
func ReadyQueryLabels() []string {
	carried := BoardCarriedLabels()
	out := make([]string, 0, len(excludedLabels))
	for _, l := range ExcludedLabels() {
		if !hasLabel(carried, l) {
			out = append(out, l)
		}
	}
	return out
}

// BoardCarried reports whether s wears a label in BoardCarriedLabels(), which
// is what makes its refusal a line the operator sees rather than a drop.
func BoardCarried(s Spec) bool {
	for _, l := range BoardCarriedLabels() {
		if s.HasLabel(l) {
			return true
		}
	}
	return false
}

// hasLabel reports whether labels carries label, matching as HasLabel does
// (case- and space-insensitive).
func hasLabel(labels []string, label string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(label)) {
			return true
		}
	}
	return false
}

// DispatchTypes are the bead types the dispatcher fills a seat with: the work
// bead kinds. They are narrower than the ready board — a docs or chore bead is
// real work, but a seat filler takes only the work kinds (seat-refill's type
// whitelist, carried over with the plugin).
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
// cannot give: gt done writes the READY TO LAND block and the
// gt:ready-to-land label in one update, and the block is the half that
// survives a label that goes missing (gt-kr5xv). The
// label spec and type feature are retired and accepted-but-ignored (gt-mmsr2),
// so neither gates a candidate: the retired type name is not what admits a
// feature bead, the type whitelist is. The shape lint runs later, on the full
// bead.
//
// maxPriority is the operator's ceiling on a candidate's priority number
// (polecat_pool.max_priority, default 2): a bead numbered higher is backlog the
// operator keeps. A negative priority is an unscored bead, which is never
// dispatched.
func Eligible(s Spec, maxPriority int) (bool, string) {
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
	// A bead mid-submission: gt done writes the READY TO LAND block with the
	// gt:ready-to-land label (internal/done markReadyToLand), but a read that
	// caught the block and not the label (a lagging read, or a label that went
	// missing) would otherwise sling work the landing worker already owns. The
	// hold reads the block, the half that survives (gt-kr5xv).
	if s.SubmittedForLanding {
		return false, "submitted for landing"
	}
	// The shared hold rule, over the bead's own fields. A decision recorded
	// there — a routing label, gt:needs-human (the spelling internal/land
	// writes, and the one excludedLabels never carried), the operator's
	// reservation, or a do-not-redispatch ruling in design or notes — takes
	// the bead off every automatic dispatch path, so the dispatcher must not
	// re-sling it (gt-lxxo4). Reading the rule here is what keeps the
	// dispatcher's draw and the sling guard's the same line.
	if why := dispatch.DispatchHoldFields(s.Status, s.Labels, s.Assignee, s.Design, s.Notes); why != "" {
		return false, "hold: " + why
	}
	if s.Priority < 0 || s.Priority > maxPriority {
		return false, fmt.Sprintf("priority P%d outside the ceiling P%d", s.Priority, maxPriority)
	}
	return true, ""
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

// SeatWindow is a seat's daily closed period: Closed reports whether t falls
// inside it, and OpensAt returns the next instant at or after t when it is
// open. It is the seam the dispatcher reads a window through, so this package
// keeps its clock rule without importing the config package that parses the
// window (gt-t8new).
type SeatWindow interface {
	Closed(t time.Time) bool
	OpensAt(t time.Time) time.Time
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
	// Window is the seat's daily closed period, nil when it never shuts. It is
	// carried rather than snapshotted into a Closed flag: the choice re-reads it
	// against Budget.Now, so a seat whose window shuts between the budget's
	// construction and the choice is seen shut (gt-t8new).
	Window SeatWindow
	// PinnedOnly keeps the seat for beads labeled agent:<Agent>. The random
	// draw for an unlabelled bead passes it over, so a seat opened for one
	// named bead takes no other (gt-zor86).
	PinnedOnly bool
}

// AgentLabelPrefix starts the label that pins a bead to one agent's seats:
// agent:claude-sonnet. An operator or a bead author sets it by hand; a scheduled
// spec entry writes none, and one that names an agent is refused.
const AgentLabelPrefix = "agent:"

// PinnedAgents returns the distinct agent names carried by the bead's agent:
// labels, in label order. One name pins the bead; two or more conflict, and the
// seat choice refuses to guess between them.
func (s Spec) PinnedAgents() []string { return s.labelValues(AgentLabelPrefix) }

// closed reports whether the seat's window holds at t. A zero t is no clock at
// all: the window cannot be read, so the seat counts as shut rather than being
// drawn on a guess. A caller that carries a window must supply Budget.Now, and
// one that does not gets a skip it can see instead of a spawn the window may
// have refused (gt-t8new).
func (s Seat) closed(t time.Time) bool {
	if s.Window == nil {
		return false
	}
	if t.IsZero() {
		return true
	}
	return s.Window.Closed(t)
}

// FreeAt reports whether the seat has room and is open at t. Its window is
// read at t rather than from a snapshot, so a caller asking either side of a
// boundary sees the window shut or open as it is then (gt-t8new).
func (s Seat) FreeAt(t time.Time) bool { return s.Agent != "" && !s.closed(t) && s.Live < s.Cap }

// closedUntil renders when a closed seat opens from t: "closed until 08:00".
// With a zero clock there is no hour to name, so the reason says the window
// cannot be read instead (gt-t8new).
func (s Seat) closedUntil(t time.Time) string {
	if t.IsZero() {
		return "closed, no clock to read the window"
	}
	return "closed until " + s.Window.OpensAt(t).Format("15:04")
}

// takes reports whether the seat may take spec: a bead pinned to an agent (an
// agent:<name> label) is that agent's alone, and a seat of any other agent
// leaves it. A pinned-only seat takes nothing unlabelled, so only its own
// agent's labelled beads reach it.
func (s Seat) takes(spec Spec) bool {
	pinned := spec.PinnedAgents()
	if len(pinned) == 0 {
		return !s.PinnedOnly
	}
	return len(pinned) == 1 && pinned[0] == s.Agent
}

// reason renders the seat for a report: "seat <agent> <live+1>/<cap>".
func (s Seat) reason() string {
	return fmt.Sprintf("seat %s %d/%d", s.Agent, s.Live+1, s.Cap)
}

// Budget is the seat picture the seat choice runs on.
type Budget struct {
	// Seats in the dispatcher's list order. An unlabelled spec draws at random
	// among the free seats that take it; the order decides only which seat an
	// agent named twice keeps.
	Seats []Seat
	// MaxTotal caps live polecats across every seat, where a Seat's Cap caps
	// one agent's. 0 is no total cap (polecat_pool.max_total absent).
	MaxTotal int
	// LiveTotal is the count across every agent. Seats' Live is per agent and
	// a town's live polecats are not all on a seat, so the total is counted
	// from the roster rather than summed from the seats. SetLive fills it and
	// Bump adds to it, so the total moves with the seats.
	LiveTotal int
	// NewestSpawn and MinSpawnGap are the stagger: no dispatch while the
	// newest polecat is younger than the gap.
	NewestSpawn time.Time
	MinSpawnGap time.Duration
	// Now is the tick's clock. The seats' windows are read against it at choice
	// time, and the picture and the skip reasons render from it, so a caller
	// that refreshes it before each choice sees a window that shut mid-tick
	// (gt-t8new).
	Now time.Time
	// Rand drives the draw among free seats. Nil draws from the package
	// source; a seeded source makes a test's choice reproducible.
	Rand *rand.Rand
}

// Picture renders the seats for a report: "claude-sonnet 1/2, ...". A seat with
// a dead-hooked occupant names it, so a full seat has a stated reason even when
// it shows fewer live sessions than its number (gt-tldj4). A town with a total
// cap names it too, so the picture a full town is reported with holds the cap
// that held it rather than only the seats under it.
func (b Budget) Picture() string {
	parts := make([]string, 0, len(b.Seats))
	for _, s := range b.Seats {
		p := fmt.Sprintf("%s %d/%d", s.Agent, s.Live, s.Cap)
		if s.DeadHooked > 0 {
			p += fmt.Sprintf(" (%d dead-hooked)", s.DeadHooked)
		}
		if s.closed(b.Now) {
			p += " (" + s.closedUntil(b.Now) + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no seats"
	}
	if b.MaxTotal > 0 {
		parts = append(parts, fmt.Sprintf("total %d/%d", b.LiveTotal, b.MaxTotal))
	}
	return strings.Join(parts, ", ")
}

// SetLive fills each seat's Live from a count per agent, and LiveTotal from the
// whole count: a live polecat on an agent the seats do not name is still a live
// polecat, so a total cap has to see it. It clears DeadHooked, which is a
// breakdown of Live and is set afterwards by SetDeadHooked.
func (b *Budget) SetLive(live map[string]int) {
	total := 0
	for i := range b.Seats {
		b.Seats[i].Live = live[b.Seats[i].Agent]
		b.Seats[i].DeadHooked = 0
	}
	for _, n := range live {
		total += n
	}
	b.LiveTotal = total
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
	b.LiveTotal++
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

// ChooseSeat picks a free seat that may take spec within the budget. It never
// exceeds a cap: a seat at its cap is not a seat, and a town at its total is a
// town with no free seat at all. An unlabelled bead draws uniformly among the
// free seats of every model in the mix, so no model is preferred by list
// order; a bead pinned with an agent:<name> label may take only that agent's
// seat. Each seat's window is read against b.Now here, not trusted from a
// snapshot, so a seat that shut since the budget was built is not drawn. A seat
// existing without room for this bead is a skip, not a refusal: the bead stays
// ready and the next tick asks again.
func ChooseSeat(b Budget, spec Spec) SeatChoice {
	// The town's total is asked before the seats are, because it caps every
	// seat at once: a town at its total has no free seat whatever the seats
	// say, and the skip names the cap the operator set rather than a seat's
	// (gt-bcpj5).
	if b.MaxTotal > 0 && b.LiveTotal >= b.MaxTotal {
		return SeatChoice{Skip: true, Reason: fmt.Sprintf("over the town total: %d/%d live polecats across every seat", b.LiveTotal, b.MaxTotal)}
	}
	if b.MinSpawnGap > 0 && !b.NewestSpawn.IsZero() {
		if age := b.Now.Sub(b.NewestSpawn); age < b.MinSpawnGap {
			return SeatChoice{Skip: true, Reason: fmt.Sprintf("min_spawn_gap: newest polecat %s old (< %s)", age.Round(time.Second), b.MinSpawnGap)}
		}
	}
	pinned := spec.PinnedAgents()
	if len(pinned) > 1 {
		return SeatChoice{Skip: true, Reason: fmt.Sprintf("conflicting %s labels (%s): a bead names one agent", AgentLabelPrefix, strings.Join(pinned, ", "))}
	}
	open := make([]Seat, 0, len(b.Seats))
	var passedOver []string
	for _, seat := range b.Seats {
		if !seat.FreeAt(b.Now) {
			continue
		}
		if seat.takes(spec) {
			open = append(open, seat)
		} else {
			passedOver = append(passedOver, seat.Agent)
		}
	}
	if len(open) > 0 {
		seat := open[b.pick(len(open))]
		return SeatChoice{Agent: seat.Agent, Reason: seat.reason()}
	}
	if len(pinned) == 1 {
		// Never another agent's seat: the label says which model may run the
		// bead, so a full or missing seat is a wait, not a fallback.
		left := ""
		if len(passedOver) > 0 {
			left = fmt.Sprintf(" (free seats left alone: %s)", strings.Join(passedOver, ", "))
		}
		for _, seat := range b.Seats {
			if seat.Agent == pinned[0] {
				if seat.closed(b.Now) {
					return SeatChoice{Skip: true, Reason: fmt.Sprintf("waiting for a %s seat, %s%s: %s", pinned[0], seat.closedUntil(b.Now), left, b.Picture())}
				}
				return SeatChoice{Skip: true, Reason: fmt.Sprintf("waiting for a %s seat%s: %s", pinned[0], left, b.Picture())}
			}
		}
		return SeatChoice{Skip: true, Reason: fmt.Sprintf("pinned to %s, which has no seat in the pool%s: %s", pinned[0], left, b.Picture())}
	}
	return SeatChoice{Skip: true, Reason: "seats full: " + b.Picture()}
}

// pick draws an index in [0,n) for the seat choice: the budget's own source
// when it has one, so a test's seed decides the draw, and the package source
// otherwise.
func (b Budget) pick(n int) int {
	if b.Rand != nil {
		return b.Rand.Intn(n)
	}
	return rand.Intn(n)
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

// FreeSeats is how many polecats the tick may still sling: the open seats'
// cap minus their live count, bounded by max_total when it is set. It is the
// tick's default per-tick limit, so a tick fills the pool rather than a
// fixed number of seats.
func (b Budget) FreeSeats() int {
	free := 0
	for _, s := range b.Seats {
		if s.FreeAt(b.Now) {
			free += s.Cap - s.Live
		}
	}
	if b.MaxTotal > 0 && b.MaxTotal-b.LiveTotal < free {
		free = b.MaxTotal - b.LiveTotal
	}
	if free < 0 {
		return 0
	}
	return free
}

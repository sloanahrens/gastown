package specdispatch

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// HostSafetyPrompt is the instruction every spec dispatch carries (stored as
// the sling args, which the polecat reads with its work).
const HostSafetyPrompt = "HOST SAFETY (spec dispatcher): any test of install or uninstall MUST use a temporary " +
	"INSTALL_DIR (mktemp -d) and never the real ~/.local/bin. Never run make install, gt dolt cleanup, " +
	"or rm -rf outside your worktree on this host."

// Labels that keep a bead away from the dispatcher whatever its lint says.
var excludedLabels = []string{"gt:ready-to-land", "needs-human", "needs-mayor-review", DispatchFailedLabel}

// ExcludedLabels returns the labels that keep a bead from the dispatcher.
func ExcludedLabels() []string { return append([]string(nil), excludedLabels...) }

// Eligible reports whether a ready bead is the dispatcher's to consider, and
// why not when it is not. It is the candidate filter: status open, unassigned,
// label spec, type feature, and none of the excluded labels or a deferred
// status. The lint itself runs later, on the full bead.
func Eligible(s Spec) (bool, string) {
	status := strings.ToLower(strings.TrimSpace(s.Status))
	switch {
	case status == "deferred":
		return false, "deferred"
	case status != "open":
		return false, "status " + status
	case strings.TrimSpace(s.Assignee) != "":
		return false, "assigned to " + s.Assignee
	case !s.HasLabel(SpecLabel):
		return false, "no spec label"
	case !strings.EqualFold(strings.TrimSpace(s.Type), SpecType):
		return false, "type " + s.Type
	}
	for _, l := range excludedLabels {
		if s.HasLabel(l) {
			return false, "label " + l
		}
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

// Seat is one capped agent the dispatcher may sling onto.
type Seat struct {
	Agent string
	Cap   int // live polecats allowed on this agent
	Live  int // live polecats (and in-flight claims) on this agent
}

// Free reports whether the seat has room.
func (s Seat) Free() bool { return s.Agent != "" && s.Live < s.Cap }

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

// Picture renders the seats for a report: "claude-sonnet 1/2, ...".
func (b Budget) Picture() string {
	parts := make([]string, 0, len(b.Seats))
	for _, s := range b.Seats {
		parts = append(parts, fmt.Sprintf("%s %d/%d", s.Agent, s.Live, s.Cap))
	}
	if len(parts) == 0 {
		return "no seats"
	}
	return strings.Join(parts, ", ")
}

// SetLive fills each seat's Live from a count per agent.
func (b *Budget) SetLive(live map[string]int) {
	for i := range b.Seats {
		b.Seats[i].Live = live[b.Seats[i].Agent]
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

// ChooseSeat picks the first free seat for a clean spec within the budget. It
// never exceeds a cap: a seat at its cap is not a seat. ChooseSeat assumes
// Lint already passed, which bounds size.
func ChooseSeat(b Budget) SeatChoice {
	if b.MinSpawnGap > 0 && !b.NewestSpawn.IsZero() {
		if age := b.Now.Sub(b.NewestSpawn); age < b.MinSpawnGap {
			return SeatChoice{Skip: true, Reason: fmt.Sprintf("min_spawn_gap: newest polecat %s old (< %s)", age.Round(time.Second), b.MinSpawnGap)}
		}
	}
	for _, seat := range b.Seats {
		if seat.Free() {
			return SeatChoice{Agent: seat.Agent, Reason: fmt.Sprintf("seat %s %d/%d", seat.Agent, seat.Live+1, seat.Cap)}
		}
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

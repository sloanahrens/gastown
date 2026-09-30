package specdispatch

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// Seat classes. A hooked seat runs a Claude-backed agent that receives the
// town's managed settings and guard hooks. A hookless seat is anything else —
// a non-claude provider, or a claude CLI pointed at another backend
// (deepseek-flash, the local model) — and until gt-be0z lands it runs with
// zero guards.
type Class string

const (
	ClassHooked   Class = "hooked"
	ClassHookless Class = "hookless"
)

// ClassifyAgent names an agent's seat class from the town's agent table.
//
// Hooked means: provider claude (or no provider and the claude command), and
// no ANTHROPIC_BASE_URL override, i.e. the real Claude backend. Every other
// agent is hookless, including a claude-CLI wrapper over another backend: that
// is the shape of the 2026-09-30 04:14 incident, where a deepseek-flash
// polecat ran the uninstall it was documenting on the host. An agent name the
// table does not carry is a built-in preset; it is hooked only when it is a
// claude preset ("claude", "claude-*"). An empty name is the role default,
// which the caller resolves before asking.
func ClassifyAgent(name string, agents map[string]*config.RuntimeConfig) Class {
	name = strings.TrimSpace(name)
	if rc, ok := agents[name]; ok && rc != nil {
		provider := strings.TrimSpace(rc.Provider)
		command := strings.TrimSpace(rc.Command)
		claude := provider == "claude" || (provider == "" && (command == "claude" || command == ""))
		if claude && strings.TrimSpace(rc.Env["ANTHROPIC_BASE_URL"]) == "" {
			return ClassHooked
		}
		return ClassHookless
	}
	if name == "claude" || strings.HasPrefix(name, "claude-") {
		return ClassHooked
	}
	return ClassHookless
}

// hostSafetyTerms are the words that put a spec on a hooked seat only. Match
// is case-insensitive substring, deliberately broad: "install" also covers
// uninstall, reinstall and make install, and a false positive only costs a
// hooked seat.
var hostSafetyTerms = []string{
	"install",
	"~/.local/bin",
	".local/bin",
	"install_dir",
	"dolt cleanup",
	"rm -rf",
}

// HostSafetyTerm reports the first host-safety term the spec's text mentions,
// or "" when none does. Title, description, notes, design and acceptance are
// all read: a hookless polecat reads every one of them.
func HostSafetyTerm(s Spec) string {
	text := strings.ToLower(strings.Join([]string{s.Title, s.Description, s.Notes, s.Design, s.Acceptance}, "\n"))
	for _, term := range hostSafetyTerms {
		if strings.Contains(text, term) {
			return term
		}
	}
	return ""
}

// HostSafetyPrompt is the instruction a host-safety spec's dispatch carries
// (stored as the sling args, which the polecat reads with its work).
const HostSafetyPrompt = "HOST SAFETY (spec dispatcher): this spec touches install/uninstall or host paths. " +
	"Any test of install or uninstall MUST use a temporary INSTALL_DIR (mktemp -d) and never the real " +
	"~/.local/bin. Never run make install, gt dolt cleanup, or rm -rf outside your worktree on this host."

// Labels that keep a bead away from the dispatcher whatever its lint says.
var excludedLabels = []string{"gt:ready-to-land", "needs-human", "needs-mayor-review", DispatchFailedLabel}

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

// Budget is the seat picture the seat choice runs on.
type Budget struct {
	HookedAgent   string // agent a hooked seat runs
	HookedCap     int    // live hooked polecats allowed
	HookedLive    int
	HooklessAgent string // agent a hookless seat runs
	HooklessCap   int    // live hookless polecats allowed (0 = none)
	HooklessLive  int
	// PreferHooked sends a spec that may take either class to a hooked seat
	// first; otherwise the hookless seat is tried first.
	PreferHooked bool
	// NewestSpawn and MinSpawnGap are the stagger: no dispatch while the
	// newest polecat is younger than the gap.
	NewestSpawn time.Time
	MinSpawnGap time.Duration
	Now         time.Time
}

// SeatChoice is the seat decision for one spec.
type SeatChoice struct {
	// Agent is the agent to sling with; empty when Skip.
	Agent string
	Class Class
	// HostSafety is the term that forced a hooked seat, or "".
	HostSafety string
	// Skip means no seat now; Reason says why. A skip is not a refusal: the
	// bead stays ready and the next tick asks again.
	Skip   bool
	Reason string
}

// ChooseSeat picks the seat for a clean spec within the budget. It never
// exceeds a cap: a class at its cap is not a seat, and a spec that must run
// hooked is skipped rather than sent to a hookless seat.
func ChooseSeat(s Spec, b Budget) SeatChoice {
	if b.MinSpawnGap > 0 && !b.NewestSpawn.IsZero() {
		if age := b.Now.Sub(b.NewestSpawn); age < b.MinSpawnGap {
			return SeatChoice{Skip: true, Reason: fmt.Sprintf("min_spawn_gap: newest polecat %s old (< %s)", age.Round(time.Second), b.MinSpawnGap)}
		}
	}
	hookedFree := b.HookedAgent != "" && b.HookedLive < b.HookedCap
	hooklessFree := b.HooklessAgent != "" && b.HooklessLive < b.HooklessCap
	hooked := SeatChoice{Agent: b.HookedAgent, Class: ClassHooked}
	hookless := SeatChoice{Agent: b.HooklessAgent, Class: ClassHookless}

	if term := HostSafetyTerm(s); term != "" {
		if !hookedFree {
			return SeatChoice{Skip: true, HostSafety: term, Reason: fmt.Sprintf("host-safety (%q) needs a hooked seat: %d/%d in use", term, b.HookedLive, b.HookedCap)}
		}
		hooked.HostSafety = term
		hooked.Reason = fmt.Sprintf("host-safety (%q) -> hooked seat %d/%d", term, b.HookedLive+1, b.HookedCap)
		return hooked
	}

	first, firstFree, second, secondFree := hookless, hooklessFree, hooked, hookedFree
	firstLive, firstCap, secondLive, secondCap := b.HooklessLive, b.HooklessCap, b.HookedLive, b.HookedCap
	if b.PreferHooked {
		first, firstFree, second, secondFree = hooked, hookedFree, hookless, hooklessFree
		firstLive, firstCap, secondLive, secondCap = b.HookedLive, b.HookedCap, b.HooklessLive, b.HooklessCap
	}
	switch {
	case firstFree:
		first.Reason = fmt.Sprintf("%s seat %d/%d", first.Class, firstLive+1, firstCap)
		return first
	case secondFree:
		second.Reason = fmt.Sprintf("%s seat %d/%d", second.Class, secondLive+1, secondCap)
		return second
	}
	return SeatChoice{Skip: true, Reason: fmt.Sprintf("seats full: hooked %d/%d, hookless %d/%d", b.HookedLive, b.HookedCap, b.HooklessLive, b.HooklessCap)}
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

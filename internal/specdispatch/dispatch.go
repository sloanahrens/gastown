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

// Seat classes, keyed on the agent's provider in settings/config.json and
// never on its model or name. A hooked seat runs a provider=claude agent: it
// gets the town's managed settings and guard hooks (deepseek-flash is one —
// a claude CLI over another backend still runs the claude hook set). A
// hookless seat runs any other provider, which until gt-be0z lands spawns
// with zero guards. Today every configured polecat agent is provider=claude;
// the hookless class exists for future non-claude providers.
type Class string

const (
	ClassHooked   Class = "hooked"
	ClassHookless Class = "hookless"
)

// ClassifyAgent names an agent's seat class from the town's agent table.
//
// An agent in the table is hooked iff its provider is "claude"; an empty
// provider is resolved from the command, since the town runtime default is
// the claude CLI. An agent the table does not carry is looked up as a
// built-in preset, and only the claude preset is hooked. Anything unknown is
// hookless: a class that cannot be established does not get the guarded one.
func ClassifyAgent(name string, agents map[string]*config.RuntimeConfig) Class {
	name = strings.TrimSpace(name)
	if rc, ok := agents[name]; ok && rc != nil {
		provider := strings.TrimSpace(rc.Provider)
		if provider == "" {
			command := strings.TrimSpace(rc.Command)
			if command == "" || command == "claude" {
				provider = "claude"
			}
		}
		if provider == "claude" {
			return ClassHooked
		}
		return ClassHookless
	}
	if preset := config.GetAgentPresetByName(name); preset != nil && string(preset.Name) == "claude" {
		return ClassHooked
	}
	return ClassHookless
}

// HostSafeLabel is the operator's explicit statement that a spec may run on a
// non-claude (hookless) seat. Without it every spec takes the hooked class.
const HostSafeLabel = "host-safe"

// Host-safety scan. It is defense in depth, not the gate: the default class is
// hooked, and the scan can only force a host-safe spec back onto it, never the
// reverse. It matches command words and paths as tokens, not raw substrings,
// so "installer docs" is not an install.
var (
	// riskyWords are command words that touch the host when they appear as a
	// whole token.
	riskyWords = map[string]bool{
		"install": true, "installs": true, "installing": true, "installed": true,
		"uninstall": true, "uninstalls": true, "uninstalling": true, "uninstalled": true,
		"reinstall": true, "reinstalling": true,
		"install_dir": true, "$install_dir": true, "${install_dir}": true,
		"shred": true, "mkfs": true, "dd": true, "sudo": true, "srm": true,
	}
	// riskyPathFragments are host paths a token may contain.
	riskyPathFragments = []string{
		".local/bin", "/usr/local/bin", "/opt/homebrew/bin", "/etc/",
		".zshrc", ".zprofile", ".bashrc", ".bash_profile", ".profile",
		"launchagents", "launchdaemons",
	}
	// riskyPairs are two-token commands: first token, then a predicate on the
	// next one.
	riskyPairs = []struct {
		first string
		next  func(string) bool
		name  string
	}{
		{"make", func(t string) bool { return t == "install" || t == "uninstall" }, "make install"},
		{"dolt", func(t string) bool { return t == "cleanup" }, "dolt cleanup"},
		{"rm", recursiveFlag, "rm -r"},
		{"chmod", recursiveFlag, "chmod -R"},
		{"chown", recursiveFlag, "chown -R"},
		{"find", func(t string) bool { return t == "/" || t == "~" || t == "$home" }, "find /"},
		{">", func(t string) bool { return strings.HasPrefix(t, "/etc") || strings.HasPrefix(t, "~/.") }, "> host path"},
		{">>", func(t string) bool { return strings.HasPrefix(t, "/etc") || strings.HasPrefix(t, "~/.") }, ">> host path"},
	}
)

// recursiveFlag reports whether a token is a short-flag cluster with r/R
// (-r, -rf, -fr, -Rf, -R) or --recursive.
func recursiveFlag(t string) bool {
	if t == "--recursive" {
		return true
	}
	if !strings.HasPrefix(t, "-") || strings.HasPrefix(t, "--") {
		return false
	}
	return strings.ContainsAny(t[1:], "rR")
}

// hostTokens splits text into lower-case tokens on whitespace, trimming the
// quoting and punctuation prose wraps around commands. A redirect glued to its
// target (">/etc/hosts") is split into ">" and the target.
func hostTokens(text string) []string {
	var out []string
	for _, raw := range strings.Fields(text) {
		t := strings.Trim(raw, "`'\"()[]{},;:!?")
		if t == "" {
			continue
		}
		for _, op := range []string{">>", ">"} {
			if !strings.HasPrefix(t, op) {
				continue
			}
			if len(t) > len(op) {
				out = append(out, op)
				t = t[len(op):]
			}
			break
		}
		t = strings.TrimSuffix(t, ".")
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// HostSafetyTerm reports the first host-touching command or path the spec's
// text names, or "" when none. Title, description, notes, design and
// acceptance are all read: a polecat reads every one of them. Case matters
// only for chmod/chown -R, which is matched as a recursive flag either way.
func HostSafetyTerm(s Spec) string {
	text := strings.Join([]string{s.Title, s.Description, s.Notes, s.Design, s.Acceptance}, "\n")
	raw := hostTokens(text)
	for i, rt := range raw {
		t := strings.ToLower(rt)
		if riskyWords[t] {
			return t
		}
		for _, frag := range riskyPathFragments {
			if strings.Contains(t, frag) {
				return frag
			}
		}
		if i+1 < len(raw) {
			next := raw[i+1]
			for _, p := range riskyPairs {
				if t == p.first && (p.next(next) || p.next(strings.ToLower(next))) {
					return p.name
				}
			}
		}
	}
	return ""
}

// HostSafetyPrompt is the instruction every spec dispatch carries (stored as
// the sling args, which the polecat reads with its work). It rides on every
// dispatch rather than only on scanned ones, because the scan can miss.
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
	Class Class
	Cap   int // live polecats allowed on this agent
	Live  int // live polecats (and in-flight claims) on this agent
}

// Free reports whether the seat has room.
func (s Seat) Free() bool { return s.Agent != "" && s.Live < s.Cap }

// Budget is the seat picture the seat choice runs on.
type Budget struct {
	// Seats in preference order. A spec takes the first free seat its class
	// rule allows.
	Seats []Seat
	// NewestSpawn and MinSpawnGap are the stagger: no dispatch while the
	// newest polecat is younger than the gap.
	NewestSpawn time.Time
	MinSpawnGap time.Duration
	Now         time.Time
}

// Picture renders the seats for a report: "claude-sonnet 1/2 hooked, ...".
func (b Budget) Picture() string {
	parts := make([]string, 0, len(b.Seats))
	for _, s := range b.Seats {
		parts = append(parts, fmt.Sprintf("%s %d/%d %s", s.Agent, s.Live, s.Cap, s.Class))
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
	Class Class
	// HostSafety is the risky term the scan found, or "".
	HostSafety string
	// OverrodeHostSafe is set when the spec carries the host-safe label but
	// the scan found HostSafety, so the label was ignored. The caller
	// annotates the bead once.
	OverrodeHostSafe bool
	// Skip means no seat now; Reason says why. A skip is not a refusal: the
	// bead stays ready and the next tick asks again.
	Skip   bool
	Reason string
}

// ChooseSeat picks the seat for a clean spec within the budget. It fails
// closed: a spec may use only hooked-class seats unless it carries the
// host-safe label and the scan finds no host-touching term; only then may it
// also use a hookless seat. It never exceeds a cap: a seat at its cap is not a
// seat, and a spec limited to hooked seats waits rather than taking a
// hookless one. ChooseSeat assumes Lint already passed, which bounds size.
func ChooseSeat(s Spec, b Budget) SeatChoice {
	term := HostSafetyTerm(s)
	hostSafe := s.HasLabel(HostSafeLabel)
	overrode := hostSafe && term != ""
	hookedOnly := !hostSafe || term != ""
	skip := func(reason string) SeatChoice {
		return SeatChoice{Skip: true, HostSafety: term, OverrodeHostSafe: overrode, Reason: reason}
	}
	if b.MinSpawnGap > 0 && !b.NewestSpawn.IsZero() {
		if age := b.Now.Sub(b.NewestSpawn); age < b.MinSpawnGap {
			return skip(fmt.Sprintf("min_spawn_gap: newest polecat %s old (< %s)", age.Round(time.Second), b.MinSpawnGap))
		}
	}
	why := HostSafeLabel
	if hookedOnly {
		why = "no " + HostSafeLabel + " label"
		if term != "" {
			why = fmt.Sprintf("mentions %q", term)
			if overrode {
				why += ", " + HostSafeLabel + " label overridden"
			}
		}
	}
	for _, seat := range b.Seats {
		if hookedOnly && seat.Class != ClassHooked {
			continue
		}
		if !seat.Free() {
			continue
		}
		return SeatChoice{Agent: seat.Agent, Class: seat.Class, HostSafety: term, OverrodeHostSafe: overrode,
			Reason: fmt.Sprintf("%s seat %s %d/%d (%s)", seat.Class, seat.Agent, seat.Live+1, seat.Cap, why)}
	}
	scope := "seats full"
	if hookedOnly {
		scope = "hooked seats only (" + why + "), all full"
	}
	return skip(fmt.Sprintf("%s: %s", scope, b.Picture()))
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

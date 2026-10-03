package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/tmuxsweep"
)

// testLeakEventWindow is how far back the check reads the event log. A rig
// removed since, or an actor spelling the town has retired, keeps its old lines
// until events_prune drops them; the window keeps those from reading as leaks
// for the life of the file.
const testLeakEventWindow = 24 * time.Hour

// testLeakTempGrace is how old an unclaimed .tmp file must be before it counts
// as residue rather than a write in flight.
const testLeakTempGrace = 10 * time.Minute

// TestLeakCheck reports state that test runs leaked into the live town: events
// from fixture actors, abandoned temp files on the town's watched surface, and
// test-named sessions on the town's tmux server. It replaces the hermetic test
// harness's before/after tripwire, which failed whole test runs whenever
// anything in the town changed while they ran (gt-ik4a1.3). Orphan test
// databases and abandoned test tmux servers have their own checks
// (dolt-orphaned-databases, tmux-test-socket).
type TestLeakCheck struct {
	BaseCheck

	nowForTest          func() time.Time
	townSessionsForTest func() ([]string, error) // nil → the town tmux socket
	fileAgeForTest      func(path string) time.Duration
}

// NewTestLeakCheck creates a check for test state leaked into the live town.
func NewTestLeakCheck() *TestLeakCheck {
	return &TestLeakCheck{
		BaseCheck: BaseCheck{
			CheckName:        "test-leaks",
			CheckDescription: "Detect test fixture state leaked into the live town",
			CheckCategory:    CategoryCleanup,
		},
	}
}

func (c *TestLeakCheck) now() time.Time {
	if c.nowForTest != nil {
		return c.nowForTest()
	}
	return time.Now()
}

func (c *TestLeakCheck) townSessions() ([]string, error) {
	if c.townSessionsForTest != nil {
		return c.townSessionsForTest()
	}
	return tmux.NewTmux().ListSessions()
}

// fileAge is how long ago the file was last written. A file that cannot be
// stat'd reads as brand new, which keeps it out of the report.
func (c *TestLeakCheck) fileAge(path string) time.Duration {
	if c.fileAgeForTest != nil {
		return c.fileAgeForTest(path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return c.now().Sub(info.ModTime())
}

// Run reads the town's watched directories, the recent event log and the
// town tmux server's session list.
func (c *TestLeakCheck) Run(ctx *CheckContext) *CheckResult {
	var leaks []string
	leaks = append(leaks, c.abandonedTemps(ctx.TownRoot)...)
	leaks = append(leaks, c.fixtureEvents(ctx.TownRoot)...)

	sessions, sessErr := c.townSessions()
	var testSessions []string
	for _, s := range sessions {
		if tmuxsweep.IsTestSessionName(s) {
			testSessions = append(testSessions, s)
		}
	}
	sort.Strings(testSessions)
	for _, s := range testSessions {
		leaks = append(leaks, fmt.Sprintf("test-named session on the town tmux server: %s", s))
	}

	if len(leaks) == 0 {
		if sessErr != nil {
			// A pass the check did not earn is the report it must not give.
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusSkipped,
				Message: "unknown: could not list the town tmux sessions",
				Details: []string{sessErr.Error()},
			}
		}
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "No test state leaked into the town"}
	}
	if sessErr != nil {
		leaks = append(leaks, fmt.Sprintf("town tmux sessions not checked: %v", sessErr))
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d sign(s) of test state leaked into the town", len(leaks)),
		Details: leaks,
		FixHint: "Find the test that wrote it and route its writes through testutil's sandbox town; " +
			"a session named gt-test-* reads as a phantom polecat (gt-2bj)",
	}
}

// watchedDirs are the town directories whose direct children the check reads,
// relative to the town root: the root itself, the tracker and the Dolt data.
var watchedDirs = []string{".", ".beads", ".dolt-data"}

// atomicWriteTemps maps each live target file on the watched surface to the
// suffixes its atomic writers append while building a replacement. Suffixes
// are the producers' own exported constants, so a renamed suffix breaks the
// build here instead of silently going stale.
var atomicWriteTemps = map[string][]string{
	events.EventsFile: {events.PruneTempSuffix},
}

// atomicTempPrefixes are CreateTemp patterns that produce temps on the watched
// surface (beads.WriteRoutes builds .routes-<random>.tmp in .beads).
var atomicTempPrefixes = []string{beads.RoutesTempPrefix}

// knownAtomicTemp reports whether a .tmp name belongs to a known town writer.
func knownAtomicTemp(base string) bool {
	for file, suffixes := range atomicWriteTemps {
		for _, suffix := range suffixes {
			if base == file+suffix {
				return true
			}
		}
	}
	for _, prefix := range atomicTempPrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// abandonedTemps reports .tmp files on the watched surface that no known town
// writer produces and that have sat past the grace period. A known writer's
// crash residue is its own to remove (events_prune does), and bd's .~ export
// temps are bd's.
func (c *TestLeakCheck) abandonedTemps(townRoot string) []string {
	var leaks []string
	for _, dir := range watchedDirs {
		entries, err := os.ReadDir(filepath.Join(townRoot, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".tmp") || knownAtomicTemp(name) {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(dir, name))
			if c.fileAge(filepath.Join(townRoot, rel)) < testLeakTempGrace {
				continue
			}
			leaks = append(leaks, fmt.Sprintf("abandoned temp file: %s", rel))
		}
	}
	sort.Strings(leaks)
	return leaks
}

// fixtureEvents reports recent events whose actor the town does not know: the
// first path segment is neither a registered rig nor a built-in town actor.
// Fixture actors like "myr/mycat" are what a test writing to the live event log
// leaves (gt-x9o). Events a town process authored are tolerated: session_death
// names a tmux session as the actor, from the daemon's crash detection
// (gt-d9423) or gt doctor's and gt down's cleanup (gt-rqajq).
func (c *TestLeakCheck) fixtureEvents(townRoot string) []string {
	data, err := os.ReadFile(filepath.Join(townRoot, events.EventsFile)) //nolint:gosec // path is under the town root
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	// The last element followed no newline: empty, or a line a writer is
	// still appending.
	lines = lines[:len(lines)-1]

	known := map[string]bool{}
	for _, p := range builtinActorPrefixes {
		known[p] = true
	}
	for name := range loadRigNames(filepath.Join(townRoot, "mayor", "rigs.json")) {
		known[name] = true
	}
	since := c.now().Add(-testLeakEventWindow)

	var leaks []string
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			leaks = append(leaks, eventLeaks(line, known, since)...)
		}
	}
	return leaks
}

// eventLeaks classifies one complete event line.
func eventLeaks(line string, known map[string]bool, since time.Time) []string {
	var ev struct {
		Timestamp string `json:"ts"`
		Actor     string `json:"actor"`
		Type      string `json:"type"`
		Payload   struct {
			Caller string `json:"caller"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return []string{fmt.Sprintf("unparseable event in %s: %.120s", events.EventsFile, line)}
	}
	if ts, err := time.Parse(time.RFC3339, ev.Timestamp); err == nil && ts.Before(since) {
		return nil
	}
	if events.IsTownProcessSessionCaller(ev.Payload.Caller) {
		return nil
	}
	prefix, _, _ := strings.Cut(strings.TrimSuffix(ev.Actor, "/"), "/")
	if known[prefix] {
		return nil
	}
	return []string{fmt.Sprintf("event with unknown actor %q (type %s, ts %s) in %s",
		ev.Actor, ev.Type, ev.Timestamp, events.EventsFile)}
}

// builtinActorPrefixes are town-level actors that are always legitimate.
//
// gt-9pn: this list went stale by hand twice (gt-ro0 "unknown", gt-kvc "dog"),
// so internal/cmd's TestDetectActorOutputsToleratedByTestLeakCheck iterates
// every internal/cmd.Role and fails if RoleInfo.ActorString() produces a value
// not listed here.
//
// "mayor", "deacon", "witness", "refinery", "polecat", "crew", "dog",
// "unknown" are the bare (no-rig) actor strings for their Roles.
//
// gt-jna: RoleBoot has two construction paths, both legitimate:
// RoleInfo.ActorString() returns "deacon-boot" (beads attribution) and
// getAgentIdentity() returns "boot" (the session_start actor Boot's gt prime
// writes). Both stay; TestGetAgentIdentityOutputsToleratedByTestLeakCheck
// covers the second path.
//
// The rest come from other construction paths:
//   - "overseer": detectSender()'s fallback mail actor (internal/cmd/mail_identity.go).
//   - "gt": events.ActorGt, town infrastructure events with no owning agent.
//   - "daemon": events.ActorDaemon, daemon-originated events.
//   - "convoy": a legacy "convoy/<id>" mail actor, still tolerated for
//     messages and fixtures that carry it.
var builtinActorPrefixes = []string{
	"mayor", "deacon", "boot", "deacon-boot", "witness", "refinery", "polecat",
	"crew", "dog", "unknown", "overseer", events.ActorGt, events.ActorDaemon, "convoy",
}

// BuiltinActorPrefixes returns a copy of the always-legitimate town-level
// actor prefixes, so internal/cmd can cross-check its actor construction
// against the check's tolerances.
func BuiltinActorPrefixes() []string {
	return append([]string(nil), builtinActorPrefixes...)
}

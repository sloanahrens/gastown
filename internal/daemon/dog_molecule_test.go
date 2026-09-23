package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

func TestParseWispID(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
	}{
		{
			name:   "standard wisp output",
			input:  "✓ Spawned wisp: gt-wisp-abc123 — Reap stale wisps",
			wantID: "gt-wisp-abc123",
		},
		{
			name:   "wisp ID with ANSI codes",
			input:  "\033[32m✓\033[0m Spawned wisp: \033[1mgt-wisp-xyz789\033[0m — Title",
			wantID: "gt-wisp-xyz789",
		},
		{
			name:   "empty output",
			input:  "",
			wantID: "",
		},
		{
			name:   "no wisp ID in output",
			input:  "Error: something went wrong",
			wantID: "",
		},
		{
			name:   "wisp ID at end of line",
			input:  "Created gt-wisp-def456",
			wantID: "gt-wisp-def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWispID(tt.input)
			if got != tt.wantID {
				t.Errorf("parseWispID(%q) = %q, want %q", tt.input, got, tt.wantID)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no ANSI", "hello", "hello"},
		{"color code", "\033[32mgreen\033[0m", "green"},
		{"bold", "\033[1mbold\033[0m", "bold"},
		{"multiple codes", "\033[32m✓\033[0m \033[1mtext\033[0m", "✓ text"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseChildrenJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "bare array",
			input:   `[{"id":"a","title":"Probe","status":"open"}]`,
			wantIDs: []string{"a"},
		},
		{
			name:    "map wrapper from bd show",
			input:   `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"},{"id":"hq-wisp-b","title":"Report","status":"open"}]}`,
			wantIDs: []string{"hq-wisp-a", "hq-wisp-b"},
		},
		{
			name:    "empty map wrapper",
			input:   `{"hq-wisp-root":[]}`,
			wantIDs: []string{},
		},
		{
			name:    "schema metadata with children",
			input:   `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}],"schema_version":1}`,
			wantIDs: []string{"hq-wisp-a"},
		},
		{
			name:    "schema metadata with empty children",
			input:   `{"hq-wisp-root":[],"schema_version":1}`,
			wantIDs: []string{},
		},
		{
			name:    "multiple child arrays are deterministic",
			input:   `{"hq-wisp-b":[{"id":"b-step","title":"Report","status":"open"}],"schema_version":1,"hq-wisp-a":[{"id":"a-step","title":"Probe","status":"open"}]}`,
			wantIDs: []string{"a-step", "b-step"},
		},
		{
			name:    "schema key is metadata even if array-valued",
			input:   `{"schema_version":[{"id":"metadata","title":"Ignore","status":"open"}],"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}]}`,
			wantIDs: []string{"hq-wisp-a"},
		},
		{
			name:    "empty array",
			input:   `[]`,
			wantIDs: []string{},
		},
		{
			name:    "empty input",
			input:   `   `,
			wantErr: true,
		},
		{
			name:    "malformed bare array",
			input:   `[`,
			wantErr: true,
		},
		{
			name:    "malformed object envelope",
			input:   `{"hq-wisp-root":[`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
		{
			name:    "malformed child array",
			input:   `{"hq-wisp-root":[{"id":1}],"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "non-array child payload",
			input:   `{"hq-wisp-root":1,"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "metadata only is not silent skip-all",
			input:   `{"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "empty object is not silent skip-all",
			input:   `{}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChildrenJSON(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			gotIDs := make([]string, 0, len(got))
			for _, child := range got {
				gotIDs = append(gotIDs, child.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("got child IDs %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// fakeDogBd simulates the subset of `bd show <root> --children --json` and
// `bd close <id> [--force] [--reason X]` that closeRemainingSteps depends on,
// modeling a dependency chain: closing an id fails with "blocked by open
// issues" until every id in blockedBy[id] has closed, unless --force is
// passed (which always succeeds, matching real bd semantics).
type fakeDogBd struct {
	rootID     string
	statuses   map[string]string   // id -> "open" | "closed"
	blockedBy  map[string][]string // id -> ids that must close first
	closeCalls []string            // every id `bd close` was invoked with, in call order
	forceCalls []string            // ids force-closed
}

func (f *fakeDogBd) run(args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakeDogBd: no args")
	}
	switch args[0] {
	case "show":
		return f.show(), nil
	case "close":
		return f.close(args[1:])
	default:
		return "", fmt.Errorf("fakeDogBd: unexpected command: %v", args)
	}
}

func (f *fakeDogBd) show() string {
	ids := make([]string, 0, len(f.statuses))
	for id := range f.statuses {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var sb strings.Builder
	fmt.Fprintf(&sb, "{%q:[", f.rootID)
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"id":%q,"title":%q,"status":%q}`, id, id, f.statuses[id])
	}
	sb.WriteString("]}")
	return sb.String()
}

func (f *fakeDogBd) close(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakeDogBd: close missing id")
	}
	id := args[0]
	force := false
	for _, a := range args[1:] {
		if a == "--force" {
			force = true
		}
	}
	f.closeCalls = append(f.closeCalls, id)

	if !force {
		for _, blocker := range f.blockedBy[id] {
			if f.statuses[blocker] != "closed" {
				return "", fmt.Errorf("cannot close %s: blocked by open issues [%s]", id, blocker)
			}
		}
	} else {
		f.forceCalls = append(f.forceCalls, id)
	}
	f.statuses[id] = "closed"
	return "", nil
}

// TestCloseRemainingSteps_DrainsDependencyChainAcrossPasses is the adversarial
// case for gt-bygj: children are returned in an order where the blocked step
// is tried before its blocker (alphabetical "hq-wisp-c1" before
// "hq-wisp-c2"). A single pass over that order — the pre-fix behavior — closes
// c2 but leaves c1 permanently stranded (3 failed attempts, never retried).
// The fix must revisit c1 after c2 closes.
func TestCloseRemainingSteps_DrainsDependencyChainAcrossPasses(t *testing.T) {
	fake := &fakeDogBd{
		rootID: "hq-wisp-root",
		statuses: map[string]string{
			"hq-wisp-c1": "open",
			"hq-wisp-c2": "open",
		},
		blockedBy: map[string][]string{
			"hq-wisp-c1": {"hq-wisp-c2"},
		},
	}

	dm := &dogMol{
		rootID:  fake.rootID,
		stepIDs: make(map[string]string),
		logger:  log.New(io.Discard, "", 0),
		runBdFn: fake.run,
	}

	dm.closeRemainingSteps()

	for id, status := range fake.statuses {
		if status != "closed" {
			t.Errorf("expected %s closed, got status %q", id, status)
		}
	}
	if len(fake.forceCalls) != 0 {
		t.Errorf("expected no force-closes for a resolvable chain, got %v", fake.forceCalls)
	}
}

// TestCloseRemainingSteps_ForceClosesUnresolvableTail covers a dependency
// cycle (or a blocker outside the root's own children) that natural retries
// can never resolve. The fix must force-close the tail instead of leaving it
// HOOKED/open forever.
func TestCloseRemainingSteps_ForceClosesUnresolvableTail(t *testing.T) {
	fake := &fakeDogBd{
		rootID: "hq-wisp-root2",
		statuses: map[string]string{
			"hq-wisp-a": "open",
			"hq-wisp-b": "open",
		},
		blockedBy: map[string][]string{
			"hq-wisp-a": {"hq-wisp-b"},
			"hq-wisp-b": {"hq-wisp-a"},
		},
	}

	dm := &dogMol{
		rootID:  fake.rootID,
		stepIDs: make(map[string]string),
		logger:  log.New(io.Discard, "", 0),
		runBdFn: fake.run,
	}

	dm.closeRemainingSteps()

	for id, status := range fake.statuses {
		if status != "closed" {
			t.Errorf("expected %s force-closed, got status %q", id, status)
		}
	}
	if len(fake.forceCalls) != 2 {
		t.Errorf("expected both deadlocked children to be force-closed, got %v", fake.forceCalls)
	}
}

func TestDogMolGracefulDegradation(t *testing.T) {
	// A dogMol with empty rootID should be a no-op for all operations.
	dm := &dogMol{
		rootID:  "",
		stepIDs: make(map[string]string),
	}

	// These should not panic or error — graceful degradation.
	dm.closeStep("scan")
	dm.failStep("scan", "test failure")
	dm.close()
}

// The tests below cover gt-i3rpw: a pour that fails must be retried, counted per
// dog, escalated after N consecutive cycles, and recorded as an explicit
// outcome so a cycle that was skipped is never read as a cycle that ran and
// found nothing.

const (
	pourTestFormula = "mol-dog-doctor"
	pourTestWispID  = "gt-wisp-poured"
)

// fakePourBd stands in for the bd subprocess during a pour. It fails every
// attempt of the first failFor pours (always, when failFor is negative) and
// succeeds after that, so one fake drives both the retry-recovery path and a
// consecutive-failure streak. `show` reports the poured root with no children,
// which is what discoverSteps and the close() drain read back.
type fakePourBd struct {
	failFor  int  // pour calls that fail; negative means every call fails
	timeout  bool // the failure is a deadline kill, whose outcome is unknown
	noID     bool // succeed but print no bead id, so the root cannot be addressed
	failShow bool // the pour succeeds but the step listing fails

	pours    int
	pourArgs [][]string
	closes   []string
}

func (f *fakePourBd) run(args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakePourBd: no args")
	}
	switch args[0] {
	case "mol":
		f.pours++
		f.pourArgs = append(f.pourArgs, args)
		if f.failFor < 0 || f.pours <= f.failFor {
			if f.timeout {
				return "", fmt.Errorf("timed out after 15s: %w", context.DeadlineExceeded)
			}
			return "", fmt.Errorf("dolt circuit breaker is open")
		}
		if f.noID {
			return "poured ok", nil
		}
		return "✓ Spawned wisp: " + pourTestWispID + " — Dog formula", nil
	case "show":
		if f.failShow {
			return "", fmt.Errorf("show: dolt server unreachable")
		}
		return fmt.Sprintf("{%q:[]}", pourTestWispID), nil
	case "close":
		f.closes = append(f.closes, args[1])
		return "", nil
	default:
		return "", fmt.Errorf("fakePourBd: unexpected command: %v", args)
	}
}

// dogPourTestRig is a daemon wired for pour tests: a fake bd, a captured log,
// and a retry backoff that records the waits instead of sleeping through them.
type dogPourTestRig struct {
	daemon *Daemon
	fake   *fakePourBd
	logbuf *bytes.Buffer
	waits  []time.Duration
}

func newDogPourTestRig(t *testing.T, townRoot string) *dogPourTestRig {
	t.Helper()

	rig := &dogPourTestRig{
		fake:   &fakePourBd{},
		logbuf: &bytes.Buffer{},
	}
	rig.daemon = &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(rig.logbuf, "", 0),
	}
	rig.daemon.dogPourBdFn = rig.fake.run
	rig.daemon.dogPourWaitFn = func(d time.Duration) {
		rig.waits = append(rig.waits, d)
	}
	return rig
}

// fakeGtWithStdin installs a fake `gt` that records each invocation's argv and
// stdin, so a test can assert on the escalation body and not just its
// fingerprint. Returns the log path.
func fakeGtWithStdin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for gt")
	}

	logPath := filepath.Join(t.TempDir(), "gt-calls.log")
	script := `#!/usr/bin/env bash
{
  printf 'ARGV: %s\n' "$*"
  printf 'STDIN: '
  cat
  printf '\n--END--\n'
} >> "` + logPath + `"
exit 0
`
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// dogCycleEvents returns the payloads of the dog_cycle_outcome events recorded
// under townRoot, oldest first.
func dogCycleEvents(t *testing.T, townRoot string) []map[string]interface{} {
	t.Helper()

	raw := readFileOrEmpty(t, filepath.Join(townRoot, events.EventsFile))
	var out []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var event struct {
			Type    string                 `json:"type"`
			Payload map[string]interface{} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse events line %q: %v", line, err)
		}
		if event.Type == events.TypeDogCycleOutcome {
			out = append(out, event.Payload)
		}
	}
	return out
}

// TestPourDogMolecule_RetriesATransientFailureWithinOneCycle covers the first
// acceptance criterion: the dominant measured cause of a failed pour is
// short-lived (an open Dolt circuit breaker, an unreachable server), so one
// retry inside the cycle must turn that back into a run instead of dropping the
// patrol until the next tick.
func TestPourDogMolecule_RetriesATransientFailureWithinOneCycle(t *testing.T) {
	gtLog := fakeGtWithStdin(t)
	rig := newDogPourTestRig(t, t.TempDir())
	rig.fake.failFor = 1 // first attempt fails, the retry succeeds

	mol := rig.daemon.pourDogMolecule(pourTestFormula, nil)
	mol.close()

	if mol.rootID != pourTestWispID {
		t.Fatalf("rootID = %q, want %q — the transient failure should have recovered inside the cycle", mol.rootID, pourTestWispID)
	}
	if mol.outcome != dogCycleRan {
		t.Errorf("outcome = %q, want %q", mol.outcome, dogCycleRan)
	}
	if rig.fake.pours != 2 {
		t.Errorf("pour attempts = %d, want 2 (one failure, one retry)", rig.fake.pours)
	}
	if want := []time.Duration{dogPourRetryDelay}; !reflect.DeepEqual(rig.waits, want) {
		t.Errorf("retry backoff waits = %v, want %v", rig.waits, want)
	}
	// The retry repeats the same command, so a second failure is a second real
	// attempt rather than a malformed argv failing the same way twice.
	wantArgs := []string{"mol", "wisp", pourTestFormula}
	if len(rig.fake.pourArgs) != 2 || !reflect.DeepEqual(rig.fake.pourArgs[0], wantArgs) || !reflect.DeepEqual(rig.fake.pourArgs[1], wantArgs) {
		t.Errorf("pour argv = %v, want two identical %v", rig.fake.pourArgs, wantArgs)
	}
	if calls := readFileOrEmpty(t, gtLog); calls != "" {
		t.Errorf("a pour that recovered must not escalate, got:\n%s", calls)
	}
}

// TestPourDogMolecule_EscalatesAfterConsecutiveFailedCycles is the core of
// gt-i3rpw: the alarm. The fake fails every attempt of every cycle, so the
// failing branch is what runs, and the escalation must fire on the Nth
// consecutive failure — and not before it.
func TestPourDogMolecule_EscalatesAfterConsecutiveFailedCycles(t *testing.T) {
	gtLog := fakeGtWithStdin(t)
	rig := newDogPourTestRig(t, t.TempDir())
	rig.fake.failFor = -1 // every pour of every cycle fails, all attempts

	for cycle := 1; cycle < dogPourEscalateAfter; cycle++ {
		mol := rig.daemon.pourDogMolecule(pourTestFormula, nil)
		mol.close()

		if mol.outcome != dogCycleSkipped {
			t.Fatalf("cycle %d: outcome = %q, want %q", cycle, mol.outcome, dogCycleSkipped)
		}
		if calls := readFileOrEmpty(t, gtLog); calls != "" {
			t.Fatalf("cycle %d escalated before the %d-consecutive-failure threshold:\n%s",
				cycle, dogPourEscalateAfter, calls)
		}
	}

	mol := rig.daemon.pourDogMolecule(pourTestFormula, nil)
	mol.close()

	calls := readFileOrEmpty(t, gtLog)
	if !strings.Contains(calls, "escalate") {
		t.Fatalf("the %dth consecutive failed pour must escalate; no escalation was raised:\n%s",
			dogPourEscalateAfter, calls)
	}
	if want := "--fingerprint " + dogPourAlertKey(pourTestFormula); !strings.Contains(calls, want) {
		t.Errorf("escalation must carry the per-dog alert key %q, got:\n%s", want, calls)
	}
	if !strings.Contains(calls, pourTestFormula) {
		t.Errorf("escalation must name the dog that cannot pour, got:\n%s", calls)
	}
	if !strings.Contains(calls, "outcome=skipped") {
		t.Errorf("escalation must say the cycles were skipped, not merely that something failed, got:\n%s", calls)
	}
	// The title is truncated at maxEscalationTitleLen, so the cause has to reach
	// the bead through the stdin body.
	if !strings.Contains(calls, "STDIN: mol-dog-doctor") {
		t.Errorf("escalation must carry the cycle detail in its body, got:\n%s", calls)
	}
	if !strings.Contains(calls, "dolt circuit breaker is open") {
		t.Errorf("escalation body must carry the underlying failure, got:\n%s", calls)
	}

	// The retry is not a substitute for the alarm: every attempt of every cycle
	// must have been spent before the daemon gave up on that cycle.
	if want := dogPourEscalateAfter * dogPourMaxAttempts; rig.fake.pours != want {
		t.Errorf("pour attempts = %d, want %d", rig.fake.pours, want)
	}

	// One alert per streak: the open escalation is the standing signal, so a dog
	// that stays broken must not mint another one every cycle (gt-vwry).
	for cycle := 0; cycle < dogPourEscalateAfter; cycle++ {
		rig.daemon.pourDogMolecule(pourTestFormula, nil).close()
	}
	if got := strings.Count(readFileOrEmpty(t, gtLog), "escalate -s HIGH"); got != 1 {
		t.Errorf("expected exactly one escalation for the whole failure streak, got %d", got)
	}

	if !strings.Contains(rig.logbuf.String(), "outcome=skipped") {
		t.Errorf("the daemon log must label the skipped cycle, got:\n%s", rig.logbuf.String())
	}
}

// TestPourDogMolecule_RecoveryClearsTheAlarmAndResetsTheStreak: the counter
// counts CONSECUTIVE failures. A dog that pours again must close its own alarm,
// and the next outage starts a fresh streak rather than tripping on the
// lifetime total.
func TestPourDogMolecule_RecoveryClearsTheAlarmAndResetsTheStreak(t *testing.T) {
	gtLog := fakeGtWithStdin(t)
	rig := newDogPourTestRig(t, t.TempDir())
	rig.fake.failFor = -1

	for cycle := 0; cycle < dogPourEscalateAfter; cycle++ {
		rig.daemon.pourDogMolecule(pourTestFormula, nil).close()
	}
	if got := strings.Count(readFileOrEmpty(t, gtLog), "escalate -s HIGH"); got != 1 {
		t.Fatalf("expected exactly one escalation after %d failed cycles, got %d", dogPourEscalateAfter, got)
	}

	// The dog pours again.
	rig.fake.failFor = rig.fake.pours
	recovered := rig.daemon.pourDogMolecule(pourTestFormula, nil)
	recovered.close()

	if recovered.outcome != dogCycleRan {
		t.Fatalf("outcome after recovery = %q, want %q", recovered.outcome, dogCycleRan)
	}
	clears := readFileOrEmpty(t, gtLog)
	if !strings.Contains(clears, "escalate clear") {
		t.Fatalf("a recovered dog must close the escalation it raised, gt calls:\n%s", clears)
	}
	if want := "--fingerprint " + dogPourAlertKey(pourTestFormula); !strings.Contains(clears, want) {
		t.Errorf("the clear must name the same key the escalation used (%q), got:\n%s", want, clears)
	}

	// The streak restarted: two more failures are NOT the fourth and fifth of
	// the old streak, so no second escalation yet.
	rig.fake.failFor = -1
	for cycle := 0; cycle < dogPourEscalateAfter-1; cycle++ {
		rig.daemon.pourDogMolecule(pourTestFormula, nil).close()
	}
	if got := strings.Count(readFileOrEmpty(t, gtLog), "escalate -s HIGH"); got != 1 {
		t.Errorf("failures after a recovery must start a new streak (want 1 escalation, got %d)", got)
	}
}

// TestDogCycleOutcome_SkippedIsDistinguishableFromACleanRun covers the third
// acceptance criterion: whatever the dog reports must not read the same for a
// skipped cycle and a clean one. A clean run's receipt is the poured molecule,
// so it records outcome=ran in the log and nothing in the feed; a skipped cycle
// has no receipt of its own and goes to the feed with its reason.
func TestDogCycleOutcome_SkippedIsDistinguishableFromACleanRun(t *testing.T) {
	fakeGtWithStdin(t)
	townRoot := t.TempDir()

	clean := newDogPourTestRig(t, townRoot)
	cleanMol := clean.daemon.pourDogMolecule("mol-dog-reaper", nil)
	cleanMol.close()

	skipped := newDogPourTestRig(t, townRoot)
	skipped.fake.failFor = -1
	skippedMol := skipped.daemon.pourDogMolecule("mol-dog-reaper", nil)
	skippedMol.close()

	if cleanMol.outcome == skippedMol.outcome {
		t.Fatalf("a clean run and a skipped cycle both report outcome %q", cleanMol.outcome)
	}
	if skippedMol.outcome != dogCycleSkipped {
		t.Errorf("skipped cycle outcome = %q, want %q", skippedMol.outcome, dogCycleSkipped)
	}
	if skippedMol.outcomeReason == "" {
		t.Error("a skipped cycle must carry the reason it was skipped")
	}

	if got := clean.logbuf.String(); !strings.Contains(got, "outcome=ran") {
		t.Errorf("a clean run must label itself outcome=ran, got:\n%s", got)
	}
	if got := skipped.logbuf.String(); !strings.Contains(got, "outcome=skipped reason=") {
		t.Errorf("a skipped cycle must label itself outcome=skipped with a reason, got:\n%s", got)
	}

	// The side effects differ too: only the clean run has a wisp to close.
	if want := []string{pourTestWispID}; !reflect.DeepEqual(clean.fake.closes, want) {
		t.Errorf("a clean run must close its own wisp, closes = %v, want %v", clean.fake.closes, want)
	}
	if len(skipped.fake.closes) != 0 {
		t.Errorf("a skipped cycle has no wisp to close, got %v", skipped.fake.closes)
	}

	recorded := dogCycleEvents(t, townRoot)
	if len(recorded) != 1 {
		t.Fatalf("expected exactly the skipped cycle in the feed, got %d event(s): %v", len(recorded), recorded)
	}
	if got := recorded[0]["outcome"]; got != string(dogCycleSkipped) {
		t.Errorf("feed event outcome = %v, want %q", got, dogCycleSkipped)
	}
	if got := recorded[0]["formula"]; got != "mol-dog-reaper" {
		t.Errorf("feed event formula = %v, want %q", got, "mol-dog-reaper")
	}
	if reason, _ := recorded[0]["reason"].(string); reason == "" {
		t.Error("feed event must carry the skip reason")
	}
}

// TestPourDogMolecule_PouredButUnaddressableIsAFailedCycle: a pour that succeeds
// without a parseable root id leaves every closeStep a no-op, so the cycle has
// no receipt. That is a broken receipt, not a clean run, and it counts toward
// the same alarm as a failed pour.
func TestPourDogMolecule_PouredButUnaddressableIsAFailedCycle(t *testing.T) {
	gtLog := fakeGtWithStdin(t)
	rig := newDogPourTestRig(t, t.TempDir())
	rig.fake.noID = true

	for cycle := 1; cycle < dogPourEscalateAfter; cycle++ {
		mol := rig.daemon.pourDogMolecule(pourTestFormula, nil)
		mol.close()
		if mol.outcome != dogCycleFailed {
			t.Fatalf("cycle %d: outcome = %q, want %q", cycle, mol.outcome, dogCycleFailed)
		}
	}

	rig.daemon.pourDogMolecule(pourTestFormula, nil).close()

	if calls := readFileOrEmpty(t, gtLog); !strings.Contains(calls, "escalate") {
		t.Fatalf("a dog whose molecules can never be addressed must reach the alarm, got:\n%s", calls)
	}
}

// TestPourDogMolecule_StepDiscoveryFailureIsNotACleanRun covers the adjacent
// broken-receipt case: the molecule pours and addresses, then the listing that
// maps its steps fails, so every closeStep is a no-op and the cycle records
// nothing. Labeling that outcome=ran would leave the silence gt-i3rpw is about
// reading as a good cycle.
func TestPourDogMolecule_StepDiscoveryFailureIsNotACleanRun(t *testing.T) {
	gtLog := fakeGtWithStdin(t)
	rig := newDogPourTestRig(t, t.TempDir())
	rig.fake.failShow = true // pour succeeds; `show --children` does not

	for cycle := 1; cycle < dogPourEscalateAfter; cycle++ {
		mol := rig.daemon.pourDogMolecule(pourTestFormula, nil)
		mol.close()
		if mol.outcome != dogCycleFailed {
			t.Fatalf("cycle %d: outcome = %q, want %q", cycle, mol.outcome, dogCycleFailed)
		}
		if mol.outcomeReason == "" {
			t.Fatalf("cycle %d: a failed receipt must say what broke", cycle)
		}
	}

	rig.daemon.pourDogMolecule(pourTestFormula, nil).close()

	if calls := readFileOrEmpty(t, gtLog); !strings.Contains(calls, "escalate") {
		t.Fatalf("a dog whose steps can never be read back must reach the alarm, got:\n%s", calls)
	}
}

// TestPourDogMolecule_DoesNotRetryAPourThatTimedOut: a pour killed by its own
// deadline is not a safe retry. It may have committed the wisp before the answer
// was lost, and re-pouring that case strands a root wisp plus its children that
// nothing will ever close — the flood closeRemainingSteps exists to prevent.
// The cycle must be skipped once, counted, and left to the alarm.
func TestPourDogMolecule_DoesNotRetryAPourThatTimedOut(t *testing.T) {
	gtLog := fakeGtWithStdin(t)
	rig := newDogPourTestRig(t, t.TempDir())
	rig.fake.failFor = -1
	rig.fake.timeout = true

	mol := rig.daemon.pourDogMolecule(pourTestFormula, nil)
	mol.close()

	if rig.fake.pours != 1 {
		t.Errorf("pour attempts = %d, want 1 — a timed-out pour must not be repeated", rig.fake.pours)
	}
	if len(rig.waits) != 0 {
		t.Errorf("a non-retryable failure must not back off, got waits %v", rig.waits)
	}
	if mol.outcome != dogCycleSkipped {
		t.Errorf("outcome = %q, want %q", mol.outcome, dogCycleSkipped)
	}
	if reason := mol.outcomeReason; !strings.Contains(reason, "timed out") {
		t.Errorf("the reason must name the timeout, got %q", reason)
	}
	if calls := readFileOrEmpty(t, gtLog); calls != "" {
		t.Errorf("one skipped cycle is below the threshold and must not escalate, got:\n%s", calls)
	}
}

// TestPourDogMolecule_RetriesAnEscalationThatFailedToSend: the alarm's whole job
// is to fire when bd/Dolt is unhealthy, which is exactly when `gt escalate`
// (a bead write) fails too. A dropped escalation must not latch the streak as
// reported, or the dog stays silent for the rest of the outage.
func TestPourDogMolecule_RetriesAnEscalationThatFailedToSend(t *testing.T) {
	// A gt that fails every call: escalate exit 1, so all three attempts drop.
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks for gt")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte("#!/usr/bin/env bash\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing gt: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	townRoot := t.TempDir()
	rig := newDogPourTestRig(t, townRoot)
	rig.fake.failFor = -1

	for cycle := 0; cycle < dogPourEscalateAfter; cycle++ {
		rig.daemon.pourDogMolecule(pourTestFormula, nil).close()
	}

	// The send failed, so the streak is not latched as reported; the next failed
	// cycle must try to escalate again rather than assume the alert landed.
	attempts := func() int {
		n := 0
		for _, line := range strings.Split(readFileOrEmpty(t, filepath.Join(townRoot, events.EventsFile)), "\n") {
			if strings.Contains(line, events.TypeEscalationDropped) {
				n++
			}
		}
		return n
	}
	before := attempts()
	if before == 0 {
		t.Fatal("a gt that always fails must leave an escalation_dropped record")
	}

	rig.daemon.pourDogMolecule(pourTestFormula, nil).close()

	if got := attempts(); got <= before {
		t.Errorf("dropped escalation records = %d, want more than %d — the next failed cycle must retry the alarm", got, before)
	}
}

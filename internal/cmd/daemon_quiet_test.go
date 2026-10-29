package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/slot"
)

// quietTestEpoch is the fake clock's start: a fixed time, so a failing wait
// names the same duration on every run.
var quietTestEpoch = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

// quietAnswer is what one probe says on every read.
type quietAnswer struct {
	value bool
	err   error
}

func (a quietAnswer) probe() (bool, error) { return a.value, a.err }

// quietPollFailsafe bounds how long a test waits for the wait under test to ask
// the fake clock for its next poll, so a wait that returns early fails in
// seconds instead of at the test timeout.
const quietPollFailsafe = 10 * time.Second

// waitQuietToTheBudget runs WaitQuiet on a fake clock and advances it one poll
// short of the budget, then over it: the wait must still be waiting one poll
// before the budget and must end when the clock reaches it, and neither step
// sleeps.
func waitQuietToTheBudget(t *testing.T, clk *clockwork.FakeClock, budget, poll time.Duration, q QuietProbes) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- WaitQuiet(ctx, q, clk, budget, poll) }()
	blockUntilPolling(t, clk)
	clk.Advance(budget - poll)
	blockUntilPolling(t, clk)
	clk.Advance(poll)
	return awaitQuiet(t, done)
}

// awaitQuiet is the wait's verdict, bounded: a wait that never ends fails the
// test in seconds rather than at the test timeout.
func awaitQuiet(t *testing.T, done <-chan error) error {
	t.Helper()
	finished, stopWaiting := context.WithTimeout(context.Background(), quietPollFailsafe)
	defer stopWaiting()
	select {
	case err := <-done:
		return err
	case <-finished.Done():
		t.Fatalf("the wait had not ended %s after the clock last moved", quietPollFailsafe)
		return nil
	}
}

// blockUntilPolling waits for the wait under test to ask the fake clock for its
// next poll: a wait that returned instead has no poll to give, and that is a
// failure the test reports rather than a hang it sits out.
func blockUntilPolling(t *testing.T, clk *clockwork.FakeClock) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), quietPollFailsafe)
	defer cancel()
	if err := clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("the wait asked the clock for no poll within %s: %v", quietPollFailsafe, err)
	}
}

// Each probe's reading, in every combination that decides the verdict, and the
// blocker WaitQuiet names when it gives up.
func TestWaitQuiet(t *testing.T) {
	t.Parallel()
	budget, poll := 3*time.Minute, time.Minute
	looking := errors.New("the probe could not look")
	cases := []struct {
		name        string
		held        quietAnswer
		sling       quietAnswer
		gate        quietAnswer
		want        string // the error WaitQuiet returns; "" wants nil
		wantElapsed time.Duration
	}{
		{name: "the town is quiet", held: quietAnswer{value: true}},
		{
			name:        "the dispatch hold is not on",
			held:        quietAnswer{},
			want:        "the dispatch hold is not on (no seat-refill.hold)",
			wantElapsed: budget,
		},
		{
			name:        "a gt sling is running",
			held:        quietAnswer{value: true},
			sling:       quietAnswer{value: true},
			want:        "a gt sling is running",
			wantElapsed: budget,
		},
		{
			name:        "the container-gate slot is busy",
			held:        quietAnswer{value: true},
			gate:        quietAnswer{value: true},
			want:        "the container-gate slot is busy",
			wantElapsed: budget,
		},
		{
			name:        "the dispatch-hold probe cannot answer",
			held:        quietAnswer{err: looking},
			want:        "the dispatch-hold probe cannot answer",
			wantElapsed: budget,
		},
		{
			name:        "the sling probe cannot answer",
			held:        quietAnswer{value: true},
			sling:       quietAnswer{err: looking},
			want:        "the sling probe cannot answer",
			wantElapsed: budget,
		},
		{
			name:        "the container-gate probe cannot answer",
			held:        quietAnswer{value: true},
			gate:        quietAnswer{err: looking},
			want:        "the container-gate probe cannot answer",
			wantElapsed: budget,
		},
		{
			name:        "the hold is named before a running sling",
			held:        quietAnswer{},
			sling:       quietAnswer{value: true},
			want:        "the dispatch hold is not on",
			wantElapsed: budget,
		},
		{
			name:        "the sling is named before a busy gate",
			held:        quietAnswer{value: true},
			sling:       quietAnswer{value: true},
			gate:        quietAnswer{value: true},
			want:        "a gt sling is running",
			wantElapsed: budget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := QuietProbes{
				DispatchHeld: tc.held.probe,
				SlingRunning: tc.sling.probe,
				GateBusy:     tc.gate.probe,
			}
			clk := clockwork.NewFakeClockAt(quietTestEpoch)
			if tc.want == "" {
				if err := WaitQuiet(context.Background(), q, clk, budget, poll); err != nil {
					t.Fatalf("WaitQuiet = %v; want nil for a quiet town", err)
				}
				return
			}
			err := waitQuietToTheBudget(t, clk, budget, poll, q)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("WaitQuiet = %v; want an error naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), budget.String()) {
				t.Errorf("WaitQuiet = %v; want the budget %s in the error", err, budget)
			}
			if elapsed := clk.Now().Sub(quietTestEpoch); elapsed != tc.wantElapsed {
				t.Errorf("the wait ended after %s; want it to last the budget %s", elapsed, tc.wantElapsed)
			}
		})
	}
}

// The wait ends as soon as the town goes quiet, not at the budget.
func TestWaitQuietReturnsOnceTheTownGoesQuiet(t *testing.T) {
	t.Parallel()
	reads := 0
	q := QuietProbes{
		DispatchHeld: func() (bool, error) { reads++; return reads > 1, nil },
		SlingRunning: quietAnswer{}.probe,
		GateBusy:     quietAnswer{}.probe,
	}
	clk := clockwork.NewFakeClockAt(quietTestEpoch)
	done := make(chan error, 1)
	go func() { done <- WaitQuiet(context.Background(), q, clk, 3*time.Minute, time.Minute) }()
	blockUntilPolling(t, clk)
	clk.Advance(time.Minute)
	if err := awaitQuiet(t, done); err != nil {
		t.Fatalf("WaitQuiet = %v; want nil once the hold appears", err)
	}
	if elapsed := clk.Now().Sub(quietTestEpoch); elapsed != time.Minute {
		t.Errorf("the wait ended after %s; want it to end at the poll the hold appeared (%s)", elapsed, time.Minute)
	}
}

// The blocker named at the budget is the one still standing, not one that was
// fixed along the way.
func TestWaitQuietNamesTheBlockerStillStanding(t *testing.T) {
	t.Parallel()
	reads := 0
	q := QuietProbes{
		DispatchHeld: func() (bool, error) { reads++; return reads > 1, nil },
		SlingRunning: quietAnswer{value: true}.probe,
		GateBusy:     quietAnswer{}.probe,
	}
	err := waitQuietToTheBudget(t, clockwork.NewFakeClockAt(quietTestEpoch), 3*time.Minute, time.Minute, q)
	if err == nil || !strings.Contains(err.Error(), "a gt sling is running") {
		t.Fatalf("WaitQuiet = %v; want the sling it was still waiting on", err)
	}
}

// A caller's unfinished probe literal is refused, so a missing probe cannot
// leave the gate reading one hazard twice.
func TestWaitQuietRefusesAnIncompleteProbeSet(t *testing.T) {
	t.Parallel()
	quiet := quietAnswer{}.probe
	cases := []struct {
		name string
		q    QuietProbes
		want string
	}{
		{"no dispatch-hold probe", QuietProbes{SlingRunning: quiet, GateBusy: quiet}, "the dispatch-hold probe is not set"},
		{"no sling probe", QuietProbes{DispatchHeld: quiet, GateBusy: quiet}, "the sling probe is not set"},
		{"no container-gate probe", QuietProbes{DispatchHeld: quiet, SlingRunning: quiet}, "the container-gate probe is not set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := WaitQuiet(context.Background(), tc.q, clockwork.NewFakeClockAt(quietTestEpoch), time.Minute, time.Minute)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("WaitQuiet = %v; want an error naming %q", err, tc.want)
			}
		})
	}
}

// A cancelled cutover stops waiting at once rather than sitting out the budget.
func TestWaitQuietStopsWhenTheCutoverIsCancelled(t *testing.T) {
	t.Parallel()
	q := QuietProbes{
		DispatchHeld: quietAnswer{}.probe,
		SlingRunning: quietAnswer{}.probe,
		GateBusy:     quietAnswer{}.probe,
	}
	clk := clockwork.NewFakeClockAt(quietTestEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WaitQuiet(ctx, q, clk, 30*time.Minute, time.Minute) }()
	blockUntilPolling(t, clk)
	cancel()
	err := awaitQuiet(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitQuiet = %v; want it to report the cancellation", err)
	}
	if !strings.Contains(err.Error(), "the dispatch hold is not on") {
		t.Errorf("WaitQuiet = %v; want the blocker it was waiting on", err)
	}
	if elapsed := clk.Now().Sub(quietTestEpoch); elapsed != 0 {
		t.Errorf("the wait advanced the clock by %s; want it to stop without waiting", elapsed)
	}
}

// A budget or poll the caller leaves unset takes the defaults, so the cutover
// cannot ask for an unbounded wait by passing zero.
func TestWaitQuietDefaultsTheBudgetAndPoll(t *testing.T) {
	t.Parallel()
	q := QuietProbes{
		DispatchHeld: quietAnswer{}.probe,
		SlingRunning: quietAnswer{}.probe,
		GateBusy:     quietAnswer{}.probe,
	}
	clk := clockwork.NewFakeClockAt(quietTestEpoch)
	done := make(chan error, 1)
	go func() { done <- WaitQuiet(context.Background(), q, clk, 0, 0) }()
	blockUntilPolling(t, clk)
	clk.Advance(defaultQuietBudget - defaultQuietPoll)
	blockUntilPolling(t, clk)
	clk.Advance(defaultQuietPoll)
	err := awaitQuiet(t, done)
	if err == nil || !strings.Contains(err.Error(), defaultQuietBudget.String()) {
		t.Fatalf("WaitQuiet = %v; want the default budget %s in the error", err, defaultQuietBudget)
	}
	if elapsed := clk.Now().Sub(quietTestEpoch); elapsed != defaultQuietBudget {
		t.Errorf("the wait ended after %s; want the default budget %s", elapsed, defaultQuietBudget)
	}
}

// The quiet gate's own reads change nothing: the hold file is the operator's to
// create and remove, a cutover that gave up leaves the town as it found it, and
// asking whether the container-gate slot is busy does not sweep the files an
// ordinary status read would (gt-qpeei.22). The town tree is compared whole
// before and after, so a write anywhere under it — a lock file created, a dead
// holder's owner file removed — fails here. Only the gate's `docker ps` is a
// stub; the rest of the gate probe, its flocks included, is the production code.
func TestWaitQuietCreatesAndRemovesNoFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hold bool   // the operator's hold file exists
		want string // the error the wait returns; "" wants nil
	}{
		{name: "the town is quiet and the wait returns", hold: true},
		{name: "the wait gives up at its budget", want: "the dispatch hold is not on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			// A free slot whose last holder died: the lock file it took and the
			// owner file it left. The ordinary status read sweeps that owner
			// file (internal/slot/pool.go); the quiet gate must leave it alone.
			if err := os.MkdirAll(slot.LockDir(town), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(slot.SlotLockPath(town, 0), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(slot.SlotOwnerPath(town, 0), []byte(`{"role":"gastown/landing","pid":999999}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.hold {
				if err := os.WriteFile(filepath.Join(town, dispatch.HoldFileName), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := townTree(t, town)

			probes, err := NewQuietProbes(town, QuietProbeDeps{
				Run:  func(string, ...string) error { return testExit(1) },
				Gate: quietGate(),
			})
			if err != nil {
				t.Fatal(err)
			}
			budget, poll := 2*time.Minute, time.Minute
			clk := clockwork.NewFakeClockAt(quietTestEpoch)
			var got error
			if tc.want == "" {
				got = WaitQuiet(context.Background(), probes, clk, budget, poll)
			} else {
				got = waitQuietToTheBudget(t, clk, budget, poll, probes)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("WaitQuiet = %v; want nil for a quiet town", got)
				}
			} else if got == nil || !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("WaitQuiet = %v; want an error naming %q", got, tc.want)
			}

			if after := townTree(t, town); !maps.Equal(before, after) {
				t.Errorf("the quiet gate changed the town:\nbefore %v\nafter  %v", before, after)
			}
			if _, err := os.Stat(slot.SlotOwnerPath(town, 0)); err != nil {
				t.Errorf("the dead holder's owner file is gone, so the gate's read swept it: %v", err)
			}
		})
	}
}

// townTree is every path under root mapped to its content (directories to ""),
// so a test can assert a wait left the town exactly as it found it.
func townTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			tree[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the town tree at %s: %v", root, err)
	}
	return tree
}

// quietGate is the container gate with its Docker runtime replaced: the gate
// probe's filesystem and flock work is the production code, and only the
// `docker ps` listing comes from here.
func quietGate(lines ...string) *slot.Gate {
	return slot.NewGate(slot.WithRuntime(quietGateRuntime{lines: lines}))
}

// quietGateRuntime is the gate's Docker stand-in: it answers the listing from
// the fixture and never touches a container.
type quietGateRuntime struct{ lines []string }

func (r quietGateRuntime) List() ([]string, error) { return r.lines, nil }

func (r quietGateRuntime) Remove(string, slot.RemoveOptions) error { return nil }

func (r quietGateRuntime) Info() (slot.VMInfo, error) { return slot.VMInfo{}, nil }

// testExit is a process exit code as os/exec reports it, so a fake pgrep can
// answer with the code the real one would.
type testExit int

func (e testExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e testExit) ExitCode() int { return int(e) }

// processExitCode reads an exit status out of an error and -1 out of anything
// else, so a pgrep that never ran cannot read as its own "nothing matched" 1.
func TestProcessExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a process exit", testExit(3), 3},
		{"not an exit at all", errors.New("exec: pgrep: executable file not found in $PATH"), -1},
		{"no error", nil, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := processExitCode(tc.err); got != tc.want {
				t.Errorf("processExitCode(%v) = %d; want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestNewQuietProbesRefusesATownRootThatIsNotAbsolute(t *testing.T) {
	t.Parallel()
	for _, townRoot := range []string{"", ".", "town", "relative/town"} {
		if _, err := NewQuietProbes(townRoot, QuietProbeDeps{}); err == nil {
			t.Errorf("NewQuietProbes(%q) was accepted; a relative town root reads the host from wherever the caller stands", townRoot)
		}
	}
}

// The host probes read the operator's hold file, pgrep and the container-gate
// pool, and every reading it cannot take leaves the town not quiet.
func TestNewQuietProbesReadTheHost(t *testing.T) {
	t.Parallel()
	budget, poll := 3*time.Minute, time.Minute
	cases := []struct {
		name           string
		hold           bool // the operator's hold file exists
		statErr        error
		pgrepExit      int      // 0 one match, 1 none, anything else a pgrep that failed
		gateHeld       bool     // the test holds the gate's only slot
		gateUnopenable bool     // the slot's lock file cannot be opened
		gateLines      []string // what the gate's `docker ps` lists
		want           string   // the error WaitQuiet returns; "" wants nil
		wantExited     bool     // WaitQuiet must give up at the budget rather than return nil
	}{
		{name: "the operator holds dispatch and the gate is free", hold: true, pgrepExit: 1},
		{
			name:       "the operator's hold file is absent",
			pgrepExit:  1,
			want:       "the dispatch hold is not on (no seat-refill.hold)",
			wantExited: true,
		},
		{
			name:       "the hold file cannot be read",
			statErr:    fs.ErrPermission,
			pgrepExit:  1,
			want:       "the dispatch-hold probe cannot answer (reading ",
			wantExited: true,
		},
		{
			name:       "a gt sling is running",
			hold:       true,
			pgrepExit:  0,
			want:       "a gt sling is running",
			wantExited: true,
		},
		{
			name:       "pgrep cannot answer",
			hold:       true,
			pgrepExit:  2,
			want:       "pgrep could not say whether a gt sling is running",
			wantExited: true,
		},
		{
			name:       "pgrep is missing",
			hold:       true,
			pgrepExit:  -1, // not an exit code at all
			want:       "pgrep could not say whether a gt sling is running",
			wantExited: true,
		},
		{
			name:       "the gate holds its only slot",
			hold:       true,
			pgrepExit:  1,
			gateHeld:   true,
			want:       "the container-gate slot is busy",
			wantExited: true,
		},
		{
			name:       "an unwrapped suite holds the docker VM",
			hold:       true,
			pgrepExit:  1,
			gateLines:  []string{"dolt/dolt-sql-server:2.2.0 unwrapped-suite"},
			want:       "the container-gate slot is busy",
			wantExited: true,
		},
		{
			name:           "the gate cannot be read",
			hold:           true,
			pgrepExit:      1,
			gateUnopenable: true,
			want:           "the container-gate probe cannot answer",
			wantExited:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.hold {
				if err := os.WriteFile(filepath.Join(town, dispatch.HoldFileName), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The gate is read through the production probe: a fixture on the
			// town is what makes the slot busy, unreadable or free.
			lockPath := slot.SlotLockPath(town, 0)
			switch {
			case tc.gateHeld:
				if err := os.MkdirAll(slot.LockDir(town), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				unlock, took, err := lock.FlockTryAcquire(lockPath)
				if err != nil || !took {
					t.Fatalf("holding the gate's slot = (%v, %v)", took, err)
				}
				t.Cleanup(unlock)
			case tc.gateUnopenable:
				if err := os.MkdirAll(slot.LockDir(town), 0o755); err != nil {
					t.Fatal(err)
				}
				// A directory opens for reading but not for a flock, which is a
				// slot read the probe cannot take.
				if err := os.Mkdir(lockPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			var ran []string
			probes, err := NewQuietProbes(town, QuietProbeDeps{
				Stat: func(name string) (fs.FileInfo, error) {
					if tc.statErr != nil {
						return nil, tc.statErr
					}
					return os.Stat(name)
				},
				Run: func(name string, args ...string) error {
					ran = append([]string{name}, args...)
					switch tc.pgrepExit {
					case 0:
						return nil
					case -1:
						return fs.ErrNotExist
					}
					return testExit(tc.pgrepExit)
				},
				Gate: quietGate(tc.gateLines...),
			})
			if err != nil {
				t.Fatal(err)
			}

			clk := clockwork.NewFakeClockAt(quietTestEpoch)
			var got error
			if tc.wantExited {
				got = waitQuietToTheBudget(t, clk, budget, poll, probes)
			} else {
				got = WaitQuiet(context.Background(), probes, clk, budget, poll)
			}

			if tc.want == "" {
				if got != nil {
					t.Fatalf("WaitQuiet = %v; want nil for a quiet town", got)
				}
			} else if got == nil || !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("WaitQuiet = %v; want an error naming %q", got, tc.want)
			}
			// A hold file the gate cannot read stops the reads before pgrep is
			// asked, so the argv is checked only where the sling probe ran.
			switch {
			case !tc.hold || tc.statErr != nil:
				if len(ran) != 0 {
					t.Errorf("pgrep ran as %v although the hold was not read as present", ran)
				}
			case !slices.Equal(ran, []string{"pgrep", "-f", `^([^ ]*/)?gt sling `}):
				t.Errorf("pgrep ran as %v; want the sling check's own argv", ran)
			}
		})
	}
}

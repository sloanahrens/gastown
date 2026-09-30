package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// refusalStderr is a failed sling's stderr when the target rig's merge queue is
// over merge_queue.max_ready_for_dispatch, wrapped the way the real command
// reports it (cobra's "Error: " plus the spawn path's "spawning polecat: ").
// internal/cmd's own test pins the unwrapped message; this one pins the parse.
const refusalStderr = "Error: spawning polecat: sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework"

// backpressureFeedRig is the standard feed fixture with a `gt` that refuses
// to sling while refuse is set and succeeds once it is cleared. Every call is
// recorded, so a test can prove the feeder called nothing else on a deferred
// bead.
func backpressureFeedRig(t *testing.T) (townRoot string, gt *fakeCLI, refuse *atomic.Bool) {
	t.Helper()
	return refusingFeedRig(t, refusalStderr)
}

// refusingFeedRig is backpressureFeedRig with the refusal's stderr chosen by
// the caller.
func refusingFeedRig(t *testing.T, refusal string) (townRoot string, gt *fakeCLI, refuse *atomic.Bool) {
	t.Helper()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	refuse = &atomic.Bool{}
	gt = newFakeCLI(func(args []string) cliReply {
		if len(args) > 0 && args[0] == "sling" && refuse.Load() {
			return cliReply{stderr: refusal + "\n", code: 1}
		}
		return cliReply{}
	})
	return townRoot, gt, refuse
}

// newFeedManager is NewConvoyManager over townRoot with its gt calls
// answered by gt.
func newFeedManager(townRoot string, logger func(string, ...interface{}), gt *fakeCLI) *ConvoyManager {
	m := NewConvoyManager(townRoot, logger, "gt", 10*time.Minute, nil, nil, nil)
	m.execCmd = gt.run
	answerScanThrough(m, gt)
	return m
}

// assertOnlySlings fails the test if gt ran anything but a sling: a deferred
// bead's state could only have changed through a second command.
func assertOnlySlings(t *testing.T, gt *fakeCLI) {
	t.Helper()
	for _, args := range gt.argvs() {
		if len(args) == 0 || args[0] != "sling" {
			t.Errorf("deferral invoked a non-sling command, so bead state may have changed: %q", args)
		}
	}
}

// newBackpressureLogger collects the feeder's log lines.
func newBackpressureLogger() (*[]string, func(string, ...interface{})) {
	logged := &[]string{}
	return logged, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
}

// TestFeedFirstReady_DefersOnQueueBackpressure is the A3 feeder contract
// (gt-xidg): a refused sling is the town at capacity, so the feeder logs a
// deferral and leaves the bead exactly as it found it — no failure line, no
// status write, no cleanup.
func TestFeedFirstReady_DefersOnQueueBackpressure(t *testing.T) {
	t.Parallel()
	townRoot, gt, refuse := backpressureFeedRig(t)
	refuse.Store(true)

	logged, logger := newBackpressureLogger()
	m := newFeedManager(townRoot, logger, gt)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	}

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Over the ceiling",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-issue1", "gt-issue2"},
	}
	m.feedFirstReady(c)

	// Both ready issues are offered and both are deferred: the ceiling is a
	// property of the rig, so the loop keeps looking for an issue that can be
	// dispatched (a convoy can span rigs) rather than stopping at the first.
	for _, issue := range []string{"gt-issue1", "gt-issue2"} {
		want := fmt.Sprintf("Convoy hq-cv1: deferring %s: sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework", issue)
		found := false
		for _, l := range *logged {
			if l == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected deferral line %q, got: %v", want, *logged)
		}
	}

	for _, l := range *logged {
		if strings.Contains(l, "failed") {
			t.Errorf("a deferred bead must not be logged as a failure, got: %q", l)
		}
	}

	// The refusal reached the feeder as a non-zero exit, so the only way the
	// bead could have been mutated is a second command. Every recorded
	// invocation is a sling: no `bd`, no `mq`, nothing that could have changed
	// the bead's status.
	if n := len(gt.argvs("sling")); n != 2 {
		t.Errorf("sling calls = %d, want 2 (one per ready issue)", n)
	}
	assertOnlySlings(t, gt)
}

// TestFeedFirstReady_ReoffersDeferredBeadNextTick is the other half of the
// contract: deferring must not strand the bead. With the queue drained the
// next scan feeds the same bead, so deferral costs a tick, not the work.
func TestFeedFirstReady_ReoffersDeferredBeadNextTick(t *testing.T) {
	t.Parallel()
	townRoot, gt, refuse := backpressureFeedRig(t)
	refuse.Store(true)

	logged, logger := newBackpressureLogger()
	m := newFeedManager(townRoot, logger, gt)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	}

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Deferred then fed",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	refuse.Store(false) // the queue drained
	m.feedFirstReady(c)

	if attempts := len(gt.argvs("sling", "gt-issue1")); attempts != 2 {
		t.Errorf("sling attempts for gt-issue1 = %d, want 2 (deferred once, then fed): %q", attempts, gt.argvs())
	}

	fed := false
	for _, l := range *logged {
		if strings.Contains(l, "feeding gt-issue1") {
			fed = true
		}
		if strings.Contains(l, "failed") {
			t.Errorf("a deferred bead must not be logged as a failure, got: %q", l)
		}
	}
	if !fed {
		t.Errorf("expected the deferred bead to be fed on the next tick, got: %v", *logged)
	}
}

// TestSlingBackpressureReason pins the marker the guard and the feeder share.
// A false positive would swallow a real dispatch failure as a deferral, so the
// parser must answer only for the refusal line.
func TestSlingBackpressureReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   string
		wantOK bool
	}{
		{
			name:   "wrapped refusal",
			stderr: refusalStderr,
			want:   "sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework",
			wantOK: true,
		},
		{
			name:   "refusal after timing lines",
			stderr: "[sling] step pool took 1.3s\nError: sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework",
			want:   "sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework",
			wantOK: true,
		},
		{
			// The pool's refusal carries the same marker, so the feeder defers
			// a full pool exactly as it defers a deep merge queue (gt-jzr1).
			name:   "pool refusal is a deferral too",
			stderr: "Error: spawning polecat: sling refused: pool: overflow full (3/3) -> no seat (type=bug); raise polecat_pool.max_local/max_overflow to spawn",
			want:   "sling refused: pool: overflow full (3/3) -> no seat (type=bug); raise polecat_pool.max_local/max_overflow to spawn",
			wantOK: true,
		},
		{
			name:   "any other failure is not a deferral",
			stderr: "Error: spawning polecat: pre-spawn health check failed: connection refused",
			wantOK: false,
		},
		{
			name:   "empty stderr",
			stderr: "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := slingBackpressureReason(tt.stderr)
			if ok != tt.wantOK {
				t.Fatalf("slingBackpressureReason(%q) ok = %v, want %v", tt.stderr, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("slingBackpressureReason(%q) = %q, want %q", tt.stderr, got, tt.want)
			}
		})
	}
}

// survivingWorkRefusalStderr is sling's refusal to re-sling a bead whose dead
// holder's branch still carries work (internal/cmd reslingSurvivingWorkGuard,
// gt-vm5g4), wrapped by cobra.
const survivingWorkRefusalStderr = `Error: refusing to re-sling gt-issue1: previous holder gt/polecats/pearl has no active session, but its branch still carries work that is not on main:
  polecat/pearl/gt-issue1+mu72g5cz
Re-slinging would start a second polecat from main on work that is already preserved.
  Resume the preserved work:  gt sling gt-issue1 <target> --branch polecat/pearl/gt-issue1+mu72g5cz
  Start fresh anyway:         gt sling gt-issue1 <target> --force`

func TestSlingDeferralReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   string
		wantOK bool
	}{
		{
			name:   "backpressure refusal",
			stderr: refusalStderr,
			want:   "sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework",
			wantOK: true,
		},
		{
			name:   "surviving work refusal",
			stderr: survivingWorkRefusalStderr,
			want:   "refusing to re-sling gt-issue1: previous holder gt/polecats/pearl has no active session, but its branch still carries work that is not on main:",
			wantOK: true,
		},
		{
			name:   "unverifiable survival refusal after timing lines",
			stderr: "[sling] step pool took 1.3s\nError: refusing to re-sling gt-issue1: previous holder gt/polecats/pearl has no active session, and sling cannot verify surviving work (timed out); resume with --branch or override with --force",
			want:   "refusing to re-sling gt-issue1: previous holder gt/polecats/pearl has no active session, and sling cannot verify surviving work (timed out); resume with --branch or override with --force",
			wantOK: true,
		},
		{
			name:   "any other failure is not a deferral",
			stderr: "Error: spawning polecat: pre-spawn health check failed: connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := slingDeferralReason(tt.stderr)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("slingDeferralReason() = %q, %v; want %q, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestFeedFirstReady_DefersOnSurvivingWorkRefusal: a bead whose dead holder's
// work survives is deferred by the feeder, never logged as a failed sling, and
// nothing but the sling ran against it.
func TestFeedFirstReady_DefersOnSurvivingWorkRefusal(t *testing.T) {
	t.Parallel()
	townRoot, gt, refuse := refusingFeedRig(t, survivingWorkRefusalStderr)
	refuse.Store(true)

	logged, logger := newBackpressureLogger()
	m := newFeedManager(townRoot, logger, gt)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	}
	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Preserved work",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	})

	want := "Convoy hq-cv1: deferring gt-issue1: refusing to re-sling gt-issue1: previous holder gt/polecats/pearl has no active session, but its branch still carries work that is not on main:"
	found := false
	for _, l := range *logged {
		if l == want {
			found = true
		}
		if strings.Contains(l, "failed") {
			t.Errorf("a surviving-work refusal must not be logged as a failure, got: %q", l)
		}
	}
	if !found {
		t.Errorf("expected deferral line %q, got: %v", want, *logged)
	}
	if n := len(gt.argvs("sling", "gt-issue1")); n != 1 {
		t.Errorf("sling calls for gt-issue1 = %d, want 1", n)
	}
	assertOnlySlings(t, gt)
}

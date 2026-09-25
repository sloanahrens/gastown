package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// refusalStderr is a failed sling's stderr when the target rig's merge queue is
// over merge_queue.max_ready_for_dispatch, wrapped the way the real command
// reports it (cobra's "Error: " plus the spawn path's "spawning polecat: ").
// internal/cmd's own test pins the unwrapped message; this one pins the parse.
const refusalStderr = "Error: spawning polecat: sling refused: gastown has 13 ready MRs (> 12); pass --force or label the bead rework"

// backpressureFeedRig extends the standard feed fixture with a mock `gt` that
// refuses to sling while refuseFlag exists and succeeds once it is removed.
// Every invocation is appended to the sling log verbatim, so a test can prove
// the feeder called nothing else on a deferred bead.
func backpressureFeedRig(t *testing.T, refuseFlag string) (townRoot, gtPath, slingLogPath string) {
	t.Helper()
	return refusingFeedRig(t, refuseFlag, refusalStderr)
}

// refusingFeedRig is backpressureFeedRig with the refusal's stderr chosen by
// the caller.
func refusingFeedRig(t *testing.T, refuseFlag, refusal string) (townRoot, gtPath, slingLogPath string) {
	t.Helper()

	binDir := t.TempDir()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath = filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
echo "$@" >> "` + slingLogPath + `"
if [ "$1" = "sling" ]; then
  if [ -f "` + refuseFlag + `" ]; then
    cat >&2 <<'REFUSAL'
` + refusal + `
REFUSAL
    exit 1
  fi
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}
	return townRoot, filepath.Join(binDir, "gt"), slingLogPath
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
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	refuseFlag := filepath.Join(t.TempDir(), "refuse")
	if err := os.WriteFile(refuseFlag, []byte("1"), 0644); err != nil {
		t.Fatalf("write refuse flag: %v", err)
	}
	townRoot, gtPath, slingLogPath := backpressureFeedRig(t, refuseFlag)
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	})

	logged, logger := newBackpressureLogger()
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

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
	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "sling ") {
			t.Errorf("deferral invoked a non-sling command, so bead state may have changed: %q", line)
		}
	}
}

// TestFeedFirstReady_ReoffersDeferredBeadNextTick is the other half of the
// contract: deferring must not strand the bead. With the queue drained the
// next scan feeds the same bead, so deferral costs a tick, not the work.
func TestFeedFirstReady_ReoffersDeferredBeadNextTick(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	refuseFlag := filepath.Join(t.TempDir(), "refuse")
	if err := os.WriteFile(refuseFlag, []byte("1"), 0644); err != nil {
		t.Fatalf("write refuse flag: %v", err)
	}
	townRoot, gtPath, slingLogPath := backpressureFeedRig(t, refuseFlag)
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	})

	logged, logger := newBackpressureLogger()
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Deferred then fed",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	if err := os.Remove(refuseFlag); err != nil {
		t.Fatalf("drain the queue: %v", err)
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	attempts := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "sling gt-issue1 ") {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("sling attempts for gt-issue1 = %d, want 2 (deferred once, then fed): %q", attempts, string(data))
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
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	refuseFlag := filepath.Join(t.TempDir(), "refuse")
	if err := os.WriteFile(refuseFlag, []byte("1"), 0644); err != nil {
		t.Fatalf("write refuse flag: %v", err)
	}
	townRoot, gtPath, slingLogPath := refusingFeedRig(t, refuseFlag, survivingWorkRefusalStderr)
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	})

	logged, logger := newBackpressureLogger()
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)
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
	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" && !strings.HasPrefix(line, "sling ") {
			t.Errorf("deferral invoked a non-sling command: %q", line)
		}
	}
}

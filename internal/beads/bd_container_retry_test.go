package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The two lines bd wrote when TestEngineerCloseMRWithReasonRecordsMergeCommit-
// AndClearsActiveMR died on a saturated Docker VM (gt-6uhq). Kept verbatim:
// the retry keys off exactly this text, so a paraphrase here would test a
// string bd never emits.
const (
	observedIoTimeoutStderr   = `read tcp 127.0.0.1:65473->127.0.0.1:55107: i/o timeout`
	observedOpenFailureStderr = `Error: failed to open database: schema skew check: ` +
		`probing schema_migrations existence: invalid connection`
)

// TestBdContainerRetryRecoversFromConnectionFailure is gt-6uhq: a bd command
// that loses its Dolt connection to the test container must be retried, and
// the retry must return the command's result rather than the failure.
func TestBdContainerRetryRecoversFromConnectionFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 2)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	out, err := b.run("show", "gt-rqn", "--json")
	if err != nil {
		t.Fatalf("run() after a transient connection failure: %v", err)
	}
	if !strings.Contains(string(out), "gt-rqn") {
		t.Fatalf("run() output = %q, want the command's own result", out)
	}
	if got := stub.calls(t); got != 3 {
		t.Errorf("bd invocations = %d, want 3 (two failures then the success)", got)
	}
}

// TestBdContainerRetryStopsAtAttemptCap pins the bound: a container that never
// comes back costs a fixed number of attempts, not an unbounded retry loop, and
// the error the caller sees is still bd's own stderr.
func TestBdContainerRetryStopsAtAttemptCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 99)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	_, err := b.run("show", "gt-rqn", "--json")
	if err == nil {
		t.Fatal("run() should fail when every attempt loses the connection")
	}
	if !strings.Contains(err.Error(), "invalid connection") {
		t.Errorf("exhausted-retry error should keep bd's stderr, got %v", err)
	}
	if got := stub.calls(t); got != bdContainerRetryAttempts {
		t.Errorf("bd invocations = %d, want %d (the cap)", got, bdContainerRetryAttempts)
	}
}

// TestBdContainerRetryLeavesNonContainerWrappersAlone is the blast-radius
// contract: the retry exists for testutil's ephemeral container, and a wrapper
// that is not pointed at one — every production caller — behaves exactly as it
// did before, one invocation per command.
func TestBdContainerRetryLeavesNonContainerWrappersAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 99)

	b := NewIsolated(t.TempDir()) // isolated, but no container port
	if _, err := b.run("show", "gt-rqn", "--json"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (no retry outside the container)", got)
	}
}

// TestBdContainerRetryLeavesBehavioralFailuresAlone: a failure that is not a
// lost connection is an answer, and retrying it would only delay the report.
func TestBdContainerRetryLeavesBehavioralFailuresAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installBdStub(t, `#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
echo $((count + 1)) > __COUNT__
echo "Error: invalid status: done" >&2
exit 1
`)

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	if _, err := b.run("update", "gt-rqn", "--status=done"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (a behavioral failure is not retried)", got)
	}
}

// TestRetryableBdConnectionFailure pins the predicate's edges, including the
// one that keeps a wedged bd from being retried five times: a subprocess killed
// at its own deadline has already spent its whole budget.
func TestRetryableBdConnectionFailure(t *testing.T) {
	container := NewIsolatedWithPort(t.TempDir(), 3306)
	noContainer := New(t.TempDir())

	// A stalled container can also time bd out mid-connect, which prints both
	// the raw driver error and bd's open-path wrapper (gt-6uhq's actual stderr).
	observed := fmt.Errorf("bd show gt-rqn --json: %s\n%s",
		observedIoTimeoutStderr, observedOpenFailureStderr)

	tests := []struct {
		name string
		b    *Beads
		err  error
		want bool
	}{
		{name: "observed container failure", b: container, err: observed, want: true},
		{name: "invalid connection alone", b: container, err: fmt.Errorf("bd show x: invalid connection"), want: true},
		{name: "broken pipe", b: container, err: fmt.Errorf("bd show x: write: broken pipe"), want: true},
		{name: "no container", b: noContainer, err: observed, want: false},
		{name: "nil error", b: container, err: nil, want: false},
		{name: "not found is an answer", b: container, err: ErrNotFound, want: false},
		{name: "behavioral failure", b: container, err: fmt.Errorf("bd update x: invalid status"), want: false},
		{
			// The deadline kill is the case that must not retry even though the
			// message can carry a connection marker alongside it.
			name: "subprocess deadline",
			b:    container,
			err:  fmt.Errorf("bd show gt-rqn: %s: %w", observedIoTimeoutStderr, context.DeadlineExceeded),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.retryableBdConnectionFailure(tt.err); got != tt.want {
				t.Errorf("retryableBdConnectionFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestContainerUnavailableNeverExcusesAProductionFailure pins the guard that
// the container-backed suites' skip rides on (gt-cbtl): the predicate is scoped
// to a wrapper built for a test container, so no production caller can turn a
// real failure into a skip.
func TestContainerUnavailableNeverExcusesAProductionFailure(t *testing.T) {
	lost := fmt.Errorf("bd init: %s\n%s", observedIoTimeoutStderr, observedOpenFailureStderr)

	production := New(t.TempDir())
	if production.ContainerUnavailable(lost) {
		t.Error("a production wrapper reported the test container unavailable")
	}
	container := NewIsolatedWithPort(t.TempDir(), 55107)
	if !container.ContainerUnavailable(lost) {
		t.Error("a wrapper for the test container did not report it unavailable")
	}
}

// TestBdContainerRetryWindowStopsTheLoop pins the bound that the attempt count
// alone does not give: attempts run subprocesses, so a container that stalls
// rather than refuses must not be retried on count alone. The window is aged
// out here rather than waited out.
func TestBdContainerRetryWindowStopsTheLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 99)
	zeroRetryBackoff(t)

	restore := bdContainerRetryWindow
	bdContainerRetryWindow = -time.Second // already expired when the loop starts
	t.Cleanup(func() { bdContainerRetryWindow = restore })

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	if _, err := b.run("show", "gt-rqn", "--json"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (a spent retry window stops the loop)", got)
	}
}

// TestBdContainerRetryBackoffBounds pins the budget the retry can spend: it
// must grow, stay under the cap, and stay close to the nominal delay so five
// attempts cannot silently become a multi-minute stall.
func TestBdContainerRetryBackoffBounds(t *testing.T) {
	total := time.Duration(0)
	prev := time.Duration(0)
	for attempt := 1; attempt < bdContainerRetryAttempts; attempt++ {
		got := bdContainerRetryBackoff(attempt)
		if got > bdContainerRetryMaxBackoff {
			t.Fatalf("backoff(attempt=%d) = %v, want <= %v", attempt, got, bdContainerRetryMaxBackoff)
		}
		if got < prev*3/4 {
			t.Errorf("backoff(attempt=%d) = %v, want no less than %v (growth is monotonic)", attempt, got, prev*3/4)
		}
		prev = got
		total += got
	}
	if total > 30*time.Second {
		t.Errorf("total backoff = %v, want a bounded pause well under the per-command budget", total)
	}
}

// flakyBdStub is a fake bd on PATH that counts its invocations and fails the
// first failUntil of them with the connection-failure stderr gt-6uhq recorded.
type flakyBdStub struct {
	countFile string
}

func (s *flakyBdStub) calls(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(s.countFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read bd call count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse bd call count %q: %v", raw, err)
	}
	return n
}

func installFlakyConnectionBDStub(t *testing.T, failUntil int) *flakyBdStub {
	t.Helper()
	return installBdStub(t, fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
if [ "$count" -le %d ]; then
  echo %q >&2
  echo %q >&2
  exit 1
fi
printf '%%s\n' '[{"id":"gt-rqn","title":"MR","status":"open"}]'
exit 0
`, failUntil, observedIoTimeoutStderr, observedOpenFailureStderr))
}

// installBdStub writes a fake bd whose body is script, substitutes __COUNT__
// with a per-test counter file, and puts it first on PATH. The counter file is
// how the retry loop's tests observe how many attempts actually happened —
// the behavior under test is the number of bd processes, not a Go-side tally.
func installBdStub(t *testing.T, script string) *flakyBdStub {
	t.Helper()
	// The allow-stale probe is cached per bd path for the whole process, so a
	// stub installed here must not leave a verdict from another test's binary.
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	dir := t.TempDir()
	countFile := filepath.Join(dir, "bd-calls")
	script = strings.ReplaceAll(script, "__COUNT__", countFile)

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("create stub bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &flakyBdStub{countFile: countFile}
}

// zeroRetryBackoff removes the real pause for the duration of one test, through
// the same seam the production loop reads. Without it the attempt-cap test would
// spend the full ~7.5s of backoff to observe that a bounded loop is bounded.
func zeroRetryBackoff(t *testing.T) {
	t.Helper()
	restore := bdContainerRetryBackoffFn
	bdContainerRetryBackoffFn = func(int) time.Duration { return 0 }
	t.Cleanup(func() { bdContainerRetryBackoffFn = restore })
}

//go:build integration

package shelltier

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// proofRuns is how many consecutive green concurrent runs the flip needs: the
// design's proof is three.
const proofRuns = 3

// proofJobs is the pool width the proof runs with.
const proofJobs = 3

// proofBudget bounds each script in the proof. It is the budget the package's
// end-to-end integration test runs its scripts under (e2e_integration_test.go),
// not a bound this proof introduces.
const proofBudget = time.Minute

// TestIntegrationForgejoTestsRunConcurrentlyThreeTimesGreen is the proof that
// lets the forgejo scripts in forgejoParallelScripts be Parallel (gt-u560q.12):
// it runs them through the real runner, in one pool of three, three times in a
// row, and requires a green summary and observed concurrency every run. Each
// run gets a fresh log dir, so the runner's per-script TMPDIR differs between
// runs too. Mutation: make two scripts write one fixed path outside their
// TMPDIR; concurrent runs then collide and a run goes RED.
func TestIntegrationForgejoTestsRunConcurrentlyThreeTimesGreen(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	entries := manifestEntries(t, forgejoParallelScripts)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
			t.Fatalf("script %s: %v", e.Path, err)
		}
		if e.Mode != Parallel {
			t.Fatalf("%s: Mode=%v, want Parallel: the proof would pass without two scripts ever running at once", e.Path, e.Mode)
		}
	}

	for run := 1; run <= proofRuns; run++ {
		logDir := t.TempDir()
		var diagnostics lockedBuffer
		recorder := &concurrencyRecorder{exec: OSExec}
		result := Run(t.Context(), Config{
			Entries:     entries,
			Root:        root,
			LogDir:      logDir,
			Budget:      proofBudget,
			Exec:        recorder.run,
			Now:         time.Now,
			WithTimeout: context.WithTimeout,
			Jobs:        proofJobs,
			Err:         &diagnostics,
		})
		summary := Summary(result, logDir)
		want := fmt.Sprintf("shell GREEN passed=%d failed=0 skipped=0", len(entries))
		if result.Passed != len(entries) || result.Failed != 0 || !strings.Contains(summary, want) {
			t.Fatalf("run %d of %d: summary = %q, want %q\nrunner diagnostics:\n%s\nfailed script logs:\n%s", run, proofRuns, summary, want, diagnostics.String(), failedScriptLogs(t, logDir, result.FailedNames))
		}
		if got := recorder.peakConcurrency(); got != proofJobs {
			t.Fatalf("run %d of %d: %d scripts ran at once, want %d: a green summary alone does not show they shared the pool", run, proofRuns, got, proofJobs)
		}
	}
}

// concurrencyRecorder wraps an Exec and counts the commands in flight, so the
// proof observes the overlap itself instead of inferring it from a short wall
// time. run is safe to call from every pool worker.
type concurrencyRecorder struct {
	exec Exec
	mu   sync.Mutex
	// running is the commands in flight now; peak is the most there have been.
	running, peak int
}

func (r *concurrencyRecorder) run(ctx context.Context, c Command) (int, error) {
	r.mu.Lock()
	r.running++
	if r.running > r.peak {
		r.peak = r.running
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running--
		r.mu.Unlock()
	}()
	return r.exec(ctx, c)
}

// peakConcurrency is the most commands the runner ever had in flight.
func (r *concurrencyRecorder) peakConcurrency() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

// failedScriptLogs returns each failed script's runner log, so the proof's
// failure carries the script's own verdict: the runner writes a script's
// combined output only to its log file, and the sweep pod's log file is gone
// by the time anyone reads the failure. A log that cannot be read is reported
// as such rather than left out.
func failedScriptLogs(t *testing.T, logDir string, failed []string) string {
	t.Helper()
	var b strings.Builder
	for _, path := range failed {
		logPath := filepath.Join(logDir, logName(path)+".log")
		log, err := os.ReadFile(logPath)
		if err != nil {
			fmt.Fprintf(&b, "%s: runner log unreadable: %v\n", path, err)
			continue
		}
		fmt.Fprintf(&b, "%s (%s):\n%s\n", path, logPath, log)
	}
	if b.Len() == 0 {
		return "none: no script failed\n"
	}
	return b.String()
}

// TestIntegrationFailedScriptLogsCarriesTheScriptsVerdict runs a failing script
// through the real runner and requires the failure text to hold the script's
// own output, so a red in the sweep names the check that failed.
func TestIntegrationFailedScriptLogsCarriesTheScriptsVerdict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fail_test.sh"),
		[]byte("echo 'boom: a check failed'\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	result := Run(t.Context(), Config{
		Entries:     entriesAt("fail_test.sh"),
		Root:        root,
		LogDir:      logDir,
		Budget:      time.Minute,
		Exec:        OSExec,
		Now:         time.Now,
		WithTimeout: context.WithTimeout,
	})
	if result.Failed != 1 {
		t.Fatalf("failed = %d, want 1", result.Failed)
	}
	got := failedScriptLogs(t, logDir, result.FailedNames)
	if !strings.Contains(got, "boom: a check failed") {
		t.Fatalf("failed script logs = %q, want the script's own output", got)
	}
}

// TestIntegrationFailedScriptLogsNamesAnUnreadableLog requires a log that
// cannot be read to be reported, not dropped: a failure that prints nothing
// proves nothing about the script that failed.
func TestIntegrationFailedScriptLogsNamesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	got := failedScriptLogs(t, t.TempDir(), []string{"scripts/gone_test.sh"})
	if !strings.Contains(got, "runner log unreadable") {
		t.Fatalf("failed script logs = %q, want the unreadable log named", got)
	}
}

// manifestEntries returns the manifest's entries for paths, in paths' order. A
// path with no entry fails the test: a proof that silently ran fewer scripts
// proves nothing.
func manifestEntries(t *testing.T, paths []string) []Entry {
	t.Helper()
	byPath := make(map[string]Entry, len(Manifest()))
	for _, e := range Manifest() {
		byPath[e.Path] = e
	}
	entries := make([]Entry, 0, len(paths))
	for _, p := range paths {
		e, ok := byPath[p]
		if !ok {
			t.Fatalf("%s has no manifest entry", p)
		}
		entries = append(entries, e)
	}
	return entries
}

// lockedBuffer is a bytes.Buffer the runner's writers can share, so the test
// stays correct whichever goroutine Run writes its diagnostics from.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

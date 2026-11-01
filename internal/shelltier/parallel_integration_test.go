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
// design's proof is three (docs/plans/2026-10-28-shell-tier-to-go-design.md).
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
			t.Fatalf("run %d of %d: summary = %q, want %q\nrunner diagnostics:\n%s", run, proofRuns, summary, want, diagnostics.String())
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

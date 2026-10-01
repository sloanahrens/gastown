//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/sling"
)

// TestIntegrationConvoyManager_FullLifecycle starts a real ConvoyManager over a
// bd-initialized hq (setupJournaledTown) with faked scan seams, lets both
// goroutines tick (event poll + stranded scan), verifies log output, then stops
// and verifies clean shutdown.
//
// Exercises: S-08 (start guard), S-09 (context cancellation), S-10 (resolved paths).
func TestIntegrationConvoyManager_FullLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows (process groups)")
	}
	townRoot, bd, store := setupJournaledTown(t)

	// The stranded scan returns one empty convoy and the completion check logs
	// the convoy it was asked to check.
	binDir := t.TempDir()
	checkLogPath := filepath.Join(binDir, "check.log")
	strandedJSON := `[{"id":"cv-test1","title":"Test Convoy","ready_count":0,"ready_issues":[]}]`

	var mu = &sync.Mutex{}
	var logged []string
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// Start with short scan interval so stranded scan fires quickly.
	m := NewConvoyManager(townRoot, logger, nil, 500*time.Millisecond, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.installScanFakes(strandedJSON, checkLogPath)

	// S-08: Start should succeed.
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// S-08: Second Start should be a no-op.
	if err := m.Start(); err != nil {
		t.Fatalf("double Start: %v", err)
	}

	// Wait for the seed poll to complete (first event poll tick at ~5s).
	// The first poll advances high-water marks without processing events.
	time.Sleep(6 * time.Second)

	// Create and close an issue AFTER seeding so the next poll detects it.
	if _, err := bd.CreateWithID("hq-integ1", beads.CreateOptions{Title: "Integration Test Issue", Priority: 2}); err != nil {
		t.Fatalf("create hq-integ1: %v", err)
	}
	if err := bd.CloseWithReason("done", "hq-integ1"); err != nil {
		t.Fatalf("close hq-integ1: %v", err)
	}

	// Wait for the next event poll tick to detect the close event (~5s).
	time.Sleep(6 * time.Second)

	// Stop and verify bounded completion (S-09: context cancellation).
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
		// Success -- shutdown completed.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not complete within 5s -- context cancellation may be broken")
	}

	// Verify event poll detected the close event.
	mu.Lock()
	logSnapshot := make([]string, len(logged))
	copy(logSnapshot, logged)
	mu.Unlock()

	foundClose := false
	foundScan := false
	foundDoubleStart := false
	for _, s := range logSnapshot {
		if strings.Contains(s, "close detected") && strings.Contains(s, "hq-integ1") {
			foundClose = true
		}
		if strings.Contains(s, "auto-closing") || strings.Contains(s, "convoy check") {
			foundScan = true
		}
		if strings.Contains(s, "already called") {
			foundDoubleStart = true
		}
	}

	if !foundClose {
		t.Errorf("event poll did not detect close event for hq-integ1; logs:\n%s", strings.Join(logSnapshot, "\n"))
	}
	if !foundScan {
		t.Errorf("stranded scan did not process the empty convoy; logs:\n%s", strings.Join(logSnapshot, "\n"))
	}
	if !foundDoubleStart {
		t.Errorf("double Start() guard did not fire; logs:\n%s", strings.Join(logSnapshot, "\n"))
	}

	// The scan reached the completion check for the empty convoy.
	if data, err := os.ReadFile(checkLogPath); err != nil || !strings.Contains(string(data), "cv-test1") {
		t.Errorf("completion check never ran for cv-test1: %v %q", err, data)
	}
}

// TestIntegrationConvoyManager_LoggingFlow verifies the end-to-end log chain when a close
// event triggers convoy tracking lookups and feeding decisions. This exercises
// both ConvoyManager event detection and CheckConvoysForIssue operations
// flowing through the same logger.
//
// Expected log chain for a close event with convoy tracking:
//  1. "Convoy: close detected: <issue>"
//  2. "Convoy: <issue> tracked by 1 convoy(s): [<convoy>]"
//  3. "Convoy: checking convoy <convoy>"
//  4. "Convoy: convoy <convoy>: feeding next ready issue <issue2> to <rig>"
//     OR "Convoy: convoy <convoy>: no ready issues to feed"
func TestIntegrationConvoyManager_LoggingFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows (process groups)")
	}
	// The real topology: the convoy in hq, its tasks in rig gt's own
	// database, each tracks edge stored as external:<prefix>:<id> the way gt
	// convoy writes it (addTrackingRelationWith).
	townRoot, hq, hqStore := setupJournaledTown(t)
	rig, rigStore := addJournaledRig(t, townRoot, "gt", "gt")

	// Closing task1 is the event; task2 (open, unassigned) is what the
	// continuation feed dispatches.
	const convoyID, task1, task2 = "hq-cv-logtest", "gt-logtask1", "gt-logtask2"
	if _, err := hq.CreateWithID(convoyID, beads.CreateOptions{Title: "Logging Test Convoy", Priority: 2}); err != nil {
		t.Fatalf("create %s: %v", convoyID, err)
	}
	for _, c := range []struct {
		id, title string
		priority  int
	}{
		{task1, "Task 1 (close me)", 2},
		{task2, "Task 2 (ready to feed)", 3},
	} {
		if _, err := rig.CreateWithID(c.id, beads.CreateOptions{Title: c.title, Priority: c.priority}); err != nil {
			t.Fatalf("create %s: %v", c.id, err)
		}
		if err := hq.AddTypedDependency(convoyID, "external:gt:"+c.id, "tracks"); err != nil {
			t.Fatalf("track %s: %v", c.id, err)
		}
	}
	if err := rig.CloseWithReason("done", task1); err != nil {
		t.Fatalf("close %s: %v", task1, err)
	}

	// Thread-safe logger
	var mu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// Start manager with short scan interval; event poll is 5s (fixed).
	stores := map[string]beadsdk.Storage{"hq": hqStore, "gt": rigStore}
	m := NewConvoyManager(townRoot, logger, nil, 1*time.Hour, stores, nil, nil)
	m.installScanFakes("[]", "")
	// The feeder dispatches in process (gt-638go.7): record the bead it feeds
	// instead of running a stub gt.
	var slung []string
	m.slingFn = func(_ string, opts sling.Options) (*sling.Result, error) {
		mu.Lock()
		slung = append(slung, opts.BeadID)
		mu.Unlock()
		return &sling.Result{BeadID: opts.BeadID, Success: true}, nil
	}
	// Start the cursor at zero so the poll processes events instead of warming up.
	startCursorsAtZero(m)
	// Drive one poll manually instead of waiting for the 5s ticker.
	m.pollStoresSnapshot(stores)

	mu.Lock()
	snapshot := make([]string, len(logged))
	copy(snapshot, logged)
	mu.Unlock()

	// Verify the complete log chain.
	// 1. ConvoyManager detects the close event
	assertLogContains(t, snapshot, "close detected", task1)
	// 2. CheckConvoysForIssue reports convoy tracking
	assertLogContains(t, snapshot, "tracked by", convoyID)
	// 3. CheckConvoysForIssue runs convoy check
	assertLogContains(t, snapshot, "checking convoy", convoyID)
	// 4. CheckConvoysForIssue feeds next ready issue (task2 is open+unassigned)
	assertLogContains(t, snapshot, "feeding next ready issue", task2)

	// Verify no format string errors (e.g., %!s(MISSING), %!(EXTRA)
	for _, line := range snapshot {
		if strings.Contains(line, "%!") {
			t.Errorf("malformed log line (format string error): %q", line)
		}
	}

	// Verify the feeder dispatched task2 through the engine.
	mu.Lock()
	dispatched := slices.Clone(slung)
	mu.Unlock()
	if !slices.Contains(dispatched, task2) {
		t.Errorf("dispatched beads = %v, want %s", dispatched, task2)
	}
}

// assertLogContains checks that at least one log line contains all specified substrings.
func assertLogContains(t *testing.T, logs []string, substrings ...string) {
	t.Helper()
	for _, line := range logs {
		allMatch := true
		for _, sub := range substrings {
			if sub != "" && !strings.Contains(line, sub) {
				allMatch = false
				break
			}
		}
		if allMatch {
			return
		}
	}
	t.Errorf("no log line contains all of %v; logs:\n%s", substrings, strings.Join(logs, "\n"))
}

// TestIntegrationConvoyManager_ShutdownKillsHangingSubprocess verifies that Stop()
// completes within bounded time even when a gt subprocess is hanging.
// This is the critical S-09 test: without CommandContext + process group kill,
// the wg.Wait() in Stop() would block indefinitely.
func TestIntegrationConvoyManager_ShutdownKillsHangingSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows (process groups)")
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// Short scan interval so the hanging gt fires immediately.
	m := NewConvoyManager(townRoot, logger, nil, 100*time.Millisecond, nil, nil, nil)
	// A stuck scan: it returns only when the manager's context is cancelled.
	m.findStrandedFn = func(ctx context.Context) ([]strandedConvoyInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let the stranded scan fire and start hanging.
	time.Sleep(500 * time.Millisecond)

	// Stop must complete within bounded time despite the hanging subprocess.
	// Before S-09 (exec.CommandContext), this would block forever.
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("Stop completed cleanly (subprocess was killed)")
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() blocked for >5s -- hanging subprocess was NOT killed by context cancellation")
	}
}

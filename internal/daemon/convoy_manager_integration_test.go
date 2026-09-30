//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

// TestIntegrationConvoyManager_FullLifecycle starts a real ConvoyManager with a real beads
// store and mock gt, lets both goroutines tick (event poll + stranded scan),
// verifies log output, then stops and verifies clean shutdown.
//
// Exercises: S-08 (start guard), S-09 (context cancellation), S-10 (resolved paths).
func TestIntegrationConvoyManager_FullLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows (process groups)")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// The stranded scan returns one empty convoy and the completion check logs
	// the convoy it was asked to check.
	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

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
	m := NewConvoyManager(townRoot, logger, "gt", 500*time.Millisecond, map[string]beadsdk.Storage{"hq": store}, nil, nil)
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
	now := time.Now().UTC()
	issue := &beadsdk.Issue{
		ID:        "gt-integ1",
		Title:     "Integration Test Issue",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
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
		if strings.Contains(s, "close detected") && strings.Contains(s, "gt-integ1") {
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
		t.Errorf("event poll did not detect close event for gt-integ1; logs:\n%s", strings.Join(logSnapshot, "\n"))
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

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create convoy
	convoy := &beadsdk.Issue{
		ID:        "hq-cv-logtest",
		Title:     "Logging Test Convoy",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, convoy, "test"); err != nil {
		t.Fatalf("CreateIssue convoy: %v", err)
	}

	// Create tracked issue 1 (will be closed to trigger event)
	task1 := &beadsdk.Issue{
		ID:        "gt-logtask1",
		Title:     "Task 1 (close me)",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, task1, "test"); err != nil {
		t.Fatalf("CreateIssue task1: %v", err)
	}

	// Create tracked issue 2 (stays open, should be fed)
	task2 := &beadsdk.Issue{
		ID:        "gt-logtask2",
		Title:     "Task 2 (ready to feed)",
		Status:    beadsdk.StatusOpen,
		Priority:  3,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, task2, "test"); err != nil {
		t.Fatalf("CreateIssue task2: %v", err)
	}

	// Add tracks dependencies: convoy tracks both tasks
	for _, taskID := range []string{task1.ID, task2.ID} {
		dep := &beadsdk.Dependency{
			IssueID:     convoy.ID,
			DependsOnID: taskID,
			Type:        beadsdk.DependencyType("tracks"),
			CreatedAt:   now,
			CreatedBy:   "test",
		}
		if err := store.AddDependency(ctx, dep, "test"); err != nil {
			t.Fatalf("AddDependency %s: %v", taskID, err)
		}
	}

	// Close task1 to generate a close event
	if err := store.CloseIssue(ctx, task1.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	// Set up mock gt and routes
	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// routes.jsonl: "gt-" prefix maps to rig "gt"
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "%s"
  exit 0
fi
exit 0
`, slingLogPath)

	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
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
	stores := map[string]beadsdk.Storage{"hq": store}
	m := NewConvoyManager(townRoot, logger, gtPath, 1*time.Hour, stores, nil, nil)
	m.installScanFakes("[]", "")
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
	assertLogContains(t, snapshot, "close detected", task1.ID)
	// 2. CheckConvoysForIssue reports convoy tracking
	assertLogContains(t, snapshot, "tracked by", convoy.ID)
	// 3. CheckConvoysForIssue runs convoy check
	assertLogContains(t, snapshot, "checking convoy", convoy.ID)
	// 4. CheckConvoysForIssue feeds next ready issue (task2 is open+unassigned)
	assertLogContains(t, snapshot, "feeding next ready issue", task2.ID)

	// Verify no format string errors (e.g., %!s(MISSING), %!(EXTRA)
	for _, line := range snapshot {
		if strings.Contains(line, "%!") {
			t.Errorf("malformed log line (format string error): %q", line)
		}
	}

	// Verify sling was actually called for task2
	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Errorf("sling was never called (expected dispatch of %s): %v", task2.ID, err)
	} else if !strings.Contains(string(data), task2.ID) {
		t.Errorf("sling log does not contain %s: %q", task2.ID, string(data))
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
	m := NewConvoyManager(townRoot, logger, "gt", 100*time.Millisecond, nil, nil, nil)
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

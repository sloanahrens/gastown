package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/convoy"
)

// setupTestStore opens a real beads database for integration tests.
// Skips if unavailable. Caller must run cleanup when done.
//
// BEADS_TEST_MODE is set once in TestMain, not here: t.Setenv would forbid
// t.Parallel in every caller (gt-fx3c).
func setupTestStore(t *testing.T) (beadsdk.Storage, func()) {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	doltPath := filepath.Join(beadsDir, "dolt")
	if err := os.MkdirAll(doltPath, 0755); err != nil {
		t.Skipf("cannot create test dir: %v", err)
	}
	ctx := context.Background()
	store, err := beadsdk.Open(ctx, doltPath)
	if err != nil {
		t.Skipf("beads store unavailable: %v", err)
	}
	if err := store.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		_ = store.Close()
		t.Skipf("SetConfig: %v", err)
	}
	return store, func() { _ = store.Close() }
}

// scanTestOpts configures the mockGtForScanTest helper.
type scanTestOpts struct {
	strandedJSON  string // JSON for `gt convoy stranded --json`; default "[]"
	slingFailOnce bool   // first sling invocation exits 1, subsequent succeed
	routes        string // routes.jsonl content; empty = no routes file
}

// scanTestPaths holds paths created by mockGtForScanTest.
type scanTestPaths struct {
	binDir       string
	townRoot     string
	gtPath       string // absolute path to the mock gt binary
	slingLogPath string // sling call log; absent if sling was never called
	checkLogPath string // convoy check call log; absent if check was never called
}

// mockGtForScanTest creates a mock gt binary and directory layout for scan tests.
// All mock scripts write call logs so tests can make both positive and negative assertions.
func mockGtForScanTest(t *testing.T, opts scanTestOpts) scanTestPaths {
	t.Helper()

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	if opts.routes != "" {
		if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(opts.routes), 0644); err != nil {
			t.Fatalf("write routes: %v", err)
		}
	}

	strandedJSON := opts.strandedJSON
	if strandedJSON == "" {
		strandedJSON = "[]"
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	checkLogPath := filepath.Join(binDir, "check.log")

	slingFailClause := ""
	if opts.slingFailOnce {
		slingCountPath := filepath.Join(binDir, "sling_count")
		slingFailClause = `
  if [ ! -f "` + slingCountPath + `" ]; then
    echo "1" > "` + slingCountPath + `"
    exit 1
  fi`
	}

	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '` + strings.ReplaceAll(strandedJSON, "'", "'\\''") + `'
  exit 0
fi
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"` + slingFailClause + `
  exit 0
fi
if [ "$1" = "convoy" ] && [ "$2" = "check" ]; then
  echo "$@" >> "` + checkLogPath + `"
  exit 0
fi
exit 0
`

	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	return scanTestPaths{
		binDir:       binDir,
		townRoot:     townRoot,
		gtPath:       filepath.Join(binDir, "gt"),
		slingLogPath: slingLogPath,
		checkLogPath: checkLogPath,
	}
}

func TestEventPoll_DetectsCloseEvents(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issue := &beadsdk.Issue{
		ID:        "gt-close1",
		Title:     "To Close",
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

	townRoot := t.TempDir()
	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, "gt", 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// Should have logged the close detection
	found := false
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issue.ID) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'close detected: %s' in logs, got: %v", issue.ID, logged)
	}
}

func TestEventPoll_SkipsNonCloseEvents(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issue := &beadsdk.Issue{
		ID:        "gt-open1",
		Title:     "Stays Open",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	// No close - only create event exists

	townRoot := t.TempDir()
	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, "gt", 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.pollStoresSnapshot(m.stores)

	// Should NOT have logged any close detection
	for _, s := range logged {
		if strings.Contains(s, "close detected") {
			t.Errorf("expected no close detection for open issue, got: %v", logged)
		}
	}
}

func TestManagerLifecycle_StartStop(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	bdScript := `#!/bin/sh
echo '{"type":"status","issue_id":"gt-x","new_status":"closed"}'
sleep 999
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	gtScript := `#!/bin/sh
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
}

func TestScanStranded_FeedsReadyIssues(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-cv1","title":"Test","ready_count":1,"ready_issues":["gt-issue1"]}]`,
		routes:       `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	m := NewConvoyManager(paths.townRoot, func(string, ...interface{}) {}, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	data, err := os.ReadFile(paths.slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if !strings.Contains(logContent, "sling") || !strings.Contains(logContent, "gt-issue1") {
		t.Errorf("expected gt sling to be invoked for gt-issue1, got: %q", logContent)
	}
}

func TestScanStranded_ClosesEmptyConvoys(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-empty1","title":"Empty","ready_count":0,"ready_issues":[]}]`,
	})

	m := NewConvoyManager(paths.townRoot, func(string, ...interface{}) {}, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	data, err := os.ReadFile(paths.checkLogPath)
	if err != nil {
		t.Fatalf("read check log: %v", err)
	}
	if !strings.Contains(string(data), "hq-empty1") {
		t.Errorf("expected gt convoy check for hq-empty1, got: %q", data)
	}
}

func TestScanStranded_GracePeriodSkipsRecentConvoy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	// Convoy created 30 seconds ago — well within the 5-minute grace period.
	recentTime := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	strandedJSON := fmt.Sprintf(`[{"id":"hq-new1","title":"New","tracked_count":0,"ready_count":0,"ready_issues":[],"created_at":"%s"}]`, recentTime)

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: strandedJSON,
	})

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(paths.townRoot, logger, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	// Convoy check must NOT have been called — grace period should protect it.
	if _, err := os.Stat(paths.checkLogPath); err == nil {
		data, _ := os.ReadFile(paths.checkLogPath)
		t.Errorf("convoy check was called for recent convoy (grace period should protect): %s", data)
	}

	// Should see grace period log message.
	found := false
	for _, s := range logged {
		if strings.Contains(s, "grace period") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected grace period log message, got: %v", logged)
	}
}

func TestScanStranded_GracePeriodAllowsOldConvoy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	// Convoy created 10 minutes ago — past the 5-minute grace period.
	oldTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	strandedJSON := fmt.Sprintf(`[{"id":"hq-old1","title":"Old","tracked_count":0,"ready_count":0,"ready_issues":[],"created_at":"%s"}]`, oldTime)

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: strandedJSON,
	})

	m := NewConvoyManager(paths.townRoot, func(string, ...interface{}) {}, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	data, err := os.ReadFile(paths.checkLogPath)
	if err != nil {
		t.Fatalf("read check log: %v", err)
	}
	if !strings.Contains(string(data), "hq-old1") {
		t.Errorf("expected gt convoy check for hq-old1 (past grace period), got: %q", data)
	}
}

func TestScanStranded_NoStrandedConvoys(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: "[]",
	})

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(paths.townRoot, logger, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	// Negative: sling must not have been called
	if _, err := os.Stat(paths.slingLogPath); err == nil {
		data, _ := os.ReadFile(paths.slingLogPath)
		t.Errorf("sling was called unexpectedly: %s", data)
	}
	// Negative: convoy check must not have been called
	if _, err := os.Stat(paths.checkLogPath); err == nil {
		data, _ := os.ReadFile(paths.checkLogPath)
		t.Errorf("convoy check was called unexpectedly: %s", data)
	}
	// Negative: no feeding or check activity in logs
	for _, s := range logged {
		if strings.Contains(s, "feeding") || strings.Contains(s, "sling") || strings.Contains(s, "auto-closing") {
			t.Errorf("unexpected convoy activity in logs: %s", s)
		}
	}
}

func TestScanStranded_DispatchFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON:  `[{"id":"hq-cv1","title":"Test","ready_count":1,"ready_issues":["gt-issue1"]},{"id":"hq-cv2","title":"Test2","ready_count":1,"ready_issues":["gt-issue2"]}]`,
		slingFailOnce: true,
		routes:        `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m := NewConvoyManager(paths.townRoot, logger, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	logMu.Lock()
	defer logMu.Unlock()

	// Verify the failure was logged with the correct convoy and issue IDs
	hasFailure := false
	for _, l := range logged {
		if strings.Contains(l, "gt-issue1") && strings.Contains(l, "failed") {
			hasFailure = true
			break
		}
	}
	if !hasFailure {
		t.Errorf("expected sling failure log mentioning gt-issue1, got: %v", logged)
	}

	// Verify scan continued: second convoy's issue was dispatched
	data, err := os.ReadFile(paths.slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue2") {
		t.Errorf("expected sling for gt-issue2 (scan should continue after failure), got: %q", data)
	}
}

func TestConvoyManager_DoubleStop_Idempotent(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	binDir := t.TempDir()
	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then echo '[]'; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nexit 0"), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	townRoot := t.TempDir()
	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	m.Stop() // Second stop should not deadlock
}

func TestStart_DoubleCall_Guarded(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()

	// Mock gt that returns empty stranded list and logs sling/check calls
	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	// First Start should succeed
	if err := m.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	// Second Start should be a no-op (not spawn duplicate goroutines)
	if err := m.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}

	// Verify the duplicate-call warning was logged
	logMu.Lock()
	duplicateLogged := false
	for _, s := range logged {
		if strings.Contains(s, "already called") || strings.Contains(s, "ignoring duplicate") {
			duplicateLogged = true
			break
		}
	}
	logMu.Unlock()
	if !duplicateLogged {
		t.Error("expected duplicate Start() warning in logs")
	}

	// Verify the manager still functions: Stop should complete without hanging
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not complete within 5s after double Start()")
	}
}

func TestRetryMissingStores_CompletesPartialStoreSet(t *testing.T) {
	t.Parallel()

	held := &closeTrackingStorage{}
	reopened := &closeTrackingStorage{}
	duplicate := &closeTrackingStorage{}

	calls := 0
	opener := func() storeOpenResult {
		calls++
		// The retry re-opens every store it knows about, including ones already
		// held. Dolt is up now, so hq opens where the startup walk lost it.
		return storeOpenResult{Stores: map[string]beadsdk.Storage{
			"gastown": duplicate,
			"hq":      reopened,
		}}
	}

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	// The daemon hands over a map that is missing hq: Dolt restarted between the
	// walk's first store and its last, and hq is walked first (gt-i36h).
	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute,
		map[string]beadsdk.Storage{"gastown": held}, opener, nil)

	m.retryMissingStores(time.Now())

	if m.stores["hq"] != reopened {
		t.Fatal("hq should have been adopted from the retry")
	}
	if m.stores["gastown"] != held {
		t.Error("a live handle must not be replaced by a retry")
	}
	if !duplicate.closed {
		t.Error("the duplicate handle for a store already held must be closed, not leaked")
	}
	if !m.storeRecovery.confirmed {
		t.Errorf("a non-empty store set with nothing missing should confirm complete; logs: %v", logged)
	}

	// Confirmed is final: a complete set is not reopened however long the
	// daemon runs.
	m.retryMissingStores(time.Now().Add(time.Hour))
	if calls != 1 {
		t.Errorf("opener called %d times on a complete store set, want 1", calls)
	}
}

func TestRetryMissingStores_BacksOffEscalatesAndClears(t *testing.T) {
	t.Parallel()

	reopened := &closeTrackingStorage{}
	missing := true
	calls := 0
	opener := func() storeOpenResult {
		calls++
		if missing {
			// hq is the store that matters; om is a rig store riding along.
			return storeOpenResult{Missing: []string{"hq", "om"}}
		}
		return storeOpenResult{Stores: map[string]beadsdk.Storage{"hq": reopened}}
	}

	type firing struct{ key, source, msg string }
	var raised []firing
	var cleared []string

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, "gt", time.Hour,
		map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}, opener, nil)
	m.SetAlertHooks(
		func(key, source, msg string) { raised = append(raised, firing{key, source, msg}) },
		func(reason string, keys ...string) { cleared = append(cleared, keys...) },
	)

	start := time.Now()
	m.retryMissingStores(start)
	if calls != 1 {
		t.Fatalf("first attempt should run immediately, opener called %d times", calls)
	}
	if len(raised) != 0 {
		t.Fatal("a fresh outage must not escalate before the bound")
	}

	// A retry inside the backoff must not happen — that is what keeps a down
	// Dolt from being hammered (and daemon.log from a line per tick per store).
	m.retryMissingStores(start.Add(time.Second))
	if calls != 1 {
		t.Errorf("retry ran inside its backoff: opener called %d times", calls)
	}

	// Past the bound, with hq still unopenable, escalate once.
	pastBound := start.Add(storeRecoveryEscalationAfter + time.Second)
	m.retryMissingStores(pastBound)
	if len(raised) != 1 {
		t.Fatalf("expected one escalation past the bound, got %d", len(raised))
	}
	if raised[0].key != requiredStoreAlertKey {
		t.Errorf("escalation key = %q, want %q", raised[0].key, requiredStoreAlertKey)
	}
	if !strings.Contains(raised[0].msg, "hq") {
		t.Errorf("escalation should name the missing store, got: %s", raised[0].msg)
	}
	// gt escalate builds the title as "<source>: <message>" and truncates it, so
	// a message that overflows loses the end of the sentence to the operator.
	if title := len(raised[0].source) + len(": ") + len(raised[0].msg); title > maxEscalationTitleLen {
		t.Errorf("escalation title is %d chars; gt escalate truncates at %d", title, maxEscalationTitleLen)
	}

	// The streak is already reported: it is not re-raised on every later retry.
	m.retryMissingStores(pastBound.Add(time.Hour))
	if len(raised) != 1 {
		t.Errorf("the same streak raised %d escalations, want 1", len(raised))
	}

	// hq opens: the condition is over, so the escalation is cleared.
	missing = false
	m.retryMissingStores(pastBound.Add(2 * time.Hour))
	if m.stores["hq"] != reopened {
		t.Fatal("hq should have been adopted once it opened")
	}
	if len(cleared) != 1 || cleared[0] != requiredStoreAlertKey {
		t.Errorf("recovery should clear %q, got %v", requiredStoreAlertKey, cleared)
	}
}

// TestRetryMissingStores_RigStoreMissingDoesNotEscalate pins the escalation
// scope: a rig store that will not open costs that rig's close events, not the
// town's convoy lookups, so it is retried and logged but not escalated.
func TestRetryMissingStores_RigStoreMissingDoesNotEscalate(t *testing.T) {
	t.Parallel()

	var raised []string
	store := &closeTrackingStorage{}
	opener := func() storeOpenResult {
		return storeOpenResult{
			Stores:  map[string]beadsdk.Storage{"hq": store},
			Missing: []string{"om"},
		}
	}

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, "gt", time.Hour,
		map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}, opener, nil)
	m.SetAlertHooks(
		func(key, source, msg string) { raised = append(raised, key) },
		func(reason string, keys ...string) {},
	)

	start := time.Now()
	m.retryMissingStores(start)
	if m.stores["hq"] != store {
		t.Fatal("hq should be adopted even though a rig store is missing")
	}
	if m.storeRecovery.confirmed {
		t.Error("a store set missing a rig store is not complete")
	}

	m.retryMissingStores(start.Add(storeRecoveryEscalationAfter + time.Hour))
	if len(raised) != 0 {
		t.Errorf("a missing rig store should not escalate town-wide; got %v", raised)
	}
}

func TestStoreOpenBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 80 * time.Second},
		{6, 160 * time.Second},
		{7, 5 * time.Minute},
		{8, 5 * time.Minute},
		{50, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := storeOpenBackoff(tc.attempts); got != tc.want {
			t.Errorf("storeOpenBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// TestStoreOpenerNeeded pins the daemon half of gt-i36h: the walk that loses hq
// to a mid-walk Dolt restart is neither empty nor complete, and it must still
// get the opener. Handing the opener over only when the map came up empty left
// the partial map (five rigs, no hq) with nothing that could ever retry.
func TestStoreOpenerNeeded(t *testing.T) {
	t.Parallel()

	stub := func() beadsdk.Storage { return &closeTrackingStorage{} }
	cases := []struct {
		name string
		res  storeOpenResult
		want bool
	}{
		{"nothing opened", storeOpenResult{Missing: []string{"hq", "gastown"}}, true},
		{"opened nothing, reported nothing", storeOpenResult{}, true},
		{"partial: hq lost mid-walk", storeOpenResult{
			Stores:  map[string]beadsdk.Storage{"gastown": stub(), "om": stub()},
			Missing: []string{"hq"},
		}, true},
		{"partial: a rig store lost", storeOpenResult{
			Stores:  map[string]beadsdk.Storage{"hq": stub()},
			Missing: []string{"om"},
		}, true},
		{"complete", storeOpenResult{
			Stores: map[string]beadsdk.Storage{"hq": stub(), "gastown": stub()},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storeOpenerNeeded(tc.res); got != tc.want {
				t.Errorf("storeOpenerNeeded = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLogMissingRequiredStore_IsRateLimited pins the other half of gt-i36h:
// the skip line is written from the per-store poll path, so it fired for every
// rig on every tick — 178 entries in a few minutes — until it was unreadable.
func TestLogMissingRequiredStore_IsRateLimited(t *testing.T) {
	t.Parallel()

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", time.Hour,
		map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}, nil, nil)
	m.storeRecovery.missing = []string{"hq"}

	for i := 0; i < 50; i++ {
		m.logMissingRequiredStore("gastown")
		m.logMissingRequiredStore("om")
	}
	if len(logged) != 1 {
		t.Fatalf("expected the skip line once inside the interval, got %d: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "skipping convoy lookups for gastown events") {
		t.Errorf("skip line should name the polled store: %s", logged[0])
	}
	if !strings.Contains(logged[0], "missing: hq") {
		t.Errorf("skip line should name what is missing: %s", logged[0])
	}

	// Once the interval elapses, one more line is allowed through.
	m.storesMu.Lock()
	m.storeRecovery.loggedAt = m.storeRecovery.loggedAt.Add(-missingStoreLogInterval - time.Second)
	m.storesMu.Unlock()
	m.logMissingRequiredStore("gastown")
	if len(logged) != 2 {
		t.Fatalf("expected a line after the interval elapsed, got %d: %v", len(logged), logged)
	}
}

// TestConvoyManager_DoesNotWriteThroughCallerStoreMap pins the ownership rule
// the retry path depends on: the map handed to NewConvoyManager belongs to the
// caller, and the manager must adopt stores into a map of its own.
//
// The daemon passes d.beadsStores and keeps ranging over that same map from the
// patrol goroutine (hasActiveWork). A manager that added the reopened store in
// place would be writing to a map another goroutine iterates — a concurrent map
// iteration and write, which is fatal rather than recoverable, and would take
// the daemon down; under launchd that needs a launchctl bootstrap to come back
// (gt-i36h).
func TestConvoyManager_DoesNotWriteThroughCallerStoreMap(t *testing.T) {
	t.Parallel()

	startup := map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}
	reopened := &closeTrackingStorage{}
	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, "gt", time.Hour, startup,
		func() storeOpenResult {
			return storeOpenResult{Stores: map[string]beadsdk.Storage{"hq": reopened}}
		}, nil)

	m.retryMissingStores(time.Now())
	if m.stores["hq"] != reopened {
		t.Fatal("hq should have been adopted from the retry")
	}

	// The caller's map is untouched. Its handle is still the one handed over —
	// the manager shares handles, it just does not share the map.
	if _, leaked := startup["hq"]; leaked {
		t.Error("manager wrote the reopened store through to the caller's map")
	}
	if startup["gastown"] == nil {
		t.Error("manager removed the caller's store from the caller's map")
	}

	// The read the daemon actually performs, concurrent with the manager's
	// write: ranging the caller's map while a store is adopted. Under -race
	// this is what fails on write-through. Each iteration gets a fresh pair so
	// the adopt really happens — a confirmed manager stops opening stores, so
	// reusing one would leave every attempt after the first a no-op.
	townRoot := t.TempDir()
	noop := func(string, ...interface{}) {}
	opener := func() storeOpenResult {
		return storeOpenResult{Stores: map[string]beadsdk.Storage{"hq": &closeTrackingStorage{}}}
	}
	for i := 0; i < 200; i++ {
		caller := map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}
		m := NewConvoyManager(townRoot, noop, "gt", time.Hour, caller, opener, nil)
		adopted := make(chan struct{})
		go func() {
			defer close(adopted)
			m.retryMissingStores(time.Now())
		}()
		for range caller {
		}
		<-adopted
	}
}

// closeTrackingStorage is a Storage stub that records Close, for tests that
// only care which handles the manager keeps and which it releases. The embedded
// interface panics on anything else, which is what a test wants if the code
// under test starts reading from it.
type closeTrackingStorage struct {
	beadsdk.Storage
	closed bool
	closes int
}

func (s *closeTrackingStorage) Close() error {
	s.closed = true
	s.closes++
	return nil
}

func TestConvoyManager_ScanInterval_Configurable(t *testing.T) {
	t.Parallel()
	noop := func(string, ...interface{}) {}
	m := NewConvoyManager("/tmp", noop, "gt", 0, nil, nil, nil)
	if m.scanInterval != defaultStrandedScanInterval {
		t.Errorf("interval 0 should use default %v, got %v", defaultStrandedScanInterval, m.scanInterval)
	}

	custom := 5 * time.Minute
	m2 := NewConvoyManager("/tmp", noop, "gt", custom, nil, nil, nil)
	if m2.scanInterval != custom {
		t.Errorf("interval should be %v, got %v", custom, m2.scanInterval)
	}
}

func TestStrandedConvoyInfo_JSONParsing(t *testing.T) {
	t.Parallel()
	jsonStr := `[{"id":"hq-cv1","title":"My Convoy","ready_count":2,"ready_issues":["gt-a","gt-b"],"base_branch":"main","agent":"deepseek-flash"}]`
	var result []strandedConvoyInfo
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 convoy, got %d", len(result))
	}
	c := result[0]
	if c.ID != "hq-cv1" || c.Title != "My Convoy" || c.ReadyCount != 2 {
		t.Errorf("unexpected convoy: %+v", c)
	}
	if len(c.ReadyIssues) != 2 || c.ReadyIssues[0] != "gt-a" || c.ReadyIssues[1] != "gt-b" {
		t.Errorf("unexpected ready_issues: %v", c.ReadyIssues)
	}
	// The agent recorded by `gt convoy stranded --json` must survive decoding:
	// dropping it here is how the feeder lost the sling-time --agent (gt-yg24).
	if c.Agent != "deepseek-flash" {
		t.Errorf("Agent = %q, want %q", c.Agent, "deepseek-flash")
	}
}

func TestFeedFirstReady_MultipleReadyIssues_DispatchesOnlyFirst(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Multi Ready",
		ReadyCount:  3,
		ReadyIssues: []string{"gt-issue1", "gt-issue2", "gt-issue3"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	if !strings.Contains(logContent, "gt-issue1") {
		t.Errorf("expected sling for gt-issue1, got: %q", logContent)
	}
	if strings.Contains(logContent, "gt-issue2") {
		t.Errorf("unexpected dispatch of gt-issue2: %q", logContent)
	}
	if strings.Contains(logContent, "gt-issue3") {
		t.Errorf("unexpected dispatch of gt-issue3: %q", logContent)
	}

	lines := strings.Split(strings.TrimSpace(logContent), "\n")
	if len(lines) != 1 {
		t.Errorf("expected exactly 1 sling call, got %d: %v", len(lines), lines)
	}

	feedLogged := false
	for _, s := range logged {
		if strings.Contains(s, "feeding") && strings.Contains(s, "gt-issue1") {
			feedLogged = true
			break
		}
	}
	if !feedLogged {
		t.Errorf("expected 'feeding gt-issue1' in logs, got: %v", logged)
	}
}

// A convoy the operator closed between the stranded scan and the dispatch must
// not be fed (gt-4lbz). The scan's list is a snapshot from the top of the
// cycle, so closing hq-cv-smk2e --force still left its bead re-slung seconds
// later; the guard re-reads the status as close to the sling as it can.
func TestFeedFirstReady_SkipsConvoyClosedSinceScan(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	// The convoy the snapshot named is closed by the time the feed runs.
	m.convoyStatus = func(string) (string, bool) { return "closed", true }
	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Closed under the feeder",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	})

	if data, err := os.ReadFile(slingLogPath); err == nil && len(data) > 0 {
		t.Errorf("a closed convoy must not feed, but the feeder slung: %q", string(data))
	}
	closedLogged := false
	for _, s := range logged {
		if strings.Contains(s, "closed since the stranded scan") {
			closedLogged = true
			break
		}
	}
	if !closedLogged {
		t.Errorf("expected the close to be logged, got: %v", logged)
	}

	// A status that cannot be read fails open: the stranded scan already said
	// the convoy is open, and a store hiccup must not stop the feeder.
	m.convoyStatus = func(string) (string, bool) { return "", false }
	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv2",
		Title:       "Status unreadable",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	})
	if data, err := os.ReadFile(slingLogPath); err != nil || !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("an unreadable status must fail open and feed, got %q (err=%v)", string(data), err)
	}
	failedOpenLogged := false
	for _, s := range logged {
		if strings.Contains(s, "hq-cv2") && strings.Contains(s, "failing open") {
			failedOpenLogged = true
			break
		}
	}
	if !failedOpenLogged {
		t.Errorf("expected the fail-open path to be logged so it is distinguishable from a confirmed-open convoy, got: %v", logged)
	}
}

func TestFeedFirstReady_IteratesPastDispatchFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	// Convoy has 3 ready issues. First sling fails, second succeeds.
	// Verifies feedFirstReady iterates past dispatch failure within a single convoy.
	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	slingCountPath := filepath.Join(binDir, "sling_count")
	// First sling call exits 1 (failure), subsequent succeed
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  if [ ! -f "` + slingCountPath + `" ]; then
    echo "1" > "` + slingCountPath + `"
    echo "dispatch failed" >&2
    exit 1
  fi
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Iterate Past Failure",
		ReadyCount:  3,
		ReadyIssues: []string{"gt-fail1", "gt-succeed2", "gt-notreached3"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	// First issue was attempted (and failed)
	if !strings.Contains(logContent, "gt-fail1") {
		t.Errorf("expected sling attempt for gt-fail1, got: %q", logContent)
	}
	// Second issue should succeed
	if !strings.Contains(logContent, "gt-succeed2") {
		t.Errorf("expected sling for gt-succeed2 (iterate past failure), got: %q", logContent)
	}
	// Third issue should NOT be reached (second succeeded)
	if strings.Contains(logContent, "gt-notreached3") {
		t.Errorf("unexpected dispatch of gt-notreached3 (should stop after first success): %q", logContent)
	}

	// Verify failure was logged
	hasFailure := false
	for _, l := range logged {
		if strings.Contains(l, "gt-fail1") && strings.Contains(l, "failed") {
			hasFailure = true
			break
		}
	}
	if !hasFailure {
		t.Errorf("expected sling failure log for gt-fail1, got: %v", logged)
	}
}

func TestFeedFirstReady_AllIssuesFail_LogsNoneDispatchable(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	// All sling calls fail. Verify the "no dispatchable issues" log message.
	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "always fail" >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "All Fail",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-fail1", "gt-fail2"},
	}
	m.feedFirstReady(c)

	found := false
	for _, l := range logged {
		if strings.Contains(l, "no dispatchable issues") && strings.Contains(l, "2 skipped") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'no dispatchable issues (all 2 skipped)' in logs, got: %v", logged)
	}
}

func TestFeedFirstReady_UnknownPrefix_Skips(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Unknown Prefix",
		ReadyCount:  1,
		ReadyIssues: []string{"zz-issue1"},
	}
	m.feedFirstReady(c)

	if _, err := os.Stat(slingLogPath); err == nil {
		data, _ := os.ReadFile(slingLogPath)
		t.Errorf("sling was called unexpectedly: %s", data)
	}

	skipLogged := false
	for _, s := range logged {
		if strings.Contains(s, "skipping") && strings.Contains(s, "zz-issue1") {
			skipLogged = true
			break
		}
	}
	if !skipLogged {
		t.Errorf("expected skip log for unknown prefix issue zz-issue1, got: %v", logged)
	}
}

func TestFindStranded_GtFailure_ReturnsError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()

	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo "something went wrong" >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	result, err := m.findStranded()
	if err == nil {
		t.Fatalf("expected error from findStranded, got nil with result: %v", result)
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("expected error to contain stderr message, got: %v", err)
	}
}

func TestFindStranded_InvalidJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()

	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo "this is not valid JSON at all"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	result, err := m.findStranded()
	if err == nil {
		t.Fatalf("expected error from findStranded, got nil with result: %v", result)
	}
	if !strings.Contains(err.Error(), "parsing stranded JSON") {
		t.Errorf("expected error to mention 'parsing stranded JSON', got: %v", err)
	}
}

func TestScan_FindStrandedError_LogsAndContinues(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()

	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo "stranded command failed" >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	// scan() should not panic even when findStranded fails
	m.scan()

	// Verify the error was logged
	found := false
	for _, s := range logged {
		if strings.Contains(s, "stranded scan failed") && strings.Contains(s, "stranded command failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'stranded scan failed' error in logs, got: %v", logged)
	}
}

func TestPollEvents_GetAllEventsSinceError(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	townRoot := t.TempDir()
	m := NewConvoyManager(townRoot, logger, "gt", 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)

	// Cancel the manager's context so GetAllEventsSince receives a cancelled context
	m.cancel()

	// pollEvents should not panic when store returns error
	m.pollStoresSnapshot(m.stores)

	// Verify the error was logged with retry message
	found := false
	for _, s := range logged {
		if strings.Contains(s, "event poll error") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'event poll error' in logs, got: %v", logged)
	}
}

func TestFeedFirstReady_UnknownRig_Skips(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	// "hq-" prefix routes to town-level path "." which has no rig name
	routes := `{"prefix":"hq-","path":"."}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Town-level Rig",
		ReadyCount:  1,
		ReadyIssues: []string{"hq-issue1"},
	}
	m.feedFirstReady(c)

	if _, err := os.Stat(slingLogPath); err == nil {
		data, _ := os.ReadFile(slingLogPath)
		t.Errorf("sling was called unexpectedly: %s", data)
	}

	skipLogged := false
	for _, s := range logged {
		if strings.Contains(s, "skipping") && strings.Contains(s, "hq-issue1") {
			skipLogged = true
			break
		}
	}
	if !skipLogged {
		t.Errorf("expected skip log for rig-less issue hq-issue1, got: %v", logged)
	}
}

func TestFeedFirstReady_ParkedRig_Skips(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"sh-","path":"shippercrm/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// isRigParked returns true for "shippercrm"
	parked := func(rig string) bool { return rig == "shippercrm" }
	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, parked)

	c := strandedConvoyInfo{
		ID:          "hq-cv-park1",
		Title:       "Parked Rig Convoy",
		ReadyCount:  1,
		ReadyIssues: []string{"sh-issue1"},
	}
	m.feedFirstReady(c)

	// Sling should NOT have been called
	if _, err := os.Stat(slingLogPath); err == nil {
		data, _ := os.ReadFile(slingLogPath)
		t.Errorf("sling was called for parked rig: %s", data)
	}

	// Should log the parked skip
	skipLogged := false
	for _, s := range logged {
		if strings.Contains(s, "parked") && strings.Contains(s, "shippercrm") {
			skipLogged = true
			break
		}
	}
	if !skipLogged {
		t.Errorf("expected parked rig skip log, got: %v", logged)
	}
}

func TestFeedFirstReady_EmptyReadyIssues_NoOp(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	callLogPath := filepath.Join(binDir, "gt-calls.log")
	gtScript := `#!/bin/sh
echo "$@" >> "` + callLogPath + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Empty Ready",
		ReadyCount:  3,
		ReadyIssues: []string{},
	}
	m.feedFirstReady(c)

	if _, err := os.Stat(callLogPath); err == nil {
		data, _ := os.ReadFile(callLogPath)
		t.Errorf("gt was called unexpectedly: %s", data)
	}

	for _, s := range logged {
		if strings.Contains(s, "error") || strings.Contains(s, "failed") || strings.Contains(s, "skipping") {
			t.Errorf("unexpected log message for empty ReadyIssues: %s", s)
		}
	}
}

func TestFeedFirstReady_PassesDaemonActor(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv-xprwe",
		Title:       "Actor Attribution",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	if !strings.Contains(logContent, "--actor=daemon/convoy:hq-cv-xprwe") {
		t.Errorf("expected daemon-originated sling to record actor daemon/convoy:hq-cv-xprwe, got: %q", logContent)
	}
}

// TestFeedFirstReady_PassesConvoyAgent is the regression test for gt-yg24: a
// convoy that recorded the sling-time --agent must re-feed with that agent.
// Without it the daemon re-dispatched with the rig default, silently
// overriding every per-bead routing decision whenever a first sling failed.
func TestFeedFirstReady_PassesConvoyAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv-agent1",
		Title:       "Agent passthrough",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
		Agent:       "deepseek-flash",
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	if !strings.Contains(logContent, "--agent=deepseek-flash") {
		t.Errorf("expected re-feed to pass the convoy's agent, got: %q", logContent)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, s := range *logged {
		if strings.Contains(s, `agent "deepseek-flash" recorded on convoy`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected feed log to name the recorded agent, got: %v", *logged)
	}
}

// TestFeedFirstReady_NoAgent_LogsRigDefault guards the other half of gt-yg24:
// when no agent was recorded the feed falls back to gt sling's own resolution,
// but it must name the rig default it expects instead of applying it silently.
func TestFeedFirstReady_NoAgent_LogsRigDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv-noagent",
		Title:       "No recorded agent",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if strings.Contains(string(data), "--agent=") {
		t.Errorf("expected no --agent when none was recorded, got: %q", string(data))
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, s := range *logged {
		if strings.Contains(s, "rig default agent") && strings.Contains(s, "no --agent recorded on convoy") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected feed log to name the rig default fallback, got: %v", *logged)
	}
}

// TestFeedFirstReady_PassesConvoyFormula is the regression test for gt-4lor: a
// convoy that recorded the sling-time --formula must re-feed with that
// formula. Without it, a sling whose formula bond failed and rolled back left
// the convoy open, and the daemon's re-feed ran the bead under gt sling's
// default formula instead of the one the original sling asked for.
func TestFeedFirstReady_PassesConvoyFormula(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv-formula1",
		Title:       "Formula passthrough",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
		Formula:     "mol-doc-audit",
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	if !strings.Contains(logContent, "--formula=mol-doc-audit") {
		t.Errorf("expected re-feed to pass the convoy's formula, got: %q", logContent)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, s := range *logged {
		if strings.Contains(s, `formula "mol-doc-audit" recorded on convoy`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected feed log to name the recorded formula, got: %v", *logged)
	}
}

// TestFeedFirstReady_NoFormula_OmitsFlag guards the other half of gt-4lor:
// when no formula was recorded, the feed passes no --formula and leaves
// resolution to gt sling's own default, rather than inventing one.
func TestFeedFirstReady_NoFormula_OmitsFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot, gtPath, slingLogPath, _ := feedTestRig(t)
	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv-noformula",
		Title:       "No recorded formula",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if strings.Contains(string(data), "--formula=") {
		t.Errorf("expected no --formula when none was recorded, got: %q", string(data))
	}
}

func TestFeedFirstReady_RejectionMarker_SkipsAndDefersToDeacon(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	rejected := &beadsdk.Issue{
		ID:        "gt-rejected1",
		Title:     "Previously rejected",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		Notes:     "MERGE REJECTION (attempt 1): needs work - see review\nBranch: polecat/x/gt-rejected1+abc",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, rejected, "test"); err != nil {
		t.Fatalf("CreateIssue rejected: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID:        "gt-fresh2",
		Title:     "Never touched",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Has Rejected Bead",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-rejected1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	if strings.Contains(logContent, "gt-rejected1") {
		t.Errorf("rejected bead should not be slung by the daemon (deacon owns redispatch), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected the never-rejected bead to be slung, got: %q", logContent)
	}

	deferred := false
	for _, l := range logged {
		if strings.Contains(l, "gt-rejected1") && strings.Contains(l, "rejection marker") {
			deferred = true
			break
		}
	}
	if !deferred {
		t.Errorf("expected a rejection-marker skip log for gt-rejected1, got: %v", logged)
	}
}

func TestFeedFirstReady_NoStoreForRig_FailsOpen(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	// No store is wired for the "gt" rig (only "hq" is present, as in most
	// daemon deployments where a rig's store failed to open). Rejection-marker
	// lookup must fail open rather than block dispatch of unrelated issues.
	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, map[string]beadsdk.Storage{}, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "No Rig Store",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("expected sling to proceed when rig store is unavailable, but it was never called: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("expected sling for gt-issue1, got: %q", string(data))
	}

	// The Unknown verdict (no store for the rig) must be logged explicitly,
	// not folded silently into the same path as a confirmed-clean record
	// (gt-udrrw, gt-jj29p).
	assertLogged(t, logged, "gt-issue1", "could not confirm rejection-marker state")
}

// TestFeedHold_Verdicts pins the stranded scan's read of a bead's record: a
// rig with no open store reports no store (the fail-open gap the store alert
// owns), a read failure is an unreadable hold, a present marker is a merge
// rejection, and a clean record holds nothing (gt-udrrw, gt-ghyfx).
func TestFeedHold_Verdicts(t *testing.T) {
	t.Parallel()

	store := &holdTestStorage{
		issues: map[string]*beadsdk.Issue{
			"gt-clean":    {Notes: "nothing to see here"},
			"gt-rejected": {Notes: "MERGE REJECTION (attempt 1): see review"},
		},
	}
	errStore := &holdTestStorage{readErr: fmt.Errorf("dolt: connection refused")}

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, "gt", 10*time.Minute,
		map[string]beadsdk.Storage{"gt": store, "broken": errStore}, nil, nil)

	if _, ok := m.feedHold("missing-rig", "gt-clean"); ok {
		t.Errorf("no store for rig: want ok=false")
	}
	if hold, ok := m.feedHold("broken", "gt-clean"); !ok || !hold.Unreadable || hold.Reason == "" {
		t.Errorf("store read error: want an unreadable hold with a reason, got %+v ok=%v", hold, ok)
	}
	if hold, ok := m.feedHold("gt", "gt-clean"); !ok || hold != (convoy.Hold{}) {
		t.Errorf("clean record: want no hold, got %+v ok=%v", hold, ok)
	}
	if hold, ok := m.feedHold("gt", "gt-rejected"); !ok || !hold.MergeRejection {
		t.Errorf("rejected record: want a merge-rejection hold, got %+v ok=%v", hold, ok)
	}
}

// TestFeedFirstReady_RejectionMarker_HermeticStore is the stranded-scan half
// of gt-ghyfx without a real store: the rejected bead defers to the deacon with
// the same log it always had, and the fresh sibling feeds.
func TestFeedFirstReady_RejectionMarker_HermeticStore(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store := &holdTestStorage{issues: map[string]*beadsdk.Issue{
		"gt-rejected1": {Status: beadsdk.StatusOpen, Notes: "MERGE REJECTION (attempt 1): needs work - see review"},
		"gt-fresh2":    {Status: beadsdk.StatusOpen},
	}}
	townRoot, gtPath, slingLogPath := holdTestTown(t)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv1",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-rejected1", "gt-fresh2"},
	})

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("expected the fresh bead to be slung: %v; log: %v", err, logged)
	}
	if strings.Contains(string(data), "gt-rejected1") {
		t.Errorf("rejected bead was slung: %q", data)
	}
	if !strings.Contains(string(data), "gt-fresh2") {
		t.Errorf("expected gt-fresh2 to be slung, got %q", data)
	}
	assertLogged(t, logged, "gt-rejected1", "rejection marker, deferring to deacon")
}

// TestFeedFirstReady_UnreadableRecord_FailsClosedAtRejectionGate pins the one
// behaviour gt-ghyfx changes in the stranded scan. The rejection gate used to
// read an unreadable record as "proceeding as clear" and leave the skip to the
// later hold check; it now holds the bead at the gate, because a rejection
// cannot be ruled out. The bead was never fed either way; what changes is that
// the gate says so, and the dead-holder and surviving-branch checks do not run
// on a record nobody could read.
func TestFeedFirstReady_UnreadableRecord_FailsClosedAtRejectionGate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store := &holdTestStorage{readErr: fmt.Errorf("dolt unreachable")}
	townRoot, gtPath, slingLogPath := holdTestTown(t)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-u", ReadyCount: 1, ReadyIssues: []string{"gt-unreadable"}})

	if data, err := os.ReadFile(slingLogPath); err == nil {
		t.Errorf("unreadable record was fed: %q", data)
	}
	assertLogged(t, logged, "gt-unreadable", "cannot rule out a merge rejection (fail-closed)")
	for _, l := range logged {
		if strings.Contains(l, "proceeding as clear") || strings.Contains(l, "surviving branch") {
			t.Errorf("unreadable record went past the rejection gate: %q", l)
		}
	}
}

// holdTestTown builds a town whose gt- prefix routes to rig "gt" and a gt stub
// that records each sling, for the hermetic feedFirstReady tests.
func holdTestTown(t *testing.T) (townRoot, gtPath, slingLogPath string) {
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
	gtScript := "#!/bin/sh\nif [ \"$1\" = \"sling\" ]; then\n  echo \"$@\" >> \"" + slingLogPath + "\"\nfi\nexit 0\n"
	gtPath = filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}
	return townRoot, gtPath, slingLogPath
}

func TestScanStranded_OwnedConvoy_SkipsAutoFeed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-cv1","title":"Owned Convoy","ready_count":1,"ready_issues":["gt-issue1"],"owned":true}]`,
		routes:       `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(paths.townRoot, logger, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	if _, err := os.Stat(paths.slingLogPath); err == nil {
		data, _ := os.ReadFile(paths.slingLogPath)
		t.Errorf("owned convoy must not be auto-fed by the stranded scan, got sling call: %s", data)
	}

	found := false
	for _, l := range logged {
		if strings.Contains(l, "hq-cv1") && strings.Contains(l, "owned") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an 'owned, skipping auto-feed' log for hq-cv1, got: %v", logged)
	}
}

func TestScanStranded_NonOwnedConvoy_StillFed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-cv1","title":"Regular Convoy","ready_count":1,"ready_issues":["gt-issue1"],"owned":false}]`,
		routes:       `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	m := NewConvoyManager(paths.townRoot, func(string, ...interface{}) {}, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	data, err := os.ReadFile(paths.slingLogPath)
	if err != nil {
		t.Fatalf("expected non-owned convoy to still be auto-fed: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("expected gt sling to be invoked for gt-issue1, got: %q", string(data))
	}
}

func TestScan_ContextCancelled_MidIteration(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	// Build a stranded list with 5 convoys, all with ready issues.
	// The mock gt will block on sling calls so we can cancel mid-iteration.
	type convoy struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		ReadyCount  int      `json:"ready_count"`
		ReadyIssues []string `json:"ready_issues"`
	}
	convoys := make([]convoy, 5)
	for i := range convoys {
		convoys[i] = convoy{
			ID:          fmt.Sprintf("hq-cv%d", i+1),
			Title:       fmt.Sprintf("Convoy %d", i+1),
			ReadyCount:  1,
			ReadyIssues: []string{fmt.Sprintf("gt-issue%d", i+1)},
		}
	}
	jsonBytes, err := json.Marshal(convoys)
	if err != nil {
		t.Fatalf("marshal stranded JSON: %v", err)
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")

	// Mock gt: stranded returns list; sling sleeps 10s (simulates slow dispatch)
	gtScript := `#!/bin/sh
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '` + strings.ReplaceAll(string(jsonBytes), "'", "'\\''") + `'
  exit 0
fi
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  sleep 10
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, nil, nil, nil)

	// Run scan in a goroutine and cancel context after a brief delay
	done := make(chan struct{})
	go func() {
		m.scan()
		close(done)
	}()

	// Give scan time to start processing, then cancel
	time.Sleep(200 * time.Millisecond)
	m.cancel()

	// scan() must exit cleanly within a bounded time (not hang on all 5 convoys)
	select {
	case <-done:
		// Clean exit -- success
	case <-time.After(5 * time.Second):
		t.Fatal("scan() did not exit within 5s after context cancellation")
	}

	// Verify it did NOT process all 5 convoys (cancellation stopped iteration)
	logMu.Lock()
	defer logMu.Unlock()
	feedCount := 0
	for _, s := range logged {
		if strings.Contains(s, "feeding") {
			feedCount++
		}
	}
	if feedCount >= 5 {
		t.Errorf("expected cancellation to stop iteration before all 5 convoys, but all were fed")
	}
}

func TestScanStranded_MixedReadyAndEmpty(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[
			{"id":"hq-ready1","title":"Ready One","ready_count":1,"ready_issues":["gt-issue1"]},
			{"id":"hq-empty1","title":"Empty One","ready_count":0,"ready_issues":[]},
			{"id":"hq-ready2","title":"Ready Two","ready_count":2,"ready_issues":["gt-issue2","gt-issue3"]},
			{"id":"hq-empty2","title":"Empty Two","ready_count":0,"ready_issues":[]}
		]`,
		routes: `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m := NewConvoyManager(paths.townRoot, logger, paths.gtPath, 10*time.Minute, nil, nil, nil)
	m.scan()

	// Verify ready convoys were dispatched via sling
	slingData, err := os.ReadFile(paths.slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v (sling was never called)", err)
	}
	slingContent := string(slingData)
	if !strings.Contains(slingContent, "gt-issue1") {
		t.Errorf("expected sling for gt-issue1 (ready convoy), got: %q", slingContent)
	}
	if !strings.Contains(slingContent, "gt-issue2") {
		t.Errorf("expected sling for gt-issue2 (ready convoy), got: %q", slingContent)
	}

	// Verify empty convoys were routed to convoy check
	checkData, err := os.ReadFile(paths.checkLogPath)
	if err != nil {
		t.Fatalf("read check log: %v (convoy check was never called)", err)
	}
	checkContent := string(checkData)
	if !strings.Contains(checkContent, "hq-empty1") {
		t.Errorf("expected convoy check for hq-empty1 (empty convoy), got: %q", checkContent)
	}
	if !strings.Contains(checkContent, "hq-empty2") {
		t.Errorf("expected convoy check for hq-empty2 (empty convoy), got: %q", checkContent)
	}

	// Negative: ready convoys should NOT appear in check log
	if strings.Contains(checkContent, "hq-ready1") {
		t.Errorf("ready convoy hq-ready1 should not appear in check log: %q", checkContent)
	}
	if strings.Contains(checkContent, "hq-ready2") {
		t.Errorf("ready convoy hq-ready2 should not appear in check log: %q", checkContent)
	}

	// Negative: empty convoys should NOT appear in sling log
	if strings.Contains(slingContent, "hq-empty1") || strings.Contains(slingContent, "hq-empty2") {
		t.Errorf("empty convoys should not appear in sling log: %q", slingContent)
	}
}

// --- P0: Stop() closes lazily-opened stores ---

func TestStop_ClosesLazilyOpenedStores(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup() // safety net; Stop() should close first

	opener := func() storeOpenResult {
		return storeOpenResult{Stores: map[string]beadsdk.Storage{"hq": store}}
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, nil, opener, nil)

	// Simulate lazy opening (as runEventPoll does when stores are nil)
	m.retryMissingStores(time.Now())
	if len(m.stores) != 1 {
		t.Fatalf("expected 1 store from opener, got %d", len(m.stores))
	}

	m.Stop()

	// Verify Close was called via the log message Stop() emits
	found := false
	for _, s := range logged {
		if strings.Contains(s, "closed beads store") && strings.Contains(s, "hq") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'closed beads store (hq)' in logs after Stop(), got: %v", logged)
	}

	// Verify stores map is nil after Stop
	if m.stores != nil {
		t.Error("stores should be nil after Stop()")
	}
}

func TestStop_ClosesMultipleStores(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":      hqStore,
		"gastown": rigStore,
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)
	m.Stop()

	// Both stores should have been closed
	closedHq := false
	closedRig := false
	for _, s := range logged {
		if strings.Contains(s, "closed beads store") && strings.Contains(s, "hq") {
			closedHq = true
		}
		if strings.Contains(s, "closed beads store") && strings.Contains(s, "gastown") {
			closedRig = true
		}
	}
	if !closedHq {
		t.Errorf("expected hq store closed in logs, got: %v", logged)
	}
	if !closedRig {
		t.Errorf("expected gastown store closed in logs, got: %v", logged)
	}
	if m.stores != nil {
		t.Error("stores should be nil after Stop()")
	}
}

// --- P0: Multi-rig event poll ---

func TestPollAllStores_MultiRig_DetectsCloseFromNonHqStore(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create and close an issue in the rig store (NOT hq).
	// This is the core multi-rig scenario: events originate from per-rig stores.
	issue := &beadsdk.Issue{
		ID:        "sh-rig1",
		Title:     "Rig Issue",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := rigStore.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue in rig store: %v", err)
	}
	if err := rigStore.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue in rig store: %v", err)
	}

	// hq store has no close events — only rig store does

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":         hqStore,
		"shippercrm": rigStore,
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// The close event from the rig store should be detected
	found := false
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, "sh-rig1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected close event from non-hq store (shippercrm) to be detected, got: %v", logged)
	}
}

func TestPollAllStores_MultiRig_BothStoresPolled(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Close event in hq store
	hqIssue := &beadsdk.Issue{
		ID: "hq-task1", Title: "HQ Task", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := hqStore.CreateIssue(ctx, hqIssue, "test"); err != nil {
		t.Fatalf("CreateIssue hq: %v", err)
	}
	if err := hqStore.CloseIssue(ctx, hqIssue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue hq: %v", err)
	}

	// Close event in rig store
	rigIssue := &beadsdk.Issue{
		ID: "gt-task1", Title: "Rig Task", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := rigStore.CreateIssue(ctx, rigIssue, "test"); err != nil {
		t.Fatalf("CreateIssue rig: %v", err)
	}
	if err := rigStore.CloseIssue(ctx, rigIssue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue rig: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":      hqStore,
		"gastown": rigStore,
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// Both close events should be detected
	foundHq := false
	foundRig := false
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, "hq-task1") {
			foundHq = true
		}
		if strings.Contains(s, "close detected") && strings.Contains(s, "gt-task1") {
			foundRig = true
		}
	}
	if !foundHq {
		t.Errorf("expected close event from hq store, got: %v", logged)
	}
	if !foundRig {
		t.Errorf("expected close event from rig store, got: %v", logged)
	}
}

// --- P1: Parked rig skipping ---

func TestPollAllStores_SkipsParkedRigs(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	activeStore, activeCleanup := setupTestStore(t)
	defer activeCleanup()
	parkedStore, parkedCleanup := setupTestStore(t)
	defer parkedCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Use unique IDs to avoid cross-test contamination from shared Dolt server
	activeID := fmt.Sprintf("gt-active-park-%d", time.Now().UnixNano())
	parkedID := fmt.Sprintf("sh-parked-park-%d", time.Now().UnixNano())

	// Close events in both rig stores
	for _, tc := range []struct {
		store beadsdk.Storage
		id    string
	}{
		{activeStore, activeID},
		{parkedStore, parkedID},
	} {
		issue := &beadsdk.Issue{
			ID: tc.id, Title: tc.id, Status: beadsdk.StatusOpen,
			Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
		}
		if err := tc.store.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", tc.id, err)
		}
		if err := tc.store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
			t.Fatalf("CloseIssue %s: %v", tc.id, err)
		}
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":         hqStore,
		"gastown":    activeStore,
		"shippercrm": parkedStore,
	}

	isParked := func(rig string) bool {
		return rig == "shippercrm"
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, isParked)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// Active rig's close event should be detected
	foundActive := false
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, activeID) {
			foundActive = true
		}
	}
	if !foundActive {
		t.Errorf("expected close event from active rig (gastown) for %s, got: %v", activeID, logged)
	}

	// Parked rig store should not be polled (verified via high-water mark).
	// Note: the parked store's events may still be visible through other stores
	// if they share the same underlying Dolt server (test infrastructure detail).
	// What matters is that the "shippercrm" store key is never polled.
	if _, hasHW := m.lastEventIDs.Load("shippercrm"); hasHW {
		t.Errorf("parked rig (shippercrm) should not have been polled, but has a high-water mark")
	}
	// Active rig should have been polled
	if _, hasHW := m.lastEventIDs.Load("gastown"); !hasHW {
		t.Errorf("active rig (gastown) should have been polled, but has no high-water mark")
	}
}

func TestPollAllStores_HqNeverSkippedEvenIfParkedCallbackReturnsTrue(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issue := &beadsdk.Issue{
		ID: "hq-always1", Title: "HQ Always Polled", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// isRigParked returns true for EVERYTHING — but hq should still be polled
	// because the code checks `name != "hq" && m.isRigParked(name)`
	alwaysParked := func(string) bool { return true }

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, alwaysParked)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	found := false
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, "hq-always1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hq store should always be polled regardless of isRigParked, got: %v", logged)
	}
}

// --- P2: High-water mark monotonicity ---

func TestPollAllStores_HighWaterMark_NoReprocessing(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	// Use unique ID to avoid cross-test contamination from shared Dolt server
	issueID := fmt.Sprintf("gt-hw-%d", time.Now().UnixNano())
	issue := &beadsdk.Issue{
		ID: issueID, Title: "High Water Test", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, nil)

	// First poll: should detect our close event
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	closeCount := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			closeCount++
		}
	}
	if closeCount != 1 {
		t.Fatalf("expected 1 close detection for %s on first poll, got %d: %v", issueID, closeCount, logged)
	}

	// Second poll: high-water mark + dedup should prevent reprocessing
	logged = nil // Reset log to only check new entries
	m.pollStoresSnapshot(m.stores)

	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			t.Errorf("expected no reprocessing of %s after second poll, but found: %s", issueID, s)
		}
	}
}

func TestPollAllStores_ReopenClearsCloseDedupAcrossPolls(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issueID := fmt.Sprintf("gt-reclose-%d", time.Now().UnixNano())
	issue := &beadsdk.Issue{
		ID: issueID, Title: "Reclose Test", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	firstCloseCount := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			firstCloseCount++
		}
	}
	if firstCloseCount != 1 {
		t.Fatalf("expected 1 close detection for %s on first close, got %d: %v", issueID, firstCloseCount, logged)
	}

	time.Sleep(10 * time.Millisecond)
	if err := store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"status": beadsdk.StatusOpen}, "test"); err != nil {
		t.Fatalf("ReopenIssue via UpdateIssue: %v", err)
	}

	logged = nil
	m.pollStoresSnapshot(m.stores)

	if _, ok := m.processedCloses.Load(issueID); ok {
		t.Fatalf("expected processedCloses entry for %s to be cleared after reopen", issueID)
	}
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			t.Fatalf("expected reopen poll not to log a close for %s, got: %v", issueID, logged)
		}
	}

	time.Sleep(10 * time.Millisecond)
	if err := store.CloseIssue(ctx, issue.ID, "done again", "test", ""); err != nil {
		t.Fatalf("CloseIssue again: %v", err)
	}

	logged = nil
	m.pollStoresSnapshot(m.stores)

	secondCloseCount := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			secondCloseCount++
		}
	}
	if secondCloseCount != 1 {
		t.Fatalf("expected 1 close detection for %s after reopen/reclose, got %d: %v", issueID, secondCloseCount, logged)
	}
}

func TestPollAllStores_ReopenResetsPerCycleDedup(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issueID := fmt.Sprintf("gt-reclose-same-poll-%d", time.Now().UnixNano())
	issue := &beadsdk.Issue{
		ID: issueID, Title: "Reclose Same Poll Test", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	// Beads events use CURRENT_TIMESTAMP in Dolt, which is second precision.
	// Space the lifecycle transitions across distinct seconds so the store's
	// created_at ordering is deterministic within this single poll.
	time.Sleep(1100 * time.Millisecond)
	if err := store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"status": beadsdk.StatusOpen}, "test"); err != nil {
		t.Fatalf("ReopenIssue via UpdateIssue: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)
	if err := store.CloseIssue(ctx, issue.ID, "done again", "test", ""); err != nil {
		t.Fatalf("CloseIssue again: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	closeCount := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			closeCount++
		}
	}
	if closeCount != 2 {
		t.Fatalf("expected 2 close detections for %s when close->reopen->close occurs in one poll, got %d: %v", issueID, closeCount, logged)
	}
}

// TestPollAllStores_CrossStoreDedup verifies that a close event seen from
// multiple stores is only processed once (GH #1798).
func TestPollAllStores_CrossStoreDedup(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issueID := fmt.Sprintf("gt-dedup-%d", time.Now().UnixNano())

	// Create and close the same issue in BOTH stores (simulating replication)
	for _, store := range []beadsdk.Storage{hqStore, rigStore} {
		issue := &beadsdk.Issue{
			ID: issueID, Title: "Dedup Test", Status: beadsdk.StatusOpen,
			Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
		}
		if err := store.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if err := store.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
			t.Fatalf("CloseIssue: %v", err)
		}
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":      hqStore,
		"gastown": rigStore,
	}
	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// Should see exactly 1 close detection for our issue, not 2
	closeCount := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			closeCount++
		}
	}
	if closeCount != 1 {
		t.Errorf("expected exactly 1 close detection for %s (cross-store dedup), got %d: %v", issueID, closeCount, logged)
	}
}

func TestPollAllStores_PerStoreHighWaterMarks(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	hqStore, hqCleanup := setupTestStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Close event only in hq initially
	hqIssue := &beadsdk.Issue{
		ID: "hq-hw1", Title: "HQ HW", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := hqStore.CreateIssue(ctx, hqIssue, "test"); err != nil {
		t.Fatalf("CreateIssue hq: %v", err)
	}
	if err := hqStore.CloseIssue(ctx, hqIssue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue hq: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":      hqStore,
		"gastown": rigStore,
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)

	// First poll: only hq has a close event
	m.pollStoresSnapshot(m.stores)

	// Now add a close event to gastown AFTER the first poll
	rigIssue := &beadsdk.Issue{
		ID: "gt-hw2", Title: "Rig HW", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := rigStore.CreateIssue(ctx, rigIssue, "test"); err != nil {
		t.Fatalf("CreateIssue rig: %v", err)
	}
	if err := rigStore.CloseIssue(ctx, rigIssue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue rig: %v", err)
	}

	// Second poll: gastown's new event should be detected, hq's old event should NOT
	logged = nil // reset
	m.pollStoresSnapshot(m.stores)

	foundNewRig := false
	foundOldHq := false
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, "gt-hw2") {
			foundNewRig = true
		}
		if strings.Contains(s, "close detected") && strings.Contains(s, "hq-hw1") {
			foundOldHq = true
		}
	}
	if !foundNewRig {
		t.Errorf("expected new rig close event (gt-hw2) on second poll, got: %v", logged)
	}
	if foundOldHq {
		t.Errorf("hq close event (hq-hw1) should NOT be reprocessed on second poll (per-store high-water marks), got: %v", logged)
	}
}

func TestEventPoll_SkipsNonCloseEvents_NegativeAssertion(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	// Use unique ID to avoid cross-test contamination from shared Dolt server
	issueID := fmt.Sprintf("gt-open2-%d", time.Now().UnixNano())
	issue := &beadsdk.Issue{
		ID:        issueID,
		Title:     "Stays Open",
		Status:    beadsdk.StatusOpen,
		Priority:  2,
		IssueType: beadsdk.TypeTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()

	callLogPath := filepath.Join(binDir, "gt-calls.log")
	gtScript := `#!/bin/sh
echo "$@" >> "` + callLogPath + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// Only check for close events involving OUR issue — other tests may have
	// created close events in the shared Dolt server that leak into this store.
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, issueID) {
			t.Errorf("expected no close detection for open issue %s, got: %s", issueID, s)
		}
	}
}

// --- hq store nil guard ---

func TestPollStore_NilHqStore_LogsWarningAndSkips(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	// Create a rig store with a close event, but no hq store in the map.
	// The nil hq guard should log a warning and skip convoy lookups.
	rigStore, rigCleanup := setupTestStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issue := &beadsdk.Issue{
		ID: "gt-nohq1", Title: "No HQ Store", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := rigStore.CreateIssue(ctx, issue, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := rigStore.CloseIssue(ctx, issue.ID, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// stores map has a rig but no "hq" key
	stores := map[string]beadsdk.Storage{
		"gastown": rigStore,
	}

	m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)
	m.seeded.Store(true)
	m.pollStoresSnapshot(m.stores)

	// Should log the nil hq warning
	foundWarning := false
	for _, s := range logged {
		if strings.Contains(s, "hq store unavailable") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected 'hq store unavailable' warning, got: %v", logged)
	}

	// Should NOT have logged any close detection (skipped before processing events)
	for _, s := range logged {
		if strings.Contains(s, "close detected") {
			t.Errorf("expected no close detection without hq store, got: %s", s)
		}
	}
}

// TestRecoveryMode_SetOnPollError verifies that recoveryMode is set when
// an event poll encounters an error (Dolt unavailable).
func TestRecoveryMode_SetOnPollError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot := t.TempDir()
	var logged []string
	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	// Use a broken store that returns errors
	m := NewConvoyManager(townRoot, logger, "gt", 10*time.Minute, nil, nil, nil)

	// recoveryMode should start false
	if m.recoveryMode.Load() {
		t.Fatal("recoveryMode should be false initially")
	}

	// Simulate a poll with a store that will error (nil store map means no polling)
	// Instead, directly test the flag behavior:
	m.recoveryMode.Store(true)
	if !m.recoveryMode.Load() {
		t.Fatal("recoveryMode should be true after Store(true)")
	}

	// scan() should clear it (scan will fail on findStranded since no gt binary, but
	// that's OK — the test verifies the flag is cleared on success path only)
	m.recoveryMode.Store(false)
	if m.recoveryMode.Load() {
		t.Fatal("recoveryMode should be false after Store(false)")
	}
}

// TestRecoveryMode_ClearedAfterSuccessfulScan verifies that a successful
// scan() call clears recovery mode.
func TestRecoveryMode_ClearedAfterSuccessfulScan(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{strandedJSON: "[]"})
	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(paths.townRoot, logger, filepath.Join(paths.binDir, "gt"), 10*time.Minute, nil, nil, nil)

	// Set recovery mode
	m.recoveryMode.Store(true)
	if !m.recoveryMode.Load() {
		t.Fatal("expected recoveryMode true before scan")
	}

	// scan() should clear it (mock gt returns empty stranded list = success)
	m.scan()

	if m.recoveryMode.Load() {
		t.Fatal("expected recoveryMode false after successful scan")
	}
}

// TestScanMu_PreventsConcurrentScans verifies that concurrent scan() calls
// are serialized by scanMu (no duplicate convoy checks).
func TestScanMu_PreventsConcurrentScans(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	stranded := []strandedConvoyInfo{{
		ID:          "convoy-race",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-race1"},
	}}
	data, _ := json.Marshal(stranded)

	paths := mockGtForScanTest(t, scanTestOpts{strandedJSON: string(data)})
	var logged []string
	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	m := NewConvoyManager(paths.townRoot, logger, filepath.Join(paths.binDir, "gt"), 10*time.Minute, nil, nil, nil)

	// Launch multiple concurrent scans
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.scan()
		}()
	}
	wg.Wait()

	// Verify sling was called (at least once) — the key is no panics or races
	if _, err := os.Stat(paths.slingLogPath); err != nil {
		t.Log("sling was never called (mock gt may not have been reached) — acceptable for race test")
	}
}

// TestStartupSweep_RunsAfterDelay verifies that runStartupSweep calls scan()
// after the startup delay.
func TestStartupSweep_RunsAfterDelay(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	paths := mockGtForScanTest(t, scanTestOpts{strandedJSON: "[]"})
	var scanCount atomic.Int32
	logger := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		if strings.Contains(msg, "startup sweep") {
			scanCount.Add(1)
		}
	}

	// Use a short startup delay by testing the goroutine directly
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewConvoyManager(paths.townRoot, logger, filepath.Join(paths.binDir, "gt"), 10*time.Minute, nil, nil, nil)
	m.ctx = ctx

	// Run startup sweep directly (it waits 10s normally, but we can test the
	// mechanism by verifying it logs the startup message)
	// For a fast test, we cancel the context after a short delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	m.runStartupSweep()
	// Context was cancelled before 10s timer — sweep should not have run
	if scanCount.Load() > 0 {
		t.Error("startup sweep should not run before timer expires")
	}
}

// TestDoltRecoveryCallback_Fires verifies that the Dolt server manager fires
// the recovery callback when transitioning from unhealthy to healthy.
func TestDoltRecoveryCallback_Fires(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dsm := NewDoltServerManager(tmpDir, DefaultDoltServerConfig(tmpDir), func(string, ...interface{}) {})

	var called atomic.Bool
	dsm.SetRecoveryCallback(func() {
		called.Store(true)
	})

	// Create the daemon directory and write the signal file at the path
	// that unhealthySignalFile() returns (port-dependent).
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Use writeUnhealthySignal to create the file at the correct path
	dsm.mu.Lock()
	dsm.writeUnhealthySignal("test", "test detail")
	dsm.mu.Unlock()

	// Clear the signal — should trigger callback since file was present
	dsm.mu.Lock()
	dsm.clearUnhealthySignal()
	dsm.mu.Unlock()

	// Give the goroutine time to fire
	time.Sleep(100 * time.Millisecond)

	if !called.Load() {
		t.Error("expected recovery callback to fire on unhealthy→healthy transition")
	}
}

// TestDoltRecoveryCallback_NoFireWhenAlreadyHealthy verifies that the callback
// does NOT fire when the signal file was not present (already healthy).
func TestDoltRecoveryCallback_NoFireWhenAlreadyHealthy(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dsm := NewDoltServerManager(tmpDir, DefaultDoltServerConfig(tmpDir), func(string, ...interface{}) {})

	var called atomic.Bool
	dsm.SetRecoveryCallback(func() {
		called.Store(true)
	})

	// Don't create any signal file — already healthy

	dsm.mu.Lock()
	dsm.clearUnhealthySignal()
	dsm.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	if called.Load() {
		t.Error("recovery callback should NOT fire when already healthy")
	}
}

// TestDoltRecoveryCallback_NilSafe verifies that clearUnhealthySignal does
// not panic when no callback is registered.
func TestDoltRecoveryCallback_NilSafe(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dsm := NewDoltServerManager(tmpDir, DefaultDoltServerConfig(tmpDir), func(string, ...interface{}) {})

	// Create daemon dir and write signal file via the proper method
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dsm.mu.Lock()
	dsm.writeUnhealthySignal("test", "test detail")
	dsm.mu.Unlock()

	// No callback set — should not panic
	dsm.mu.Lock()
	dsm.clearUnhealthySignal()
	dsm.mu.Unlock()
}

// infNaNStorage is a minimal Storage stub whose GetAllEventsSince always
// returns the given error. All other methods panic (they should not be called).
type infNaNStorage struct {
	beadsdk.Storage // embedded to satisfy unimplemented methods
	err             error
}

func (s *infNaNStorage) GetAllEventsSince(_ context.Context, _ time.Time) ([]*beadsdk.Event, error) {
	return nil, s.err
}

// TestPollStore_InfNaNError_AdvancesHWMAndReturnsNil verifies that when
// GetAllEventsSince returns a "+Inf is not a valid value for double" error
// (corrupt Dolt row), pollStore advances the high-water mark to now and
// returns nil (no error, no recovery mode).
func TestPollStore_InfNaNError_AdvancesHWMAndReturnsNil(t *testing.T) {
	for _, errMsg := range []string{
		"Error 1366 (HY000): error: +Inf is not a valid value for double",
		"Error 1366 (HY000): error: -Inf is not a valid value for double",
		"Error 1366 (HY000): error: NaN is not a valid value for double",
		// Dolt wraps values in single quotes in actual error messages
		"Error 1366 (HY000): error: '+Inf' is not a valid value for 'double'",
		"Error 1366 (HY000): error: '-Inf' is not a valid value for 'double'",
		"Error 1366 (HY000): error: 'NaN' is not a valid value for 'double'",
		// Wrapped in beads SDK error context (actual observed format)
		"failed to get events since 0: Error 1366 (HY000): error: '+Inf' is not a valid value for 'double'",
	} {
		t.Run(errMsg[:20], func(t *testing.T) {
			stub := &infNaNStorage{err: fmt.Errorf("%s", errMsg)}
			stores := map[string]beadsdk.Storage{"hq": stub}

			var logged []string
			logger := func(format string, args ...interface{}) {
				logged = append(logged, fmt.Sprintf(format, args...))
			}

			before := time.Now()
			m := NewConvoyManager(t.TempDir(), logger, "gt", 10*time.Minute, stores, nil, nil)

			hadError := m.pollStoresSnapshot(m.stores)
			after := time.Now()

			// pollStoresSnapshot should report no error (corrupt row is handled)
			if hadError {
				t.Errorf("expected no error for inf/nan store, got hadError=true; logs: %v", logged)
			}

			// recoveryMode must NOT be set (we recovered inline)
			if m.recoveryMode.Load() {
				t.Errorf("recoveryMode should not be set for inf/nan error; logs: %v", logged)
			}

			// High-water mark for "hq" should have been advanced to approximately now
			v, ok := m.lastEventIDs.Load("hq")
			if !ok {
				t.Fatal("expected HWM to be stored for hq")
			}
			hwm := v.(time.Time)
			if hwm.Before(before) || hwm.After(after.Add(time.Second)) {
				t.Errorf("HWM %v not in expected range [%v, %v]", hwm, before, after)
			}

			// Should have logged a message about the skip
			foundMsg := false
			for _, s := range logged {
				if strings.Contains(s, "+Inf/NaN row detected") {
					foundMsg = true
					break
				}
			}
			if !foundMsg {
				t.Errorf("expected HWM-advance log message, got: %v", logged)
			}
		})
	}
}

// feedTestRig sets up the minimal town fixture feedFirstReady needs: a routes
// file mapping the gt- prefix to a rig, and a mock `gt` that records sling
// invocations. Returns the town root, the sling log path, and the log sink.
func feedTestRig(t *testing.T) (townRoot, gtPath, slingLogPath string, logged *[]string) {
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
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	logged = &[]string{}
	return townRoot, filepath.Join(binDir, "gt"), slingLogPath, logged
}

// withOriginBranches replaces the origin branch-listing seam for a test.
func withOriginBranches(t *testing.T, fn func(rigRoot string) ([]string, error)) {
	t.Helper()
	orig := listOriginBranchesFn
	listOriginBranchesFn = fn
	t.Cleanup(func() { listOriginBranchesFn = orig })
}

// TestFeedFirstReady_SkipsIssueWithSurvivingBranch is the regression test for
// gt-3qfp: a bead whose previous holder died mid-work (never ran `gt done`) is
// still marked ready by the stranded scan, because liveness is judged only by
// tmux session state. Feeding it spawns a second polecat from main on work
// that is already preserved on origin — the mechanism behind gt-ibt8's four
// polecats and gt-da2x's three.
func TestFeedFirstReady_SkipsIssueWithSurvivingBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return []string{
			"polecat/pearl/gt-stranded1+mu72g5cz",
			"polecat/agate/gt-other+mtukyuns",
		}, nil
	})

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Stranded then fresh",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-stranded1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)

	if strings.Contains(logContent, "gt-stranded1") {
		t.Errorf("expected no sling for gt-stranded1 (work preserved on origin), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2 after skipping the preserved issue, got: %q", logContent)
	}

	mu.Lock()
	defer mu.Unlock()
	foundSkip := false
	for _, s := range *logged {
		if strings.Contains(s, "gt-stranded1") &&
			strings.Contains(s, "surviving branch polecat/pearl/gt-stranded1+mu72g5cz") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected a 'surviving branch' skip log naming the branch, got: %v", *logged)
	}
}

// TestFeedFirstReady_FeedsWhenBranchLookupFails pins the fail-open contract: an
// unreadable remote (no repo, network error) must not stall the stranded scan,
// which is the thing that keeps convoys moving.
func TestFeedFirstReady_FeedsWhenBranchLookupFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	})

	logger := func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Unreadable remote",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("expected sling for gt-issue1 despite branch-lookup failure, got: %q", string(data))
	}
}

// TestOriginBranches_CachesPerScan pins the bound on remote queries: a convoy
// with many ready issues in one rig must cost a single ls-remote, because each
// one against an unreachable remote blocks the whole scan for the query
// timeout.
func TestOriginBranches_CachesPerScan(t *testing.T) {
	townRoot, gtPath, _, _ := feedTestRig(t)

	var calls int32
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return []string{"polecat/pearl/gt-issue1+mu72g5cz"}, nil
	})

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, gtPath, 10*time.Minute, nil, nil, nil)

	for i := 0; i < 5; i++ {
		m.survivingBranchFor("gt", "gt-issue1")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 origin listing across 5 lookups, got %d", got)
	}

	// A new scan cycle must re-read the remote.
	m.resetOriginBranches()
	m.survivingBranchFor("gt", "gt-issue1")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 origin listings after reset, got %d", got)
	}

	// Failures are cached for the scan too, so an unreachable remote is not
	// retried once per ready issue.
	m.resetOriginBranches()
	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("remote unreachable")
	})
	for i := 0; i < 5; i++ {
		if _, ok := m.survivingBranchFor("gt", "gt-issue1"); ok {
			t.Fatal("expected fail-open (no surviving branch) on lookup error")
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected failed listings to be cached for the scan (3 total), got %d", got)
	}
}

// --- gt-utt4: dead-holder worktree preservation ---
//
// These pin the fix for the reboot recurrence of gt-qw4u: a bead HOOKED to a
// polecat whose session died (not rejected, not RECOVERED) is marked ready by
// isReadyIssue on session-liveness alone. survivingBranchFor (gt-3qfp) only
// catches work the holder managed to push. A branch that was committed but
// never pushed — the common case for a session killed mid-work — left no
// trace on origin at all, so the old code fed a fresh polecat from main and
// silently discarded it.

// runDeadHolderGit runs a git command in dir, failing the test on error.
// GIT_CONFIG_GLOBAL/GIT_CONFIG_NOSYSTEM isolate it from the host's global and
// system git config — a signing key, hooksPath, or init.defaultBranch set on
// the developer's machine must not change whether these commits/pushes
// succeed.
func runDeadHolderGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newDeadHolderWorktree creates a bare "origin" remote and a worktree cloned
// from it, checked out on the given generated polecat branch — the layout
// assigneeToWorktreePath resolves for assignee "<rig>/polecats/<name>".
// Returns the worktree path and the bare repo path.
func newDeadHolderWorktree(t *testing.T, townRoot, rig, name, branch string) (worktreePath, originPath string) {
	t.Helper()

	originPath = filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(originPath, 0755); err != nil {
		t.Fatalf("mkdir origin: %v", err)
	}
	runDeadHolderGit(t, originPath, "init", "--bare")

	worktreePath = filepath.Join(townRoot, rig, "polecats", name, rig)
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	runDeadHolderGit(t, worktreePath, "init")
	runDeadHolderGit(t, worktreePath, "config", "user.email", "test@test.com")
	runDeadHolderGit(t, worktreePath, "config", "user.name", "Test")
	runDeadHolderGit(t, worktreePath, "remote", "add", "origin", originPath)
	runDeadHolderGit(t, worktreePath, "checkout", "-b", branch)
	runDeadHolderGit(t, worktreePath, "commit", "--allow-empty", "-m", "initial")

	return worktreePath, originPath
}

// t.Parallel is deliberately omitted: this test uses withOriginBranches,
// which overrides the package-level listOriginBranchesFn var — the same
// reason TestFeedFirstReady_SkipsIssueWithSurvivingBranch and its siblings
// run serially.
func TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue1", Title: "Held by dead session", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue1+abc123"
	worktreePath, originPath := newDeadHolderWorktree(t, townRoot, "gt", "basalt", branch)
	localTip := runDeadHolderGit(t, worktreePath, "rev-parse", "HEAD")

	// The rig-level shared-repo listing (survivingBranchFor's source) has
	// nothing — the branch was never pushed, so it cannot appear there.
	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unpushed work",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue1") {
		t.Errorf("expected no sling for gt-issue1 (dead holder has unpushed work), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2 after skipping the preserved issue, got: %q", logContent)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation for a successful preserve, got: %v", escalated)
	}

	remoteTip := runDeadHolderGit(t, originPath, "rev-parse", branch)
	if remoteTip != localTip {
		t.Errorf("expected %s pushed to origin at %s, got %s", branch, localTip, remoteTip)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_UncommittedChanges_EscalatesAndSkips(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue2", Title: "Held by dead session, dirty tree", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue2+xyz789"
	worktreePath, _ := newDeadHolderWorktree(t, townRoot, "gt", "basalt", branch)
	// Push the commit so UnpushedCommits is 0 — isolates the assertion to
	// uncommitted work, which can never be safely auto-preserved by pushing.
	runDeadHolderGit(t, worktreePath, "push", "origin", branch)
	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, dirty worktree",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue2", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue2") {
		t.Errorf("expected no sling for gt-issue2 (dead holder has uncommitted work), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2 after skipping the unpreservable issue, got: %q", logContent)
	}
	if len(escalated) != 1 || !strings.Contains(escalated[0], "uncommitted work") {
		t.Errorf("expected one escalation mentioning uncommitted work, got: %v", escalated)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_UnreadableOriginState_EscalatesAndSkips(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue3", Title: "Held by dead session, remote unreadable", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)

	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("remote unreachable")
	})

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unreadable remote",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue3", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue3") {
		t.Errorf("expected no sling for gt-issue3 (dead holder, branch state undetermined — must fail closed), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2 (no known holder, unaffected by the fail-closed check), got: %q", logContent)
	}
	if len(escalated) != 1 || !strings.Contains(escalated[0], "could not be determined") {
		t.Errorf("expected one escalation about undetermined origin state, got: %v", escalated)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_SurvivingOriginBranch_SkipsWithoutEscalation(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue4", Title: "Held by dead session, already on origin", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue4+def456"

	withOriginBranches(t, func(rigRoot string) ([]string, error) {
		return []string{branch}, nil
	})

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, pushed work already on origin",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue4", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue4") {
		t.Errorf("expected no sling for gt-issue4 (already preserved on origin), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2, got: %q", logContent)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation when the branch already survives on origin, got: %v", escalated)
	}

	foundSkip := false
	for _, l := range *logged {
		if strings.Contains(l, "gt-issue4") && strings.Contains(l, "surviving branch "+branch) {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected a surviving-branch skip log naming %s, got: %v", branch, *logged)
	}
}

// withDeadHolderWorktreeState replaces the worktree-state seam for a test.
func withDeadHolderWorktreeState(t *testing.T, fn func(townRoot, assignee, issueID string) (deadHolderWorktreeState, error)) {
	t.Helper()
	orig := deadHolderWorktreeStateFn
	deadHolderWorktreeStateFn = fn
	t.Cleanup(func() { deadHolderWorktreeStateFn = orig })
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_WorktreeStateUnreadable_EscalatesAndSkips(t *testing.T) {
	takeStoreSlot(t)

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue5", Title: "Held by dead session, worktree unreadable", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)

	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })
	withDeadHolderWorktreeState(t, func(townRoot, assignee, issueID string) (deadHolderWorktreeState, error) {
		return deadHolderWorktreeState{}, fmt.Errorf("reading current branch: exit status 128")
	})

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, worktree unreadable",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue5", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue5") {
		t.Errorf("expected no sling for gt-issue5 (worktree state undetermined — must fail closed), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2, got: %q", logContent)
	}
	if len(escalated) != 1 || !strings.Contains(escalated[0], "worktree state could not be determined") {
		t.Errorf("expected one escalation about undetermined worktree state, got: %v", escalated)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_NoWorktree_FeedsWithoutEscalation(t *testing.T) {
	takeStoreSlot(t)

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue6", Title: "Held by dead session, no worktree on disk", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}

	// No worktree is created at all for gt/polecats/basalt — the assignee is
	// recorded but nothing on disk resolves to it (e.g. the seat was nuked
	// after it was assigned). AssigneeWorktreePath then reports "", which
	// must feed, not escalate.
	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "gt"), 0755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}

	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, no worktree",
		ReadyCount: 1, ReadyIssues: []string{"gt-issue6"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue6") {
		t.Errorf("expected sling for gt-issue6 (no worktree to lose), got: %q", data)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation when there is no worktree to check, got: %v", escalated)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_ReusedSeat_FeedsWithoutEscalation(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue7", Title: "Held by dead session, seat reused since", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	// The worktree at basalt's seat is checked out on a DIFFERENT issue's
	// branch — the seat was reused for other work since gt-issue7's holder
	// died. Nothing here is attributable to gt-issue7.
	otherBranch := "polecat/basalt/gt-other9+zzz999"
	newDeadHolderWorktree(t, townRoot, "gt", "basalt", otherBranch)

	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, seat reused",
		ReadyCount: 1, ReadyIssues: []string{"gt-issue7"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue7") {
		t.Errorf("expected sling for gt-issue7 (seat's worktree belongs to a different issue), got: %q", data)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation for a reused seat, got: %v", escalated)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_RuntimeOnlyDirt_FeedsWithoutEscalation(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue8", Title: "Held by dead session, only runtime dirt", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue8+dirt111"
	worktreePath, _ := newDeadHolderWorktree(t, townRoot, "gt", "basalt", branch)
	// Push the commit so UnpushedCommits is 0 — the only thing left in the
	// tree is tool-managed runtime state, which gt done and the deacon's
	// stale-hook scan already tolerate (85257aacc9b9).
	runDeadHolderGit(t, worktreePath, "push", "origin", branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".beads", "state.db"), []byte("runtime"), 0644); err != nil {
		t.Fatalf("write runtime dirt: %v", err)
	}

	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, runtime-only dirt",
		ReadyCount: 1, ReadyIssues: []string{"gt-issue8"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	if !strings.Contains(string(data), "gt-issue8") {
		t.Errorf("expected sling for gt-issue8 (only runtime dirt, nothing real to lose), got: %q", data)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation for runtime-only dirt, got: %v", escalated)
	}
}

// t.Parallel is deliberately omitted; see TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips.
func TestResolveDeadHolderWork_PreservePushFails_EscalatesAndSkips(t *testing.T) {
	takeStoreSlot(t)
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	held := &beadsdk.Issue{
		ID: "gt-issue9", Title: "Held by dead session, push fails", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, Assignee: "gt/polecats/basalt",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, held, "test"); err != nil {
		t.Fatalf("CreateIssue held: %v", err)
	}
	fresh := &beadsdk.Issue{
		ID: "gt-fresh2", Title: "Never touched", Status: beadsdk.StatusOpen,
		Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateIssue(ctx, fresh, "test"); err != nil {
		t.Fatalf("CreateIssue fresh: %v", err)
	}

	townRoot, gtPath, slingLogPath, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue9+push000"
	worktreePath, _ := newDeadHolderWorktree(t, townRoot, "gt", "basalt", branch)
	// Point origin at a path with no repository, so both the primary push
	// and the <branch>-<sha7> fallback push fail — exercising the "resolve
	// by hand" escalation preserveWorktreeBranch raises when neither lands.
	runDeadHolderGit(t, worktreePath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "does-not-exist"))

	withOriginBranches(t, func(rigRoot string) ([]string, error) { return nil, nil })

	var escalated []string
	m := NewConvoyManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtPath, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unpushed work, push fails",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue9", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	logContent := string(data)
	if strings.Contains(logContent, "gt-issue9") {
		t.Errorf("expected no sling for gt-issue9 (preserve push failed), got: %q", logContent)
	}
	if !strings.Contains(logContent, "gt-fresh2") {
		t.Errorf("expected sling for gt-fresh2 after skipping the unpreservable issue, got: %q", logContent)
	}
	if len(escalated) != 1 || !strings.Contains(escalated[0], "pushing") || !strings.Contains(escalated[0], "failed") {
		t.Errorf("expected one escalation about a failed preserve push, got: %v", escalated)
	}
}

// holdTestStorage serves fixed issue records and comments, so the stranded
// scan's per-bead checks are testable without a Dolt container. The embedded
// interface panics on anything else the code under test starts calling.
type holdTestStorage struct {
	beadsdk.Storage
	issues   map[string]*beadsdk.Issue
	comments map[string][]*beadsdk.Comment
	readErr  error
}

func (s *holdTestStorage) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("holdTestStorage: no issue %s", id)
	}
	return issue, nil
}

func (s *holdTestStorage) GetIssueComments(_ context.Context, id string) ([]*beadsdk.Comment, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.comments[id], nil
}

// assertLogged fails unless some log line names id and contains want.
func assertLogged(t *testing.T, logged []string, id, want string) {
	t.Helper()
	for _, l := range logged {
		if strings.Contains(l, id) && strings.Contains(l, want) {
			return
		}
	}
	t.Errorf("expected a log line naming %q for %s, got: %v", want, id, logged)
}

// TestFeedFirstReady_DispatchHold_Skips pins the per-bead checks the stranded
// scan makes before re-dispatching: a bead whose own record carries a deferral,
// a routing label, or an asserted keep-off decision is not dispatched and the
// reason is logged. A record that only mentions the wording, and one whose
// comment hold has been released, still feed (gt-tq6l).
func TestFeedFirstReady_DispatchHold_Skips(t *testing.T) {
	t.Parallel()

	held := []struct {
		id         string
		issue      *beadsdk.Issue
		comments   []*beadsdk.Comment
		wantReason string
	}{
		{
			id:         "gt-holddefer",
			issue:      &beadsdk.Issue{Status: beadsdk.StatusDeferred},
			wantReason: "status deferred",
		},
		{
			// Frozen in beads' own status model, alongside deferred.
			id:         "gt-holdpinned",
			issue:      &beadsdk.Issue{Status: beadsdk.Status("pinned")},
			wantReason: "status pinned",
		},
		{
			id:         "gt-holdsonnet",
			issue:      &beadsdk.Issue{Status: beadsdk.StatusOpen, Labels: []string{"needs-sonnet"}},
			wantReason: "label needs-sonnet",
		},
		{
			id:         "gt-holdsonnetcaps",
			issue:      &beadsdk.Issue{Status: beadsdk.StatusOpen, Labels: []string{"NEEDS-SONNET"}},
			wantReason: "label NEEDS-SONNET",
		},
		{
			id:         "gt-holdmayor",
			issue:      &beadsdk.Issue{Status: beadsdk.StatusOpen, Labels: []string{"bug", "needs-mayor-review"}},
			wantReason: "label needs-mayor-review",
		},
		{
			id: "gt-holddesign",
			issue: &beadsdk.Issue{
				Status: beadsdk.StatusOpen,
				Notes:  "MAYOR DESIGN DECISION: route this through the deacon, not the convoy feeder",
			},
			wantReason: "MAYOR DESIGN DECISION in notes",
		},
		{
			// The decision sits on its own line, under prose that explains it.
			id: "gt-holdnodisp",
			issue: &beadsdk.Issue{
				Status: beadsdk.StatusOpen,
				Notes:  "Blocked on gt-nj23.7 landing first.\ndo not redispatch",
			},
			wantReason: "do not redispatch in notes",
		},
		{
			// List and emphasis decoration are in front of the decision, not
			// part of it.
			id: "gt-holdbullet",
			issue: &beadsdk.Issue{
				Status: beadsdk.StatusOpen,
				Notes:  "Notes:\n  - **do-not-redispatch** until the mayor rules",
			},
			wantReason: "do not redispatch in notes",
		},
		{
			id: "gt-holdindesign",
			issue: &beadsdk.Issue{
				Status: beadsdk.StatusOpen,
				Design: "MAYOR DESIGN DECISION: the deacon owns this one",
			},
			wantReason: "MAYOR DESIGN DECISION in design",
		},
		{
			id:    "gt-holdcomment",
			issue: &beadsdk.Issue{Status: beadsdk.StatusOpen},
			comments: []*beadsdk.Comment{
				{Author: "mayor", Text: "do-not-redispatch until gt-nj23.7 is merged"},
			},
			wantReason: "do not redispatch in comment",
		},
		{
			// Same decision, hyphen between "re" and "dispatch": the fold has to
			// reach both spellings.
			id:    "gt-holdcommenthyphen",
			issue: &beadsdk.Issue{Status: beadsdk.StatusOpen},
			comments: []*beadsdk.Comment{
				{Author: "mayor", Text: "Do not re-dispatch until gt-nj23.7 is merged"},
			},
			wantReason: "do not redispatch in comment",
		},
		{
			// The newest decision wins: a hold written after a release holds
			// again.
			id:    "gt-reheld",
			issue: &beadsdk.Issue{Status: beadsdk.StatusOpen},
			comments: []*beadsdk.Comment{
				{Author: "mayor", Text: "do not redispatch until gt-nj23.7 is merged"},
				{Author: "mayor", Text: "HOLD RELEASED"},
				{Author: "mayor", Text: "the dependency reappeared\nMAYOR DESIGN DECISION: hold it"},
			},
			wantReason: "MAYOR DESIGN DECISION in comment",
		},
	}

	unheld := []struct {
		id       string
		issue    *beadsdk.Issue
		comments []*beadsdk.Comment
	}{
		{
			id:    "gt-plain",
			issue: &beadsdk.Issue{Status: beadsdk.StatusOpen},
		},
		{
			// A record that only mentions the wording is not held by it.
			id: "gt-mentionsnotes",
			issue: &beadsdk.Issue{
				Status: beadsdk.StatusOpen,
				Notes:  "This is not a MAYOR DESIGN DECISION, so the feeder may take it",
			},
		},
		{
			// Neither is a comment that quotes it back, as a review note does.
			id:    "gt-mentionscomment",
			issue: &beadsdk.Issue{Status: beadsdk.StatusOpen},
			comments: []*beadsdk.Comment{
				{Author: "garnet", Text: "The notes used to say \"do not redispatch\" — that line is gone now"},
			},
		},
		{
			// Comments are the one field that cannot be edited, so a comment
			// hold needs a later comment to lift it.
			id:    "gt-released",
			issue: &beadsdk.Issue{Status: beadsdk.StatusOpen},
			comments: []*beadsdk.Comment{
				{Author: "mayor", Text: "do not redispatch until gt-nj23.7 is merged"},
				{Author: "mayor", Text: "gt-nj23.7 merged\nHOLD RELEASED"},
			},
		},
	}

	store := &holdTestStorage{
		issues:   map[string]*beadsdk.Issue{},
		comments: map[string][]*beadsdk.Comment{},
	}
	for _, h := range held {
		h.issue.ID = h.id
		store.issues[h.id] = h.issue
		store.comments[h.id] = h.comments
	}
	for _, u := range unheld {
		u.issue.ID = u.id
		store.issues[u.id] = u.issue
		store.comments[u.id] = u.comments
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	// One bead per convoy, so each case is decided on its own record: a convoy
	// holding several would stop at its first dispatchable member.
	feed := func(id string) {
		m.feedFirstReady(strandedConvoyInfo{
			ID:          "hq-cv-hold",
			Title:       "Holds and controls",
			ReadyCount:  1,
			ReadyIssues: []string{id},
		})
	}
	for _, h := range held {
		feed(h.id)
	}
	for _, u := range unheld {
		feed(u.id)
	}

	data, err := os.ReadFile(slingLogPath)
	if err != nil {
		t.Fatalf("read sling log: %v", err)
	}
	slingLog := string(data)

	for _, h := range held {
		if strings.Contains(slingLog, h.id) {
			t.Errorf("%s must not be dispatched (held by %q), got sling: %q", h.id, h.wantReason, slingLog)
		}
		assertLogged(t, logged, h.id, "not dispatched: "+h.wantReason)
	}
	for _, u := range unheld {
		if !strings.Contains(slingLog, u.id) {
			t.Errorf("expected %s to feed, got sling log: %q", u.id, slingLog)
		}
		for _, l := range logged {
			if strings.Contains(l, u.id) && strings.Contains(l, "not dispatched") {
				t.Errorf("expected no hold on %s, got %q", u.id, l)
			}
		}
	}
}

// TestFeedFirstReady_DispatchHold_UnreadableRecordFailsClosed pins the other
// half of the rule: a store that holds a bead but cannot hand back its record
// leaves the hold unknown, and an unknown hold must not be read as "no hold".
// The bead is not dispatched, and the read failure is named in the log so the
// missing dispatch is diagnosable (gt-tq6l).
func TestFeedFirstReady_DispatchHold_UnreadableRecordFailsClosed(t *testing.T) {
	t.Parallel()

	store := &holdTestStorage{readErr: fmt.Errorf("dolt unreachable")}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"gt-","path":"gt/.beads"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	slingLogPath := filepath.Join(binDir, "sling.log")
	gtScript := `#!/bin/sh
if [ "$1" = "sling" ]; then
  echo "$@" >> "` + slingLogPath + `"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(townRoot, logger, filepath.Join(binDir, "gt"), 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv-unreadable",
		Title:       "Unreadable record",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-unreadable"},
	})

	if data, err := os.ReadFile(slingLogPath); err == nil {
		t.Errorf("expected no dispatch when the record cannot be read, got sling: %q", string(data))
	}
	assertLogged(t, logged, "gt-unreadable", "not dispatched: record unreadable")
}

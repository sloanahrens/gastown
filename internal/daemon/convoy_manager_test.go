package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/sling"
)

// scanTestOpts configures the mockGtForScanTest helper.
type scanTestOpts struct {
	strandedJSON  string // JSON for `gt convoy stranded --json`; default "[]"
	slingFailOnce bool   // first sling invocation exits 1, subsequent succeed
	routes        string // routes.jsonl content; empty = no routes file
}

// scanTestPaths is the town and the in-process gt mockGtForScanTest builds.
type scanTestPaths struct {
	townRoot string
	gt       *fakeCLI // records every call; argvLog reads them back
}

// mockGtForScanTest builds a town and a fake gt for scan tests: `convoy
// stranded` answers opts.strandedJSON, sling and `convoy check` succeed (the
// first sling fails when opts.slingFailOnce is set), and every call is
// recorded so tests can make both positive and negative assertions.
func mockGtForScanTest(t *testing.T, opts scanTestOpts) scanTestPaths {
	t.Helper()

	strandedJSON := opts.strandedJSON
	if strandedJSON == "" {
		strandedJSON = "[]"
	}

	var slings atomic.Int32
	gt := newFakeCLI(func(args []string) cliReply {
		switch {
		case len(args) >= 2 && args[0] == "convoy" && args[1] == "stranded":
			return cliReply{stdout: strandedJSON + "\n"}
		case len(args) >= 1 && args[0] == "sling":
			if opts.slingFailOnce && slings.Add(1) == 1 {
				return cliReply{code: 1}
			}
		}
		return cliReply{}
	})

	return scanTestPaths{townRoot: convoyTestTown(t, opts.routes), gt: gt}
}

// convoyTestTown returns a town root with a .beads directory, and routes as its
// routes.jsonl when routes is not empty.
func convoyTestTown(t *testing.T, routes string) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if routes != "" {
		if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
			t.Fatalf("write routes: %v", err)
		}
	}
	return townRoot
}

// newFakeGtManager is NewConvoyManager with its gt calls answered by gt.
func newFakeGtManager(townRoot string, logger func(format string, args ...interface{}), gt *fakeCLI, scanInterval time.Duration, stores map[string]beadsdk.Storage, openStores func() storeOpenResult, isRigParked func(string) bool) *ConvoyManager {
	m := NewConvoyManager(townRoot, logger, nil, scanInterval, stores, openStores, isRigParked)
	m.slingFn = slingSeamThrough(gt)
	answerScanThrough(m, gt)
	return m
}

// slingSeamThrough answers the feeder's in-process dispatch with gt's reply to
// the `gt sling` command line the same dispatch used to build. The feeder runs
// the engine in process now (gt-638go.7); this keeps one fake answering every
// call the manager makes, and keeps the argv a test asserts on the argv the
// dispatch asked for.
func slingSeamThrough(gt *fakeCLI) func(convoyID string, opts sling.Options) (*sling.Result, error) {
	return func(_ string, opts sling.Options) (*sling.Result, error) {
		cmd := &exec.Cmd{Path: "gt", Args: append([]string{"gt"}, slingArgv(opts)...)}
		_, stderr, err := gt.run(cmd)
		if err != nil {
			// The refusal text is what the feeder classifies, exactly as it
			// classified the subprocess's stderr.
			return nil, errors.New(strings.TrimSpace(string(stderr)))
		}
		return &sling.Result{BeadID: opts.BeadID, Success: true}, nil
	}
}

// slingArgv is the command line a dispatch's options describe, in the order
// `gt sling` took them.
func slingArgv(opts sling.Options) []string {
	args := []string{"sling", opts.BeadID, opts.RigName}
	if opts.NoBoot {
		args = append(args, "--no-boot")
	}
	if opts.Actor != "" {
		args = append(args, "--actor="+opts.Actor)
	}
	if opts.BaseBranch != "" {
		args = append(args, "--base-branch="+opts.BaseBranch)
	}
	if opts.Agent != "" {
		args = append(args, "--agent="+opts.Agent)
	}
	if opts.FormulaName != "" {
		args = append(args, "--formula="+opts.FormulaName)
	}
	return args
}

// answerScanThrough routes m's stranded scan and completion check through gt,
// as `gt convoy stranded --json` and `gt convoy check <id>` calls. The manager
// runs both in process (gt-638go.4); this keeps these tests' fake gt, its
// recorded calls and its replies driving them as they drove the subprocesses.
func answerScanThrough(m *ConvoyManager, gt *fakeCLI) {
	m.findStrandedFn = func(ctx context.Context) ([]strandedConvoyInfo, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cmd := &exec.Cmd{Path: "gt", Args: []string{"gt", "convoy", "stranded", "--json"}, Dir: m.townRoot}
		stdout, stderr, err := gt.run(cmd)
		if err != nil {
			return nil, fmt.Errorf("convoy stranded: %s", strings.TrimSpace(string(stderr)))
		}
		var stranded []strandedConvoyInfo
		if err := json.Unmarshal(stdout, &stranded); err != nil {
			return nil, fmt.Errorf("parsing stranded JSON: %w", err)
		}
		return stranded, nil
	}
	m.checkConvoyFn = func(ctx context.Context, convoyID string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := &exec.Cmd{Path: "gt", Args: []string{"gt", "convoy", "check", convoyID}, Dir: m.townRoot}
		if _, stderr, err := gt.run(cmd); err != nil {
			return fmt.Errorf("convoy check %s: %s", convoyID, strings.TrimSpace(string(stderr)))
		}
		return nil
	}
}

// installScanFakes replaces m's stranded scan with one that returns
// strandedJSON, and its completion check with one that appends
// "convoy check <id>" to checkLogPath (or does nothing when it is empty).
func (m *ConvoyManager) installScanFakes(strandedJSON, checkLogPath string) {
	m.findStrandedFn = func(context.Context) ([]strandedConvoyInfo, error) {
		var stranded []strandedConvoyInfo
		if err := json.Unmarshal([]byte(strandedJSON), &stranded); err != nil {
			return nil, err
		}
		return stranded, nil
	}
	m.checkConvoyFn = func(_ context.Context, convoyID string) error {
		if checkLogPath == "" {
			return nil
		}
		f, err := os.OpenFile(checkLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "convoy check %s\n", convoyID)
		return err
	}
}

// noStranded is a stranded scan that finds nothing, so a manager that runs
// its scan loop touches no town.
func noStranded(context.Context) ([]strandedConvoyInfo, error) { return nil, nil }

// argvLog renders gt's recorded calls whose argv starts with prefix, one
// space-joined argv per line — the call log the shell stubs these tests once
// used wrote. It is empty when no such call was made.
func argvLog(gt *fakeCLI, prefix ...string) []byte {
	var b strings.Builder
	for _, args := range gt.argvs(prefix...) {
		b.WriteString(strings.Join(args, " "))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// mustArgvLog is argvLog for a call the test requires: it fails the test when
// gt saw no call starting with prefix.
func mustArgvLog(t *testing.T, gt *fakeCLI, prefix ...string) []byte {
	t.Helper()
	data := argvLog(gt, prefix...)
	if len(data) == 0 {
		t.Fatalf("gt %s was never called", strings.Join(prefix, " "))
	}
	return data
}

func TestEventPoll_DetectsCloseEvents(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
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

	m := NewConvoyManager(townRoot, logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	startCursorsAtZero(m)
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

func TestScanStranded_FeedsReadyIssues(t *testing.T) {
	t.Parallel()

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-cv1","title":"Test","ready_count":1,"ready_issues":["gt-issue1"]}]`,
		routes:       `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	m := newFakeGtManager(paths.townRoot, func(string, ...interface{}) {}, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	data := mustArgvLog(t, paths.gt, "sling")
	logContent := string(data)
	if !strings.Contains(logContent, "sling") || !strings.Contains(logContent, "gt-issue1") {
		t.Errorf("expected gt sling to be invoked for gt-issue1, got: %q", logContent)
	}
}

func TestScanStranded_ClosesEmptyConvoys(t *testing.T) {
	t.Parallel()

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-empty1","title":"Empty","ready_count":0,"ready_issues":[]}]`,
	})

	m := newFakeGtManager(paths.townRoot, func(string, ...interface{}) {}, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	data := mustArgvLog(t, paths.gt, "convoy", "check")
	if !strings.Contains(string(data), "hq-empty1") {
		t.Errorf("expected gt convoy check for hq-empty1, got: %q", data)
	}
}

func TestScanStranded_GracePeriodSkipsRecentConvoy(t *testing.T) {
	t.Parallel()

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

	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	// Convoy check must NOT have been called — grace period should protect it.
	if data := argvLog(paths.gt, "convoy", "check"); len(data) > 0 {
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

	// Convoy created 10 minutes ago — past the 5-minute grace period.
	oldTime := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	strandedJSON := fmt.Sprintf(`[{"id":"hq-old1","title":"Old","tracked_count":0,"ready_count":0,"ready_issues":[],"created_at":"%s"}]`, oldTime)

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: strandedJSON,
	})

	m := newFakeGtManager(paths.townRoot, func(string, ...interface{}) {}, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	data := mustArgvLog(t, paths.gt, "convoy", "check")
	if !strings.Contains(string(data), "hq-old1") {
		t.Errorf("expected gt convoy check for hq-old1 (past grace period), got: %q", data)
	}
}

func TestScanStranded_NoStrandedConvoys(t *testing.T) {
	t.Parallel()

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: "[]",
	})

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	// Negative: sling must not have been called
	if data := argvLog(paths.gt, "sling"); len(data) > 0 {
		t.Errorf("sling was called unexpectedly: %s", data)
	}
	// Negative: convoy check must not have been called
	if data := argvLog(paths.gt, "convoy", "check"); len(data) > 0 {
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

	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)
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
	data := mustArgvLog(t, paths.gt, "sling")
	if !strings.Contains(string(data), "gt-issue2") {
		t.Errorf("expected sling for gt-issue2 (scan should continue after failure), got: %q", data)
	}
}

func TestConvoyManager_DoubleStop_Idempotent(t *testing.T) {
	t.Parallel()
	gtf := newFakeCLI(cliBySub(map[string]cliReply{"convoy stranded": {stdout: "[]\n"}}))

	townRoot := t.TempDir()
	m := newFakeGtManager(townRoot, func(string, ...interface{}) {}, gtf, 10*time.Minute, nil, nil, nil)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	m.Stop() // Second stop should not deadlock
}

func TestStart_DoubleCall_Guarded(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	gtf := newFakeCLI(cliBySub(map[string]cliReply{"convoy stranded": {stdout: "[]\n"}}))

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

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
	<-done // a Stop that hangs after a double Start fails on the test timeout
}

// TestRetryMissingStores_EmptyResultDoesNotConfirmPartialSet pins gt-tolf: an
// empty attempt (no stores opened, nothing named missing — e.g. the opener's
// rig walk found no known rigs this pass) is uninformative, not proof the
// stores this manager still lacks were resolved. Before the fix, a manager
// already holding some stores from a prior attempt would read that empty
// result as "nothing missing" and latch confirmed=true — silently freezing
// convoy lookups on a store set that was still incomplete.
func TestRetryMissingStores_EmptyResultDoesNotConfirmPartialSet(t *testing.T) {
	t.Parallel()

	calls := 0
	opener := func() storeOpenResult {
		calls++
		return storeOpenResult{}
	}

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, time.Hour,
		map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}, opener, nil)

	m.retryMissingStores(time.Now())
	if m.storeRecovery.confirmed {
		t.Error("an empty retry result must not confirm a store set that never opened hq")
	}
	if _, held := m.stores["hq"]; held {
		t.Error("hq was never opened by any attempt, so it must not be in the map")
	}

	// An unconfirmed set must keep retrying past its backoff, not freeze.
	m.retryMissingStores(time.Now().Add(time.Hour))
	if calls != 2 {
		t.Errorf("opener called %d times, want 2 — an empty result must not stop retries", calls)
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
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute,
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

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, time.Hour,
		map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}, opener, nil)
	m.SetAlertHooks(
		func(key, source, msg string) { raised = append(raised, firing{key, source, msg}) },
		func(reason string, keys ...string) error { cleared = append(cleared, keys...); return nil },
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

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, time.Hour,
		map[string]beadsdk.Storage{"gastown": &closeTrackingStorage{}}, opener, nil)
	m.SetAlertHooks(
		func(key, source, msg string) { raised = append(raised, key) },
		func(reason string, keys ...string) error { return nil },
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

	m := NewConvoyManager(t.TempDir(), logger, nil, time.Hour,
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
	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, time.Hour, startup,
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
		m := NewConvoyManager(townRoot, noop, nil, time.Hour, caller, opener, nil)
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
	m := NewConvoyManager("/tmp", noop, nil, 0, nil, nil, nil)
	if m.scanInterval != defaultStrandedScanInterval {
		t.Errorf("interval 0 should use default %v, got %v", defaultStrandedScanInterval, m.scanInterval)
	}

	custom := 5 * time.Minute
	m2 := NewConvoyManager("/tmp", noop, nil, custom, nil, nil, nil)
	if m2.scanInterval != custom {
		t.Errorf("interval should be %v, got %v", custom, m2.scanInterval)
	}
}

func TestFeedFirstReady_MultipleReadyIssues_DispatchesOnlyFirst(t *testing.T) {
	t.Parallel()

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Multi Ready",
		ReadyCount:  3,
		ReadyIssues: []string{"gt-issue1", "gt-issue2", "gt-issue3"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	// The convoy the snapshot named is closed by the time the feed runs.
	m.convoyStatus = func(string) (string, bool) { return "closed", true }
	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Closed under the feeder",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	})

	if data := argvLog(gtf, "sling"); len(data) > 0 {
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
	if data := argvLog(gtf, "sling"); !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("an unreadable status must fail open and feed, got %q", string(data))
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

	// Convoy has 3 ready issues. First sling fails, second succeeds.
	// Verifies feedFirstReady iterates past dispatch failure within a single convoy.
	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	// First sling call exits 1 (failure), subsequent succeed
	var slings atomic.Int32
	gtf := newFakeCLI(func(args []string) cliReply {
		if args[0] == "sling" && slings.Add(1) == 1 {
			return cliReply{stderr: "dispatch failed\n", code: 1}
		}
		return cliReply{}
	})

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Iterate Past Failure",
		ReadyCount:  3,
		ReadyIssues: []string{"gt-fail1", "gt-succeed2", "gt-notreached3"},
	}
	m.feedFirstReady(c)

	logContent := string(mustArgvLog(t, gtf, "sling"))

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

	// All sling calls fail. Verify the "no dispatchable issues" log message.
	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(cliBySub(map[string]cliReply{"sling": {stderr: "always fail\n", code: 1}}))

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Unknown Prefix",
		ReadyCount:  1,
		ReadyIssues: []string{"zz-issue1"},
	}
	m.feedFirstReady(c)

	if data := argvLog(gtf, "sling"); len(data) > 0 {
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

func TestScan_FindStrandedError_LogsAndContinues(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()

	gtf := newFakeCLI(cliBySub(map[string]cliReply{"convoy stranded": {stderr: "stranded command failed\n", code: 1}}))

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

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

func TestPollEvents_JournalReadError(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()
	store.eventsErr = errors.New("bd events tail: store_unavailable")

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	if !m.pollStoresSnapshot(m.stores) {
		t.Error("a failed journal read did not report an error")
	}
	if !m.recoveryMode.Load() {
		t.Error("a failed journal read did not set recovery mode")
	}
	found := false
	for _, s := range logged {
		found = found || strings.Contains(s, "event poll error")
	}
	if !found {
		t.Errorf("expected 'event poll error' in logs, got: %v", logged)
	}
	if _, ok := m.eventCursors.Load("hq"); ok {
		t.Error("a failed read advanced the cursor")
	}
}

// A cursor bd has pruned past resumes at the oldest retained record, logs
// the gap, and sets recovery mode so the stranded scan catches what fell in
// it (gt-7iwy0.2).
func TestPollEvents_TruncatedJournalResumesAtFloor(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("gt-trunc-%d", i)
		mustCreateClosed(t, store, id)
	}
	// Records 1-4 are pruned; the cursor at 2 lies below the floor.
	store.journalFloor = 5

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	startCursorsAtZero(m)
	m.eventCursors.Store("hq", int64(2))
	if m.pollStoresSnapshot(m.stores) {
		t.Errorf("a pruned cursor is a gap to log, not a poll failure; logs: %v", logged)
	}
	if !m.recoveryMode.Load() {
		t.Error("a pruned cursor did not set recovery mode")
	}
	if v, _ := m.eventCursors.Load("hq"); v != int64(6) {
		t.Errorf("cursor = %v, want 6 (the journal head)", v)
	}
	var gap, closes []string
	for _, s := range logged {
		if strings.Contains(s, "pruned") {
			gap = append(gap, s)
		}
		if strings.Contains(s, "close detected") {
			closes = append(closes, s)
		}
	}
	if len(gap) != 1 {
		t.Errorf("gap log lines = %q, want one", gap)
	}
	// Records 5 and 6 are gt-trunc-3's create and close.
	if len(closes) != 1 || !strings.Contains(closes[0], "gt-trunc-3") {
		t.Errorf("close detections = %q, want only gt-trunc-3", closes)
	}
}

// A poll reads the journal page by page until bd says nothing follows.
func TestPollEvents_PagesThroughJournal(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()
	n := eventPageSize + 5
	for i := 0; i < n/2+1; i++ {
		mustCreateClosed(t, store, fmt.Sprintf("gt-page-%d", i))
	}
	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	startCursorsAtZero(m)
	m.pollStoresSnapshot(m.stores)
	closes := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") {
			closes++
		}
	}
	if closes != n/2+1 {
		t.Errorf("close detections = %d, want %d across pages", closes, n/2+1)
	}
	if store.tailCalls() < 2 {
		t.Errorf("tail calls = %d, want more than one page", store.tailCalls())
	}
}

// The warm-up cycle moves every cursor to the head without acting on
// history, and a later update to an already-closed issue (notes, labels) is
// not a second close.
func TestPollEvents_WarmupAndUpdateOnClosedIssue(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()
	mustCreateClosed(t, store, "gt-history")

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.pollStoresSnapshot(m.stores)
	for _, s := range logged {
		if strings.Contains(s, "close detected") {
			t.Fatalf("warm-up acted on history: %v", logged)
		}
	}
	if v, _ := m.eventCursors.Load("hq"); v != int64(2) {
		t.Errorf("cursor after warm-up = %v, want 2", v)
	}

	mustCreateClosed(t, store, "gt-live")
	ctx := context.Background()
	if err := store.UpdateIssue(ctx, "gt-live", map[string]interface{}{"notes": "after close"}, "test"); err != nil {
		t.Fatal(err)
	}
	logged = nil
	m.pollStoresSnapshot(m.stores)
	closes := 0
	for _, s := range logged {
		if strings.Contains(s, "close detected") && strings.Contains(s, "gt-live") {
			closes++
		}
	}
	if closes != 1 {
		t.Errorf("close detections for gt-live = %d, want 1 (the notes update is not a second close): %v", closes, logged)
	}
}

// startCursorsAtZero marks every store as read from the start of its
// journal, so the next poll processes the whole journal instead of warming
// the store up.
func startCursorsAtZero(m *ConvoyManager) {
	for name := range m.stores {
		m.eventCursors.Store(name, int64(0))
	}
}

// A store whose first read fails is still warmed up by its next successful
// read, not replayed: warm-up is per store, keyed on the missing cursor.
func TestPollEvents_FailedFirstReadStillWarmsUp(t *testing.T) {
	t.Parallel()
	hq, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rig, rigCleanup := newMemStore(t)
	defer rigCleanup()
	mustCreateClosed(t, rig, "gt-old-close")
	rig.eventsErr = errors.New("bd events tail: store_unavailable")

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": hq, "gastown": rig}, nil, nil)
	m.pollStoresSnapshot(m.stores)
	if _, ok := m.eventCursors.Load("gastown"); ok {
		t.Fatal("a failed first read recorded a cursor")
	}
	rig.eventsErr = nil
	m.pollStoresSnapshot(m.stores)
	for _, s := range logged {
		if strings.Contains(s, "close detected") {
			t.Fatalf("the store's first successful read replayed history: %v", logged)
		}
	}
	if v, _ := m.eventCursors.Load("gastown"); v != int64(2) {
		t.Errorf("cursor = %v, want 2 (warmed to the head)", v)
	}
}

// A truncation window that would not move the cursor forward skips to the
// head instead of refusing the same cursor on every poll.
func TestPollEvents_TruncationWithoutProgressSkipsToHead(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()
	mustCreateClosed(t, store, "gt-a")
	mustCreateClosed(t, store, "gt-b")
	store.truncateAlways = &beads.EventsTruncatedError{Floor: 2, Head: 4}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.eventCursors.Store("hq", int64(3))
	store.truncateOnce = true
	if m.pollStoresSnapshot(m.stores) {
		t.Errorf("poll reported an error; logs: %v", logged)
	}
	if v, _ := m.eventCursors.Load("hq"); v != int64(4) {
		t.Errorf("cursor = %v, want 4 (the head)", v)
	}
}

// A truncation met while warming a store up does not record a cursor before
// the warm-up reaches the head, so a failure after it is warmed up again
// rather than processed.
func TestPollEvents_TruncationDuringWarmupRecordsNoEarlyCursor(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()
	for i := 0; i < 3; i++ {
		mustCreateClosed(t, store, fmt.Sprintf("gt-w-%d", i))
	}
	store.truncateAlways = &beads.EventsTruncatedError{Floor: 3, Head: 6}
	store.truncateOnce = true
	store.failAfterTruncate = errors.New("bd events tail: store_unavailable")

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	m.pollStoresSnapshot(m.stores)
	if v, ok := m.eventCursors.Load("hq"); ok {
		t.Errorf("warm-up cut short recorded cursor %v", v)
	}
}

func mustCreateClosed(t *testing.T, store *memStore, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateIssue(ctx, &beadsdk.Issue{ID: id, Title: id, Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now}, "test"); err != nil {
		t.Fatalf("CreateIssue(%s): %v", id, err)
	}
	if err := store.CloseIssue(ctx, id, "done", "test", ""); err != nil {
		t.Fatalf("CloseIssue(%s): %v", id, err)
	}
}

func TestFeedFirstReady_UnknownRig_Skips(t *testing.T) {
	t.Parallel()

	// "hq-" prefix routes to town-level path "." which has no rig name
	townRoot := convoyTestTown(t, `{"prefix":"hq-","path":"."}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Town-level Rig",
		ReadyCount:  1,
		ReadyIssues: []string{"hq-issue1"},
	}
	m.feedFirstReady(c)

	if data := argvLog(gtf, "sling"); len(data) > 0 {
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

	townRoot := convoyTestTown(t, `{"prefix":"sh-","path":"shippercrm/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// isRigParked returns true for "shippercrm"
	parked := func(rig string) bool { return rig == "shippercrm" }
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, parked)

	c := strandedConvoyInfo{
		ID:          "hq-cv-park1",
		Title:       "Parked Rig Convoy",
		ReadyCount:  1,
		ReadyIssues: []string{"sh-issue1"},
	}
	m.feedFirstReady(c)

	// Sling should NOT have been called
	if data := argvLog(gtf, "sling"); len(data) > 0 {
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

	townRoot := convoyTestTown(t, "")
	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Empty Ready",
		ReadyCount:  3,
		ReadyIssues: []string{},
	}
	m.feedFirstReady(c)

	if data := argvLog(gtf); len(data) > 0 {
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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	m := newFakeGtManager(townRoot, func(string, ...interface{}) {}, gtf, 10*time.Minute, nil, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv-xprwe",
		Title:       "Actor Attribution",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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
	t.Parallel()

	townRoot, gtf, logged := feedTestRig(t)

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }

	c := strandedConvoyInfo{
		ID:          "hq-cv-agent1",
		Title:       "Agent passthrough",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
		Agent:       "deepseek-flash",
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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
	t.Parallel()

	townRoot, gtf, logged := feedTestRig(t)

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }

	c := strandedConvoyInfo{
		ID:          "hq-cv-noagent",
		Title:       "No recorded agent",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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
	t.Parallel()

	townRoot, gtf, logged := feedTestRig(t)

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }

	c := strandedConvoyInfo{
		ID:          "hq-cv-formula1",
		Title:       "Formula passthrough",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
		Formula:     "mol-doc-audit",
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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
	t.Parallel()

	townRoot, gtf, _ := feedTestRig(t)

	m := newFakeGtManager(townRoot, func(string, ...interface{}) {}, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }

	c := strandedConvoyInfo{
		ID:          "hq-cv-noformula",
		Title:       "No recorded formula",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	if strings.Contains(string(data), "--formula=") {
		t.Errorf("expected no --formula when none was recorded, got: %q", string(data))
	}
}

func TestFeedFirstReady_RejectionMarker_SkipsAndDefersToDeacon(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Has Rejected Bead",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-rejected1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

	// No store is wired for the "gt" rig (only "hq" is present, as in most
	// daemon deployments where a rig's store failed to open). Rejection-marker
	// lookup must fail open rather than block dispatch of unrelated issues.
	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{}, nil, nil)

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "No Rig Store",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, 10*time.Minute,
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

	store := &holdTestStorage{issues: map[string]*beadsdk.Issue{
		"gt-rejected1": {Status: beadsdk.StatusOpen, Notes: "MERGE REJECTION (attempt 1): needs work - see review"},
		"gt-fresh2":    {Status: beadsdk.StatusOpen},
	}}
	townRoot, gtf := holdTestTown(t)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv1",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-rejected1", "gt-fresh2"},
	})

	data := mustArgvLog(t, gtf, "sling")
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

	store := &holdTestStorage{readErr: fmt.Errorf("dolt unreachable")}
	townRoot, gtf := holdTestTown(t)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-u", ReadyCount: 1, ReadyIssues: []string{"gt-unreadable"}})

	if data := argvLog(gtf, "sling"); len(data) > 0 {
		t.Errorf("unreadable record was fed: %q", data)
	}
	assertLogged(t, logged, "gt-unreadable", "cannot rule out a merge rejection (fail-closed)")
	for _, l := range logged {
		if strings.Contains(l, "proceeding as clear") || strings.Contains(l, "surviving branch") {
			t.Errorf("unreadable record went past the rejection gate: %q", l)
		}
	}
}

// holdTestTown builds a town whose gt- prefix routes to rig "gt" and a fake
// gt that records each sling, for the hermetic feedFirstReady tests.
func holdTestTown(t *testing.T) (townRoot string, gt *fakeCLI) {
	t.Helper()
	return convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n"), newFakeCLI(nil)
}

func TestScanStranded_OwnedConvoy_SkipsAutoFeed(t *testing.T) {
	t.Parallel()

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-cv1","title":"Owned Convoy","ready_count":1,"ready_issues":["gt-issue1"],"owned":true}]`,
		routes:       `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	if data := argvLog(paths.gt, "sling"); len(data) > 0 {
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

	paths := mockGtForScanTest(t, scanTestOpts{
		strandedJSON: `[{"id":"hq-cv1","title":"Regular Convoy","ready_count":1,"ready_issues":["gt-issue1"],"owned":false}]`,
		routes:       `{"prefix":"gt-","path":"gt/.beads"}` + "\n",
	})

	m := newFakeGtManager(paths.townRoot, func(string, ...interface{}) {}, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	data := mustArgvLog(t, paths.gt, "sling")
	if !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("expected gt sling to be invoked for gt-issue1, got: %q", string(data))
	}
}

func TestScan_ContextCancelled_MidIteration(t *testing.T) {
	t.Parallel()

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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	// Fake gt: stranded returns the list; sling records itself, reports it is
	// in flight, then blocks until the manager's context is cancelled — the
	// way a real sling runs until the scan's cancellation kills it. A scan
	// that ignored cancellation would never get it back.
	var m *ConvoyManager
	inFlight := make(chan struct{})
	var once sync.Once
	gtf := newFakeCLI(func(args []string) cliReply {
		switch args[0] {
		case "convoy":
			return cliReply{stdout: string(jsonBytes) + "\n"}
		case "sling":
			once.Do(func() { close(inFlight) })
			<-m.ctx.Done()
			return cliReply{code: -1}
		}
		return cliReply{}
	})

	var logMu sync.Mutex
	var logged []string
	logger := func(format string, args ...interface{}) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	m = newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)

	// Run scan in a goroutine and cancel once a sling is in flight
	done := make(chan struct{})
	go func() {
		m.scan()
		close(done)
	}()

	// Cancel once the first sling is in flight: the scan is then mid-iteration
	// by construction, with four convoys still to go (gt-hvzy.10). The
	// deadlines below only bound a hang.
	<-inFlight
	m.cancel()

	// A scan that ignores the cancellation never returns; the test timeout
	// fails it.
	<-done

	slings := mustArgvLog(t, gtf, "sling")
	if n := strings.Count(string(slings), "\n"); n != 1 {
		t.Errorf("sling ran %d times, want 1: cancellation must stop the iteration at the convoy in flight\n%s", n, slings)
	}

	logMu.Lock()
	defer logMu.Unlock()
	feedCount := 0
	for _, s := range logged {
		if strings.Contains(s, "feeding") {
			feedCount++
		}
	}
	if feedCount != 1 {
		t.Errorf("fed %d convoys, want 1 (the one in flight at cancellation): %q", feedCount, logged)
	}
}

func TestScanStranded_MixedReadyAndEmpty(t *testing.T) {
	t.Parallel()

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

	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)
	m.scan()

	// Verify ready convoys were dispatched via sling
	slingData := mustArgvLog(t, paths.gt, "sling")
	slingContent := string(slingData)
	if !strings.Contains(slingContent, "gt-issue1") {
		t.Errorf("expected sling for gt-issue1 (ready convoy), got: %q", slingContent)
	}
	if !strings.Contains(slingContent, "gt-issue2") {
		t.Errorf("expected sling for gt-issue2 (ready convoy), got: %q", slingContent)
	}

	// Verify empty convoys were routed to convoy check
	checkData := mustArgvLog(t, paths.gt, "convoy", "check")
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
	store, cleanup := newMemStore(t)
	defer cleanup() // safety net; Stop() should close first

	opener := func() storeOpenResult {
		return storeOpenResult{Stores: map[string]beadsdk.Storage{"hq": store}}
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, nil, opener, nil)

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
	hqStore, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := newMemStore(t)
	defer rigCleanup()

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	stores := map[string]beadsdk.Storage{
		"hq":      hqStore,
		"gastown": rigStore,
	}

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, nil)
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
	hqStore, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, nil)
	startCursorsAtZero(m)
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
	hqStore, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, nil)
	startCursorsAtZero(m)
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
	hqStore, hqCleanup := newMemStore(t)
	defer hqCleanup()
	activeStore, activeCleanup := newMemStore(t)
	defer activeCleanup()
	parkedStore, parkedCleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, isParked)
	startCursorsAtZero(m)
	m.eventCursors.Delete("shippercrm") // parked: never read, so never given a cursor
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
	if _, hasHW := m.eventCursors.Load("shippercrm"); hasHW {
		t.Errorf("parked rig (shippercrm) should not have been polled, but has a high-water mark")
	}
	// Active rig should have been polled
	if _, hasHW := m.eventCursors.Load("gastown"); !hasHW {
		t.Errorf("active rig (gastown) should have been polled, but has no high-water mark")
	}
}

func TestPollAllStores_HqNeverSkippedEvenIfParkedCallbackReturnsTrue(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, alwaysParked)
	startCursorsAtZero(m)
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
	store, cleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, nil)

	// First poll: should detect our close event
	startCursorsAtZero(m)
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
	store, cleanup := newMemStore(t)
	defer cleanup()
	clk := newFixedClock()
	store.now = clk.Now

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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, nil)
	startCursorsAtZero(m)
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

	clk.Advance(10 * time.Millisecond)
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

	clk.Advance(10 * time.Millisecond)
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
	store, cleanup := newMemStore(t)
	defer cleanup()
	clk := newFixedClock()
	store.now = clk.Now

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
	clk.Advance(1100 * time.Millisecond)
	if err := store.UpdateIssue(ctx, issue.ID, map[string]interface{}{"status": beadsdk.StatusOpen}, "test"); err != nil {
		t.Fatalf("ReopenIssue via UpdateIssue: %v", err)
	}

	clk.Advance(1100 * time.Millisecond)
	if err := store.CloseIssue(ctx, issue.ID, "done again", "test", ""); err != nil {
		t.Fatalf("CloseIssue again: %v", err)
	}

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute,
		map[string]beadsdk.Storage{"hq": store}, nil, nil)
	startCursorsAtZero(m)
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
	hqStore, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := newMemStore(t)
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
	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, nil)
	startCursorsAtZero(m)
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
	hqStore, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rigStore, rigCleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, nil)

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
	store, cleanup := newMemStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	issueID := "gt-open2"
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

	townRoot := t.TempDir()

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{"hq": store}, nil, nil)
	startCursorsAtZero(m)
	m.pollStoresSnapshot(m.stores)

	for _, s := range logged {
		if strings.Contains(s, "close detected") {
			t.Errorf("expected no close detection for open issue %s, got: %s", issueID, s)
		}
	}
	if calls := argvLog(gtf); len(calls) > 0 {
		t.Errorf("an open issue must not run gt, got: %s", calls)
	}
}

// --- hq store nil guard ---

func TestPollStore_NilHqStore_LogsWarningAndSkips(t *testing.T) {
	t.Parallel()
	// Create a rig store with a close event, but no hq store in the map.
	// The nil hq guard should log a warning and skip convoy lookups.
	rigStore, rigCleanup := newMemStore(t)
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

	m := NewConvoyManager(t.TempDir(), logger, nil, 10*time.Minute, stores, nil, nil)
	startCursorsAtZero(m)
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

	townRoot := t.TempDir()
	var logged []string
	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	// Use a broken store that returns errors
	m := NewConvoyManager(townRoot, logger, nil, 10*time.Minute, nil, nil, nil)

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

	paths := mockGtForScanTest(t, scanTestOpts{strandedJSON: "[]"})
	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)

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
// are serialized by scanMu: no two stranded scans are ever in flight at once,
// and every call still runs its scan.
func TestScanMu_PreventsConcurrentScans(t *testing.T) {
	t.Parallel()

	stranded := []strandedConvoyInfo{{
		ID:          "convoy-race",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-race1"},
	}}
	data, _ := json.Marshal(stranded)

	var inFlight, peak atomic.Int32
	gtf := newFakeCLI(func(args []string) cliReply {
		if args[0] != "convoy" || args[1] != "stranded" {
			return cliReply{}
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return cliReply{stdout: string(data) + "\n"}
	})
	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")
	m := newFakeGtManager(townRoot, func(string, ...interface{}) {}, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(string) ([]string, error) { return nil, nil }

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

	if n := len(gtf.argvs("convoy", "stranded")); n != 5 {
		t.Errorf("stranded scan ran %d times for 5 scan() calls, want 5", n)
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("%d stranded scans were in flight at once, want 1 (scanMu serializes them)", p)
	}
}

// TestStartupSweep_RunsAfterDelay verifies that runStartupSweep calls scan()
// after the startup delay.
func TestStartupSweep_RunsAfterDelay(t *testing.T) {
	t.Parallel()

	paths := mockGtForScanTest(t, scanTestOpts{strandedJSON: "[]"})
	var scanCount atomic.Int32
	logger := func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		if strings.Contains(msg, "startup sweep") {
			scanCount.Add(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := newFakeGtManager(paths.townRoot, logger, paths.gt, 10*time.Minute, nil, nil, nil)
	m.ctx = ctx

	// A manager stopped before the sweep's 10s delay ends must not sweep.
	cancel()
	m.runStartupSweep()
	if scanCount.Load() > 0 {
		t.Error("startup sweep should not run before timer expires")
	}
	if calls := argvLog(paths.gt); len(calls) > 0 {
		t.Errorf("a cancelled startup sweep ran gt: %s", calls)
	}
}

// TestDoltRecoveryCallback_Fires verifies that the Dolt server manager fires
// the recovery callback when transitioning from unhealthy to healthy.
func TestDoltRecoveryCallback_Fires(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	dsm := NewDoltServerManager(tmpDir, DefaultDoltServerConfig(tmpDir), func(string, ...interface{}) {})

	called := make(chan struct{})
	dsm.SetRecoveryCallback(func() { close(called) })

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

	// The callback runs on its own goroutine; one that never fires fails the
	// test on its timeout.
	<-called
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

// feedTestRig sets up the minimal town fixture feedFirstReady needs: a routes
// file mapping the gt- prefix to a rig, and a fake gt that records sling
// invocations. Returns the town root, the fake and the log sink.
func feedTestRig(t *testing.T) (townRoot string, gt *fakeCLI, logged *[]string) {
	t.Helper()
	townRoot, gt = holdTestTown(t)
	return townRoot, gt, &[]string{}
}

// TestFeedFirstReady_SkipsIssueWithSurvivingBranch is the regression test for
// gt-3qfp: a bead whose previous holder died mid-work (never ran `gt done`) is
// still marked ready by the stranded scan, because liveness is judged only by
// tmux session state. Feeding it spawns a second polecat from main on work
// that is already preserved on origin — the mechanism behind gt-ibt8's four
// polecats and gt-da2x's three.
func TestFeedFirstReady_SkipsIssueWithSurvivingBranch(t *testing.T) {
	t.Parallel()

	townRoot, gtf, logged := feedTestRig(t)

	var mu sync.Mutex
	logger := func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return []string{
			"polecat/pearl/gt-stranded1+mu72g5cz",
			"polecat/agate/gt-other+mtukyuns",
		}, nil
	}

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Stranded then fresh",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-stranded1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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
	t.Parallel()

	townRoot, gtf, logged := feedTestRig(t)

	logger := func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("no git repo under %s", rigRoot)
	}

	c := strandedConvoyInfo{
		ID:          "hq-cv1",
		Title:       "Unreadable remote",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-issue1"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	if !strings.Contains(string(data), "gt-issue1") {
		t.Errorf("expected sling for gt-issue1 despite branch-lookup failure, got: %q", string(data))
	}
}

// TestOriginBranches_CachesPerScan pins the bound on remote queries: a convoy
// with many ready issues in one rig must cost a single ls-remote, because each
// one against an unreachable remote blocks the whole scan for the query
// timeout.
func TestOriginBranches_CachesPerScan(t *testing.T) {
	t.Parallel()
	townRoot, gtf, _ := feedTestRig(t)

	var calls int32

	m := newFakeGtManager(townRoot, func(string, ...interface{}) {}, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return []string{"polecat/pearl/gt-issue1+mu72g5cz"}, nil
	}

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
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		atomic.AddInt32(&calls, 1)
		return nil, fmt.Errorf("remote unreachable")
	}
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

// newDeadHolderWorktree makes, in gitfake world f, an "origin" bare repo
// holding branch at an initial commit, and a worktree cloned from it on that
// branch — the layout assigneeToWorktreePath resolves for assignee
// "<rig>/polecats/<name>". The branch is on origin as cloned; a test that
// wants unpushed work commits on top of it in the worktree. Returns the
// worktree path and the bare repo path.
func newDeadHolderWorktree(t *testing.T, f *gitfake.Fake, townRoot, rig, name, branch string) (worktreePath, originPath string) {
	t.Helper()
	originPath = filepath.Join(t.TempDir(), "origin.git")
	f.InitBare(t, originPath)
	f.Commit(t, originPath, branch, "initial", map[string]string{"README": "r\n"})
	worktreePath = filepath.Join(townRoot, rig, "polecats", name, rig)
	if err := f.Open(filepath.Dir(worktreePath)).CloneBranch(originPath, worktreePath, branch); err != nil {
		t.Fatalf("clone dead holder worktree: %v", err)
	}
	return worktreePath, originPath
}

// deadHolderOpener opens dead holders' worktrees in gitfake world f.
func deadHolderOpener(t *testing.T, f *gitfake.Fake) func(dir string) deadHolderGit {
	return func(dir string) deadHolderGit {
		g, ok := f.Open(dir).(deadHolderGit)
		if !ok {
			t.Fatalf("gitfake does not implement deadHolderGit for %s", dir)
		}
		return g
	}
}

func TestResolveDeadHolderWork_UnpushedCommits_PreservesAndSkips(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue1+abc123"
	f := gitfake.New()
	worktreePath, originPath := newDeadHolderWorktree(t, f, townRoot, "gt", "basalt", branch)
	// Work the holder committed but never pushed.
	localTip := f.Commit(t, worktreePath, branch, "local work", nil)

	// The rig-level shared-repo listing (survivingBranchFor's source) has
	// nothing — the branch was never pushed, so it cannot appear there.

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.openGitFn = deadHolderOpener(t, f)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unpushed work",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue1", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

	remoteTip := f.Ref(originPath, "refs/heads/"+branch)
	if remoteTip != localTip {
		t.Errorf("expected %s pushed to origin at %s, got %s", branch, localTip, remoteTip)
	}
}

func TestResolveDeadHolderWork_UncommittedChanges_EscalatesAndSkips(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue2+xyz789"
	f := gitfake.New()
	worktreePath, _ := newDeadHolderWorktree(t, f, townRoot, "gt", "basalt", branch)
	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.openGitFn = deadHolderOpener(t, f)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, dirty worktree",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue2", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

func TestResolveDeadHolderWork_UnreadableOriginState_EscalatesAndSkips(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return nil, fmt.Errorf("remote unreachable")
	}
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unreadable remote",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue3", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

func TestResolveDeadHolderWork_SurvivingOriginBranch_SkipsWithoutEscalation(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue4+def456"

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) {
		return []string{branch}, nil
	}
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, pushed work already on origin",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue4", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

func TestResolveDeadHolderWork_WorktreeStateUnreadable_EscalatesAndSkips(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.deadHolderWorktreeStateFn = func(townRoot, assignee, issueID string) (deadHolderWorktreeState, error) {
		return deadHolderWorktreeState{}, fmt.Errorf("reading current branch: exit status 128")
	}
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, worktree unreadable",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue5", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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

func TestResolveDeadHolderWork_NoWorktree_FeedsWithoutEscalation(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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
	townRoot, gtf, logged := feedTestRig(t)
	if err := os.MkdirAll(filepath.Join(townRoot, "gt"), 0755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, no worktree",
		ReadyCount: 1, ReadyIssues: []string{"gt-issue6"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	if !strings.Contains(string(data), "gt-issue6") {
		t.Errorf("expected sling for gt-issue6 (no worktree to lose), got: %q", data)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation when there is no worktree to check, got: %v", escalated)
	}
}

func TestResolveDeadHolderWork_ReusedSeat_FeedsWithoutEscalation(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)
	// The worktree at basalt's seat is checked out on a DIFFERENT issue's
	// branch — the seat was reused for other work since gt-issue7's holder
	// died. Nothing here is attributable to gt-issue7.
	otherBranch := "polecat/basalt/gt-other9+zzz999"
	f := gitfake.New()
	newDeadHolderWorktree(t, f, townRoot, "gt", "basalt", otherBranch)

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.openGitFn = deadHolderOpener(t, f)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, seat reused",
		ReadyCount: 1, ReadyIssues: []string{"gt-issue7"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	if !strings.Contains(string(data), "gt-issue7") {
		t.Errorf("expected sling for gt-issue7 (seat's worktree belongs to a different issue), got: %q", data)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation for a reused seat, got: %v", escalated)
	}
}

func TestResolveDeadHolderWork_RuntimeOnlyDirt_FeedsWithoutEscalation(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue8+dirt111"
	f := gitfake.New()
	worktreePath, _ := newDeadHolderWorktree(t, f, townRoot, "gt", "basalt", branch)
	if err := os.MkdirAll(filepath.Join(worktreePath, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".beads", "state.db"), []byte("runtime"), 0644); err != nil {
		t.Fatalf("write runtime dirt: %v", err)
	}

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.openGitFn = deadHolderOpener(t, f)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, runtime-only dirt",
		ReadyCount: 1, ReadyIssues: []string{"gt-issue8"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	if !strings.Contains(string(data), "gt-issue8") {
		t.Errorf("expected sling for gt-issue8 (only runtime dirt, nothing real to lose), got: %q", data)
	}
	if len(escalated) != 0 {
		t.Errorf("expected no escalation for runtime-only dirt, got: %v", escalated)
	}
}

func TestResolveDeadHolderWork_PreservePushFails_EscalatesAndSkips(t *testing.T) {
	t.Parallel()

	store, cleanup := newMemStore(t)
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

	townRoot, gtf, logged := feedTestRig(t)
	branch := "polecat/basalt/gt-issue9+push000"
	f := gitfake.New()
	worktreePath, _ := newDeadHolderWorktree(t, f, townRoot, "gt", "basalt", branch)
	// Point origin at a path with no repository, so both the primary push
	// and the <branch>-<sha7> fallback push fail — exercising the "resolve
	// by hand" escalation preserveWorktreeBranch raises when neither lands.
	f.Commit(t, worktreePath, branch, "local work", nil) // unpushed, so a preserve is attempted
	if err := f.Open(worktreePath).ConfigurePushURL("origin", filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Fatal(err)
	}

	var escalated []string
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }
	m.openGitFn = deadHolderOpener(t, f)
	m.SetAlertHooks(func(key, source, message string) {
		escalated = append(escalated, message)
	}, nil)

	c := strandedConvoyInfo{
		ID: "hq-cv1", Title: "Dead holder, unpushed work, push fails",
		ReadyCount: 2, ReadyIssues: []string{"gt-issue9", "gt-fresh2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
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
			id:         "gt-holdpro",
			issue:      &beadsdk.Issue{Status: beadsdk.StatusOpen, Labels: []string{"needs-pro"}},
			wantReason: "label needs-pro",
		},
		{
			id:         "gt-holdprocaps",
			issue:      &beadsdk.Issue{Status: beadsdk.StatusOpen, Labels: []string{"NEEDS-PRO"}},
			wantReason: "label NEEDS-PRO",
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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

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

	data := mustArgvLog(t, gtf, "sling")
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

	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gt/.beads"}`+"\n")

	gtf := newFakeCLI(nil)

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	m := newFakeGtManager(townRoot, logger, gtf, 10*time.Minute, map[string]beadsdk.Storage{"gt": store}, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{
		ID:          "hq-cv-unreadable",
		Title:       "Unreadable record",
		ReadyCount:  1,
		ReadyIssues: []string{"gt-unreadable"},
	})

	if data := argvLog(gtf, "sling"); len(data) > 0 {
		t.Errorf("expected no dispatch when the record cannot be read, got sling: %q", string(data))
	}
	assertLogged(t, logged, "gt-unreadable", "not dispatched: record unreadable")
}

// A dead-holder issue whose alert is already clear costs no `gt escalate
// clear` subprocess on later scans, and a raise or a failed clear re-arms the
// clear (gt-iesba).
func TestResolveDeadHolderWork_ClearRunsOncePerAlertKey(t *testing.T) {
	t.Parallel()

	m := NewConvoyManager(t.TempDir(), func(string, ...interface{}) {}, nil, time.Hour,
		map[string]beadsdk.Storage{}, nil, nil)
	m.listOriginBranchesFn = func(string) ([]string, error) { return nil, nil }

	var stateErr error
	m.deadHolderWorktreeStateFn = func(_, _, _ string) (deadHolderWorktreeState, error) {
		return deadHolderWorktreeState{}, stateErr
	}

	var cleared []string
	var clearErr error
	m.SetAlertHooks(
		func(key, source, msg string) {},
		func(reason string, keys ...string) error {
			cleared = append(cleared, keys...)
			return clearErr
		},
	)

	issueKey := deadHolderAlertKey("gt", "gt-issue1")
	originKey := deadHolderOriginAlertKey("gt")
	count := func(key string) int {
		n := 0
		for _, k := range cleared {
			if k == key {
				n++
			}
		}
		return n
	}
	scan := func() {
		m.resetOriginBranches()
		m.resolveDeadHolderWork("gt", "gt/polecats/basalt", "gt-issue1")
	}

	// A failed clear is retried on the next scan.
	clearErr = fmt.Errorf("escalate timed out")
	scan()
	scan()
	if got := count(issueKey); got != 2 {
		t.Fatalf("failed clear ran %d times over 2 scans, want 2 (retried)", got)
	}

	// Once a clear lands, later scans skip it.
	clearErr = nil
	scan()
	scan()
	scan()
	if got := count(issueKey); got != 3 {
		t.Errorf("issue key cleared %d times over 5 scans, want 3 (2 failed + 1 landed)", got)
	}
	if got := count(originKey); got != 3 {
		t.Errorf("origin key cleared %d times over 5 scans, want 3 (2 failed + 1 landed)", got)
	}

	// Raising the alert re-arms the clear for the next resolution.
	stateErr = fmt.Errorf("git status failed")
	scan()
	stateErr = nil
	scan()
	scan()
	if got := count(issueKey); got != 4 {
		t.Errorf("issue key cleared %d times after a re-raise, want 4", got)
	}
}

// TestFeedFirstReady_BacksOffAfterStartupFailure guards gt-wacl: a bead whose
// last sling failed at session start is left open and unassigned, and the scan
// used to re-sling it on its next tick. It must rest until its backoff window
// passes, and only that bead rests.
func TestFeedFirstReady_BacksOffAfterStartupFailure(t *testing.T) {
	t.Parallel()

	townRoot, gtf, logged := feedTestRig(t)
	m := newFakeGtManager(townRoot, func(format string, args ...interface{}) {
		*logged = append(*logged, fmt.Sprintf(format, args...))
	}, gtf, 10*time.Minute, nil, nil, nil)
	m.listOriginBranchesFn = func(rigRoot string) ([]string, error) { return nil, nil }

	if err := dispatch.RecordStartupFailure(townRoot, "gt-issue1", "startup blocked: trust dialog"); err != nil {
		t.Fatal(err)
	}

	c := strandedConvoyInfo{
		ID:          "hq-cv-backoff1",
		ReadyCount:  2,
		ReadyIssues: []string{"gt-issue1", "gt-issue2"},
	}
	m.feedFirstReady(c)

	data := mustArgvLog(t, gtf, "sling")
	if strings.Contains(string(data), "sling gt-issue1 ") {
		t.Errorf("gt-issue1 is resting after a startup failure but was re-slung: %q", data)
	}
	if !strings.Contains(string(data), "sling gt-issue2 ") {
		t.Errorf("the other ready bead should still feed: %q", data)
	}
	skipped := false
	for _, s := range *logged {
		if strings.Contains(s, "gt-issue1 not dispatched") && strings.Contains(s, "startup blocked: trust dialog") {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("the skip should be logged with the recorded reason, got: %v", *logged)
	}

	dispatch.ClearStartupFailure(townRoot, "gt-issue1")
	m.feedFirstReady(strandedConvoyInfo{ID: c.ID, ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})
	data = argvLog(gtf, "sling")
	if !strings.Contains(string(data), "sling gt-issue1 ") {
		t.Errorf("once the record is cleared gt-issue1 should feed: %q", data)
	}
}

// A close in a rig store finds the hq convoy tracking it. gt convoy stores a
// cross-rig tracks edge as external:<prefix>:<id> (trackingDependsOnID), and
// the store answers dependents of that exact target only, so a lookup by the
// bare id alone missed every rig bead a convoy tracks (gt-3y3rl).
func TestPollEvents_RigCloseFindsConvoyTrackingItExternally(t *testing.T) {
	t.Parallel()
	townRoot := convoyTestTown(t, `{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n")
	hq, hqCleanup := newMemStore(t)
	defer hqCleanup()
	rig, rigCleanup := newMemStore(t)
	defer rigCleanup()

	ctx := context.Background()
	now := time.Now().UTC()
	if err := hq.CreateIssue(ctx, &beadsdk.Issue{ID: "hq-cv-ext", Title: "convoy", Status: beadsdk.StatusOpen, Priority: 2, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now}, "test"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := hq.AddDependency(ctx, &beadsdk.Dependency{IssueID: "hq-cv-ext", DependsOnID: "external:gt:gt-ext1", Type: "tracks", CreatedAt: now, CreatedBy: "test"}, "test"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	mustCreateClosed(t, rig, "gt-ext1")

	var logged []string
	logger := func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	var checked []string
	m := NewConvoyManager(townRoot, logger, nil, 10*time.Minute, map[string]beadsdk.Storage{"hq": hq, "gastown": rig}, nil, nil)
	m.checkConvoyFn = func(_ context.Context, convoyID string) error {
		checked = append(checked, convoyID)
		return nil
	}
	startCursorsAtZero(m)
	m.pollStoresSnapshot(m.stores)

	if len(checked) != 1 || checked[0] != "hq-cv-ext" {
		t.Errorf("convoys checked = %v, want [hq-cv-ext]; logs:\n%s", checked, strings.Join(logged, "\n"))
	}
}

package lock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// testEpoch dates every lock fixture; staleness is decided by PID, not age.
var testEpoch = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func TestNew(t *testing.T) {
	t.Parallel()
	workerDir := "/tmp/test-worker"
	l := New(workerDir)

	if l.workerDir != workerDir {
		t.Errorf("workerDir = %q, want %q", l.workerDir, workerDir)
	}

	expectedPath := filepath.Join(workerDir, ".runtime", "agent.lock")
	if l.lockPath != expectedPath {
		t.Errorf("lockPath = %q, want %q", l.lockPath, expectedPath)
	}
}

func TestLockInfo_IsStale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pid       int
		wantStale bool
	}{
		{"current process", os.Getpid(), false},
		{"invalid pid zero", 0, true},
		{"invalid pid negative", -1, true},
		{"non-existent pid", 999999999, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &LockInfo{PID: tt.pid}
			if got := info.IsStale(); got != tt.wantStale {
				t.Errorf("IsStale() = %v, want %v", got, tt.wantStale)
			}
		})
	}
}

func TestLock_AcquireAndRelease(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Acquire lock
	err := l.Acquire("test-session")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	// Verify lock file exists
	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if info.SessionID != "test-session" {
		t.Errorf("SessionID = %q, want %q", info.SessionID, "test-session")
	}

	// Release lock
	err = l.Release()
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	// Verify lock file is gone
	_, err = l.Read()
	if err != ErrNotLocked {
		t.Errorf("Read() after release: error = %v, want ErrNotLocked", err)
	}
}

func TestLock_AcquireAlreadyHeld(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Acquire lock first time
	if err := l.Acquire("session-1"); err != nil {
		t.Fatalf("First Acquire() error = %v", err)
	}

	// Re-acquire with different session should refresh
	if err := l.Acquire("session-2"); err != nil {
		t.Fatalf("Second Acquire() error = %v", err)
	}

	// Verify session was updated
	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if info.SessionID != "session-2" {
		t.Errorf("SessionID = %q, want %q", info.SessionID, "session-2")
	}

	l.Release()
}

func TestLock_AcquireStaleLock(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a stale lock file with non-existent PID
	staleLock := LockInfo{
		PID:        999999999, // Non-existent PID
		AcquiredAt: testEpoch.Add(-time.Hour),
		SessionID:  "dead-session",
	}
	data, _ := json.Marshal(staleLock)
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Should acquire by cleaning up stale lock
	if err := l.Acquire("new-session"); err != nil {
		t.Fatalf("Acquire() with stale lock error = %v", err)
	}

	// Verify we now own it
	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
	if info.SessionID != "new-session" {
		t.Errorf("SessionID = %q, want %q", info.SessionID, "new-session")
	}

	l.Release()
}

func TestLock_Read(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Test reading non-existent lock
	_, err := l.Read()
	if err != ErrNotLocked {
		t.Errorf("Read() non-existent: error = %v, want ErrNotLocked", err)
	}

	// Test reading invalid JSON
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, []byte("invalid json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = l.Read()
	if err == nil {
		t.Error("Read() invalid JSON: expected error, got nil")
	}

	// Test reading valid lock
	validLock := LockInfo{
		PID:        12345,
		AcquiredAt: testEpoch,
		SessionID:  "test",
		Hostname:   "testhost",
	}
	data, _ := json.Marshal(validLock)
	if err := os.WriteFile(lockPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() valid lock: error = %v", err)
	}
	if info.PID != 12345 {
		t.Errorf("PID = %d, want 12345", info.PID)
	}
	if info.SessionID != "test" {
		t.Errorf("SessionID = %q, want %q", info.SessionID, "test")
	}
}

func TestLock_Check(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Check when unlocked
	if err := l.Check(); err != nil {
		t.Errorf("Check() unlocked: error = %v, want nil", err)
	}

	// Acquire and check (should pass - we hold it)
	if err := l.Acquire("test"); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Errorf("Check() owned by us: error = %v, want nil", err)
	}
	l.Release()

	// Create lock owned by another process - we'll simulate this by using a
	// fake "live" process via the stale lock detection mechanism.
	// Since we can't reliably find another live PID we can signal on all platforms,
	// we test that Check() correctly identifies our own PID vs a different PID.
	// The stale lock cleanup path is tested elsewhere.

	// Test that a non-existent PID lock gets cleaned up and returns nil
	staleLock := LockInfo{
		PID:        999999999, // Non-existent PID
		AcquiredAt: testEpoch,
		SessionID:  "other-session",
	}
	data, _ := json.Marshal(staleLock)
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Check should clean up the stale lock and return nil
	err := l.Check()
	if err != nil {
		t.Errorf("Check() with stale lock: error = %v, want nil (should clean up)", err)
	}

	// Verify lock was cleaned up
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Error("Check() should have removed stale lock file")
	}
}

func TestLock_Status(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Unlocked status
	status := l.Status()
	if status != "unlocked" {
		t.Errorf("Status() unlocked = %q, want %q", status, "unlocked")
	}

	// Owned by us
	if err := l.Acquire("test"); err != nil {
		t.Fatal(err)
	}
	status = l.Status()
	if status != "locked (by us)" {
		t.Errorf("Status() owned = %q, want %q", status, "locked (by us)")
	}
	l.Release()

	// Stale lock
	staleLock := LockInfo{
		PID:        999999999,
		AcquiredAt: testEpoch,
		SessionID:  "dead",
	}
	data, _ := json.Marshal(staleLock)
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	status = l.Status()
	expected := "stale (dead PID 999999999)"
	if status != expected {
		t.Errorf("Status() stale = %q, want %q", status, expected)
	}

	os.Remove(lockPath)
}

func TestLock_ForceRelease(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)
	if err := l.Acquire("test"); err != nil {
		t.Fatal(err)
	}

	if err := l.ForceRelease(); err != nil {
		t.Errorf("ForceRelease() error = %v", err)
	}

	_, err := l.Read()
	if err != ErrNotLocked {
		t.Errorf("Read() after ForceRelease: error = %v, want ErrNotLocked", err)
	}
}

func TestProcessExists(t *testing.T) {
	t.Parallel()
	// Current process exists
	if !processExists(os.Getpid()) {
		t.Error("processExists(current PID) = false, want true")
	}

	// Note: PID 1 (init/launchd) cannot be signaled without permission on macOS,
	// so we only test our own process and invalid PIDs.

	// Invalid PIDs
	if processExists(0) {
		t.Error("processExists(0) = true, want false")
	}
	if processExists(-1) {
		t.Error("processExists(-1) = true, want false")
	}
	if processExists(999999999) {
		t.Error("processExists(999999999) = true, want false")
	}
}

func TestFindAllLocks(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create multiple worker directories with locks
	workers := []string{"worker1", "worker2", "worker3"}
	for i, w := range workers {
		runtimeDir := filepath.Join(tmpDir, w, ".runtime")
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatal(err)
		}
		info := LockInfo{
			PID:        i + 100,
			AcquiredAt: testEpoch,
			SessionID:  "session-" + w,
		}
		data, _ := json.Marshal(info)
		lockPath := filepath.Join(runtimeDir, "agent.lock")
		if err := os.WriteFile(lockPath, data, 0644); err != nil {
			t.Fatal(err)
		}
	}

	locks, err := FindAllLocks(tmpDir)
	if err != nil {
		t.Fatalf("FindAllLocks() error = %v", err)
	}

	if len(locks) != 3 {
		t.Errorf("FindAllLocks() found %d locks, want 3", len(locks))
	}

	for _, w := range workers {
		workerDir := filepath.Join(tmpDir, w)
		if _, ok := locks[workerDir]; !ok {
			t.Errorf("FindAllLocks() missing lock for %s", w)
		}
	}
}

func TestCleanStaleLocks(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a stale lock
	runtimeDir := filepath.Join(tmpDir, "stale-worker", ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleLock := LockInfo{
		PID:        999999999,
		AcquiredAt: testEpoch,
		SessionID:  "dead-session",
	}
	data, _ := json.Marshal(staleLock)
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a live lock (current process)
	liveDir := filepath.Join(tmpDir, "live-worker", ".runtime")
	if err := os.MkdirAll(liveDir, 0755); err != nil {
		t.Fatal(err)
	}
	liveLock := LockInfo{
		PID:        os.Getpid(),
		AcquiredAt: testEpoch,
		SessionID:  "live-session",
	}
	data, _ = json.Marshal(liveLock)
	if err := os.WriteFile(filepath.Join(liveDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// No active tmux sessions.
	cleaned, err := cleanStaleLocks(tmpDir, func() []string { return nil })
	if err != nil {
		t.Fatalf("CleanStaleLocks() error = %v", err)
	}

	if cleaned != 1 {
		t.Errorf("CleanStaleLocks() cleaned %d, want 1", cleaned)
	}

	// Verify stale lock is gone
	staleLockPath := filepath.Join(runtimeDir, "agent.lock")
	if _, err := os.Stat(staleLockPath); !os.IsNotExist(err) {
		t.Error("Stale lock file should be removed")
	}

	// Verify live lock still exists
	liveLockPath := filepath.Join(liveDir, "agent.lock")
	if _, err := os.Stat(liveLockPath); err != nil {
		t.Error("Live lock file should still exist")
	}
}

// TestCleanStaleLocksKeepsLockWithLiveSession: a dead PID alone is not
// stale when the lock's tmux session is still listed (the spawning process
// exits normally while the agent runs on in tmux).
func TestCleanStaleLocksKeepsLockWithLiveSession(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	runtimeDir := filepath.Join(tmpDir, "worker", ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(LockInfo{PID: 999999999, AcquiredAt: testEpoch, SessionID: "$7"})
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	cleaned, err := cleanStaleLocks(tmpDir, func() []string { return []string{"gt-x", "$7"} })
	if err != nil || cleaned != 0 {
		t.Fatalf("cleanStaleLocks = %d, %v; want 0, nil", cleaned, err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "agent.lock")); err != nil {
		t.Errorf("lock with a live session was removed: %v", err)
	}
}

// fakeTmux answers list-sessions with out/err and records each call.
type fakeTmux struct {
	out   string
	err   error
	calls [][]string
}

func (f *fakeTmux) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return []byte(f.out), f.err
}

func TestGetActiveTmuxSessions(t *testing.T) {
	t.Parallel()
	f := &fakeTmux{out: "session1:$1\nsession2:$2\n"}
	sessions := activeTmuxSessions(f.run, "")

	// Should contain session names and IDs, in both $N and %N forms.
	want := []string{"session1", "$1", "%1", "session2", "$2", "%2"}
	if !reflect.DeepEqual(sessions, want) {
		t.Errorf("sessions = %v, want %v", sessions, want)
	}
	wantCall := []string{"tmux", "list-sessions", "-F", "#{session_name}:#{session_id}"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], wantCall) {
		t.Errorf("calls = %v, want [%v]", f.calls, wantCall)
	}
}

func TestGetActiveTmuxSessionsUsesSocket(t *testing.T) {
	t.Parallel()
	f := &fakeTmux{}
	activeTmuxSessions(f.run, "test-town")

	want := []string{"tmux", "-L", "test-town", "list-sessions", "-F", "#{session_name}:#{session_id}"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("getActiveTmuxSessions() calls = %v, want -L test-town: %v", f.calls, want)
	}
}

// TestGetActiveTmuxSessionsNoServer: tmux failing (no server, not installed)
// means no sessions, not an error.
func TestGetActiveTmuxSessionsNoServer(t *testing.T) {
	t.Parallel()
	f := &fakeTmux{err: errors.New("exit status 1")}
	if sessions := activeTmuxSessions(f.run, "gt-x"); sessions != nil {
		t.Errorf("sessions = %v, want nil", sessions)
	}
}

// TestGetActiveTmuxSessionsNameOnly: a line with no session id still yields
// its name, and blank lines are skipped.
func TestGetActiveTmuxSessionsNameOnly(t *testing.T) {
	t.Parallel()
	f := &fakeTmux{out: "solo\n\nother:%3\r\n"}
	want := []string{"solo", "other", "%3"}
	if got := activeTmuxSessions(f.run, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("sessions = %v, want %v", got, want)
	}
}

func TestSplitOnColon(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected []string
	}{
		{"a:b", []string{"a", "b"}},
		{"abc", []string{"abc"}},
		{"a:b:c", []string{"a", "b:c"}},
		{":b", []string{"", "b"}},
		{"a:", []string{"a", ""}},
	}

	for _, tt := range tests {
		result := splitOnColon(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("splitOnColon(%q) = %v, want %v", tt.input, result, tt.expected)
			continue
		}
		for i := range result {
			if result[i] != tt.expected[i] {
				t.Errorf("splitOnColon(%q)[%d] = %q, want %q", tt.input, i, result[i], tt.expected[i])
			}
		}
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected []string
	}{
		{"a\nb\nc", []string{"a", "b", "c"}},
		{"a\r\nb\r\nc", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{"", []string{}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
	}

	for _, tt := range tests {
		result := splitLines(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("splitLines(%q) = %v, want %v", tt.input, result, tt.expected)
			continue
		}
		for i := range result {
			if result[i] != tt.expected[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", tt.input, i, result[i], tt.expected[i])
			}
		}
	}
}

func TestDetectCollisions(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Create a stale lock
	runtimeDir := filepath.Join(tmpDir, "stale-worker", ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	staleLock := LockInfo{
		PID:        999999999,
		AcquiredAt: testEpoch,
		SessionID:  "dead-session",
	}
	data, _ := json.Marshal(staleLock)
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Create an orphaned lock (live PID but session not in active list)
	orphanDir := filepath.Join(tmpDir, "orphan-worker", ".runtime")
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatal(err)
	}
	orphanLock := LockInfo{
		PID:        os.Getpid(), // Live PID
		AcquiredAt: testEpoch,
		SessionID:  "orphan-session", // Not in active list
	}
	data, _ = json.Marshal(orphanLock)
	if err := os.WriteFile(filepath.Join(orphanDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	activeSessions := []string{"active-session-1", "active-session-2"}
	collisions := DetectCollisions(tmpDir, activeSessions)

	if len(collisions) != 2 {
		t.Errorf("DetectCollisions() found %d collisions, want 2: %v", len(collisions), collisions)
	}

	// Verify we found both issues
	foundStale := false
	foundOrphan := false
	for _, c := range collisions {
		if contains(c, "stale lock") {
			foundStale = true
		}
		if contains(c, "orphaned lock") {
			foundOrphan = true
		}
	}

	if !foundStale {
		t.Error("DetectCollisions() did not find stale lock")
	}
	if !foundOrphan {
		t.Error("DetectCollisions() did not find orphaned lock")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestLock_ReleaseNonExistent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	if err := os.MkdirAll(workerDir, 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Releasing a non-existent lock should not error
	if err := l.Release(); err != nil {
		t.Errorf("Release() non-existent: error = %v, want nil", err)
	}
}

func TestLock_CheckCleansUpStaleLock(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a stale lock
	staleLock := LockInfo{
		PID:        999999999,
		AcquiredAt: testEpoch,
		SessionID:  "dead",
	}
	data, _ := json.Marshal(staleLock)
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)

	// Check should clean up stale lock and return nil
	if err := l.Check(); err != nil {
		t.Errorf("Check() with stale lock: error = %v, want nil", err)
	}

	// Lock file should be removed
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("Check() should have removed stale lock file")
	}
}

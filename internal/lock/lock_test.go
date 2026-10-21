package lock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/procid"
)

// testEpoch dates every lock fixture; staleness is decided by the process the
// lock names, not by age.
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

// TestLockInfo_IsStale covers the locks this host settles without a scripted
// table: a pid with no process behind it, and the two ways a live pid is or is
// not the lock's writer. The rule is tabled against a scripted process table
// in TestLockInfoHolder.
func TestLockInfo_IsStale(t *testing.T) {
	t.Parallel()
	live, ok := procid.StartToken(os.Getpid())
	if !ok {
		t.Fatalf("no start token for this process (pid %d)", os.Getpid())
	}
	// A start time this process does not have: what the pid carries once the
	// kernel has handed it to somebody else.
	const stranger = "1700000000.12"
	if live == stranger {
		t.Fatalf("this process's start token is the fixture for a stranger (%q)", live)
	}

	tests := []struct {
		name      string
		info      LockInfo
		wantStale bool
	}{
		{"invalid pid zero", LockInfo{PID: 0}, true},
		{"invalid pid negative", LockInfo{PID: -1}, true},
		{"non-existent pid", LockInfo{PID: 999999999}, true},
		{"live pid carrying its recorded token", LockInfo{PID: os.Getpid(), Start: live}, false},
		// The case a bare pid gets wrong: the number is in use again, by a
		// process that started at another time.
		{"live pid carrying another token", LockInfo{PID: os.Getpid(), Start: stranger}, true},
		// A lock written before the token was recorded names no process a
		// reader can confirm, and none it can rule out either, so its live pid
		// still holds it; only that pid's death makes it stale.
		{"live pid, no recorded token", LockInfo{PID: os.Getpid()}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.IsStale(); got != tt.wantStale {
				t.Errorf("IsStale() = %v, want %v", got, tt.wantStale)
			}
		})
	}
}

// scriptedProcesses is a process table a test controls: pid -> the start token
// the live process with that pid carries. A pid absent from it has no process;
// a pid mapped to "" is a live process whose kernel will not give a start time.
type scriptedProcesses map[int]string

func (p scriptedProcesses) read(pid int) (string, bool) {
	token, ok := p[pid]
	return token, ok
}

// TestLockInfoHolder tables the staleness rule against a scripted process
// table, so every case is decided by what the lock recorded rather than by
// which processes this host happens to be running.
func TestLockInfoHolder(t *testing.T) {
	t.Parallel()
	const pid = 4242
	written := LockInfo{PID: pid, Start: "1700000000.12"}

	tests := []struct {
		name     string
		info     LockInfo
		table    scriptedProcesses
		wantHeld bool
		wantWhy  string
	}{
		{"the process that recorded it", written, scriptedProcesses{pid: written.Start}, true, ""},
		{"a recycled pid", written, scriptedProcesses{pid: "1700000999.5"}, false, "PID 4242 now names another process"},
		{"a dead pid", written, scriptedProcesses{}, false, "dead PID 4242"},
		// The kernel will not say when the live pid started: no evidence
		// against the holder, so the lock stands.
		{"a live pid with no readable token", written, scriptedProcesses{pid: ""}, true, ""},
		// A lock written by an older gt records no token: nothing to compare
		// against the live pid, so the lock stands.
		{"live pid, no recorded token", LockInfo{PID: pid}, scriptedProcesses{pid: "1700000000.12"}, true, ""},
		{"dead pid, no recorded token", LockInfo{PID: pid}, scriptedProcesses{}, false, "dead PID 4242"},
		{"zero pid", LockInfo{Start: "1700000000.12"}, scriptedProcesses{0: "1700000000.12"}, false, "PID 0 is not a process number"},
		{"negative pid", LockInfo{PID: -1, Start: "1700000000.12"}, scriptedProcesses{-1: "1700000000.12"}, false, "PID -1 is not a process number"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			held, why := tt.info.holder(tt.table.read)
			if held != tt.wantHeld {
				t.Errorf("holder() held = %v, want %v", held, tt.wantHeld)
			}
			if why != tt.wantWhy {
				t.Errorf("holder() gone = %q, want %q", why, tt.wantWhy)
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

// TestAcquireIgnoresLockHeldByRecycledPid: the pid a lock file names is the
// holder only while it still carries the start token the lock recorded. Once
// the process that wrote the lock dies the kernel hands its number to the
// next process, and a lock gt prime left behind then reads as held by a
// stranger, which returns ErrLocked and delivers no role context in that
// worktree (gt-xa65s). Acquire has to take such a lock.
//
// The pid named is the test binary's parent, which is live and is not this
// process, so the pid rule this replaces called the lock held and the case
// fails without the token; the start time recorded is not the one that pid
// carries.
func TestAcquireIgnoresLockHeldByRecycledPid(t *testing.T) {
	t.Parallel()
	pid := os.Getppid()
	if pid <= 0 || pid == os.Getpid() {
		t.Fatalf("$PPID %d is no other process to name", pid)
	}
	const stranger = "1700000000.12"
	if live, ok := procid.StartToken(pid); !ok {
		t.Fatalf("no start token for pid %d, so the case cannot tell a reused pid", pid)
	} else if live == stranger {
		t.Fatalf("pid %d carries the fixture for a stranger (%q)", pid, live)
	}

	l := New(writeLockFixture(t, LockInfo{
		PID:        pid,
		Start:      stranger,
		AcquiredAt: testEpoch,
		SessionID:  "gone-session",
	}))

	if err := l.Acquire("new-session"); err != nil {
		t.Fatalf("Acquire() over a lock held by a recycled pid: %v", err)
	}

	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if info.PID != os.Getpid() || info.SessionID != "new-session" {
		t.Errorf("lock = %+v, want this process (pid %d, session new-session)", info, os.Getpid())
	}
}

// TestAcquireRefusesLockWithNoRecordedStartToken is the degraded branch: a
// lock written before the start token was recorded names a pid that is live
// and nothing else a reader can compare, so Acquire keeps the pid rule that
// lock was written under and refuses it. Calling it stale would hand a
// running agent's identity to a second one on nothing but a guess.
func TestAcquireRefusesLockWithNoRecordedStartToken(t *testing.T) {
	t.Parallel()
	const pid = 4242

	l := New(writeLockFixture(t, LockInfo{PID: pid, AcquiredAt: testEpoch, SessionID: "older-gt"}))
	l.table = scriptedProcesses{pid: "1700000000.12"}.read

	if err := l.Acquire("new-session"); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire() over a live writer with no recorded token = %v, want ErrLocked", err)
	}

	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if info.PID != pid || info.SessionID != "older-gt" {
		t.Errorf("lock = %+v, want the older gt's record (pid %d, session older-gt)", info, pid)
	}
}

// TestAcquireRefusesLockHeldByALiveProcess is the other side of the rule: the
// pid still carries the recorded start token, so its process is still the
// holder and Acquire must refuse rather than take the lock. The process table
// is scripted, so the case does not depend on the host's own pids.
func TestAcquireRefusesLockHeldByALiveProcess(t *testing.T) {
	t.Parallel()
	const pid = 4242
	holder := LockInfo{PID: pid, Start: "1700000000.12", AcquiredAt: testEpoch, SessionID: "live-session"}

	l := New(writeLockFixture(t, holder))
	l.table = scriptedProcesses{pid: holder.Start}.read

	if err := l.Acquire("new-session"); !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire() over a live holder = %v, want ErrLocked", err)
	}

	// The holder's lock is untouched.
	info, err := l.Read()
	if err != nil {
		t.Fatalf("Read() = %v", err)
	}
	if info.PID != holder.PID || info.Start != holder.Start || info.SessionID != holder.SessionID {
		t.Errorf("lock = %+v, want the holder's record %+v", info, holder)
	}
}

// writeLockFixture writes info as the agent lock of a fresh worker directory
// and returns that directory.
func writeLockFixture(t *testing.T, info LockInfo) string {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return writeLockBytes(t, string(data))
}

// writeLockBytes writes raw as the agent lock of a fresh worker directory and
// returns that directory.
func writeLockBytes(t *testing.T, raw string) string {
	t.Helper()
	workerDir := filepath.Join(t.TempDir(), "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	return workerDir
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

// TestLock_Status covers every verdict Status prints, including both pieces of
// evidence that make a lock stale: a dead pid leaves a lock to remove, a reused
// pid leaves one whose writer is long gone, and the reader gets told which.
func TestLock_Status(t *testing.T) {
	t.Parallel()
	const livePid = 4242
	mine, ok := procid.StartToken(os.Getpid())
	if !ok {
		t.Fatalf("no start token for this process (pid %d)", os.Getpid())
	}

	tests := []struct {
		name    string
		fixture *LockInfo // written as the lock file; nil for no lock file at all
		raw     string    // written instead of fixture when set
		table   scriptedProcesses
		want    string
		prefix  bool // want is a prefix: the error text carries the detail
	}{
		{name: "unlocked", want: "unlocked"},
		{name: "locked by us", fixture: &LockInfo{PID: os.Getpid(), Start: mine, AcquiredAt: testEpoch, SessionID: "ours"}, want: "locked (by us)"},
		{
			name:    "stale, process gone",
			fixture: &LockInfo{PID: 999999999, AcquiredAt: testEpoch, SessionID: "dead"},
			want:    "stale (dead PID 999999999)",
		},
		{
			name:    "stale, pid reused",
			fixture: &LockInfo{PID: livePid, Start: "1700000000.12", AcquiredAt: testEpoch, SessionID: "gone"},
			table:   scriptedProcesses{livePid: "1700000999.5"},
			want:    "stale (PID 4242 now names another process)",
		},
		{
			name:    "locked by another process",
			fixture: &LockInfo{PID: livePid, Start: "1700000000.12", AcquiredAt: testEpoch, SessionID: "other"},
			table:   scriptedProcesses{livePid: "1700000000.12"},
			want:    "locked by PID 4242 (session: other)",
		},
		{name: "unreadable lock file", raw: "not json", want: "error: ", prefix: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l *Lock
			switch {
			case tt.raw != "":
				l = New(writeLockBytes(t, tt.raw))
			case tt.fixture != nil:
				l = New(writeLockFixture(t, *tt.fixture))
			default:
				l = New(filepath.Join(t.TempDir(), "worker"))
			}
			if tt.table != nil {
				l.table = tt.table.read
			}

			got := l.Status()
			if tt.prefix {
				if !strings.HasPrefix(got, tt.want) {
					t.Errorf("Status() = %q, want the prefix %q", got, tt.want)
				}
				return
			}
			if got != tt.want {
				t.Errorf("Status() = %q, want %q", got, tt.want)
			}
		})
	}
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
		Start:      "1700000000.12", // the process the table below reports
		AcquiredAt: testEpoch,
		SessionID:  "live-session",
	}
	data, _ = json.Marshal(liveLock)
	if err := os.WriteFile(filepath.Join(liveDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a lock from an older gt, which recorded no start token: the live
	// pid it names is all the evidence there is, and it keeps the lock.
	bareDir := filepath.Join(tmpDir, "bare-worker", ".runtime")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(LockInfo{PID: os.Getpid(), AcquiredAt: testEpoch, SessionID: "older-gt"})
	if err := os.WriteFile(filepath.Join(bareDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// No active tmux sessions; the live worker is the one the table holds.
	table := scriptedProcesses{os.Getpid(): liveLock.Start}
	cleaned, err := cleanStaleLocks(tmpDir, func() []string { return nil }, table.read)
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

	// Verify the tokenless lock still exists
	if _, err := os.Stat(filepath.Join(bareDir, "agent.lock")); err != nil {
		t.Error("Lock with no recorded token should still exist")
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

	cleaned, err := cleanStaleLocks(tmpDir, func() []string { return []string{"gt-x", "$7"} }, scriptedProcesses{}.read)
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
		PID:        os.Getpid(),     // Live PID: the table below holds it
		Start:      "1700000000.12", // ...with the token it recorded
		AcquiredAt: testEpoch,
		SessionID:  "orphan-session", // Not in active list
	}
	data, _ = json.Marshal(orphanLock)
	if err := os.WriteFile(filepath.Join(orphanDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a lock from an older gt, which recorded no start token: the live
	// pid it names is the only evidence there is, so the lock is reported
	// neither stale nor orphaned.
	bareDir := filepath.Join(tmpDir, "bare-worker", ".runtime")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatal(err)
	}
	bareLock := LockInfo{PID: os.Getpid(), AcquiredAt: testEpoch, SessionID: "active-session-1"}
	data, _ = json.Marshal(bareLock)
	if err := os.WriteFile(filepath.Join(bareDir, "agent.lock"), data, 0644); err != nil {
		t.Fatal(err)
	}

	activeSessions := []string{"active-session-1", "active-session-2"}
	table := scriptedProcesses{os.Getpid(): orphanLock.Start}
	collisions := detectCollisions(tmpDir, activeSessions, table.read)

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

// acquireOnRead is a process table that acquires the lock for the worker the
// first time it is asked about pid, then reports pid from base. The staleness
// verdict reads the table between the lock scan and the removal, so the
// Acquire lands in exactly the window a concurrent process can rewrite the
// stale lock in — on every run, not by luck.
type acquireOnRead struct {
	base    processTable
	pid     int
	acquire func()
	once    *sync.Once
}

func (a acquireOnRead) read(pid int) (string, bool) {
	if pid == a.pid {
		a.once.Do(a.acquire)
	}
	return a.base(pid)
}

// TestCheckReportsStaleLockItCannotRemove: Check used to read a failed
// cleanup as "not locked", so the caller acted on an identity whose lock was
// still on disk. The failure is returned instead.
func TestCheckReportsStaleLockItCannotRemove(t *testing.T) {
	t.Parallel()
	workerDir := filepath.Join(t.TempDir(), "worker")
	runtimeDir := filepath.Join(workerDir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(LockInfo{PID: 999999999, AcquiredAt: testEpoch, SessionID: "dead-session"})
	lockPath := filepath.Join(runtimeDir, "agent.lock")
	if err := os.WriteFile(lockPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	// A directory where the flock file belongs makes the removal's flock fail.
	if err := os.Mkdir(lockPath+".flock", 0755); err != nil {
		t.Fatal(err)
	}

	l := New(workerDir)
	l.table = scriptedProcesses{}.read
	err := l.Check()
	if err == nil {
		t.Fatal("Check() = nil, want an error for the stale lock it could not remove")
	}
	if !strings.Contains(err.Error(), "agent.lock") {
		t.Errorf("Check() error = %v, want it to name the lock it could not remove", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Errorf("stale lock removed despite the failed cleanup: %v", statErr)
	}
}

// TestCleanStaleLocksReportsLockItCannotRemove: a lock the sweep cannot remove
// is reported rather than silently counted as clean, and one worker it cannot
// repair does not stop it repairing the rest.
func TestCleanStaleLocksReportsLockItCannotRemove(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	writeStale := func(name string) string {
		t.Helper()
		runtimeDir := filepath.Join(tmpDir, name, ".runtime")
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(LockInfo{PID: 999999999, AcquiredAt: testEpoch, SessionID: "dead-session"})
		lockPath := filepath.Join(runtimeDir, "agent.lock")
		if err := os.WriteFile(lockPath, data, 0644); err != nil {
			t.Fatal(err)
		}
		return lockPath
	}

	// A directory where the flock file belongs makes this worker's removal fail.
	stuckPath := writeStale("stuck-worker")
	if err := os.Mkdir(stuckPath+".flock", 0755); err != nil {
		t.Fatal(err)
	}
	cleanablePath := writeStale("cleanable-worker")

	cleaned, err := cleanStaleLocks(tmpDir, func() []string { return nil }, scriptedProcesses{}.read)
	if err == nil {
		t.Fatal("cleanStaleLocks() = nil error, want an error for the lock it could not remove")
	}
	if !strings.Contains(err.Error(), "stuck-worker") {
		t.Errorf("cleanStaleLocks() error = %v, want it to name the worker it could not clean", err)
	}
	if cleaned != 1 {
		t.Errorf("cleanStaleLocks() cleaned %d, want 1", cleaned)
	}
	if _, statErr := os.Stat(stuckPath); statErr != nil {
		t.Errorf("stuck lock removed despite the failed cleanup: %v", statErr)
	}
	if _, statErr := os.Stat(cleanablePath); !os.IsNotExist(statErr) {
		t.Errorf("cleanable lock still exists (stat error %v)", statErr)
	}
}

// TestStaleLockRemovalSparesLockRewrittenBeforeFlock: a stale verdict comes
// from a read taken without the flock, so an Acquire for the same worker can
// remove that stale lock and write its own live one before the remover gets
// there. The remover has to find the live lock and leave it; deleting it
// leaves one identity with a running agent and no lock on disk, and a third
// Acquire then succeeds beside it.
func TestStaleLockRemovalSparesLockRewrittenBeforeFlock(t *testing.T) {
	t.Parallel()
	const deadPID = 999999999
	holder := scriptedProcesses{os.Getpid(): "1700000000.12"}

	tests := []struct {
		name string
		// remove judges the stale lock in workerDir and removes it, reading
		// the process table through table.
		remove func(t *testing.T, workerDir string, table processTable)
	}{
		{"sweep", func(t *testing.T, workerDir string, table processTable) {
			cleaned, err := cleanStaleLocks(filepath.Dir(workerDir), func() []string { return nil }, table)
			if err != nil || cleaned != 0 {
				t.Errorf("cleanStaleLocks = %d, %v; want 0, nil", cleaned, err)
			}
		}},
		{"check", func(t *testing.T, workerDir string, table processTable) {
			l := New(workerDir)
			l.table = table
			if err := l.Check(); err != nil {
				t.Errorf("Check() = %v, want nil (the rewritten lock is ours)", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workerDir := filepath.Join(t.TempDir(), "worker")
			runtimeDir := filepath.Join(workerDir, ".runtime")
			if err := os.MkdirAll(runtimeDir, 0755); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(LockInfo{PID: deadPID, AcquiredAt: testEpoch, SessionID: "dead-session"})
			if err := os.WriteFile(filepath.Join(runtimeDir, "agent.lock"), data, 0644); err != nil {
				t.Fatal(err)
			}

			var once sync.Once
			table := acquireOnRead{base: holder.read, pid: deadPID, once: &once, acquire: func() {
				acquirer := New(workerDir)
				acquirer.table = holder.read
				if err := acquirer.Acquire("live-session"); err != nil {
					t.Errorf("injected Acquire() = %v", err)
				}
			}}

			tt.remove(t, workerDir, table.read)

			info, err := New(workerDir).Read()
			if err != nil {
				t.Fatalf("the live lock is gone: Read() = %v", err)
			}
			if info.PID != os.Getpid() || info.SessionID != "live-session" {
				t.Errorf("lock = pid %d session %q, want pid %d session %q",
					info.PID, info.SessionID, os.Getpid(), "live-session")
			}
		})
	}
}

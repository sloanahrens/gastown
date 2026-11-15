//go:build !windows

// Advisory flock is a no-op on Windows, so these tests run elsewhere only.

package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

func TestTryAcquireSlingBeadLock_Contention(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	beadID := "gt-race123"

	release1, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err != nil {
		t.Fatalf("first lock acquire failed: %v", err)
	}

	release2, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err == nil {
		release2()
		t.Fatal("expected second lock acquire to fail due to contention")
	}
	if !strings.Contains(err.Error(), "already being slung") {
		t.Fatalf("expected deterministic contention error, got: %v", err)
	}

	release1()

	release3, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err != nil {
		t.Fatalf("expected lock acquire to succeed after release: %v", err)
	}
	release3()
}

func TestTryAcquireSlingAssigneeLock_DifferentAgents(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()

	// Different agents should not block each other.
	release1, err := tryAcquireSlingAssigneeLock(townRoot, "rig/polecats/cat1")
	if err != nil {
		t.Fatalf("first agent lock failed: %v", err)
	}
	defer release1()

	release2, err := tryAcquireSlingAssigneeLock(townRoot, "rig/polecats/cat2")
	if err != nil {
		t.Fatalf("second agent lock should not be blocked by first: %v", err)
	}
	defer release2()
}

// A held assignee lock is waited for, not refused: the retry sleeps between
// attempts and takes the lock once the holder lets go. The wait is injected, so
// the holder's release happens inside the first sleep rather than racing it.
func TestTryAcquireSlingAssigneeLock_WaitsForTheHolder(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/racecat"
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}

	var sleeps []time.Duration
	release, err := tryAcquireSlingAssigneeLockWith(townRoot, agent, func(d time.Duration) {
		sleeps = append(sleeps, d)
		holder()
	})
	if err != nil {
		t.Fatalf("the waiter must take the lock once the holder releases: %v", err)
	}
	release()
	if len(sleeps) != 1 || sleeps[0] != 500*time.Millisecond {
		t.Errorf("sleeps = %v, want one 500ms wait before the retry", sleeps)
	}
}

// A holder that never lets go is a stuck sling: the waiter gives up after its
// bounded retries and says so, rather than blocking the sling forever.
func TestTryAcquireSlingAssigneeLock_TimesOutOnAStuckHolder(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/stuckcat"
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}
	defer holder()

	sleeps := 0
	release, err := tryAcquireSlingAssigneeLockWith(townRoot, agent, func(time.Duration) { sleeps++ })
	if err == nil {
		release()
		t.Fatal("a lock held throughout must not be acquired")
	}
	if !strings.Contains(err.Error(), "timed out acquiring assignee sling lock for "+agent+" after 10s") {
		t.Errorf("error = %v, want the timeout named", err)
	}
	if sleeps != 19 {
		t.Errorf("slept %d times, want 19 (a wait between each of 20 attempts)", sleeps)
	}
}

// A give-up on the assignee lock is a typed error: the spec dispatcher answers
// the timeout differently from every other sling failure, and it matches the
// sentinel through the engine's "serializing hook write" wrap without reading
// the message (gt-a4cdcd).
func TestAssigneeLockTimeoutIsTyped(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/sbx-flash-1"
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}
	defer holder()

	_, err = tryAcquireSlingAssigneeLockWith(townRoot, agent, func(time.Duration) {})
	if err == nil {
		t.Fatal("a lock held throughout must not be acquired")
	}
	if !errors.Is(err, ErrAssigneeLockTimeout) {
		t.Errorf("errors.Is(%v, ErrAssigneeLockTimeout) = false, want the timeout to be typed", err)
	}
	// internal/sling wraps the lock error before it reaches the dispatcher.
	wrapped := fmt.Errorf("serializing hook write for %s: %w", agent, err)
	if !errors.Is(wrapped, ErrAssigneeLockTimeout) {
		t.Errorf("errors.Is(%v, ErrAssigneeLockTimeout) = false, want the sentinel to survive the engine's wrap", wrapped)
	}
}

// An acquired lock leaves a record of who holds it, and gives the record back
// on release: the next sling that times out on this lock prints whose it was.
func TestTryAcquireSlingAssigneeLock_WritesAndRemovesTheHolderRecord(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/recordcat"
	release, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("lock acquire failed: %v", err)
	}
	records := assigneeHolderFiles(t, townRoot)
	if len(records) != 1 || filepath.Base(records[0]) != "assignee_gastown_polecats_recordcat.holder" {
		t.Fatalf("held lock's holder records = %v, want one for %s", records, agent)
	}
	data, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatalf("reading the holder record: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 4 || fields[0] != "pid" || fields[2] != "started" {
		t.Fatalf("holder record = %q, want \"pid <pid> started <RFC3339>\"", data)
	}
	if fields[1] != strconv.Itoa(os.Getpid()) {
		t.Errorf("holder record = %q, want this process's pid %d", data, os.Getpid())
	}
	if _, err := time.Parse(time.RFC3339, fields[3]); err != nil {
		t.Errorf("holder record = %q, want its start time to be RFC3339: %v", data, err)
	}

	release()
	if got := assigneeHolderFiles(t, townRoot); len(got) != 0 {
		t.Errorf("released lock left its holder record behind: %v", got)
	}
}

// The timeout names the holder, so the next occurrence points at the sling that
// kept the lock rather than only at the seat that lost it. The test holds the
// lock from a second acquirer whose wait is injected, so nothing sleeps.
func TestTryAcquireSlingAssigneeLock_TimeoutNamesTheHolder(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/heldcat"
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}
	defer holder()

	release, err := tryAcquireSlingAssigneeLockWith(townRoot, agent, func(time.Duration) {})
	if err == nil {
		release()
		t.Fatal("a lock held throughout must not be acquired")
	}
	if !errors.Is(err, ErrAssigneeLockTimeout) {
		t.Errorf("errors.Is(%v, ErrAssigneeLockTimeout) = false, want the timeout to stay typed", err)
	}
	at := strings.Index(err.Error(), "held by pid ")
	if at < 0 {
		t.Fatalf("error = %v, want it to name the holder", err)
	}
	fields := strings.Fields(err.Error()[at+len("held by pid "):])
	if len(fields) < 3 {
		t.Fatalf("error = %v, want the holder's pid and start time", err)
	}
	if fields[0] != strconv.Itoa(os.Getpid()) {
		t.Errorf("error names pid %s, want the holder's pid %d", fields[0], os.Getpid())
	}
	started, parseErr := time.Parse(time.RFC3339, strings.TrimSuffix(fields[2], ";"))
	if parseErr != nil {
		t.Fatalf("error = %v, want the holder's start time after \"started\": %v", err, parseErr)
	}
	if started.After(time.Now().Add(time.Minute)) {
		t.Errorf("holder start time %s is in the future", started)
	}
}

// A holder record the timeout cannot use is described on the timeout, never
// fatal to it and never echoed: the file is written by another process, so its
// bytes are not trusted enough to print. A record naming another holder is read
// as written.
func TestTryAcquireSlingAssigneeLock_TimeoutDescribesAnUnusableHolderRecord(t *testing.T) {
	t.Parallel()

	const agent = "gastown/polecats/cluecat"
	const recordName = "assignee_gastown_polecats_cluecat.holder"
	tests := []struct {
		name string
		// write replaces the holder's record before the waiter asks for it.
		write func(t *testing.T, recordPath string)
		want  string
		// notWant is text the recorded file holds and the error must not echo.
		notWant string
	}{
		{
			name: "another holder's record",
			write: func(t *testing.T, p string) {
				writeHolderRecord(t, p, "pid 4242 started 2026-11-15T11:05:03Z\n")
			},
			want: "held by pid 4242 started 2026-11-15T11:05:03Z",
		},
		{
			name: "missing",
			write: func(t *testing.T, p string) {
				if err := os.Remove(p); err != nil {
					t.Fatalf("removing the record: %v", err)
				}
			},
			want: "no holder recorded: the holder sidecar is missing",
		},
		{
			name: "unreadable",
			write: func(t *testing.T, p string) {
				if err := os.Remove(p); err != nil {
					t.Fatalf("removing the record: %v", err)
				}
				// A directory at the record's path: reading it fails.
				if err := os.Mkdir(p, 0755); err != nil {
					t.Fatalf("replacing the record with a directory: %v", err)
				}
			},
			want: "no holder recorded: the holder sidecar could not be read",
		},
		{
			name: "not a holder record",
			write: func(t *testing.T, p string) {
				writeHolderRecord(t, p, "sling 77 holds this\n")
			},
			want:    "no holder recorded: the holder sidecar is not a holder record",
			notWant: "sling 77 holds this",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			townRoot := t.TempDir()
			holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
			if err != nil {
				t.Fatalf("holder lock acquire failed: %v", err)
			}
			defer holder()
			tt.write(t, filepath.Join(townRoot, ".runtime", "locks", "sling", recordName))

			_, err = tryAcquireSlingAssigneeLockWith(townRoot, agent, func(time.Duration) {})
			if err == nil {
				t.Fatal("a lock held throughout must not be acquired")
			}
			if !errors.Is(err, ErrAssigneeLockTimeout) {
				t.Fatalf("errors.Is(%v, ErrAssigneeLockTimeout) = false, want the timeout to stay typed", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to say %q", err, tt.want)
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error = %v echoes the record's own text; a record the reader cannot parse must be described, not printed", err)
			}
		})
	}
}

// A lock whose holder cannot be recorded fails the acquire and keeps no lock:
// a sling that held the lock without the record would leave the next timeout
// with nothing to name, and the older record of the sling it replaced with it.
func TestTryAcquireSlingAssigneeLock_HolderRecordFailureFailsClosed(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	agent := "gastown/polecats/norecord"
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A directory where the record goes: the write cannot succeed.
	recordPath := filepath.Join(lockDir, "assignee_gastown_polecats_norecord.holder")
	if err := os.Mkdir(recordPath, 0755); err != nil {
		t.Fatal(err)
	}

	release, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err == nil {
		release()
		t.Fatal("acquire succeeded, want the unrecordable holder to fail it")
	}
	if !strings.Contains(err.Error(), "recording the holder") {
		t.Errorf("error = %v, want it to name the holder record", err)
	}
	if _, statErr := os.Stat(filepath.Join(lockDir, "assignee_gastown_polecats_norecord.flock")); !os.IsNotExist(statErr) {
		t.Errorf("the failed acquire left its lock file behind (stat err %v)", statErr)
	}

	// The lock itself must be free: clear the obstacle and take it again. The
	// failed acquire's cleanup already removed the directory standing in for
	// the record, so its absence is the expected case.
	if err := os.RemoveAll(recordPath); err != nil {
		t.Fatal(err)
	}
	release, err = tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("the failed acquire kept the lock: %v", err)
	}
	release()
}

// A sling that fails after taking the lock still gives the holder record back:
// the engine releases the lock through defer, and the record goes with it.
func TestAssigneeLockHolderRecordGoesWhenTheSlingFails(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	h.run.lockAssignee = func(_, agent string) (func(), error) {
		return tryAcquireSlingAssigneeLock(townRoot, agent)
	}
	var recordAtHook string
	h.run.hook = func(id, agent, _, _ string, _ *beadFieldUpdates) error {
		records := assigneeHolderFiles(t, townRoot)
		if len(records) == 1 {
			data, readErr := os.ReadFile(records[0])
			if readErr == nil {
				recordAtHook = string(data)
			}
		}
		return errors.New("bd is down")
	}

	if _, err := h.run.executeSling(executeParams()); err == nil {
		t.Fatal("sling succeeded, want the hook failure to fail it")
	}
	if !strings.Contains(recordAtHook, "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("holder record at the hook write = %q, want it to name this process while the lock is held", recordAtHook)
	}
	if got := assigneeHolderFiles(t, townRoot); len(got) != 0 {
		t.Errorf("the failed sling left its holder record behind: %v", got)
	}
}

// The dispatcher's deferral line carries the holder text: the line prints the
// sling's error, so the holder the timeout names is what an operator reading
// the tick's output sees.
func TestSpecDeferralLineCarriesTheLockHolder(t *testing.T) {
	t.Parallel()

	const agent = "gastown/polecats/sbx-flash-1"
	townRoot := t.TempDir()
	holder, err := tryAcquireSlingAssigneeLock(townRoot, agent)
	if err != nil {
		t.Fatalf("holder lock acquire failed: %v", err)
	}
	defer holder()
	_, lockErr := tryAcquireSlingAssigneeLockWith(townRoot, agent, func(time.Duration) {})
	if lockErr == nil {
		t.Fatal("a lock held throughout must not be acquired")
	}

	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	f.seats = []specdispatch.Seat{{Agent: "sbx-flash", Cap: 1}}
	// Wrapped the way the engine returns it.
	f.slingErrs["gt-a"] = []error{fmt.Errorf("serializing hook write for %s: %w", agent, lockErr)}
	r := runSpecDispatchCycle(f.env())

	if len(r.Deferred) != 1 {
		t.Fatalf("deferred %+v, want one deferral", r.Deferred)
	}
	if want := "held by pid " + strconv.Itoa(os.Getpid()); !strings.Contains(r.Deferred[0].Line, want) {
		t.Errorf("deferral line = %q, want it to carry the holder (%s)", r.Deferred[0].Line, want)
	}
}

func writeHolderRecord(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing the holder record: %v", err)
	}
}

func assigneeHolderFiles(t *testing.T, townRoot string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(townRoot, ".runtime", "locks", "sling", "*.holder"))
	if err != nil {
		t.Fatalf("globbing holder records: %v", err)
	}
	return matches
}

func TestTryAcquireSlingAssigneeLock_AgentNameSanitization(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()

	// Agent names with slashes and colons should be sanitized for filesystem safety.
	agents := []string{
		"gastown/polecats/dementus",
		"rig:with:colons",
		"deacon/",
	}
	for _, agent := range agents {
		release, err := tryAcquireSlingAssigneeLock(townRoot, agent)
		if err != nil {
			t.Fatalf("lock acquire failed for agent %q: %v", agent, err)
		}
		release()
	}
}

func slingLockFiles(t *testing.T, townRoot string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(townRoot, ".runtime", "locks", "sling", "*.flock"))
	if err != nil {
		t.Fatalf("globbing sling lock files: %v", err)
	}
	return matches
}

// A released lock takes its sentinel file with it: one file per bead and per
// assignee ever slung is the unbounded growth gt-10u8 reports.
func TestSlingLocks_ReleaseRemovesSentinelFile(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()

	releaseBead, err := tryAcquireSlingBeadLock(townRoot, "gt-cleanup1")
	if err != nil {
		t.Fatalf("bead lock acquire failed: %v", err)
	}
	releaseAssignee, err := tryAcquireSlingAssigneeLock(townRoot, "gastown/polecats/cleanup")
	if err != nil {
		t.Fatalf("assignee lock acquire failed: %v", err)
	}
	if got := slingLockFiles(t, townRoot); len(got) != 2 {
		t.Fatalf("held locks left %d sentinel files, want 2: %v", len(got), got)
	}

	releaseBead()
	releaseAssignee()

	if got := slingLockFiles(t, townRoot); len(got) != 0 {
		t.Fatalf("released locks left sentinel files behind: %v", got)
	}
}

// The unlink on release must not let two slings hold one bead lock: the name
// goes while the flock is still held, so a waiter that opened the file before
// the unlink finds the name gone and reopens (gt-xtfnq; the reopen itself is
// pinned in internal/lock). Unlocking first would let a waiter lock the old
// inode and then lose its name to this unlink.
func TestUnlinkThenUnlock_RemovesTheNameBeforeTheLockDrops(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gt-race-unlink.flock")
	sidecar := strings.TrimSuffix(path, ".flock") + ".holder"
	for _, p := range []string{path, sidecar} {
		if err := os.WriteFile(p, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	unlocked := false
	unlinkThenUnlock(path, func() {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the lock dropped while its name still existed (stat err %v)", err)
		}
		// The holder record goes with the name, while the lock is still held,
		// so the sling that takes it next never reads who held it before.
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Errorf("the lock dropped while its holder record still existed (stat err %v)", err)
		}
		unlocked = true
	}, sidecar)()
	if !unlocked {
		t.Error("release never dropped the lock")
	}
}

// A sling killed before its release leaves its sentinel behind. The next sling
// sweeps it, but never a file whose lock a live sling still holds.
func TestSweepStaleSlingFlocks_RemovesOnlyUnheld(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gt-dead1.flock", "assignee_rig_polecats_dead.flock"} {
		if err := os.WriteFile(filepath.Join(lockDir, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Not a sentinel: the sweep leaves anything that is not a .flock file alone.
	other := filepath.Join(lockDir, "notes.txt")
	if err := os.WriteFile(other, nil, 0644); err != nil {
		t.Fatal(err)
	}

	held, err := tryAcquireSlingBeadLock(townRoot, "gt-live1")
	if err != nil {
		t.Fatalf("bead lock acquire failed: %v", err)
	}
	// tryAcquire sweeps on the way in, so the dead sentinels are already gone
	// and the live one must have survived that sweep.
	if got := slingLockFiles(t, townRoot); len(got) != 1 || filepath.Base(got[0]) != "gt-live1.flock" {
		t.Fatalf("after sweep, sentinel files = %v, want only gt-live1.flock", got)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("sweep removed a file that is not a .flock sentinel: %v", err)
	}

	// Sweeping again while the lock is held must not steal it.
	sweepStaleSlingFlocks(lockDir)
	if release, err := tryAcquireSlingBeadLock(townRoot, "gt-live1"); err == nil {
		release()
		t.Fatal("sweep freed a held lock: a second sling acquired the same bead")
	}

	held()
}

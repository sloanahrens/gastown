package slot

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// stubNoContainers makes runningGateContainers deterministic for tests that
// aren't exercising the docker-ps cross-check itself: the flock semantics
// tests below must not depend on (or be made flaky by) whatever container
// suites happen to be running on the host, including this bug's own
// premise — other rigs' Docker-backed suites running concurrently on a
// shared Gas Town host.
func stubNoContainers(t *testing.T) {
	t.Helper()
	orig := runningGateContainers
	runningGateContainers = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() { runningGateContainers = orig })
}

// TestMatchGateContainers covers the `docker ps` output parsing/matching
// logic in isolation: which lines count as a gate container, case
// sensitivity, and non-matching/empty input.
func TestMatchGateContainers(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "empty output",
			out:  "",
			want: nil,
		},
		{
			name: "no matching containers",
			out:  "nginx:latest my-web-server\npostgres:16 my-db\n",
			want: nil,
		},
		{
			name: "matches dolt image",
			out:  "dolt/dolt-sql-server:2.2.0 some-suite\n",
			want: []string{"dolt/dolt-sql-server:2.2.0 some-suite"},
		},
		{
			name: "matches testcontainers and ryuk, skips unrelated",
			out: "nginx:latest my-web-server\n" +
				"testcontainers/ryuk:0.5.1 reaper\n" +
				"some/testcontainers-postgres:1.0 pg-suite\n",
			want: []string{
				"testcontainers/ryuk:0.5.1 reaper",
				"some/testcontainers-postgres:1.0 pg-suite",
			},
		},
		{
			name: "matches regardless of case",
			out:  "MyRegistry/DOLT-Server:latest CONTAINER-NAME\n",
			want: []string{"MyRegistry/DOLT-Server:latest CONTAINER-NAME"},
		},
		{
			name: "trims surrounding whitespace and blank lines",
			out:  "\n\ndolt/dolt-sql-server:2.2.0 suite\n\n",
			want: []string{"dolt/dolt-sql-server:2.2.0 suite"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchGateContainers(tt.out)
			if len(got) != len(tt.want) {
				t.Fatalf("matchGateContainers(%q) = %v, want %v", tt.out, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("matchGateContainers(%q)[%d] = %q, want %q", tt.out, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestAcquireReleaseStatus(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	rep, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status before acquire: %v", err)
	}
	if rep.Busy() {
		t.Fatalf("Status reported busy before any Acquire: %+v", rep)
	}

	h, err := Acquire(townRoot, "test-role", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	rep, err = Status(townRoot)
	if err != nil {
		t.Fatalf("Status after acquire: %v", err)
	}
	if !rep.Held {
		t.Fatalf("Status reported not held while held")
	}
	if rep.Owner == nil || rep.Owner.Role != "test-role" || rep.Owner.PID != os.Getpid() {
		t.Fatalf("owner metadata wrong: %+v", rep.Owner)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rep, err = Status(townRoot)
	if err != nil {
		t.Fatalf("Status after release: %v", err)
	}
	if rep.Busy() {
		t.Fatalf("Status reported busy after Release: %+v", rep)
	}

	if _, err := os.Stat(OwnerPath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("owner file should be removed after Release, stat err=%v", err)
	}
}

// TestAcquire_MutualExclusion asserts the slot cannot be double-held: a
// second Acquire on an already-held slot must time out, and the exact wait
// window is a count (elapsed >= timeout), not merely "eventually returns an
// error" — a timeout implementation that returns immediately would also
// satisfy a looser assertion without actually enforcing exclusion.
func TestAcquire_MutualExclusion(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer h.Release()

	timeout := DefaultPollInterval + 500*time.Millisecond
	start := time.Now()
	_, err = Acquire(townRoot, "waiter", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("second Acquire on a held slot succeeded; mutual exclusion broken")
	}
	if elapsed < timeout {
		t.Fatalf("second Acquire returned after %s, before its %s timeout elapsed — it did not actually wait/retry", elapsed, timeout)
	}
}

// TestAcquire_ReleaseUnblocksWaiter is the mutual-exclusion test's
// complement: it proves a released slot is genuinely acquirable again, not
// just that a held one is unavailable.
func TestAcquire_ReleaseUnblocksWaiter(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		h2, err := Acquire(townRoot, "waiter", 5*time.Second)
		if err != nil {
			done <- err
			return
		}
		done <- h2.Release()
	}()

	// Give the waiter a couple of poll cycles to observe the held lock
	// before releasing, so this actually exercises the wait path.
	time.Sleep(DefaultPollInterval + 200*time.Millisecond)
	if err := h.Release(); err != nil {
		t.Fatalf("releasing holder: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter Acquire/Release after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the slot after it was released")
	}
}

// TestAcquire_KernelReleasesOnProcessDeath is the adversarial check for the
// mechanism this package exists to provide: the whole point of using
// flock(2) instead of a hand-rolled pid/mkdir lock (see the gt-bcsq
// discussion of v1-v2.3) is that the KERNEL releases the lock when the
// holding process dies by any means, including SIGKILL, with no reclaim
// logic to get wrong. A regression that swapped the real lock file for e.g.
// a plain "does this path exist" check would still pass every
// acquire/release/status assertion above yet fail this one, because nothing
// would ever remove the file after a SIGKILL.
func TestAcquire_KernelReleasesOnProcessDeath(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()
	lockPath := LockPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		t.Fatal(err)
	}

	bin, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}

	cmd := exec.Command(bin, "-test.run=TestHelperHoldSlotUntilKilled")
	cmd.Env = append(os.Environ(), "GT_SLOT_HELPER=1", "GT_SLOT_TOWN_ROOT="+townRoot)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper process: %v", err)
	}

	// Wait for the helper to finish its grant before killing it: the flock
	// held AND the grant's history entry written. Status().Held reads the
	// flock alone, which the grant takes before it writes the owner file and
	// the history entry (see grant in AcquirePool). Killing on Held alone
	// raced that write: under load the SIGKILL landed first and the entry
	// never existed ("the SIGKILLed holder left no history entry", gt-6920e).
	// Waiting for the entry keeps what this test proves — the helper never
	// releases, so its entry can only be the grant-time record, and it must
	// survive the kill with its hold left open. The deadline stays under the
	// helper's 30s sleep and only costs time when the helper is truly stuck.
	deadline := time.Now().Add(20 * time.Second)
	for {
		rep, err := Status(townRoot)
		if err != nil {
			t.Fatalf("Status while waiting for helper: %v", err)
		}
		if rep.Held && helperGrantRecorded(t, townRoot) {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("helper process never acquired the slot and recorded its grant (held=%v)", rep.Held)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := cmd.Process.Kill(); err != nil { // SIGKILL — no cleanup handlers run
		t.Fatalf("killing helper: %v", err)
	}
	_ = cmd.Wait()

	// The kernel must release the flock the instant the process's file
	// descriptors close, with no timeout or reclaim step required.
	// 30s only bounds a broken run: the kernel frees the flock at once, so a
	// healthy Acquire returns immediately however loaded the host is.
	h2, err := Acquire(townRoot, "post-kill", 30*time.Second)
	if err != nil {
		t.Fatalf("slot still held after SIGKILLing the holder: %v", err)
	}
	_ = h2.Release()

	// The killed holder's acquisition is still in the ring file, with its hold
	// left open: the record is written at grant time for exactly this case, so
	// a suite that dies mid-run is accounted for rather than lost with the
	// process (gt-dc81).
	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History after the holder was killed: %v", err)
	}
	var killed *HistoryEntry
	for i := range history {
		if history[i].Role == "helper" {
			killed = &history[i]
		}
	}
	if killed == nil {
		t.Fatalf("the SIGKILLed holder left no history entry: %+v", history)
	}
	if killed.HeldS != nil {
		t.Errorf("the killed holder's hold was closed by a Release that never ran: %+v", killed)
	}
}

// helperGrantRecorded reports whether the history ring holds the helper's
// grant-time entry. History reads under the ring's own lock, so a partial
// write is never observed.
func helperGrantRecorded(t *testing.T, townRoot string) bool {
	t.Helper()
	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History while waiting for helper: %v", err)
	}
	for _, e := range history {
		if e.Role == "helper" {
			return true
		}
	}
	return false
}

// TestHelperHoldSlotUntilKilled is not a real test; it is spawned as a
// subprocess by TestAcquire_KernelReleasesOnProcessDeath to hold the slot
// until SIGKILLed, with no Release() call in its shutdown path. It stubs
// runningGateContainers itself since it runs in a separate process from the
// parent test and doesn't inherit the parent's var override.
func TestHelperHoldSlotUntilKilled(t *testing.T) {
	if os.Getenv("GT_SLOT_HELPER") != "1" {
		t.Skip("not invoked as slot-holder helper")
	}
	stubNoContainers(t)
	townRoot := os.Getenv("GT_SLOT_TOWN_ROOT")
	// The parent's Status polls probe the same flock, so a try can lose to a
	// probe and wait a poll interval; 15s only costs time if truly stuck.
	if _, err := Acquire(townRoot, "helper", 15*time.Second); err != nil {
		t.Fatalf("helper failed to acquire: %v", err)
	}
	time.Sleep(30 * time.Second) // outlived by the parent's SIGKILL
}

// TestStatus_ReportsUnwrappedContainers is the regression test for gt-tuiy:
// a container-backed suite that never went through gt slot run leaves the
// flock free, but Status must still surface it as busy rather than
// reporting free.
func TestStatus_ReportsUnwrappedContainers(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return []string{"dolt/dolt-sql-server:2.2.0 unwrapped-suite-1"}, nil
	}

	rep, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.Held {
		t.Fatalf("Status reported held with no flock holder: %+v", rep)
	}
	if !rep.Busy() {
		t.Fatalf("Status reported free while an unwrapped container suite is running: %+v", rep)
	}
	if len(rep.UnwrappedContainers) != 1 {
		t.Fatalf("UnwrappedContainers = %v, want 1 entry", rep.UnwrappedContainers)
	}
}

// TestStatus_DockerUnreachableIsNotFree is the other half of gt-tuiy's
// deliverable: when the docker daemon can't be reached, Status must report
// unknown/busy, never free — a "free" reading on an unverifiable host is
// what let two container suites collide in the first place.
func TestStatus_DockerUnreachableIsNotFree(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}

	rep, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !rep.DockerUnknown {
		t.Fatalf("DockerUnknown = false, want true when docker is unreachable: %+v", rep)
	}
	if !rep.Busy() {
		t.Fatalf("Status reported free while docker is unreachable (should be unknown/busy): %+v", rep)
	}
}

// TestStatus_HeldContainersAreNotUnwrapped ensures a holder's own
// containers (found while the flock IS held) are never mislabeled as an
// "unwrapped" suite — that label is reserved for containers running
// without a token holder. The holder acquires while the check is clear
// (mirroring Acquire's own requirement that nothing be running yet), then
// starts its containers, exactly as a real gt slot run holder does.
func TestStatus_HeldContainersAreNotUnwrapped(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) { return nil, nil }

	h, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()

	// The holder's suite now starts its own containers.
	runningGateContainers = func() ([]string, error) {
		return []string{"dolt/dolt-sql-server:2.2.0 holders-own-container"}, nil
	}

	rep, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !rep.Held {
		t.Fatalf("Status reported not held: %+v", rep)
	}
	if len(rep.UnwrappedContainers) != 0 {
		t.Fatalf("UnwrappedContainers = %v, want empty — these are the holder's own containers", rep.UnwrappedContainers)
	}
}

// TestAcquire_WaitsForUnwrappedContainersToClear proves the flock alone
// does not make the slot available: Acquire must keep waiting while an
// unwrapped suite's containers are up even though the flock itself is
// free, and succeed once they clear.
func TestAcquire_WaitsForUnwrappedContainersToClear(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()

	var containersUp atomic.Bool
	containersUp.Store(true)
	runningGateContainers = func() ([]string, error) {
		if containersUp.Load() {
			return []string{"testcontainers/ryuk:0.5.1 unwrapped-reaper"}, nil
		}
		return nil, nil
	}

	done := make(chan error, 1)
	go func() {
		h, err := Acquire(townRoot, "waiter", 5*time.Second)
		if err != nil {
			done <- err
			return
		}
		done <- h.Release()
	}()

	// Give the waiter a couple of poll cycles to observe containers up
	// before clearing them, so this actually exercises the wait path.
	time.Sleep(DefaultPollInterval + 200*time.Millisecond)
	containersUp.Store(false)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Acquire after containers cleared: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire never succeeded after unwrapped containers cleared")
	}
}

// TestAcquire_ProceedsOnFlockWhenDockerUnreachable is the regression test
// for gt-tuiy attempt 2's finding: an unreachable docker daemon must not
// block Acquire. Treating it as an unverifiable "busy" (the attempt-1
// behavior) turned a routinely-stopped Docker Desktop into a town-wide gate
// outage for every suite, including ones that never touch Docker. Since an
// unreachable daemon cannot be running any containers either, Acquire must
// proceed on the flock alone, quickly, rather than failing or waiting.
func TestAcquire_ProceedsOnFlockWhenDockerUnreachable(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}

	timeout := 30 * time.Second
	start := time.Now()
	h, err := Acquire(townRoot, "waiter", timeout)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Acquire failed while docker was unreachable: %v", err)
	}
	defer h.Release()
	if elapsed > 2*DefaultPollInterval {
		t.Fatalf("Acquire took %s — expected it to proceed on the first check, not after polling", elapsed)
	}
}

// TestAcquire_DoesNotProceedOnWedgedDockerDaemon is the regression test for
// om-editorial attempt 3's minor finding: "unreachable daemon ⇒ no
// containers" only holds when the daemon itself refused the connection. A
// docker ps call that times out (the daemon is wedged, not absent) or fails
// for some other reason (e.g. a permission-denied socket) says nothing
// about whether containers are running, so Acquire must treat it like a
// real unwrapped container — release and keep waiting — never as a green
// light to hand out the slot.
func TestAcquire_DoesNotProceedOnWedgedDockerDaemon(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	probeErr := errors.New("docker ps did not respond within 5s: context deadline exceeded")
	runningGateContainers = func() ([]string, error) {
		return nil, probeErr
	}

	origWriter := probeWriter
	buf := &strings.Builder{}
	probeWriter = buf
	t.Cleanup(func() { probeWriter = origWriter })

	timeout := DefaultPollInterval + 500*time.Millisecond
	start := time.Now()
	_, err := Acquire(townRoot, "waiter", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Acquire succeeded while docker ps was timing out (wedged daemon) — should be inconclusive, not treated as free")
	}
	if elapsed < timeout {
		t.Fatalf("Acquire returned after %s, before its %s timeout — it treated the wedged-daemon error as a green light instead of waiting", elapsed, timeout)
	}

	// gt-a8kx: the error Acquire finally returns names the timeout and nothing
	// else, and the caller's budget can be an hour (runMQBatchRun), so the
	// reason for the wait has to be on the record — once, not once per poll.
	got := buf.String()
	if n := strings.Count(got, "docker ps check inconclusive"); n != 1 {
		t.Fatalf("inconclusive-probe diagnostic printed %d time(s), want exactly 1: %q", n, got)
	}
	if !strings.Contains(got, probeErr.Error()) {
		t.Fatalf("diagnostic %q does not carry the underlying probe error %q", got, probeErr)
	}
}

// TestIsDaemonUnreachable pins the exact classification isDaemonUnreachable
// draws: only the docker CLI's own "cannot connect to the docker daemon"
// wording (covers both "not running" and "connection refused") counts as
// verified-empty; a timeout or a permission-denied socket does not.
func TestIsDaemonUnreachable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"daemon not running", errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"), true},
		{"mixed case", errors.New("cannot CONNECT to the DOCKER daemon"), true},
		{"wedged/timeout", errors.New("docker ps did not respond within 5s: context deadline exceeded"), false},
		{"permission denied", errors.New("docker ps failed: permission denied while trying to connect to the Docker daemon socket"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDaemonUnreachable(tt.err); got != tt.want {
				t.Errorf("isDaemonUnreachable(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestAcquire_SameProcessSecondCallStillContends guards the boundary of the
// reentrant fast path: two Acquire calls from the *same* process/PID (no
// subprocess involved) must NOT take the reentrant shortcut — that would
// silently break mutual exclusion for any genuinely concurrent, unrelated
// caller that happens to share a process. Only a real descendant process
// (proven by TestAcquire_ReentrantChildProcessSkipsFlock below) qualifies.
func TestAcquire_SameProcessSecondCallStillContends(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer h.Release()

	timeout := DefaultPollInterval + 500*time.Millisecond
	start := time.Now()
	_, err = Acquire(townRoot, "same-process-caller", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("second same-process Acquire succeeded — reentrant fast path wrongly applied to an unrelated caller")
	}
	if elapsed < timeout {
		t.Fatalf("second Acquire returned after %s, before its %s timeout — it took the reentrant shortcut instead of really contending", elapsed, timeout)
	}
}

// TestAcquire_ReentrantChildProcessSkipsFlock proves the deadlock fix: a
// child process spawned while this process holds the slot inherits the
// parent's ReentrantEnvVar marker (as `gt slot run` does by default, and as
// a Go call path like runMQBatchRun's spawned formula-driven subprocess
// would) and takes the reentrant fast path instead of blocking on a flock
// its own ancestor is still holding — the crux of the runMQBatchRun +
// formula-wrap nesting gt-tuiy identified. The child names the SAME role as
// the holder, which is what scopes the fast path to the holder's own work
// (gt-off9, see TestAcquire_MarkerRoleScopesTheFastPath for the other half).
func TestAcquire_ReentrantChildProcessSkipsFlock(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()
	const role = "gastown/coral"

	h, err := Acquire(townRoot, role, time.Second)
	if err != nil {
		t.Fatalf("ancestor Acquire: %v", err)
	}
	defer h.Release()

	spawnSlotChild(t, townRoot, role, "fast-path")

	// The ancestor's real hold must be unaffected by the child's Release.
	rep, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !rep.Held {
		t.Fatalf("Status reported not held after child's reentrant Release — reentrant Release must not touch the real lock: %+v", rep)
	}
}

// TestAcquire_MarkerRoleScopesTheFastPath is the regression test for
// gt-off9. A marker is copied into a child's environment at fork(2) and the
// holder can never clear it afterwards, so a holder that arms one hands it
// to everything it spawns for the rest of those processes' lives. Presence
// alone therefore exempted unrelated work from the lock: while the daemon's
// main_branch_test hold was up, the agent sessions it spawned carried the
// marker, and the refinery gates and `gt done` verify suites descending
// from them ran invisibly beside the very suite they must serialize
// against. The marker now names the holder's role, and only that role's
// work skips the lock — everyone else contends and queues.
func TestAcquire_MarkerRoleScopesTheFastPath(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()
	// The role the daemon's main_branch_test runner acquires under, and the
	// roles of the two callers gt-off9 named as victims.
	const (
		daemonRole   = "gastown/main-branch-test"
		refineryRole = "gastown/refinery"
	)

	h, err := Acquire(townRoot, daemonRole, time.Second)
	if err != nil {
		t.Fatalf("daemon-role Acquire: %v", err)
	}

	// The holder's own nested work keeps its fast path: gt-tuiy's deadlock
	// fix has to survive this change, or a suite that spawns a nested
	// `gt slot run` of its own deadlocks against its ancestor's flock.
	spawnSlotChild(t, townRoot, daemonRole, "fast-path")

	// A refinery gate descending from the hold is different work. It must
	// wait for the real lock rather than run alongside the daemon's suite.
	spawnSlotChild(t, townRoot, refineryRole, "contends")

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// ...and the queue drains as soon as the hold ends.
	spawnSlotChild(t, townRoot, refineryRole, "acquires")
}

// spawnSlotChild runs the role-scoped helper below in a fresh process,
// inheriting this process's environment — including whatever ReentrantEnvVar
// the parent's own Acquire armed. A real fork, not a simulated one, so the
// marker travels exactly the way it does in production.
func spawnSlotChild(t *testing.T, townRoot, role, expect string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}
	cmd := exec.Command(bin, "-test.run=TestHelperRoleScopedAcquire")
	cmd.Env = append(os.Environ(),
		"GT_SLOT_TOWN_ROOT="+townRoot,
		"GT_SLOT_CHILD_ROLE="+role,
		"GT_SLOT_CHILD_EXPECT="+expect,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("slot child (role=%s, expect=%s) failed: %v\n%s", role, expect, err, out)
	}
}

// TestHelperRoleScopedAcquire is not a real test; it is spawned as a
// subprocess by the reentrant tests above. It acquires the slot under
// GT_SLOT_CHILD_ROLE, inheriting whatever marker its parent was carrying,
// and asserts the outcome GT_SLOT_CHILD_EXPECT calls for:
//
//   - "fast-path": the marker names this role, so Acquire must return
//     near-instantly without touching the ancestor's flock.
//   - "contends": the marker names different work, so Acquire must block on
//     that flock for its full timeout and fail rather than slipping past
//     the hold.
//   - "acquires": nothing holds the slot any more, so Acquire must take the
//     real lock.
func TestHelperRoleScopedAcquire(t *testing.T) {
	role := os.Getenv("GT_SLOT_CHILD_ROLE")
	if role == "" {
		t.Skip("not invoked as a slot-child helper")
	}
	stubNoContainers(t)
	townRoot := os.Getenv("GT_SLOT_TOWN_ROOT")
	timeout := DefaultPollInterval + 500*time.Millisecond

	start := time.Now()
	h, err := Acquire(townRoot, role, timeout)
	elapsed := time.Since(start)

	switch expect := os.Getenv("GT_SLOT_CHILD_EXPECT"); expect {
	case "fast-path":
		if err != nil {
			t.Fatalf("Acquire(%q) under a same-role marker: %v", role, err)
		}
		if elapsed > time.Second {
			t.Fatalf("Acquire(%q) took %s — expected the near-instant reentrant fast path, not a poll/wait", role, elapsed)
		}
		if err := h.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
	case "contends":
		if err == nil {
			t.Fatalf("Acquire(%q) succeeded while an ancestor held the slot — it rode the reentrant fast path with a foreign role instead of queueing", role)
		}
		if elapsed < timeout {
			t.Fatalf("Acquire(%q) returned after %s, before its %s timeout — it did not actually contend for the lock", role, elapsed, timeout)
		}
	case "acquires":
		if err != nil {
			t.Fatalf("Acquire(%q) after the holder released: %v", role, err)
		}
		if err := h.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
	default:
		t.Fatalf("unknown GT_SLOT_CHILD_EXPECT %q", expect)
	}
}

// TestReentrantMarkGrants pins the marker-reading rules the fast path is
// built on (gt-off9): an ancestor's marker exempts a caller doing the same
// role's work, a legacy marker written before roles were recorded exempts
// anyone (it cannot be role-checked, and making it contend would deadlock
// against the very ancestor that wrote it), and everything else contends.
func TestReentrantMarkGrants(t *testing.T) {
	townRoot := t.TempDir()
	foreignPID := strconv.Itoa(os.Getpid() + 100000)
	lockPath := LockPath(townRoot)

	tests := []struct {
		name  string
		value string
		role  string
		want  bool
	}{
		{"same role", lockPath + "|" + foreignPID + "|gastown/refinery", "gastown/refinery", true},
		{"different role", lockPath + "|" + foreignPID + "|gastown/main-branch-test", "gastown/refinery", false},
		{"legacy marker without a role", lockPath + "|" + foreignPID, "gastown/refinery", true},
		{"own pid is a sibling, not an ancestor", lockPath + "|" + strconv.Itoa(os.Getpid()) + "|gastown/refinery", "gastown/refinery", false},
		{"another town's lock", LockPath(t.TempDir()) + "|" + foreignPID + "|gastown/refinery", "gastown/refinery", false},
		{"not a slot lock path", filepath.Join(LockDir(townRoot), "notes.txt") + "|" + foreignPID + "|gastown/refinery", "gastown/refinery", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, ok := parseReentrantMark(tt.value)
			if !ok {
				t.Fatalf("parseReentrantMark(%q) failed", tt.value)
			}
			if got := m.grants(townRoot, tt.role); got != tt.want {
				t.Errorf("grants(%q, role=%q) = %v, want %v", tt.value, tt.role, got, tt.want)
			}
		})
	}

	for _, bad := range []string{"", "no-separator", lockPath + "|not-a-pid", lockPath + "|not-a-pid|gastown/refinery"} {
		if _, ok := parseReentrantMark(bad); ok {
			t.Errorf("parseReentrantMark(%q) parsed, want failure", bad)
		}
	}
}

// TestInheritedRole pins what a nested wrap gets back when it asks "what
// role am I nested under" instead of guessing (gt-cet2): the whole point is
// letting a caller that omits --role ride its true ancestor's hold rather
// than a per-invocation placeholder that guarantees a mismatch and
// reintroduces the gt-tuiy deadlock class.
func TestInheritedRole(t *testing.T) {
	townRoot := t.TempDir()
	foreignPID := strconv.Itoa(os.Getpid() + 100000)
	lockPath := LockPath(townRoot)

	tests := []struct {
		name     string
		envValue string
		wantRole string
		wantOK   bool
	}{
		{"genuine ancestor hold", lockPath + "|" + foreignPID + "|gastown/refinery-batch", "gastown/refinery-batch", true},
		{"no marker at all", "", "", false},
		{"legacy marker has no role to hand back", lockPath + "|" + foreignPID, "", false},
		{"own pid is a sibling, not an ancestor", lockPath + "|" + strconv.Itoa(os.Getpid()) + "|gastown/refinery", "", false},
		{"another town's lock", LockPath(t.TempDir()) + "|" + foreignPID + "|gastown/refinery", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(ReentrantEnvVar, tt.envValue)
			role, ok := InheritedRole(townRoot)
			if ok != tt.wantOK || role != tt.wantRole {
				t.Errorf("InheritedRole() = (%q, %v), want (%q, %v)", role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

// TestAcquire_TimesOutWhileUnwrappedContainersPersist is the timeout-side
// complement: Acquire must not succeed (or silently hand out the slot)
// while unwrapped containers never clear.
func TestAcquire_TimesOutWhileUnwrappedContainersPersist(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return []string{"dolt/dolt-sql-server:2.2.0 stuck-suite"}, nil
	}

	timeout := DefaultPollInterval + 500*time.Millisecond
	start := time.Now()
	_, err := Acquire(townRoot, "waiter", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Acquire succeeded while unwrapped containers never cleared")
	}
	if elapsed < timeout {
		t.Fatalf("Acquire returned after %s, before its %s timeout elapsed", elapsed, timeout)
	}

	// Giving up is recorded (gt-dc81): a caller that times out behind an
	// unwrapped suite is the strongest evidence of a constricting gate, and the
	// one case a history of successful acquisitions alone would drop.
	history, err := History(townRoot)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history holds %d entries for a timed-out wait, want 1: %+v", len(history), history)
	}
	if !history[0].TimedOut {
		t.Errorf("the timed-out wait is not marked timed_out: %+v", history[0])
	}
	if history[0].Reason != WaitReasonUnwrappedContainers {
		t.Errorf("reason = %q, want %q", history[0].Reason, WaitReasonUnwrappedContainers)
	}
	if history[0].HeldS != nil {
		t.Errorf("a slot nobody held has held_s set: %+v", history[0])
	}

	waits := slotEventsOfType(t, townRoot, events.TypeSlotWait)
	if len(waits) != 1 {
		t.Fatalf("slot_wait events = %d, want the timed-out wait reported", len(waits))
	}
	if got := waits[0].Payload["outcome"]; got != "timeout" {
		t.Errorf("slot_wait outcome = %v, want timeout", got)
	}
	msg, _ := waits[0].Payload["message"].(string)
	for _, want := range []string{"gave up", "cap"} {
		if !strings.Contains(msg, want) {
			t.Errorf("slot_wait message = %q, want it to say the caller gave up against its cap", msg)
		}
	}
}

// TestAcquire_ProceedsDespiteOrphanContainer is the regression test for
// gt-ul1k: an hours-old container with no ryuk reaper left nobody to wait for,
// but Acquire held the flock back anyway, and every wrapped suite town-wide
// queued behind it until someone removed the container by hand.
func TestAcquire_ProceedsDespiteOrphanContainer(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return []string{dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg",
			time.Now().Add(-5*time.Hour), nil)}, nil
	}

	start := time.Now()
	h, err := Acquire(townRoot, "waiter", 30*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Acquire failed while only an orphan container was running: %v", err)
	}
	defer h.Release()
	if elapsed > 2*DefaultPollInterval {
		t.Fatalf("Acquire took %s — expected it to walk past the orphan on the first check", elapsed)
	}
}

// TestAcquire_StillWaitsForAYoungContainer is the other side of the verdict:
// a container young enough to belong to a suite still running must keep
// holding the gate, or the town would admit two suites into one Docker VM.
func TestAcquire_StillWaitsForAYoungContainer(t *testing.T) {
	townRoot := t.TempDir()

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return []string{dockerPSLine("live-id", "dolt/dolt-sql-server:2.2.0", "running-suite",
			time.Now().Add(-2*time.Minute), nil)}, nil
	}

	timeout := DefaultPollInterval + 500*time.Millisecond
	start := time.Now()
	_, err := Acquire(townRoot, "waiter", timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Acquire succeeded while a young container suite was running")
	}
	if elapsed < timeout {
		t.Fatalf("Acquire returned after %s, before its %s timeout elapsed", elapsed, timeout)
	}
}

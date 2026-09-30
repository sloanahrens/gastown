package slot

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// TestMatchGateContainers covers the `docker ps` output parsing/matching
// logic in isolation: which lines count as a gate container, case
// sensitivity, and non-matching/empty input.
func TestMatchGateContainers(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()

	rep, err := tg.Status(townRoot)
	if err != nil {
		t.Fatalf("Status before acquire: %v", err)
	}
	if rep.Busy() {
		t.Fatalf("Status reported busy before any Acquire: %+v", rep)
	}

	h, err, _ := tg.run(t, func() (*Handle, error) { return tg.Acquire(townRoot, "test-role", time.Second) })
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	rep, err = tg.Status(townRoot)
	if err != nil {
		t.Fatalf("Status after acquire: %v", err)
	}
	if !rep.Held {
		t.Fatalf("Status reported not held while held")
	}
	if rep.Owner == nil || rep.Owner.Role != "test-role" || rep.Owner.PID != os.Getpid() {
		t.Fatalf("owner metadata wrong: %+v", rep.Owner)
	}

	release(t, h)

	rep, err = tg.Status(townRoot)
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
// satisfy a looser assertion without actually enforcing exclusion. Both calls
// come from one process, so this is also the boundary of the reentrant fast
// path: the first call's marker names this process, which makes it a sibling
// that must contend, not an ancestor whose hold it may ride.
func TestAcquire_MutualExclusion(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()

	h := tg.mustAcquirePool(t, townRoot, "holder", DefaultPool)
	defer release(t, h)

	timeout := tg.pollInterval + 500*time.Millisecond
	_, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.Acquire(townRoot, "waiter", timeout) })

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
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()

	h := tg.mustAcquirePool(t, townRoot, "holder", DefaultPool)

	done := goAcquire(func() (*Handle, error) { return tg.Acquire(townRoot, "waiter", 5*time.Second) })

	// The waiter has observed the held lock and is waiting out a poll, so
	// this exercises the wait path.
	waitBlocked(t, tg.clk)
	release(t, h)

	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("waiter Acquire after release: %v", got.err)
	}
	release(t, got.h)
}

// TestStatus_ReportsUnwrappedContainers is the regression test for gt-tuiy:
// a container-backed suite that never went through gt slot run leaves the
// flock free, but Status must still surface it as busy rather than
// reporting free.
func TestStatus_ReportsUnwrappedContainers(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 unwrapped-suite-1")

	rep, err := tg.Status(t.TempDir())
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
	t.Parallel()
	tg := newTestGate(t)
	tg.rt.failList(errors.New("Cannot connect to the Docker daemon"))

	rep, err := tg.Status(t.TempDir())
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
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()

	h := tg.mustAcquirePool(t, townRoot, "holder", DefaultPool)
	defer release(t, h)

	// The holder's suite now starts its own containers.
	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 holders-own-container")

	rep, err := tg.Status(townRoot)
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

// TestAcquire_ProceedsOnFlockWhenDockerUnreachable is the regression test
// for gt-tuiy attempt 2's finding: an unreachable docker daemon must not
// block Acquire. Treating it as an unverifiable "busy" (the attempt-1
// behavior) turned a routinely-stopped Docker Desktop into a town-wide gate
// outage for every suite, including ones that never touch Docker. Since an
// unreachable daemon cannot be running any containers either, Acquire must
// proceed on the flock alone, at once, rather than failing or waiting.
func TestAcquire_ProceedsOnFlockWhenDockerUnreachable(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	tg.rt.failList(errors.New("Cannot connect to the Docker daemon"))

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.Acquire(t.TempDir(), "waiter", 30*time.Second) })
	if err != nil {
		t.Fatalf("Acquire failed while docker was unreachable: %v", err)
	}
	defer release(t, h)
	if elapsed != 0 {
		t.Fatalf("Acquire waited %s — expected it to proceed on the first check, not after polling", elapsed)
	}
	if !strings.Contains(tg.probe.String(), "proceeding on flock alone") {
		t.Errorf("probe output = %q, want the unreachable daemon reported", tg.probe.String())
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
	t.Parallel()
	tg := newTestGate(t)
	probeErr := errors.New("docker ps did not respond within 5s: context deadline exceeded")
	tg.rt.failList(probeErr)

	timeout := tg.pollInterval + 500*time.Millisecond
	_, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.Acquire(t.TempDir(), "waiter", timeout) })

	if err == nil {
		t.Fatalf("Acquire succeeded while docker ps was timing out (wedged daemon) — should be inconclusive, not treated as free")
	}
	if elapsed < timeout {
		t.Fatalf("Acquire returned after %s, before its %s timeout — it treated the wedged-daemon error as a green light instead of waiting", elapsed, timeout)
	}

	// gt-18zj: the timeout error carries the probe failure the wait was
	// attributed to, so the failure alone says why the slot never came free.
	if !strings.Contains(err.Error(), probeErr.Error()) {
		t.Errorf("timeout error = %q, want the inconclusive probe that blocked the wait", err)
	}

	// gt-a8kx: the caller's budget can be an hour (runMQBatchRun), so the
	// reason for the wait has to be on the record as soon as it blocks — once,
	// not once per poll.
	got := tg.probe.String()
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
	t.Parallel()
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
//
// The child here is a gate with the environment a forked child inherits and
// a pid of its own; TestIntegrationAcquire_ReentrantChildProcessSkipsFlock
// checks the marker survives a real fork.
func TestAcquire_MarkerRoleScopesTheFastPath(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()
	// The role the daemon's main_branch_test runner acquires under, and the
	// roles of the two callers gt-off9 named as victims.
	const (
		daemonRole   = "gastown/main-branch-test"
		refineryRole = "gastown/refinery"
	)

	h := tg.mustAcquirePool(t, townRoot, daemonRole, DefaultPool)
	child := tg.child()
	timeout := tg.pollInterval + 500*time.Millisecond

	// The holder's own nested work keeps its fast path: gt-tuiy's deadlock
	// fix has to survive this change, or a suite that spawns a nested
	// `gt slot run` of its own deadlocks against its ancestor's flock.
	nested, err, elapsed := tg.run(t, func() (*Handle, error) { return child.Acquire(townRoot, daemonRole, timeout) })
	if err != nil {
		t.Fatalf("Acquire(%q) under a same-role marker: %v", daemonRole, err)
	}
	if !nested.reentrant || elapsed != 0 {
		t.Fatalf("Acquire(%q) under a same-role marker: reentrant=%v after %s, want the immediate reentrant fast path", daemonRole, nested.reentrant, elapsed)
	}
	release(t, nested)

	// A refinery gate descending from the hold is different work. It must
	// wait for the real lock rather than run alongside the daemon's suite.
	_, err, elapsed = tg.run(t, func() (*Handle, error) { return child.Acquire(townRoot, refineryRole, timeout) })
	if err == nil {
		t.Fatalf("Acquire(%q) succeeded while an ancestor held the slot — it rode the reentrant fast path with a foreign role instead of queueing", refineryRole)
	}
	if elapsed < timeout {
		t.Fatalf("Acquire(%q) returned after %s, before its %s timeout — it did not actually contend for the lock", refineryRole, elapsed, timeout)
	}

	// The ancestor's real hold is unaffected by the reentrant Release.
	rep, err := tg.Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !rep.Held {
		t.Fatalf("Status reported not held after the child's reentrant Release — reentrant Release must not touch the real lock: %+v", rep)
	}

	release(t, h)
	// ...and the queue drains as soon as the hold ends.
	gate, err, _ := tg.run(t, func() (*Handle, error) { return child.Acquire(townRoot, refineryRole, timeout) })
	if err != nil {
		t.Fatalf("Acquire(%q) after the holder released: %v", refineryRole, err)
	}
	if gate.reentrant {
		t.Fatalf("Acquire(%q) after the holder released took the fast path, want the real lock", refineryRole)
	}
}

// TestReentrantMarkGrants pins the marker-reading rules the fast path is
// built on (gt-off9): an ancestor's marker exempts a caller doing the same
// role's work, a legacy marker written before roles were recorded exempts
// anyone (it cannot be role-checked, and making it contend would deadlock
// against the very ancestor that wrote it), and everything else contends.
func TestReentrantMarkGrants(t *testing.T) {
	t.Parallel()
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
			if got := m.grants(townRoot, tt.role, os.Getpid()); got != tt.want {
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
	t.Parallel()
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
			t.Parallel()
			tg := newTestGate(t)
			tg.env.Setenv(ReentrantEnvVar, tt.envValue)
			role, ok := tg.InheritedRole(townRoot)
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
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()
	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 stuck-suite")

	timeout := tg.pollInterval + 500*time.Millisecond
	_, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.Acquire(townRoot, "waiter", timeout) })

	if err == nil {
		t.Fatalf("Acquire succeeded while unwrapped containers never cleared")
	}
	if elapsed < timeout {
		t.Fatalf("Acquire returned after %s, before its %s timeout elapsed", elapsed, timeout)
	}
	// The failure names the suite it queued behind, so its first reader does
	// not have to run `gt slot status` to learn what held the slot (gt-18zj).
	if !strings.Contains(err.Error(), "dolt/dolt-sql-server:2.2.0 stuck-suite") {
		t.Errorf("timeout error = %q, want the unwrapped suite it waited behind", err)
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

// TestAcquire_StillWaitsForAYoungContainer is the other side of the gt-ul1k
// verdict: a container young enough to belong to a suite still running must
// keep holding the gate, or the town would admit two suites into one Docker
// VM.
func TestAcquire_StillWaitsForAYoungContainer(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	tg.rt.setLines(dockerPSLine("live-id", "dolt/dolt-sql-server:2.2.0", "running-suite",
		tg.clk.Now().Add(-2*time.Minute), nil))

	timeout := tg.pollInterval + 500*time.Millisecond
	_, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.Acquire(t.TempDir(), "waiter", timeout) })
	if err == nil {
		t.Fatal("Acquire succeeded while a young container suite was running")
	}
	if elapsed < timeout {
		t.Fatalf("Acquire returned after %s, before its %s timeout elapsed", elapsed, timeout)
	}
}

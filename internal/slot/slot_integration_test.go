//go:build integration

package slot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// integrationPoll keeps the real-clock waits below short.
const integrationPoll = 50 * time.Millisecond

// realGate is a Gate on the real clock and the real process environment, with
// an empty docker listing so no test here depends on the host's containers.
func realGate() *Gate {
	return NewGate(WithRuntime(&fakeRuntime{}), WithPollInterval(integrationPoll))
}

// TestIntegrationProcessGone_ReapedChild pins processGone's one "true" — the
// answer that licenses deleting a pid's containers — against a real child that
// has exited and been reaped.
func TestIntegrationProcessGone_ReapedChild(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("cannot run true: %v", err)
	}
	if !processGone(cmd.Process.Pid) {
		t.Errorf("processGone(%d) = false for a child that exited and was reaped", cmd.Process.Pid)
	}
}

// TestIntegrationAcquire_KernelReleasesOnProcessDeath is the adversarial check
// for the mechanism this package exists to provide: the whole point of using
// flock(2) instead of a hand-rolled pid/mkdir lock (see the gt-bcsq
// discussion of v1-v2.3) is that the KERNEL releases the lock when the
// holding process dies by any means, including SIGKILL, with no reclaim
// logic to get wrong. A regression that swapped the real lock file for e.g.
// a plain "does this path exist" check would still pass every
// acquire/release/status unit test yet fail this one, because nothing would
// ever remove the file after a SIGKILL.
func TestIntegrationAcquire_KernelReleasesOnProcessDeath(t *testing.T) {
	g := realGate()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(LockPath(townRoot)), 0755); err != nil {
		t.Fatal(err)
	}

	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot resolve test binary: %v", err)
	}

	cmd := exec.Command(bin, "-test.run=^TestIntegrationHelperHoldSlotUntilKilled$")
	cmd.Env = append(os.Environ(), "GT_SLOT_HELPER=1", "GT_SLOT_TOWN_ROOT="+townRoot)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper process: %v", err)
	}

	// Wait for the helper to finish its grant before killing it: the flock
	// held AND the grant's history entry written. Status().Held reads the
	// flock alone, which the grant takes before it writes the owner file and
	// the history entry (see grant in acquirePool). Killing on Held alone
	// raced that write: under load the SIGKILL landed first and the entry
	// never existed ("the SIGKILLed holder left no history entry", gt-6920e).
	// Waiting for the entry keeps what this test proves — the helper never
	// releases, so its entry can only be the grant-time record, and it must
	// survive the kill with its hold left open. The deadline stays under the
	// helper's 30s sleep and only costs time when the helper is truly stuck.
	deadline := time.Now().Add(20 * time.Second)
	for {
		rep, err := g.Status(townRoot)
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
	h2, err := g.Acquire(townRoot, "post-kill", 30*time.Second)
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

// TestIntegrationHelperHoldSlotUntilKilled is not a real test; it is spawned
// as a subprocess by TestIntegrationAcquire_KernelReleasesOnProcessDeath to
// hold the slot until SIGKILLed, with no Release() call in its shutdown path.
func TestIntegrationHelperHoldSlotUntilKilled(t *testing.T) {
	if os.Getenv("GT_SLOT_HELPER") != "1" {
		t.Skip("not invoked as slot-holder helper")
	}
	townRoot := os.Getenv("GT_SLOT_TOWN_ROOT")
	// The parent's Status polls probe the same flock, so a try can lose to a
	// probe and wait a poll interval; 15s only costs time if truly stuck.
	if _, err := realGate().Acquire(townRoot, "helper", 15*time.Second); err != nil {
		t.Fatalf("helper failed to acquire: %v", err)
	}
	time.Sleep(30 * time.Second) // outlived by the parent's SIGKILL
}

// TestIntegrationAcquire_ReentrantChildProcessSkipsFlock proves the deadlock
// fix across a real fork: a child process spawned while this process holds
// the slot inherits the parent's ReentrantEnvVar marker (as `gt slot run` does
// by default, and as a Go call path like runMQBatchRun's spawned
// formula-driven subprocess would) and takes the reentrant fast path instead
// of blocking on a flock its own ancestor is still holding — the crux of the
// runMQBatchRun + formula-wrap nesting gt-tuiy identified. The child names
// the SAME role as the holder, which is what scopes the fast path to the
// holder's own work (gt-off9; TestAcquire_MarkerRoleScopesTheFastPath covers
// the other roles).
func TestIntegrationAcquire_ReentrantChildProcessSkipsFlock(t *testing.T) {
	g := realGate()
	townRoot := t.TempDir()
	const role = "gastown/coral"

	h, err := g.Acquire(townRoot, role, time.Second)
	if err != nil {
		t.Fatalf("ancestor Acquire: %v", err)
	}
	defer h.Release()

	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot resolve test binary: %v", err)
	}
	// A real fork, not a simulated one, so the marker travels exactly the way
	// it does in production: through the environment this process armed.
	cmd := exec.Command(bin, "-test.run=^TestIntegrationHelperReentrantAcquire$", "-test.v")
	cmd.Env = append(os.Environ(), "GT_SLOT_TOWN_ROOT="+townRoot, "GT_SLOT_CHILD_ROLE="+role)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("slot child (role=%s) failed: %v\n%s", role, err, out)
	}
	// A -test.run pattern matching nothing also exits 0.
	if !strings.Contains(string(out), "--- PASS: TestIntegrationHelperReentrantAcquire") {
		t.Fatalf("slot child did not run the helper:\n%s", out)
	}

	// The ancestor's real hold must be unaffected by the child's Release.
	rep, err := g.Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !rep.Held {
		t.Fatalf("Status reported not held after child's reentrant Release — reentrant Release must not touch the real lock: %+v", rep)
	}
}

// TestIntegrationHelperReentrantAcquire is not a real test; it is spawned as a
// subprocess by TestIntegrationAcquire_ReentrantChildProcessSkipsFlock. It
// acquires the slot under GT_SLOT_CHILD_ROLE, inheriting the marker its parent
// armed, and must get the near-instant reentrant fast path without touching
// the ancestor's flock.
func TestIntegrationHelperReentrantAcquire(t *testing.T) {
	role := os.Getenv("GT_SLOT_CHILD_ROLE")
	if role == "" {
		t.Skip("not invoked as a slot-child helper")
	}
	townRoot := os.Getenv("GT_SLOT_TOWN_ROOT")

	start := time.Now()
	h, err := realGate().Acquire(townRoot, role, time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Acquire(%q) under a same-role marker: %v", role, err)
	}
	if !h.reentrant || elapsed > time.Second {
		t.Fatalf("Acquire(%q): reentrant=%v after %s — expected the near-instant reentrant fast path", role, h.reentrant, elapsed)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

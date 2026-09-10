package cmd

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// stubNoContainers overrides package slot's docker-ps lookup for the
// duration of t so these tests never shell out to the real docker CLI. This
// is the CRITICAL fix for gt-tuiy attempt 4: acquireBatchGateSlot drives
// slot.Acquire, whose runningGateContainers var was previously unreachable
// from this package, so these tests ran the REAL docker ps and would poll
// for the full batchSlotTimeout (60m) — hanging the whole package's test
// run — whenever any dolt/testcontainers/ryuk container was up on the host
// (deterministic under the integration build's shared Dolt TestMain
// container, and a real risk on any shared Gas Town host per this bead's
// own premise). See slot.SetContainerListerForTest.
func stubNoContainers(t *testing.T) {
	t.Helper()
	t.Cleanup(slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil }))
}

// TestAcquireBatchGateSlot_NeverInvokesRealDockerCLI proves stubNoContainers
// is actually wired to the machinery acquireBatchGateSlot uses, rather than
// silently being a no-op stub nobody consults. Docker is removed from PATH
// entirely: if acquireBatchGateSlot's Acquire call ever fell through to the
// REAL runningGateContainers instead of the stub, the missing-binary branch
// reports "no containers" (docker absent ⇒ nothing can be running) and
// slot.Acquire would succeed just as fast — masking exactly the wiring bug
// this test exists to catch. So the stub below claims a container IS
// running: only consulting the stub (not the real, docker-absent lister)
// explains a timeout instead of a fast success.
func TestAcquireBatchGateSlot_NeverInvokesRealDockerCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	restore := slot.SetContainerListerForTest(func() ([]string, error) {
		return []string{"dolt/dolt-sql-server:2.2.0 someone-elses-suite"}, nil
	})
	defer restore()

	townRoot := t.TempDir()
	timeout := slot.DefaultPollInterval + 500*time.Millisecond
	_, err := slot.Acquire(townRoot, "gastown/refinery-batch", timeout)
	if err == nil {
		t.Fatalf("Acquire succeeded even though the stubbed lister reported a running container — the real (docker-absent) lister must have been consulted instead of the stub")
	}
}

// TestAcquireBatchGateSlot_SkipsWhenNoGateCommand is the skip-when-no-gate
// branch attempt 2's om-editorial review flagged as untested: a batch with
// no configured gate command never touches Docker, so acquireBatchGateSlot
// must not touch the townwide lock at all.
func TestAcquireBatchGateSlot_SkipsWhenNoGateCommand(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := acquireBatchGateSlot(townRoot, "gastown", "")
	if err != nil {
		t.Fatalf("acquireBatchGateSlot with empty gateCmd: %v", err)
	}
	if h != nil {
		t.Fatalf("acquireBatchGateSlot with empty gateCmd returned a non-nil handle: %+v", h)
	}

	rep, err := slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status: %v", err)
	}
	if rep.Held {
		t.Fatalf("slot reported held after a no-gate-command call that should never have touched it: %+v", rep)
	}
}

// TestAcquireBatchGateSlot_AcquiresWhenGateCommandConfigured is the
// acquire-around-batch branch attempt 2 flagged as untested: a configured
// gate command must hold the townwide slot until the caller releases it.
func TestAcquireBatchGateSlot_AcquiresWhenGateCommandConfigured(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := acquireBatchGateSlot(townRoot, "gastown", "make test")
	if err != nil {
		t.Fatalf("acquireBatchGateSlot with gateCmd set: %v", err)
	}
	if h == nil {
		t.Fatalf("acquireBatchGateSlot with gateCmd set returned a nil handle")
	}

	rep, err := slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status while held: %v", err)
	}
	if !rep.Held {
		t.Fatalf("slot.Status reports not held while acquireBatchGateSlot's handle is outstanding: %+v", rep)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rep, err = slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status after release: %v", err)
	}
	if rep.Held {
		t.Fatalf("slot.Status still reports held after Release: %+v", rep)
	}
}

// mqBatchReentrantHelperEnvVar and mqBatchReentrantTownRootEnvVar trigger
// TestHelperMQBatchReentrantAcquire when it's re-executed as a subprocess of
// TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock. Deliberately NOT
// GT_-prefixed (gt-tuiy attempt 4, MAJOR): this package's TestMain
// (hermetic_main_test.go) scrubs every GT_*/BD_*/BEADS_* variable from the
// process environment before running any test — including in the child,
// since it's the same test binary re-executed and runs the same TestMain.
// A GT_-prefixed trigger here would be wiped before the helper's body ever
// ran, so it would always hit t.Skip and exit 0 without exercising
// anything, and the parent's assertion (rep.Held) would pass trivially on
// its own, unaffected hold — a vacuous test that would pass identically if
// reentrancy were deleted. The underlying reentrant marker itself
// (slot's reentrantEnvVar) was renamed off GT_ for the same reason — see
// its doc comment in internal/slot/slot.go.
const (
	mqBatchReentrantHelperEnvVar   = "MQ_BATCH_REENTRANT_HELPER"
	mqBatchReentrantTownRootEnvVar = "MQ_BATCH_REENTRANT_TOWN_ROOT"
)

// TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock is the reentrant
// case attempt 2 flagged as untested: once runMQBatchRun holds the slot
// in-process around ProcessBatch, any gate-command subprocess it spawns
// (and anything that subprocess itself runs through `gt slot run`) must not
// deadlock against its own ancestor's flock — it inherits the reentrant
// marker acquireBatchGateSlot's underlying slot.Acquire call sets, and takes
// the fast path instead of blocking for the full batchSlotTimeout.
func TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := acquireBatchGateSlot(townRoot, "gastown", "make test")
	if err != nil {
		t.Fatalf("acquireBatchGateSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("acquireBatchGateSlot returned a nil handle with gateCmd set")
	}
	defer h.Release()

	bin, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}

	cmd := exec.Command(bin, "-test.run=TestHelperMQBatchReentrantAcquire")
	cmd.Env = append(os.Environ(), mqBatchReentrantHelperEnvVar+"=1", mqBatchReentrantTownRootEnvVar+"="+townRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reentrant child process failed: %v\n%s", err, out)
	}

	// The ancestor's own hold must be unaffected by the child's reentrant
	// Release — that Release is a no-op against the real lock (see
	// slot.Handle.Release).
	rep, err := slot.Status(townRoot)
	if err != nil {
		t.Fatalf("slot.Status: %v", err)
	}
	if !rep.Held {
		t.Fatalf("slot.Status reports not held after the reentrant child released — its Release must not touch the real lock: %+v", rep)
	}
}

// TestHelperMQBatchReentrantAcquire is not a real test; it is spawned as a
// subprocess by TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock. It
// inherits the reentrant marker from its parent's successful
// acquireBatchGateSlot call and must acquire near-instantly via the
// reentrant fast path rather than blocking on the flock its parent still
// holds.
func TestHelperMQBatchReentrantAcquire(t *testing.T) {
	if os.Getenv(mqBatchReentrantHelperEnvVar) != "1" {
		t.Skip("not invoked as mq-batch reentrant-acquire helper")
	}
	// Defense in depth: the reentrant fast path should mean this never
	// reaches the docker-ps check at all, but stub it anyway so a
	// regression in the fast path fails on a fast, deterministic error
	// instead of hanging on the real docker CLI.
	stubNoContainers(t)
	townRoot := os.Getenv(mqBatchReentrantTownRootEnvVar)

	start := time.Now()
	h, err := acquireBatchGateSlot(townRoot, "gastown-child", "make test")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reentrant child acquireBatchGateSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("reentrant child acquireBatchGateSlot returned a nil handle with gateCmd set")
	}
	if elapsed > time.Second {
		t.Fatalf("child acquireBatchGateSlot took %s — expected the near-instant reentrant fast path, not a poll/wait", elapsed)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("child Release: %v", err)
	}
}

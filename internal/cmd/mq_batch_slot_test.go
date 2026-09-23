package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gtevents "github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/slot"
)

// stubNoContainersOnce installs the stub below exactly once per test binary.
// Installed from a t.Parallel test, the seam's own runningGateContainers var
// would otherwise be written concurrently — the writes race even though every
// caller installs the same value.
var stubNoContainersOnce sync.Once

// stubNoContainers overrides package slot's docker-ps lookup so these tests
// never shell out to the real docker CLI. This is the CRITICAL fix for gt-tuiy
// attempt 4: acquireBatchGateSlot drives slot.Acquire, whose
// runningGateContainers var was previously unreachable from this package, so
// these tests ran the REAL docker ps and would poll for the full
// batchSlotTimeout (60m) — hanging the whole package's test run — whenever any
// dolt/testcontainers/ryuk container was up on the host (deterministic under
// the integration build's shared Dolt TestMain container, and a real risk on
// any shared Gas Town host per this bead's own premise). See
// slot.SetContainerListerForTest.
//
// It installs once and never restores (gt-k317). Every caller wants the same
// lister, and a per-test restore made the helper unusable from t.Parallel: two
// parallel callers saved each other's value and the last cleanup to run put
// back a lister that was already stale, so a peer could start consulting the
// real docker CLI. A test that needs a different lister calls
// slot.SetContainerListerForTest directly, as
// TestAcquireBatchGateSlot_NeverInvokesRealDockerCLI does.
func stubNoContainers(t *testing.T) {
	t.Helper()
	stubNoContainersOnce.Do(func() {
		slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil })
	})
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

	h, err := acquireBatchGateSlot(townRoot, "gastown", false)
	if err != nil {
		t.Fatalf("acquireBatchGateSlot with no gate configured: %v", err)
	}
	if h != nil {
		t.Fatalf("acquireBatchGateSlot with no gate configured returned a non-nil handle: %+v", h)
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

	h, err := acquireBatchGateSlot(townRoot, "gastown", true)
	if err != nil {
		t.Fatalf("acquireBatchGateSlot with a gate configured: %v", err)
	}
	if h == nil {
		t.Fatalf("acquireBatchGateSlot with a gate configured returned a nil handle")
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
//
// The child acquires under the SAME role as the holder, which is what the
// fast path admits since gt-off9: the marker names the holder's role, so
// only the holder's own work rides it (a caller naming different work
// contends instead — see internal/slot's TestAcquire_MarkerRoleScopesTheFastPath,
// and the `--role` flag's doc in slot.go). A gate-command subprocess that
// nests its own `gt slot run` must pass this role to stay reentrant.
func TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := acquireBatchGateSlot(townRoot, "gastown", true)
	if err != nil {
		t.Fatalf("acquireBatchGateSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("acquireBatchGateSlot returned a nil handle with a gate configured")
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
// acquireBatchGateSlot call — the same rig, so the same
// "<rig>/refinery-batch" role the marker names — and must acquire
// near-instantly via the reentrant fast path rather than blocking on the
// flock its parent still holds.
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
	h, err := acquireBatchGateSlot(townRoot, "gastown", true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reentrant child acquireBatchGateSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("reentrant child acquireBatchGateSlot returned a nil handle with a gate configured")
	}
	if elapsed > time.Second {
		t.Fatalf("child acquireBatchGateSlot took %s — expected the near-instant reentrant fast path, not a poll/wait", elapsed)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("child Release: %v", err)
	}
}

// TestAcquireBatchGateSlot_RecordsTheSameTelemetry is gt-dc81's acceptance for
// the batch path: the refinery's batch gate acquires through the same
// slot.Acquire the CLI does, so its wait, hold and wait reason must land in the
// same event log and ring file — the batch gate is the holder that queued five
// MRs behind a 29-minute wait (gt-dc81), and it is the one an operator needs to
// see without reading panes.
func TestAcquireBatchGateSlot_RecordsTheSameTelemetry(t *testing.T) {
	stubNoContainers(t)
	townRoot := t.TempDir()

	h, err := acquireBatchGateSlot(townRoot, "gastown", true)
	if err != nil {
		t.Fatalf("acquireBatchGateSlot: %v", err)
	}
	if h == nil {
		t.Fatalf("acquireBatchGateSlot returned a nil handle with a gate configured")
	}
	time.Sleep(120 * time.Millisecond)
	if err := h.ReleaseWithExit(1); err != nil {
		t.Fatalf("ReleaseWithExit: %v", err)
	}

	history, err := slot.History(townRoot)
	if err != nil {
		t.Fatalf("slot.History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history holds %d entries, want one per acquisition: %+v", len(history), history)
	}
	if history[0].Role != "gastown/refinery-batch" {
		t.Errorf("history role = %q, want the batch gate's own role", history[0].Role)
	}
	if history[0].HeldS == nil || *history[0].HeldS < 0.1 {
		t.Errorf("history held_s = %v, want the ~0.12s hold", history[0].HeldS)
	}

	rawEvents, err := os.ReadFile(filepath.Join(townRoot, gtevents.EventsFile))
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	seen := map[string]gtevents.Event{}
	for _, line := range strings.Split(strings.TrimSpace(string(rawEvents)), "\n") {
		if line == "" {
			continue
		}
		var event gtevents.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unmarshal event %q: %v", line, err)
		}
		seen[event.Type] = event
	}
	for _, want := range []string{gtevents.TypeSlotWait, gtevents.TypeSlotHold} {
		event, ok := seen[want]
		if !ok {
			t.Fatalf("no %s event in %s: %s", want, gtevents.EventsFile, rawEvents)
		}
		assertPayloadString(t, event.Payload, "role", "gastown/refinery-batch")
		if event.Visibility != gtevents.VisibilityBoth {
			t.Errorf("%s visibility = %q, want %q so gt feed --plain shows it", want, event.Visibility, gtevents.VisibilityBoth)
		}
	}
	// The exit status the batch gate reports is its own outcome, not the CLI's.
	if got := seen[gtevents.TypeSlotHold].Payload["exit_status"]; got != float64(1) {
		t.Errorf("slot_hold exit_status = %#v, want 1", got)
	}
}

package cmd

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// TestAcquireBatchGateSlot_SkipsWhenNoGateCommand is the skip-when-no-gate
// branch attempt 2's om-editorial review flagged as untested: a batch with
// no configured gate command never touches Docker, so acquireBatchGateSlot
// must not touch the townwide lock at all.
func TestAcquireBatchGateSlot_SkipsWhenNoGateCommand(t *testing.T) {
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

// TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock is the reentrant
// case attempt 2 flagged as untested: once runMQBatchRun holds the slot
// in-process around ProcessBatch, any gate-command subprocess it spawns
// (and anything that subprocess itself runs through `gt slot run`) must not
// deadlock against its own ancestor's flock — it inherits the reentrant
// marker acquireBatchGateSlot's underlying slot.Acquire call sets, and takes
// the fast path instead of blocking for the full batchSlotTimeout.
func TestAcquireBatchGateSlot_ReentrantChildProcessSkipsFlock(t *testing.T) {
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
	cmd.Env = append(os.Environ(), "GT_MQ_BATCH_REENTRANT_HELPER=1", "GT_MQ_BATCH_TOWN_ROOT="+townRoot)
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
	if os.Getenv("GT_MQ_BATCH_REENTRANT_HELPER") != "1" {
		t.Skip("not invoked as mq-batch reentrant-acquire helper")
	}
	townRoot := os.Getenv("GT_MQ_BATCH_TOWN_ROOT")

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

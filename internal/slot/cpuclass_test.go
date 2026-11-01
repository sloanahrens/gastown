package slot

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// acquiredCPU is one CPU-class acquire's outcome, the CPUHandle counterpart of
// the container gate's acquired.
type acquiredCPU struct {
	h   *CPUHandle
	err error
}

// goAcquireCPU runs one CPU-class acquire in the background.
func goAcquireCPU(fn func() (*CPUHandle, error)) <-chan acquiredCPU {
	done := make(chan acquiredCPU, 1)
	go func() {
		h, err := fn()
		done <- acquiredCPU{h: h, err: err}
	}()
	return done
}

// runCPU runs one CPU-class acquire to completion on the fake clock. It is the
// CPUHandle counterpart of testGate.run.
func (tg *testGate) runCPU(t *testing.T, fn func() (*CPUHandle, error)) (*CPUHandle, error) {
	t.Helper()
	got := driveClock(t, tg.clk, tg.pollInterval, goAcquireCPU(fn))
	return got.h, got.err
}

// releaseCPU releases a CPU-class handle, failing the test on error.
func releaseCPU(t *testing.T, h *CPUHandle) {
	t.Helper()
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// mustAcquireCPU acquires a CPU-class slot the test expects to be free. It runs
// on the fake clock with a bounded wait, so a regression that parks the caller
// fails the test instead of hanging it.
func mustAcquireCPU(t *testing.T, tg *testGate, town, role string, size int) *CPUHandle {
	t.Helper()
	h, err := tg.runCPU(t, func() (*CPUHandle, error) {
		return tg.AcquireCPU(town, role, 5*time.Second, size)
	})
	if err != nil {
		t.Fatalf("AcquireCPU(%q): %v", role, err)
	}
	return h
}

// driveCPU drives the fake clock until done yields. Unlike driveClock it allows
// for a moment when no waiter is sleeping — a second waiter may be midway
// through a pass, or already finishing — by reading the result before calling
// that a failure.
func driveCPU(t *testing.T, tg *testGate, done <-chan acquiredCPU) acquiredCPU {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		blocked := make(chan error, 1)
		go func() { blocked <- tg.clk.BlockUntilContext(ctx, 1) }()
		select {
		case got := <-done:
			cancel()
			return got
		case err := <-blocked:
			cancel()
			if err != nil {
				select {
				case got := <-done:
					return got
				default:
					t.Fatal("no CPU-class waiter is sleeping and none returned")
				}
			}
			tg.clk.Advance(tg.pollInterval)
		}
	}
}

// waitCPUQueued waits until n callers hold a place in the arrival queue, so a
// test that releases a slot knows every caller it started has joined. An
// acquire writes its ticket before its first sleep on the fake clock, so n
// sleepers are n tickets.
func waitCPUQueued(t *testing.T, tg *testGate, town string, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tg.clk.BlockUntilContext(ctx, n); err != nil {
		t.Fatalf("fewer than %d CPU-class caller(s) reached the queue: %v", n, err)
	}
	queue, err := tg.readCPUQueue(town)
	if err != nil {
		t.Fatalf("reading the CPU-class queue: %v", err)
	}
	if len(queue.Tickets) != n {
		t.Fatalf("the CPU-class queue holds %d tickets, want %d", len(queue.Tickets), n)
	}
}

// TestCPUClassQueuesBeyondSize: with a class size of two, a third holder waits
// for one of the two running holders to release, and a fourth caller that
// arrives later never jumps ahead of the third (gt-bq68w acceptance).
func TestCPUClassQueuesBeyondSize(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()

	first := mustAcquireCPU(t, tg, town, "gastown/crew/olive", 2)
	second := mustAcquireCPU(t, tg, town, "gastown/crew/teal", 2)
	if first.Index != 0 || second.Index != 1 {
		t.Fatalf("first two holders took slots %d and %d, want 0 and 1", first.Index, second.Index)
	}

	// A third caller waits for a release. The fake-time cap keeps a regression
	// from parking it forever.
	thirdDone := goAcquireCPU(func() (*CPUHandle, error) {
		return tg.AcquireCPU(town, "gastown/crew/russet", 20*tg.pollInterval, 2)
	})
	waitBlocked(t, tg.clk)
	select {
	case early := <-thirdDone:
		t.Fatalf("a third caller was granted while both slots were held: h=%+v err=%v", early.h, early.err)
	default:
	}

	releaseCPU(t, first)
	third := driveCPU(t, tg, thirdDone)
	if third.err != nil {
		t.Fatalf("third caller after a release: %v", third.err)
	}
	if third.h.Index != 0 {
		t.Fatalf("third caller took slot %d, want the released slot 0", third.h.Index)
	}

	// Arrival order: the fourth caller joins before the fifth, and when the
	// second slot frees only the head of the queue may take it. A fresh
	// admission pass — what any newly arriving caller runs first — is the
	// deterministic probe: with two tickets ahead of it, it must not grant.
	fourthDone := goAcquireCPU(func() (*CPUHandle, error) {
		return tg.AcquireCPU(town, "gastown/crew/sable", 20*tg.pollInterval, 2)
	})
	waitCPUQueued(t, tg, town, 1)
	fifthDone := goAcquireCPU(func() (*CPUHandle, error) {
		return tg.AcquireCPU(town, "gastown/crew/umber", 20*tg.pollInterval, 2)
	})
	waitCPUQueued(t, tg, town, 2)

	releaseCPU(t, second)
	late, _, err := tg.admitCPU(town, "gastown/crew/latecomer", "latecomer", 2)
	if err != nil {
		t.Fatalf("admission pass: %v", err)
	}
	if late != nil {
		_ = late.Release()
		t.Fatal("a caller that arrived after two waiters took the free slot instead of queueing behind them")
	}
	tg.withdrawCPUTicket(town, "latecomer")

	fourth := driveCPU(t, tg, fourthDone)
	if fourth.err != nil {
		t.Fatalf("fourth caller after a release: %v", fourth.err)
	}
	if fourth.h.Index != 1 {
		t.Fatalf("fourth caller took slot %d, want the released slot 1", fourth.h.Index)
	}
	select {
	case early := <-fifthDone:
		t.Fatalf("the fifth caller, which arrived later, was granted before the fourth released: h=%+v err=%v", early.h, early.err)
	default:
	}

	releaseCPU(t, fourth.h)
	fifth := driveCPU(t, tg, fifthDone)
	if fifth.err != nil {
		t.Fatalf("fifth caller after a release: %v", fifth.err)
	}
	if fifth.h.Index != 1 {
		t.Fatalf("fifth caller took slot %d, want the slot the fourth released", fifth.h.Index)
	}
	releaseCPU(t, fifth.h)
	releaseCPU(t, third.h)
}

// TestCPUClassReapsDeadHolder: a waiter whose process died leaves a queue
// ticket and an owner file, and neither keeps a live caller from the slot. The
// kernel dropped the dead holder's flock; the ticket is pruned by the same
// liveness rule the owner files use (gt-bq68w acceptance).
func TestCPUClassReapsDeadHolder(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	deadPID := foreignPID()
	tg.gone[deadPID] = true
	tg.starts[deadPID] = "start-1"

	// What a crashed waiter left behind: its ticket at the head of the queue,
	// and the owner file it wrote before dying. The flock itself is gone with
	// the process.
	if err := tg.writeCPUQueue(town, cpuQueue{
		Next: 1,
		Tickets: []cpuTicket{{
			ID: "crashed", Seq: 0, PID: deadPID, Start: "start-1", Role: "gastown/crew/gone",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	writeCPUOwner(town, 0, "gastown/crew/gone", deadPID, testNow)

	h := mustAcquireCPU(t, tg, town, "gastown/crew/olive", 1)
	if h.Index != 0 {
		t.Fatalf("live caller took slot %d, want the dead holder's slot 0", h.Index)
	}
	t.Cleanup(func() { _ = h.Release() })

	queue, err := tg.readCPUQueue(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Tickets) != 0 {
		t.Fatalf("queue after the grant = %+v, want the dead ticket pruned and the live one removed", queue.Tickets)
	}
	owner := readOwnerFile(CPUOwnerPath(town, h.Index))
	if owner == nil || owner.PID != tg.pid || owner.Role != "gastown/crew/olive" {
		t.Fatalf("slot 0 owner = %+v, want the live caller", owner)
	}
}

// TestCPUClassRunsNicedWithPFlag: the command a CPU-class slot was taken for
// runs at nice 10 and with -p=4 in GOFLAGS, added only when the caller has not
// already named a -p (gt-bq68w acceptance).
func TestCPUClassRunsNicedWithPFlag(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		environ   []string
		wantFlags string
	}{
		{name: "unset GOFLAGS", wantFlags: "-p=4"},
		{name: "GOFLAGS without -p", environ: []string{"GOFLAGS=-mod=mod"}, wantFlags: "-mod=mod -p=4"},
		{name: "GOFLAGS already sets -p", environ: []string{"GOFLAGS=-p=8 -mod=mod"}, wantFlags: "-p=8 -mod=mod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := newTestGate(t)
			town := t.TempDir()
			var got CPUExecRequest
			ran := false
			tg.Gate.execCPU = func(req CPUExecRequest) (int, error) {
				got = req
				ran = true
				return 0, nil
			}

			environ := append([]string{"PATH=/usr/bin", "HOME=/home/crew"}, tc.environ...)
			code, err := RunCPU(town, CPURunOptions{
				Role:    "gastown/crew/olive",
				Timeout: 5 * time.Second,
				Size:    1,
				Args:    []string{"GT_TEST_DOCKER=0", exe, "build", "./..."},
				Path:    "/usr/bin",
				Environ: func() []string { return environ },
				Stderr:  io.Discard,
				gate:    tg.Gate,
			})
			if err != nil || code != 0 {
				t.Fatalf("RunCPU = (%d, %v), want (0, nil)", code, err)
			}
			if !ran {
				t.Fatal("the executor never ran the command")
			}
			if got.Nice != 10 {
				t.Errorf("executor nice = %d, want 10", got.Nice)
			}
			if want := []string{exe, "build", "./..."}; !slices.Equal(got.Args, want) {
				t.Errorf("executor argv = %v, want %v", got.Args, want)
			}
			var flags []string
			for _, entry := range got.Env {
				if v, ok := strings.CutPrefix(entry, "GOFLAGS="); ok {
					flags = append(flags, v)
				}
			}
			if len(flags) != 1 || flags[0] != tc.wantFlags {
				t.Errorf("GOFLAGS in the child env = %v, want exactly [%q]", flags, tc.wantFlags)
			}
			if !containsEntry(got.Env, "GT_TEST_DOCKER=0") {
				t.Errorf("child env = %v, want the leading assignment applied", got.Env)
			}
			if _, statErr := os.Stat(CPUOwnerPath(town, 0)); !os.IsNotExist(statErr) {
				t.Errorf("slot 0 owner file after RunCPU: stat err=%v, want the slot released", statErr)
			}
		})
	}
}

// TestCPUClassWaitBound: a wait past the caller's bound fails naming the
// holders, and the caller takes its queue ticket with it (gt-bq68w
// acceptance).
func TestCPUClassWaitBound(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	held := mustAcquireCPU(t, tg, town, "gastown/crew/olive", 1)
	defer func() { _ = held.Release() }()

	timeout := 3 * tg.pollInterval
	h, err := tg.runCPU(t, func() (*CPUHandle, error) {
		return tg.AcquireCPU(town, "gastown/crew/teal", timeout, 1)
	})
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU past its bound = (%v, %v), want an error", h, err)
	}
	for _, want := range []string{
		"timed out after",
		"gastown/crew/olive",
		"pid " + strconv.Itoa(tg.pid),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error = %q, want it to name %q", err, want)
		}
	}
	queue, readErr := tg.readCPUQueue(town)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(queue.Tickets) != 0 {
		t.Fatalf("queue after the timed-out caller left = %+v, want its ticket withdrawn", queue.Tickets)
	}
	if n := strings.Count(tg.probe.String(), "waiting for a CPU-class build slot"); n != 1 {
		t.Errorf("wait lines = %d, want one line for the whole wait: %q", n, tg.probe.String())
	}
}

// TestCPUClassRefusesUnreadableQueue: a queue that cannot be parsed stops the
// caller with the file named, rather than being read as an empty queue and
// handing out slots out of arrival order.
func TestCPUClassRefusesUnreadableQueue(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CPUQueuePath(town), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU over an unreadable queue = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), CPUQueuePath(town)) {
		t.Errorf("error = %q, want it to name the queue file %q", err, CPUQueuePath(town))
	}
}

// TestCPUClassRefusesUnusableLockDir: a town whose .runtime path is a file
// cannot hold a CPU-class slot, and the caller is told why instead of hanging.
func TestCPUClassRefusesUnusableLockDir(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU with an unusable lock directory = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "creating lock directory") {
		t.Errorf("error = %q, want it to name the lock directory", err)
	}
}

// TestCPUClassRefusesUnusableAdmissionLock: an admission lock that cannot be
// opened stops the caller rather than letting it take a slot without joining
// the arrival queue.
func TestCPUClassRefusesUnusableAdmissionLock(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(CPUAdmitPath(town), 0o755); err != nil {
		t.Fatal(err)
	}

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU with an unusable admission lock = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "admission lock") {
		t.Errorf("error = %q, want it to name the admission lock", err)
	}
}

// TestCPUClassRefusesQueueThatIsADirectory: a queue path that cannot be read
// as a file stops the caller with the file named, rather than being read as an
// empty queue.
// Mutation: read any queue error as an empty queue, and the caller takes a slot
// out of arrival order with nothing said.
func TestCPUClassRefusesQueueThatIsADirectory(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(CPUQueuePath(town), 0o755); err != nil {
		t.Fatal(err)
	}

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU over an unreadable queue file = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), CPUQueuePath(town)) {
		t.Errorf("error = %q, want it to name the queue file %q", err, CPUQueuePath(town))
	}
}

// TestCPUClassRefusesUnprobeableSlot: a slot lock that cannot be opened stops
// the caller instead of being read as a free slot.
// Mutation: drop the probe's error, and a slot that may be held is handed out.
func TestCPUClassRefusesUnprobeableSlot(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(CPULockPath(town, 0), 0o755); err != nil {
		t.Fatal(err)
	}

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU over an unopenable slot lock = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "checking CPU-class slot 0") {
		t.Errorf("error = %q, want it to name the slot it could not check", err)
	}
}

// TestCPUClassRefusesUncreatableSlotLock: a slot the probe reports free but
// whose lock cannot be created stops the caller rather than running the build
// without a held lock.
// Mutation: ignore the acquire error, and the build runs on a slot it does not
// hold, beside as many others as the class allows.
func TestCPUClassRefusesUncreatableSlotLock(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink reads as an absent lock file to the probe (which does
	// not create) and fails to open for the acquire (which does).
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing", "lock"), CPULockPath(town, 0)); err != nil {
		t.Fatal(err)
	}

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU over a lock it cannot create = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "taking CPU-class slot 0") {
		t.Errorf("error = %q, want it to name the slot it could not take", err)
	}
}

// TestCPUClassReportsUnwritableQueueAfterGrant: a slot taken for a caller whose
// queue update then fails is released and the failure reported, so a lost grant
// is neither silent nor a held lock.
// Mutation: ignore the write error, and the caller holds a slot the queue says
// the next waiter still queued for.
func TestCPUClassReportsUnwritableQueueAfterGrant(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	tg.Gate.writeJSON = func(string, any) error { return errors.New("disk full") }
	town := t.TempDir()

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU over an unwritable queue = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "writing the CPU-class queue") {
		t.Errorf("error = %q, want it to name the queue write", err)
	}
	// The slot the caller took must be free again: a reader that does not
	// create can take it.
	unlock, free, probeErr := probeCPUSlot(town, 0)
	if probeErr != nil || !free {
		t.Fatalf("slot 0 after the failed queue write = (free %v, err %v), want it released", free, probeErr)
	}
	if unlock != nil {
		unlock()
	}
}

// TestCPUClassReportsUnwritableQueueWhileWaiting: a waiter whose ticket cannot
// be written stops with the failure instead of waiting on a queue it never
// joined.
// Mutation: return the wait cause with the write error dropped, and the caller
// loops forever behind a ticket nobody else can see.
func TestCPUClassReportsUnwritableQueueWhileWaiting(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	tg.Gate.writeJSON = func(string, any) error { return errors.New("disk full") }
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	held, ok, err := lock.FlockTryAcquire(CPULockPath(town, 0))
	if err != nil || !ok {
		t.Fatalf("holding slot 0 for the case = (ok %v, err %v)", ok, err)
	}
	defer held()

	h, err := tg.AcquireCPU(town, "gastown/crew/teal", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU over an unwritable queue = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "writing the CPU-class queue") {
		t.Errorf("error = %q, want it to name the queue write", err)
	}
}

// TestCPUClassRefusesTicketEntropyFailure: a ticket that cannot be named stops
// the caller before it takes a slot.
// Mutation: ignore the error and use the empty id, and two callers become one
// ticket and one of them waits behind itself.
func TestCPUClassRefusesTicketEntropyFailure(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	tg.Gate.ticketRand = failingReader{}
	town := t.TempDir()

	h, err := tg.AcquireCPU(town, "gastown/crew/olive", time.Second, 1)
	if h != nil || err == nil {
		t.Fatalf("AcquireCPU without entropy for its ticket = (%v, %v), want an error", h, err)
	}
	if !strings.Contains(err.Error(), "generating a CPU-class queue ticket") {
		t.Errorf("error = %q, want it to name the ticket", err)
	}
}

// failingReader is an entropy source that never produces a byte.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// TestCPUClassExitResultWrapsANonExitWaitError: a wait that was not an exit
// status is the wrapper's failure, reported with the program it was running.
// Mutation: return the code with no error, and a build the wrapper could not
// wait on reads as passed.
func TestCPUClassExitResultWrapsANonExitWaitError(t *testing.T) {
	t.Parallel()
	_, err := exitResult("go", errors.New("copy to stdout failed"))
	if err == nil {
		t.Fatal("exitResult over a non-exit wait error returned no error")
	}
	for _, want := range []string{"running go", "copy to stdout failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestCPUClassExitCodeForASignalledChild: a child killed by a signal reports
// the shell's 128+signal, not -1 read as 255, so a caller sees the same code
// the shell would have reported.
// Mutation: return exitErr.ExitCode() alone, and a killed build reads as 255.
func TestCPUClassExitCodeForASignalledChild(t *testing.T) {
	t.Parallel()
	if got := shellExitCode(syscall.WaitStatus(9)); got != 137 {
		t.Errorf("shellExitCode(SIGKILL) = %d, want 137", got)
	}
	if got := shellExitCode(syscall.WaitStatus(2 << 8)); got != 2 {
		t.Errorf("shellExitCode(exit 2) = %d, want 2", got)
	}
}

// TestCPUClassAppliesOnlyUnderCrew: the CPU class covers a build started from
// a crew checkout and nothing else, so polecats, the landing worker and a
// checkout outside the town keep their current path.
func TestCPUClassAppliesOnlyUnderCrew(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cases := []struct {
		name     string
		dir      string
		wantRole string
		wantOK   bool
	}{
		{name: "crew checkout", dir: filepath.Join(town, "gastown", "crew", "olive"), wantRole: "gastown/crew/olive", wantOK: true},
		{name: "inside a crew checkout", dir: filepath.Join(town, "gastown", "crew", "olive", "internal", "slot"), wantRole: "gastown/crew/olive", wantOK: true},
		{name: "polecat checkout", dir: filepath.Join(town, "gastown", "polecats", "dag", "gastown")},
		{name: "the town root", dir: town},
		{name: "the crew directory itself", dir: filepath.Join(town, "gastown", "crew")},
		{name: "outside the town", dir: filepath.Join(t.TempDir(), "gastown", "crew", "olive")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, ok := CPUBuildRole(town, tc.dir)
			if ok != tc.wantOK || role != tc.wantRole {
				t.Errorf("CPUBuildRole(%q) = (%q, %v), want (%q, %v)", tc.dir, role, ok, tc.wantRole, tc.wantOK)
			}
		})
	}
}

// TestCPUClassRunRefusesNoCommand: the wrapper refuses to take a slot for an
// invocation that names no command.
func TestCPUClassRunRefusesNoCommand(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()

	if _, err := RunCPU(town, CPURunOptions{Role: "gastown/crew/olive", Size: 1, Args: []string{"ONLY=assigned"}, Stderr: io.Discard, gate: tg.Gate}); err == nil {
		t.Fatal("RunCPU over only an environment assignment succeeded, want an error")
	}
	if _, err := RunCommandUnslotted(nil, "", nil, io.Discard, io.Discard); err == nil {
		t.Fatal("RunCommandUnslotted with no command succeeded, want an error")
	}
}

// TestCPUClassRunReportsSlotError: an acquire that fails is reported by RunCPU
// and nothing is executed.
func TestCPUClassRunReportsSlotError(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CPUQueuePath(town), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ran := false
	tg.Gate.execCPU = func(CPUExecRequest) (int, error) {
		ran = true
		return 0, nil
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunCPU(town, CPURunOptions{Role: "gastown/crew/olive", Size: 1, Args: []string{exe}, Stderr: io.Discard, gate: tg.Gate}); err == nil {
		t.Fatal("RunCPU over an unreadable queue succeeded, want an error")
	}
	if ran {
		t.Fatal("the command ran without a slot")
	}
}

// TestCPUClassRunReportsExecutorError: a command that cannot start is reported
// and the slot goes back. The executor is the production refusal seam, so this
// drives RunCPU, not the executor directly.
func TestCPUClassRunReportsExecutorError(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	tg.Gate.execCPU = func(CPUExecRequest) (int, error) {
		return 0, errors.New("exec failed")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunCPU(town, CPURunOptions{Role: "gastown/crew/olive", Size: 1, Args: []string{exe}, Stderr: io.Discard, gate: tg.Gate}); err == nil {
		t.Fatal("RunCPU succeeded though the executor failed, want an error")
	}
	if _, statErr := os.Stat(CPUOwnerPath(town, 0)); !os.IsNotExist(statErr) {
		t.Errorf("slot 0 owner file after a failed exec: stat err=%v, want the slot released", statErr)
	}
}

// TestCPUClassExecRefusesMissingProgram: the real executor reports a command
// it cannot start instead of returning a clean exit.
func TestCPUClassExecRefusesMissingProgram(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "not-a-program")
	if _, err := runCPUExec(CPUExecRequest{Program: missing, Args: []string{missing}, Env: os.Environ(), Stderr: io.Discard}); err == nil {
		t.Fatal("runCPUExec started a missing program, want an error")
	}
}

// TestCPUClassExecSetsPriorityBeforeStarting: the executor lowers the priority
// before it starts the command, so the child inherits it. The program is
// missing on purpose, which keeps the test free of a started process: the
// priority call comes first and is recorded.
// Mutation: drop the setpriority call, and no priority is recorded.
func TestCPUClassExecSetsPriorityBeforeStarting(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "not-a-program")
	var calls []int
	_, err := runCPUExec(CPUExecRequest{
		Program:     missing,
		Args:        []string{missing},
		Env:         os.Environ(),
		Nice:        10,
		SetPriority: func(n int) error { calls = append(calls, n); return nil },
		Stderr:      io.Discard,
	})
	if err == nil {
		t.Fatal("runCPUExec started a missing program, want an error")
	}
	if !slices.Equal(calls, []int{10}) {
		t.Fatalf("priority calls = %v, want one call with the request's nice 10", calls)
	}
}

// TestCPUClassExecRunsWhenPriorityRefused: a kernel that refuses the priority
// is reported by name, and the command still runs — the class is a load bound,
// not a correctness guard, so it must not fail a crew's build (gt-bq68w).
// Mutation: return the priority error instead of reporting it, and the start
// is never attempted.
func TestCPUClassExecRunsWhenPriorityRefused(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "not-a-program")
	var stderr strings.Builder
	_, err := runCPUExec(CPUExecRequest{
		Program:     missing,
		Args:        []string{missing},
		Env:         os.Environ(),
		Nice:        10,
		SetPriority: func(int) error { return errors.New("operation not permitted") },
		Stderr:      &stderr,
	})
	if err == nil || !strings.Contains(err.Error(), "running ") {
		t.Fatalf("runCPUExec after a refused priority = %v, want the missing program's start error", err)
	}
	for _, want := range []string{"could not lower", "nice 10", "operation not permitted"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to name %q", stderr.String(), want)
		}
	}
}

// containsEntry reports whether env holds entry.
func containsEntry(env []string, entry string) bool {
	return slices.Contains(env, entry)
}

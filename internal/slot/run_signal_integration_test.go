//go:build integration && !windows

package slot

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// wrapperOutput is what the wrapper and the command it wraps wrote, for the
// test to quote when it fails. A failing test reads it while the wrapper may
// still be writing, which is more than bytes.Buffer's own locking allows.
type wrapperOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *wrapperOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *wrapperOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// The run under test has two ends, and the test binary is neither: the wrapper
// stands where `gt slot run` does, and the probe stands where the suite it
// wraps does. Both are this binary re-executed, so the test can signal a
// wrapper, and hold a suite open, that it owns by pid.
const (
	runWrapperEnv  = "GT_SLOT_SIGNAL_TEST_WRAPPER"
	runWrapperTown = "GT_SLOT_SIGNAL_TEST_TOWN"
	runWrapperRole = "GT_SLOT_SIGNAL_TEST_ROLE"
	runProbeDir    = "GT_SLOT_SIGNAL_TEST_DIR"
)

const (
	// runSignalWait is how long the test waits for a probe marker. It is
	// generous because the failure it guards against is a wrapper that dies
	// before it can forward anything, which no shorter wait would catch
	// sooner.
	runSignalWait = 15 * time.Second
	// runSignalHoldWait caps one acquire while the test checks that the slot
	// is held. The acquire is expected to time out, so it costs this much per
	// check.
	runSignalHoldWait = 300 * time.Millisecond
	// runSignalFreeWait bounds the poll for the hold to end after the probe
	// exits.
	runSignalFreeWait = 5 * time.Second
	// probeSelfReleaseWait bounds the probe's own wait for the test to release
	// it, so a test that dies mid-run cannot leave the probe behind.
	probeSelfReleaseWait = 60 * time.Second
	// runProbeRelease is the file the test writes to let the probe exit, and
	// so to end the hold.
	runProbeRelease = "release"
)

// TestIntegrationSlotRunWrapper is the re-executed stand-in for the `gt slot
// run` process: it runs Run, so the test can send a terminating signal to a
// wrapper rather than to itself.
func TestIntegrationSlotRunWrapper(t *testing.T) {
	if os.Getenv(runWrapperEnv) != "1" {
		t.Skip("helper: runs only as the re-executed wrapper")
	}
	code, err := Run(os.Getenv(runWrapperTown), RunOptions{
		Role:    os.Getenv(runWrapperRole),
		Timeout: 30 * time.Second,
		Pool:    Pool{Slots: 1},
		Nice:    0,
		Args:    []string{os.Args[0], "-test.run=^TestIntegrationSlotRunProbe$", "-test.count=1"},
		Path:    os.Getenv("PATH"),
		Env:     os.Environ,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		// The hold this test judges is the flock, and a real `docker ps` would
		// let a stray gate container on the host stall the acquire here.
		gate: NewGate(WithRuntime(&fakeRuntime{}), WithPollInterval(integrationPoll)),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wrapper:", err)
		os.Exit(3)
	}
	os.Exit(code)
}

// TestIntegrationSlotRunProbe is the re-executed stand-in for the wrapped
// suite: it reports that it started and every interrupt it is sent, and then
// waits for the test's release file. The test decides when the run ends, so
// the window it checks the hold in is the test's to close, not a sleep's.
func TestIntegrationSlotRunProbe(t *testing.T) {
	dir := os.Getenv(runProbeDir)
	if dir == "" {
		t.Skip("helper: runs only as the wrapped command")
	}
	mark := func(name string) { _ = os.WriteFile(filepath.Join(dir, name), nil, 0o644) }
	mark("started")

	// Signals are read in the same loop as the release file: the probe is the
	// one process here that must not leave a goroutine behind, of all things.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	release := filepath.Join(dir, runProbeRelease)
	deadline := time.Now().Add(probeSelfReleaseWait)
	for {
		select {
		case <-interrupts:
			mark("interrupt")
			continue
		default:
		}
		if _, err := os.Stat(release); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("no release within %s; exiting so the wrapper can finish", probeSelfReleaseWait)
			break
		}
		time.Sleep(integrationPoll)
	}
	mark("exited")
}

// TestIntegrationRunKeepsTheHoldThroughTerminatingSignals pins gt-1j5rj end to
// end: a SIGTERM or SIGHUP sent to the wrapper — what a daemon or tmux kill
// sends — reaches the wrapped command as an interrupt, and the slot stays held
// until that command exits. Both are checked against the flock, which is the
// only thing that cannot outlive its holder: the owner file a killed wrapper
// leaves behind still names a hold the kernel has already dropped.
func TestIntegrationRunKeepsTheHoldThroughTerminatingSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			town := t.TempDir()
			dir := t.TempDir()
			const role = "gastown/slot-signal-test"

			wrapper := exec.Command(os.Args[0], "-test.run=^TestIntegrationSlotRunWrapper$", "-test.count=1")
			wrapper.Env = append(os.Environ(),
				runWrapperEnv+"=1",
				runWrapperTown+"="+town,
				runWrapperRole+"="+role,
				runProbeDir+"="+dir,
			)
			output := new(wrapperOutput)
			wrapper.Stdout, wrapper.Stderr = output, output
			if err := wrapper.Start(); err != nil {
				t.Fatalf("starting the wrapper: %v", err)
			}
			var waitErr error
			waited := false
			wait := func() error {
				if !waited {
					waited = true
					waitErr = wrapper.Wait()
				}
				return waitErr
			}
			t.Cleanup(func() {
				// The wrapper is a process this test owns: release the probe
				// and let the run finish rather than leave either behind.
				_ = os.WriteFile(filepath.Join(dir, runProbeRelease), nil, 0o644)
				_ = wait()
			})

			// The probe is the command Run started; wait for it to say so,
			// then confirm nothing has gone wrong before the signal.
			requireProbeMarker(t, dir, "started", output)
			requireSlotHeld(t, town)

			if err := wrapper.Process.Signal(sig); err != nil {
				t.Fatalf("sending %s to the wrapper: %v", sig, err)
			}

			// The signal reached the suite — and the wrapper is still here,
			// still holding the slot, rather than gone under the signal's
			// default action with the suite running unwrapped.
			requireProbeMarker(t, dir, "interrupt", output)
			requireSlotHeld(t, town)

			// The hold ends when the suite does, and not before.
			_ = os.WriteFile(filepath.Join(dir, runProbeRelease), nil, 0o644)
			requireProbeMarker(t, dir, "exited", output)
			if err := wait(); err != nil {
				t.Fatalf("the wrapper exited with %v\nwrapper output:\n%s", err, output.String())
			}
			requireSlotFree(t, town)
		})
	}
}

// requireProbeMarker waits for the probe to write marker in dir, failing with
// the wrapper's output when it never does: with the bug this test pins, the
// wrapper dies on the signal and the probe never reports.
func requireProbeMarker(t *testing.T, dir, marker string, output fmt.Stringer) {
	t.Helper()
	path := filepath.Join(dir, marker)
	deadline := time.Now().Add(runSignalWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wrapped command did not report %q within %s\nwrapper output:\n%s",
				marker, runSignalWait, output.String())
		}
		time.Sleep(integrationPoll)
	}
}

// signalTestSlot acquires the test's own view of the pool's one slot.
func signalTestSlot(town string) (*Handle, error) {
	return NewGate(WithRuntime(&fakeRuntime{}), WithPollInterval(integrationPoll)).
		AcquirePool(town, "pid-slot-signal-probe", runSignalHoldWait, Pool{Slots: 1})
}

// requireSlotHeld fails unless the wrapper's flock is still in force: an
// acquire that times out is the kernel's answer, and unlike the owner file it
// cannot outlive the process that wrote it. Any other error is a failure of
// the check itself, not evidence of a hold.
func requireSlotHeld(t *testing.T, town string) {
	t.Helper()
	h, err := signalTestSlot(town)
	if err == nil {
		_ = h.Release()
		t.Fatal("the slot was free while the wrapped command was still running")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("checking whether the slot was held: %v", err)
	}
}

// requireSlotFree fails unless the hold ended once the wrapped command exited.
func requireSlotFree(t *testing.T, town string) {
	t.Helper()
	deadline := time.Now().Add(runSignalFreeWait)
	for {
		h, err := signalTestSlot(town)
		if err == nil {
			if rerr := h.Release(); rerr != nil {
				t.Fatalf("releasing the test's own acquire: %v", rerr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot is still held after the wrapped command exited: %v", err)
		}
	}
}

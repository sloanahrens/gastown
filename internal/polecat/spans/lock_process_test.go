//go:build unix

package spans

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spanLockHoldEnv names the lock file a holder child takes. The holder is this
// test binary re-run on TestSpanLockHolderHelper, and it is inert unless the
// variable is set.
const spanLockHoldEnv = "SPAN_LOCK_HOLD"

// spanLockWait bounds every wait on the holder child: a child that never reports
// or never releases fails the test rather than hanging the gate.
const spanLockWait = 10 * time.Second

// TestSpanLockHolderHelper is the holder child: it takes the span log's append
// lock, says so on stdout, and holds it until stdin closes.
//
//testpolicy:allow parallel — child-process helper, inert unless its environment variable names a lock file, so there is nothing to run in parallel
func TestSpanLockHolderHelper(t *testing.T) {
	path := os.Getenv(spanLockHoldEnv)
	if path == "" {
		return
	}
	lk, err := lockLog(path)
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		return
	}
	os.Stdout.WriteString("held\n")
	io.ReadAll(os.Stdin)
	lk.unlock()
}

// startSpanLockHolder starts a child process that takes the span log's append
// lock at path, and returns once the child says it holds the lock. The returned
// stop releases the lock and waits for the child to exit. A holder still running
// when the test ends is killed, not left behind.
func startSpanLockHolder(t *testing.T, path string) (stop func()) {
	t.Helper()
	//testpolicy:allow no-subprocess — the record lock is per process, so the holder must be another process: this test re-runs the test binary as the holder
	cmd := exec.Command(os.Args[0], "-test.run=^TestSpanLockHolderHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), spanLockHoldEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("holder stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("holder stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- s
	}()
	select {
	case got := <-line:
		if !strings.HasPrefix(got, "held") {
			stdin.Close()
			cmd.Wait()
			t.Fatalf("holder did not take the lock: line %q", got)
		}
	//testpolicy:allow no-sleep — bound the wait for the holder's line, which fails rather than hangs
	case <-time.After(spanLockWait):
		stdin.Close()
		cmd.Process.Kill()
		t.Fatal("holder never reported that it took the lock")
	}
	return func() {
		stdin.Close()
		stopped = true
		if err := cmd.Wait(); err != nil {
			t.Errorf("holder exited with %v", err)
		}
	}
}

// The append lock is a record lock, so another process holding it makes an
// append in this process wait until that process releases it (gt-rm0ab).
func TestAppend_WaitsForAnotherProcessToReleaseTheLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := New(dir, fixedClock{at: testEpoch})
	lockPath := filepath.Join(dir, "gt-abc.jsonl.lock")
	stop := startSpanLockHolder(t, lockPath)

	done := make(chan error, 1)
	go func() { done <- st.Start("gt-abc", "organic") }()
	// While the other process holds the lock, the append must not complete. A
	// negative check needs a window; the wait is the one the writer is in.
	select {
	case err := <-done:
		stop()
		t.Fatalf("Start completed while another process held the log's lock: %v", err)
	//testpolicy:allow no-sleep — a lock the writer must wait on has no event to select on
	case <-time.After(50 * time.Millisecond):
	}

	// The holder still holds the lock after the window: the negative wait above
	// was against a live lock, not one that had already gone.
	if !recordLockHeld(t, lockPath) {
		stop()
		t.Fatal("the holder had released the lock during the wait, so the wait proved nothing")
	}

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after the other process released the lock: %v", err)
		}
	//testpolicy:allow no-sleep — bound the wait for a blocked writer, which fails rather than hangs
	case <-time.After(spanLockWait):
		t.Fatal("Start did not complete after the other process released the lock")
	}
}

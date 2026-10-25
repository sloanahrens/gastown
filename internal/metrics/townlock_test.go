package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// townLockHoldEnv names the lock a holder child process takes. The holder is
// this test binary re-run on TestTownLockHolderHelper, and it is inert unless
// the variable is set, so a normal test run never takes the lock.
const townLockHoldEnv = "TOWNLOCK_HOLD"

// townLockWait bounds every wait on the holder child: a child that never
// reports, or never exits, fails the test rather than hanging the gate.
const townLockWait = 10 * time.Second

// TestTownLockHolderHelper is the holder child: it takes the lock, says so on
// its stdout, and holds it until its stdin closes. A test that needs the lock
// held by another process starts one with startTownLockHolder.
//
//testpolicy:allow parallel — child-process helper, inert unless its environment variable names a lock, so there is nothing to run in parallel
func TestTownLockHolderHelper(t *testing.T) {
	path := os.Getenv(townLockHoldEnv)
	if path == "" {
		return
	}
	lock, err := AcquireTownLock(path)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	fmt.Println("held")
	io.ReadAll(os.Stdin)
	lock.Release()
}

// startTownLockHolder runs a child process that holds the lock at path. It
// returns once the child reports it holds the lock, and stop ends the child,
// which releases the lock when it exits. No sleep: the child's own line says
// when the lock is taken, and each wait is bounded by townLockWait.
func startTownLockHolder(t *testing.T, path string) (stop func()) {
	t.Helper()
	// testpolicy exemption: cross-process contention is the property under test; a record lock never conflicts within one process, so the holder must be another process.
	//testpolicy:allow no-subprocess — cross-process contention is the property under test: a record lock never conflicts within one process, so the holder must be another process
	cmd := exec.Command(os.Args[0], "-test.run=^TestTownLockHolderHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), townLockHoldEnv+"="+path)
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
	case <-time.After(townLockWait):
		stdin.Close()
		cmd.Process.Kill()
		t.Fatal("holder never reported that it took the lock")
	}
	return func() {
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("holder exited with %v", err)
			}
		//testpolicy:allow no-sleep — bound the wait for the holder to exit, which fails rather than hangs
		case <-time.After(townLockWait):
			cmd.Process.Kill()
			t.Error("holder did not exit after its stdin closed")
		}
	}
}

func TestTownLockIsHeldUntilReleased(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	stop := startTownLockHolder(t, path)
	// The holder is another process, which is what a second run is. Its record
	// lock makes this acquire return ErrLockHeld rather than wait.
	if _, err := AcquireTownLock(path); !errors.Is(err, ErrLockHeld) {
		stop()
		t.Fatalf("acquire while another process holds it: err = %v, want ErrLockHeld", err)
	}
	stop()
	// The holder has exited, so its lock is gone with it.
	lock, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire after the holder exited: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// Within one process the lock still refuses a second holder: a record lock
// alone would not, so the in-process record is what keeps the single-flight
// property here.
func TestTownLockRefusesASecondAcquireInTheSameProcess(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	first, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireTownLock(path); !errors.Is(err, ErrLockHeld) {
		t.Errorf("second acquire in the same process: err = %v, want ErrLockHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("release again: %v", err)
	}
}

func TestTownLockRecordsTheHolderPid(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	lock, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the lock file: %v", err)
	}
	if want := fmt.Sprintf("%d\n", os.Getpid()); string(data) != want {
		t.Errorf("lock file = %q, want the holder's pid %q", data, want)
	}
}

func TestTownLockIsNotBlockedByAPidLeftByADeadRun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	// A run the daemon killed leaves its pid in the file and no lock on it.
	if err := os.WriteFile(path, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	lock, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire over a dead run's leftover: %v", err)
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the lock file: %v", err)
	}
	if strings.Contains(string(data), "999999") {
		t.Errorf("lock file still names the dead pid: %q", data)
	}
}

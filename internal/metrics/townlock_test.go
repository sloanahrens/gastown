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
	stopped := false
	// A holder still running when the test ends is killed and reaped, on every
	// path, including a failed wait for its line.
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
	case <-time.After(townLockWait):
		stdin.Close()
		cmd.Process.Kill()
		t.Fatal("holder never reported that it took the lock")
	}
	return func() {
		stdin.Close()
		stopped = true
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

// townLockProbeEnv names the lock file a probe child tries to take.
const townLockProbeEnv = "TOWNLOCK_PROBE"

// TestTownLockProbeHelper is the probe child: it reports whether the lock at the
// path the probe names is free to take in another process.
//
//testpolicy:allow parallel — child-process helper, inert unless its environment variable names a lock, so there is nothing to run in parallel
func TestTownLockProbeHelper(t *testing.T) {
	path := os.Getenv(townLockProbeEnv)
	if path == "" {
		return
	}
	lock, err := AcquireTownLock(path)
	if err != nil {
		fmt.Println("held")
		return
	}
	fmt.Println("free")
	lock.Release()
}

// lockFreeInAnotherProcess reports whether a child process can take the lock at
// path. The probe runs no wait, so it answers at once.
func lockFreeInAnotherProcess(t *testing.T, path string) bool {
	t.Helper()
	//testpolicy:allow no-subprocess — the lock's answer is per process, so the probe must be another process: this re-runs the test binary
	cmd := exec.Command(os.Args[0], "-test.run=^TestTownLockProbeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), townLockProbeEnv+"="+path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe the lock in another process: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch strings.TrimSpace(line) {
		case "free":
			return true
		case "held":
			return false
		}
	}
	t.Fatalf("probe printed neither free nor held: %q", out)
	return false
}

// A hard link or a symlink to a held lock is the same lock: the acquire through
// either is refused, and the refused acquires leave the holder's record lock in
// place. Closing any descriptor of the file would have dropped it.
func TestTownLockRefusesAnAliasOfAHeldLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "town-metrics.lock")
	first, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	hard := filepath.Join(dir, "hard.lock")
	if err := os.Link(path, hard); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	sym := filepath.Join(dir, "sym.lock")
	if err := os.Symlink(path, sym); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, alias := range []string{hard, sym} {
		lk, err := AcquireTownLock(alias)
		if !errors.Is(err, ErrLockHeld) {
			if lk != nil {
				lk.Release()
			}
			t.Errorf("acquire through %s: err = %v, want ErrLockHeld", filepath.Base(alias), err)
		}
	}
	if lockFreeInAnotherProcess(t, path) {
		t.Error("a refused alias acquire dropped the first holder's record lock")
	}
}

// Releasing a lock twice does nothing the second time: it does not forget a
// lock another acquire took after the first release.
func TestTownLockSecondReleaseDoesNotForgetANewerHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	first, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	second, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire after the first release: %v", err)
	}
	defer second.Release()
	if err := first.Release(); err != nil {
		t.Errorf("second release of the first lock: %v", err)
	}
	if _, err := AcquireTownLock(path); !errors.Is(err, ErrLockHeld) {
		t.Errorf("acquire while the second holder is live: err = %v, want ErrLockHeld", err)
	}
}

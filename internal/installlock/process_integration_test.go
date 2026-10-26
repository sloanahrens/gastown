//go:build integration

package installlock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/steveyegge/gastown/internal/testutil"
)

// The process-level tests re-run this test binary as the two programs they
// need, selected by INSTALL_LOCK_HELPER: "run" is the install-lock command, and
// "report" is the install it execs. Both skip the unit tier's setup, which
// would leave a refusing-tools directory behind per child.
const (
	helperEnv    = "INSTALL_LOCK_HELPER"
	helperRun    = "run"
	helperReport = "report"

	// envLockPath names the lock file for both helper modes; the reporter
	// compares its open descriptors against it.
	envLockPath = "INSTALL_LOCK_TEST_PATH"
	// envReportExit is the status the reporter exits with once its stdin
	// closes.
	envReportExit = "INSTALL_LOCK_TEST_EXIT"
)

// TestMain runs the helper programs when this binary is re-run as one, and the
// integration tier's hermetic harness otherwise.
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case helperRun:
		os.Exit(runAsInstallLock())
	case helperReport:
		os.Exit(runAsInstall())
	}
	os.Exit(testutil.HermeticMain(m))
}

// runAsInstallLock is the install-lock command: argv is
// <lock path> <wait in ms> <marker> <command...>. The command it execs is this
// same binary in report mode.
func runAsInstallLock() int {
	a := os.Args[1:]
	waitMS, err := strconv.Atoi(a[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad wait:", err)
		return 2
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, helperEnv+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, helperEnv+"="+helperReport)
	r := runner{now: time.Now, sleep: time.Sleep, exec: execAndName(inheritable())}
	return r.run(Config{
		LockPath: a[0],
		Wait:     time.Duration(waitMS) * time.Millisecond,
		Marker:   a[2],
		Command:  a[3:],
		Env:      env,
		Stderr:   os.Stderr,
	})
}

// inheritable returns the descriptors above stderr that are not close-on-exec.
func inheritable() map[int]bool {
	out := map[int]bool{}
	for fd := 3; fd < 256; fd++ {
		if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil && flags&unix.FD_CLOEXEC == 0 {
			out[fd] = true
		}
	}
	return out
}

// execAndName returns the real exec, preceded by a line on stdout for each
// descriptor it is about to hand on that was not already inheritable when this
// program started (before): the descriptors Run itself added. A descriptor the
// parent passed down, such as one a race-detector build leaves open, is not
// Run's doing.
func execAndName(before map[int]bool) func(path string, argv, env []string) error {
	return func(path string, argv, env []string) error {
		lock := os.Getenv(envLockPath)
		var lockStat unix.Stat_t
		_ = unix.Stat(lock, &lockStat)
		for fd := range inheritable() {
			if before[fd] {
				continue
			}
			var st unix.Stat_t
			if unix.Fstat(fd, &st) != nil {
				continue
			}
			fmt.Printf("handed-on fd=%d lock=%t\n", fd, st.Dev == lockStat.Dev && st.Ino == lockStat.Ino)
		}
		return syscall.Exec(path, argv, env)
	}
}

// runAsInstall is the install: it reports what it inherited, says it is ready,
// and stays alive until its stdin closes. The descriptors are listed first,
// before this program touches any file of its own.
func runAsInstall() int {
	lock := os.Getenv(envLockPath)
	var lockStat unix.Stat_t
	if err := unix.Stat(lock, &lockStat); err != nil {
		fmt.Fprintln(os.Stderr, "stat lock:", err)
		return 2
	}
	var fds []string
	for fd := 3; fd < 256; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			continue
		}
		fds = append(fds, fmt.Sprintf("fd=%d lock=%t", fd, st.Dev == lockStat.Dev && st.Ino == lockStat.Ino))
	}
	w := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(w, "pid=%d\n", os.Getpid())
	fmt.Fprintf(w, "locked=%s\n", os.Getenv("INSTALL_GT_LOCKED"))
	for _, f := range fds {
		fmt.Fprintln(w, f)
	}
	fmt.Fprintln(w, "ready")
	w.Flush()

	buf := make([]byte, 1)
	for {
		n, err := unix.Read(0, buf)
		if n == 0 || (err != nil && !errors.Is(err, unix.EINTR)) {
			break
		}
	}
	code, _ := strconv.Atoi(os.Getenv(envReportExit))
	return code
}

func hasLockFd(fds []string) bool {
	for _, fd := range fds {
		if strings.HasSuffix(fd, "lock=true") {
			return true
		}
	}
	return false
}

// The process-level tests below run the real thing: this test binary is both
// the install-lock command and the install it execs.

// proc is a started install-lock process. Its stdout is read through out and
// its stdin closes the install it became.
type proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Scanner
}

// report is what the install printed about itself.
type report struct {
	pid    string
	locked string
	// handedOn is what the helper passed across the exec; fds is what the
	// install found open once it was running.
	handedOn []string
	fds      []string
}

// start runs the install-lock command on lock with the given wait, marker and
// exit status for the install. A test that does not call wait is ended by the
// context, so no test can hang.
func start(t *testing.T, lock string, wait time.Duration, marker, installExit string) *proc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, self(t), lock, strconv.Itoa(int(wait.Milliseconds())), marker, self(t))
	cmd.Env = append(os.Environ(), helperEnv+"="+helperRun, envLockPath+"="+lock, envReportExit+"="+installExit)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &proc{cmd: cmd, stdin: stdin, out: bufio.NewScanner(stdout)}
}

// ready reads the install's report up to its ready line.
func (p *proc) ready(t *testing.T) report {
	t.Helper()
	var r report
	for p.out.Scan() {
		line := p.out.Text()
		switch {
		case line == "ready":
			return r
		case strings.HasPrefix(line, "pid="):
			r.pid = strings.TrimPrefix(line, "pid=")
		case strings.HasPrefix(line, "locked="):
			r.locked = strings.TrimPrefix(line, "locked=")
		case strings.HasPrefix(line, "handed-on "):
			r.handedOn = append(r.handedOn, strings.TrimPrefix(line, "handed-on "))
		case strings.HasPrefix(line, "fd="):
			r.fds = append(r.fds, line)
		}
	}
	t.Fatalf("the install never reported ready (scan error: %v)", p.out.Err())
	return r
}

// exitCode waits for the process and returns its exit status.
func (p *proc) exitCode(t *testing.T) int {
	t.Helper()
	err := p.cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("waiting for the process: %v", err)
		return -1
	}
}

func TestIntegrationTheInstallBecomesTheLockHolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, marker := filepath.Join(dir, "install-gt.lock"), filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := start(t, lock, 0, marker, "0")

	r := p.ready(t)

	if r.pid != strconv.Itoa(p.cmd.Process.Pid) {
		t.Errorf("the install is pid %s but the process started was %d: Run did not replace the process", r.pid, p.cmd.Process.Pid)
	}
	if r.locked != "1" {
		t.Errorf("the install saw INSTALL_GT_LOCKED=%q, want 1", r.locked)
	}
	if len(r.handedOn) != 1 || !strings.HasSuffix(r.handedOn[0], "lock=true") {
		t.Errorf("descriptors handed across the exec = %v, want exactly one, the lock file", r.handedOn)
	}
	if !hasLockFd(r.fds) {
		t.Errorf("the install has no open descriptor on the lock file: %v", r.fds)
	}
	if fileExists(marker) {
		t.Error("the marker survives a command that started")
	}
	if lockIsFree(t, lock) {
		t.Error("the lock is free while the install runs")
	}
	p.stdin.Close()
	if code := p.exitCode(t); code != 0 {
		t.Errorf("the install exited %d, want 0", code)
	}
	if !lockIsFree(t, lock) {
		t.Error("the lock is still held after the install exited")
	}
}

func TestIntegrationKillingTheInstallReleasesTheLock(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	p := start(t, lock, 0, "", "0")
	p.ready(t)
	if lockIsFree(t, lock) {
		t.Fatal("the lock is free while the install runs")
	}

	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = p.cmd.Wait()

	if !lockIsFree(t, lock) {
		t.Error("the lock is still held after the install was killed: something besides the install holds it")
	}
}

func TestIntegrationSecondInstallProcessExitsLockBusyWhileTheFirstRuns(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")
	marker := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	first := start(t, lock, 0, "", "0")
	first.ready(t)

	second := start(t, lock, 0, marker, "0")
	// Closing its stdin ends it at once if it wrongly got the lock and became an
	// install, so a failure here is fast, not a wait for the context.
	second.stdin.Close()

	if code := second.exitCode(t); code != ExitLockBusy {
		t.Errorf("the second install exited %d while the first held the lock, want %d", code, ExitLockBusy)
	}
	if !fileExists(marker) {
		t.Error("the second install's marker was removed though it never started")
	}
}

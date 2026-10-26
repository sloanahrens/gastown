//go:build integration

package imagebuild

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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
// need, selected by IMAGEBUILD_HELPER: "supervise" is the supervisor, and
// "build" is the build it runs. Both skip the integration harness.
const (
	helperEnv       = "IMAGEBUILD_HELPER"
	helperSupervise = "supervise"
	helperBuild     = "build"

	// envBuild picks what the build does: exit, selfkill, tree, ignore-term,
	// sleep, report-fds.
	envBuild = "IMAGEBUILD_BUILD"
	// envCode is the status the build exits with.
	envCode = "IMAGEBUILD_CODE"
	// envLock is the lock directory the supervisor guards and the build makes.
	envLock = "IMAGEBUILD_LOCK"
	// envLockPid, when set, is the pid the build writes into the lock instead of
	// its own: a lock that belongs to another process.
	envLockPid = "IMAGEBUILD_LOCK_PID"
	// envPids is the file the build writes the pid of its child to.
	envPids = "IMAGEBUILD_PIDS"
	// envDetach makes the supervisor detach first, as the command does.
	envDetach = "IMAGEBUILD_DETACH"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case helperSupervise:
		os.Exit(runAsSupervisor())
	case helperBuild:
		os.Exit(runAsBuild())
	}
	os.Exit(testutil.HermeticMain(m))
}

// runAsSupervisor supervises one build. The alarm goes off when the test writes
// a line to its stdin, so no test depends on how fast the host is.
func runAsSupervisor() int {
	if os.Getenv(envDetach) == "1" {
		Detach()
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "executable:", err)
		return 2
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, helperEnv+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, helperEnv+"="+helperBuild)

	s := realSystem()
	s.termGrace = 300 * time.Millisecond
	s.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time)
		go func() {
			buf := make([]byte, 1)
			for {
				n, err := unix.Read(0, buf)
				if n > 0 || (err != nil && !errors.Is(err, unix.EINTR)) {
					break
				}
			}
			close(ch)
		}()
		return ch
	}
	return s.run(Config{
		Alarm:   time.Hour,
		Lock:    os.Getenv(envLock),
		Command: []string{exe},
		Env:     env,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})
}

// runAsBuild is the build the supervisor runs.
func runAsBuild() int {
	switch os.Getenv(envBuild) {
	case "exit":
		code, _ := strconv.Atoi(os.Getenv(envCode))
		return code
	case "selfkill":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		return 3
	case "sleep":
		time.Sleep(time.Hour)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(time.Hour)
	case "tree":
		return runAsTree()
	case "report-fds":
		for fd := 3; fd < 256; fd++ {
			if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil && flags&unix.FD_CLOEXEC == 0 {
				fmt.Printf("fd=%d\n", fd)
			}
		}
		fmt.Println("done")
	}
	return 0
}

// runAsTree takes a lock the way run.sh does, starts a child in its own group,
// says it is ready and waits to be stopped.
func runAsTree() int {
	lock := os.Getenv(envLock)
	pid := os.Getenv(envLockPid)
	if pid == "" {
		pid = strconv.Itoa(os.Getpid())
	}
	if err := os.Mkdir(lock, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "lock:", err)
		return 2
	}
	if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(pid+"\n"), 0o644); err != nil {
		return 2
	}
	exe, _ := os.Executable()
	child := exec.Command(exe)
	child.Env = append(os.Environ(), envBuild+"=sleep")
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "child:", err)
		return 2
	}
	if err := os.WriteFile(os.Getenv(envPids), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
		return 2
	}
	fmt.Println("ready")
	time.Sleep(time.Hour)
	return 0
}

// supervised is a started supervisor. Its stdout carries the build's lines.
type supervised struct {
	cmd    *exec.Cmd
	stdin  *os.File
	out    *bufio.Scanner
	stderr *strings.Builder
}

// supervise starts the supervisor around a build. extra, when not nil, is
// handed down as fd 3.
func supervise(t *testing.T, env []string, extra *os.File) *supervised {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=" + helperSupervise}, env...)...)
	if extra != nil {
		cmd.ExtraFiles = []*os.File{extra}
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = stdinR
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	stdinR.Close()
	t.Cleanup(func() {
		stdinW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &supervised{cmd: cmd, stdin: stdinW, out: bufio.NewScanner(stdout), stderr: stderr}
}

// waitFor reads the build's output until it prints line.
func (s *supervised) waitFor(t *testing.T, line string) {
	t.Helper()
	for s.out.Scan() {
		if s.out.Text() == line {
			return
		}
	}
	t.Fatalf("the build never printed %q (scan error: %v; supervisor stderr: %s)", line, s.out.Err(), s.stderr.String())
}

// fire goes the alarm off.
func (s *supervised) fire(t *testing.T) {
	t.Helper()
	if _, err := s.stdin.WriteString("fire\n"); err != nil {
		t.Fatalf("firing the alarm: %v", err)
	}
}

// exitCode waits for the supervisor and returns its status.
func (s *supervised) exitCode(t *testing.T) int {
	t.Helper()
	err := s.cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("waiting for the supervisor: %v", err)
		return -1
	}
}

func gone(pid int) bool { return errors.Is(unix.Kill(pid, 0), unix.ESRCH) }

func readPid(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid file %s holds %q", path, data)
	}
	return pid
}

func TestIntegrationAlarmStopsTheBuildAndEverythingItStarted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, pids := filepath.Join(dir, "image.lock"), filepath.Join(dir, "pids")
	sup := supervise(t, []string{envBuild + "=tree", envLock + "=" + lock, envPids + "=" + pids}, nil)
	sup.waitFor(t, "ready")
	child := readPid(t, pids)
	if gone(child) {
		t.Fatal("the build's child is not running before the alarm")
	}

	sup.fire(t)

	if code := sup.exitCode(t); code != ExitAlarm {
		t.Errorf("supervisor exited %d, want %d (stderr: %s)", code, ExitAlarm, sup.stderr.String())
	}
	if !gone(child) {
		t.Errorf("the build's child %d is still running after the alarm reported the group stopped", child)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("the lock the stopped build took is still there")
	}
	if !strings.Contains(sup.stderr.String(), "stopped the build") {
		t.Errorf("supervisor said %q, want that it stopped the build", sup.stderr.String())
	}
}

func TestIntegrationBuildThatIgnoresTermIsKilled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "image.lock")
	sup := supervise(t, []string{envBuild + "=ignore-term", envLock + "=" + lock}, nil)
	sup.waitFor(t, "ready")

	sup.fire(t)

	if code := sup.exitCode(t); code != ExitAlarm {
		t.Errorf("supervisor exited %d, want %d (stderr: %s)", code, ExitAlarm, sup.stderr.String())
	}
	if !strings.Contains(sup.stderr.String(), "stopped the build") {
		t.Errorf("supervisor said %q, want that it stopped the build", sup.stderr.String())
	}
}

func TestIntegrationLockOfAnotherProcessStaysAfterTheStop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, pids := filepath.Join(dir, "image.lock"), filepath.Join(dir, "pids")
	sup := supervise(t, []string{envBuild + "=tree", envLock + "=" + lock, envPids + "=" + pids,
		envLockPid + "=" + strconv.Itoa(os.Getpid())}, nil)
	sup.waitFor(t, "ready")

	sup.fire(t)

	if code := sup.exitCode(t); code != ExitAlarm {
		t.Errorf("supervisor exited %d, want %d", code, ExitAlarm)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("a lock that names another process was removed: %v", err)
	}
}

func TestIntegrationFinishedBuildStatusIsReturned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  []string
		want int
	}{
		{"success", []string{envBuild + "=exit", envCode + "=0"}, 0},
		{"failure", []string{envBuild + "=exit", envCode + "=7"}, 7},
		{"killed by a signal", []string{envBuild + "=selfkill"}, 128 + int(syscall.SIGKILL)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sup := supervise(t, tc.env, nil)
			if got := sup.exitCode(t); got != tc.want {
				t.Errorf("supervisor exited %d, want %d (stderr: %s)", got, tc.want, sup.stderr.String())
			}
		})
	}
}

// Detach closes what the supervisor inherited, so a build it starts does not
// hold the install lock. Without it the same descriptor reaches the build,
// which is how this test knows it can see the difference.
func TestIntegrationDetachKeepsInheritedDescriptorsFromTheBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		detach  string
		wantFd3 bool
	}{
		{"detached", "1", false},
		{"not detached", "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra, extraW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer extra.Close()
			defer extraW.Close()
			sup := supervise(t, []string{envBuild + "=report-fds", envDetach + "=" + tc.detach}, extra)
			var fds []string
			for sup.out.Scan() {
				line := sup.out.Text()
				if line == "done" {
					break
				}
				fds = append(fds, line)
			}
			if code := sup.exitCode(t); code != 0 {
				t.Fatalf("supervisor exited %d (stderr: %s)", code, sup.stderr.String())
			}
			has3 := false
			for _, fd := range fds {
				if fd == "fd=3" {
					has3 = true
				}
			}
			if has3 != tc.wantFd3 {
				t.Errorf("the build inherited %v, want fd 3 present = %v", fds, tc.wantFd3)
			}
			if !tc.wantFd3 && len(fds) != 0 {
				t.Errorf("a detached supervisor's build inherited %v, want none", fds)
			}
		})
	}
}

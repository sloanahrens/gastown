//go:build integration

package forgejoresync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// standInEnv and standInPidEnv let a bounded-call test drive a real process tree
// through the production exec path. The test binary stands in for git: it is
// re-executed as argv[0], starts a child that inherits its stdout and stderr,
// writes the child's pid to standInPidEnv's file, and then holds the call open.
// A stand-in for git rather than real git is deliberate: one case needs a helper
// outside the call's process group, and only a process the test starts can be
// put there deterministically. Real git over a stalling remote is covered by the
// internal/git integration tests.
const (
	standInEnv      = "FORGEJORESYNC_STANDIN"       // "holder", "escape", or "child"; unset outside the stand-in
	standInPidEnv   = "FORGEJORESYNC_STANDIN_PID"   // file the stand-in writes the child's pid to
	standInReadyEnv = "FORGEJORESYNC_STANDIN_READY" // FIFO the stand-in writes the child's pid to once it is up
)

// standInBound is how long a stand-in call runs before the bound ends it. It must
// outlast the stand-in starting its child and writing the pid file, or the call
// ends before the child exists and the test reports "never started" instead of
// testing the bound.
const standInBound = time.Second

// TestIntegrationHelperGitStandIn is not a test on its own: it is the process the bounded-call
// test runs in place of git. It skips when run as a normal test.
func TestIntegrationHelperGitStandIn(t *testing.T) {
	mode := os.Getenv(standInEnv)
	if mode == "" {
		t.Skip("not the git stand-in")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestIntegrationHelperStandInChild$")
	child.Env = append(os.Environ(), standInEnv+"=child")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if mode == "escape" {
		util.SetDetachedProcessGroup(child)
	}
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "stand-in: start child:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Getenv(standInPidEnv), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "stand-in: write pid:", err)
		os.Exit(1)
	}
	if ready := os.Getenv(standInReadyEnv); ready != "" {
		if err := announceChild(ready, child.Process.Pid); err != nil {
			fmt.Fprintln(os.Stderr, "stand-in: announce child:", err)
			os.Exit(1)
		}
	}
	// Hold the call open. The bound ends this process long before this sleep does.
	time.Sleep(time.Minute)
}

// announceChild reports the child's pid to the test over the FIFO it made. The open
// blocks until the test reads, so the write is a handshake: the test knows the child
// is up before it cancels, without polling for the pid file.
func announceChild(fifo string, pid int) error {
	f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, pid)
	return err
}

// TestIntegrationHelperStandInChild is the child the stand-in starts. It holds the pipes the
// call waits on until the test kills it.
func TestIntegrationHelperStandInChild(t *testing.T) {
	if os.Getenv(standInEnv) != "child" {
		t.Skip("not the stand-in child")
	}
	time.Sleep(time.Minute)
}

// killStandInChild kills the child the stand-in recorded, so a case that ends with
// the child still holding the pipe leaves nothing running. A pid already gone is
// not an error.
func killStandInChild(pidFile string) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// TestIntegrationBoundedGitCallReturnsAfterBound pins that a git call whose
// helper keeps the output pipe open returns at its bound, as an error, instead of
// waiting for the helper. The helper is killed with its group when it shares the
// call's group; a helper that left the group is cut off by WaitDelay. Removing
// the group kill turns the first case red, removing WaitDelay turns the second red.
func TestIntegrationBoundedGitCallReturnsAfterBound(t *testing.T) {
	cases := []struct {
		name string
		mode string
		// limit is the most the call may take: its bound, the grace a group kill
		// may use, and a second of slack. The second case allows WaitDelay instead.
		limit time.Duration
	}{
		{
			name:  "helper in the call's group is killed at the bound",
			mode:  "holder",
			limit: standInBound + util.ProcessGroupKillGrace + time.Second,
		},
		{
			name:  "helper outside the call's group is cut off by WaitDelay",
			mode:  "escape",
			limit: standInBound + gitWaitDelay + time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv(standInEnv, tc.mode)
			t.Setenv(standInPidEnv, pidFile)
			t.Cleanup(func() { killStandInChild(pidFile) })

			// The call runs on its own goroutine, so a call that never returns fails
			// this test at its own limit instead of hanging the run.
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				_, err := gitRunner(standInBound)(context.Background(), []string{os.Args[0], "-test.run=^TestIntegrationHelperGitStandIn$"})
				done <- err
			}()
			wait := tc.limit + 5*time.Second
			var err error
			select {
			case err = <-done:
			case <-time.After(wait):
				t.Fatalf("call still running after %v: it did not return at its bound", wait)
			}
			elapsed := time.Since(start)

			if _, statErr := os.Stat(pidFile); statErr != nil {
				t.Fatalf("stand-in never started its child before the bound: %v", statErr)
			}
			if err == nil {
				t.Fatal("the call returned success; a bound that ends a call must surface as an error")
			}
			if elapsed > tc.limit {
				t.Fatalf("call took %v, want at most %v", elapsed.Round(10*time.Millisecond), tc.limit)
			}
			t.Logf("returned in %v (bound %v, limit %v)", elapsed.Round(10*time.Millisecond), standInBound, tc.limit)
		})
	}
}

// awaitStandInChild waits for the stand-in to report its child's pid over the FIFO,
// so the cancel that follows lands on a call that is in flight. The read blocks on
// the stand-in's write, so the wait ends the moment the child is up rather than on a
// poll, and awaitReceive bounds it.
func awaitStandInChild(t *testing.T, fifo string) {
	t.Helper()
	reported := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY, 0)
		if err != nil {
			reported <- err
			return
		}
		defer f.Close()
		var pid int
		_, err = fmt.Fscan(f, &pid)
		reported <- err
	}()
	if err := awaitReceive(t, reported); err != nil {
		t.Fatalf("the stand-in never reported its child's pid: %v", err)
	}
}

// TestIntegrationCanceledRunEndsTheGitCall pins that a signal canceling the run ends
// a git call in flight, with its process group, rather than leaving it to finish. The
// stand-in holds its call for a minute and its child holds the output pipe, which
// only a group kill closes before gitWaitDelay (over three seconds); the call returns
// in under a second, so the cancellation, not the bound or the pipe, ended it.
// Mutation: run the call under context.Background() and it stays up until its bound.
func TestIntegrationCanceledRunEndsTheGitCall(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ready := filepath.Join(dir, "child.ready")
	if err := syscall.Mkfifo(ready, 0o600); err != nil {
		t.Fatalf("make the readiness fifo: %v", err)
	}
	t.Setenv(standInEnv, "holder")
	t.Setenv(standInPidEnv, pidFile)
	t.Setenv(standInReadyEnv, ready)
	t.Cleanup(func() { killStandInChild(pidFile) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// A minute's bound: only the cancellation can end the call in the limit.
		_, err := gitRunner(time.Minute)(ctx, []string{os.Args[0], "-test.run=^TestIntegrationHelperGitStandIn$"})
		done <- err
	}()
	awaitStandInChild(t, ready)
	canceledAt := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the call returned success; a canceled call must surface as an error")
		}
	case <-time.After(standInBound):
		t.Fatalf("the call was still running %v after it was canceled: the run's context did not end it", standInBound)
	}
	if elapsed := time.Since(canceledAt); elapsed > standInBound {
		t.Fatalf("the call took %v to end after cancellation, want under %v", elapsed.Round(10*time.Millisecond), standInBound)
	}
}

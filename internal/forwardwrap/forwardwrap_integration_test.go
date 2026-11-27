//go:build integration

package forwardwrap

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// forwardBound caps each wait; every wait ends as soon as its condition holds.
const forwardBound = 10 * time.Second

// fakeClient answers the get as $FAKE_GET names: "empty" prints nothing,
// "fail" exits 1, and any other value prints the Running pod labeled
// app.kubernetes.io/name=dashboard. Its forward opens $FAKE_HOLD before
// printing its arguments, then blocks on it. Its wait opens $FAKE_WAIT before
// printing its arguments on stderr, then blocks on it.
const fakeClient = `#!/bin/sh
if [ "$3" = get ]; then
  case "$FAKE_GET" in
    empty) exit 0 ;;
    fail) exit 1 ;;
  esac
  [ "$4 $5 $6 $7" = "pods -l app.kubernetes.io/name=dashboard --field-selector=status.phase=Running" ] && echo pod/gt-dashboard-7f9c2
elif [ "$3" = wait ]; then
  exec 4<"$FAKE_WAIT"; printf '%s\n' "$*" >&2; read -r _ <&4
else exec 3<"$FAKE_HOLD"; printf '%s\n' "$*"; read -r _ <&3
fi`

// wrapperRun is one started run of Script against the fake client.
type wrapperRun struct {
	ctx    context.Context
	out    *bufio.Reader // the wrapper's stdout, read with readLine
	errOut *bufio.Reader // the wrapper's stderr, read with readLine
	exited chan error    // the wrapper's Run error, sent once it ends
	// endForward ends the fake forward; it closes the FIFO exactly once.
	endForward func() error
	// endWait ends the fake wait, which kills the forward. Call it only after
	// the wrapper's watcher has reported on stderr: the fake wait's open of the
	// FIFO blocks while no writer holds it.
	endWait func() error
}

// startWrapper writes the fake client, makes the FIFOs its forward and wait
// block on, and starts Script with the positional values, its get answering as
// FAKE_GET selects. The caller reads the wrapper's stdout and stderr with
// readLine.
func startWrapper(t *testing.T, get string) *wrapperRun {
	t.Helper()
	dir := t.TempDir()
	client := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(client, []byte(fakeClient), 0o700); err != nil {
		t.Fatalf("write the fake client: %v", err)
	}
	hold := filepath.Join(dir, "forward.hold")
	if err := syscall.Mkfifo(hold, 0o600); err != nil {
		t.Fatalf("make the FIFO the forward blocks on: %v", err)
	}
	// The test's own read-write open never blocks; closing it ends the forward.
	holdF, err := os.OpenFile(hold, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open the FIFO: %v", err)
	}
	wait := filepath.Join(dir, "wait.hold")
	if err := syscall.Mkfifo(wait, 0o600); err != nil {
		t.Fatalf("make the FIFO the wait blocks on: %v", err)
	}
	// Left unclosed until the test ends the wait: the fake wait's open of it
	// blocks while no writer holds it.
	waitF, err := os.OpenFile(wait, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open the wait FIFO: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("make the wrapper's stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("make the wrapper's stderr pipe: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), forwardBound)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", Script, "sh", client, "ctx", "ns", "dashboard", "127.0.0.1", "8080")
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Env = append(os.Environ(), "FAKE_HOLD="+hold, "FAKE_WAIT="+wait, "FAKE_GET="+get)
	// Its own process group: the cleanup's cancel kills every member of it.
	util.SetProcessGroup(cmd)
	exited := make(chan error, 1)
	go func() {
		err := cmd.Run()
		stdoutW.Close()
		stderrW.Close()
		exited <- err
	}()

	forwardEnded, waitEnded := false, false
	t.Cleanup(func() {
		if !forwardEnded {
			holdF.Close()
		}
		if !waitEnded {
			waitF.Close()
		}
		stdoutR.Close()
		stderrR.Close()
	})
	return &wrapperRun{
		ctx:    ctx,
		out:    bufio.NewReader(stdoutR),
		errOut: bufio.NewReader(stderrR),
		exited: exited,
		endForward: func() error {
			forwardEnded = true
			return holdF.Close()
		},
		endWait: func() error {
			waitEnded = true
			return waitF.Close()
		},
	}
}

// TestIntegrationForwardWrapper runs Script under /bin/sh against a fake client:
// its one forward goes to the pod the fake get printed, with the positional
// values, and the wrapper waits for it and then exits non-zero. A forward that
// ends by itself ends the wrapper even while the fake wait blocks.
func TestIntegrationForwardWrapper(t *testing.T) {
	run := startWrapper(t, "")

	forward := "--context=ctx --namespace=ns port-forward --address=127.0.0.1 pod/gt-dashboard-7f9c2 8080:8080"
	if line, err := readLine(run.ctx, run.out); err != nil {
		t.Fatalf("the wrapper started no forward: %v", err)
	} else if line != forward {
		t.Errorf("the wrapper started %q, want %q", line, forward)
	}
	const watcher = "--context=ctx --namespace=ns wait --for=delete pod/gt-dashboard-7f9c2 --timeout=8760h"
	if line, err := readLine(run.ctx, run.errOut); err != nil {
		t.Fatalf("the wrapper started no watcher: %v", err)
	} else if line != watcher {
		t.Errorf("the wrapper's watcher ran %q, want %q", line, watcher)
	}
	// A wrapper that did not wait has ended here, while its forward still runs.
	select {
	case err := <-run.exited:
		t.Fatalf("the wrapper exited before its forward ended: %v", err)
	default:
	}
	// Ending the forward must end the wrapper non-zero, so KeepAlive restarts it.
	if err := run.endForward(); err != nil {
		t.Fatalf("end the forward: %v", err)
	}
	select {
	case err := <-run.exited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Errorf("the wrapper ended %v; want exit status 1 after its forward ended", err)
		}
	case <-run.ctx.Done():
		t.Fatalf("the wrapper did not exit after its forward ended: %v", run.ctx.Err())
	}
	// The watcher still holds the wrapper's stdout, so end its wait first.
	if err := run.endWait(); err != nil {
		t.Fatalf("end the wait: %v", err)
	}
	// One forward: nothing more may reach the wrapper's stdout.
	if line, err := readLine(run.ctx, run.out); err == nil || !errors.Is(err, io.EOF) {
		t.Errorf("the wrapper's stdout after its one forward = %q, %v; want end of file", line, err)
	}
}

// TestIntegrationForwardWrapperEndsWithThePod runs Script against the fake
// client and lets the fake wait return: the watcher kills the forward, so the
// wrapper ends non-zero as it does when a roll deletes the pod.
func TestIntegrationForwardWrapperEndsWithThePod(t *testing.T) {
	run := startWrapper(t, "")

	forward := "--context=ctx --namespace=ns port-forward --address=127.0.0.1 pod/gt-dashboard-7f9c2 8080:8080"
	if line, err := readLine(run.ctx, run.out); err != nil {
		t.Fatalf("the wrapper started no forward: %v", err)
	} else if line != forward {
		t.Errorf("the wrapper started %q, want %q", line, forward)
	}
	const watcher = "--context=ctx --namespace=ns wait --for=delete pod/gt-dashboard-7f9c2 --timeout=8760h"
	if line, err := readLine(run.ctx, run.errOut); err != nil {
		t.Fatalf("the wrapper started no watcher: %v", err)
	} else if line != watcher {
		t.Errorf("the wrapper's watcher ran %q, want %q", line, watcher)
	}
	// The pod is deleted: the fake wait returns, and the watcher kills the forward.
	if err := run.endWait(); err != nil {
		t.Fatalf("end the wait: %v", err)
	}
	select {
	case err := <-run.exited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Errorf("the wrapper ended %v; want exit status 1 after its pod was deleted", err)
		}
	case <-run.ctx.Done():
		t.Fatalf("the wrapper did not exit after its pod was deleted: %v", run.ctx.Err())
	}
}

// TestIntegrationForwardWrapperRefuses runs Script against a get that prints
// nothing and a get that fails. Each must end the script non-zero before it
// starts a forward, with one line on stderr naming the selector.
func TestIntegrationForwardWrapperRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		get  string
	}{
		{name: "empty get", get: "empty"},
		{name: "failing get", get: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := startWrapper(t, tc.get)

			// No forward started, so the wrapper's stdout holds nothing and ends.
			if line, err := readLine(run.ctx, run.out); line != "" || !errors.Is(err, io.EOF) {
				t.Errorf("the wrapper's stdout = %q, %v; want an empty line and end of file", line, err)
			}
			const want = "no Running pod for app.kubernetes.io/name=dashboard"
			if line, err := readLine(run.ctx, run.errOut); err != nil || line != want {
				t.Errorf("the wrapper's stderr = %q, %v; want %q", line, err, want)
			}
			if line, err := readLine(run.ctx, run.errOut); err == nil || !errors.Is(err, io.EOF) {
				t.Errorf("the wrapper's stderr after its one line = %q, %v; want end of file", line, err)
			}
			select {
			case err := <-run.exited:
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Errorf("the wrapper ended %v; want exit status 1", err)
				}
			case <-run.ctx.Done():
				t.Fatalf("the wrapper did not exit: %v", run.ctx.Err())
			}
		})
	}
}

// readLine reads one line from the shared reader, bounded by ctx.
func readLine(ctx context.Context, r *bufio.Reader) (string, error) {
	type reply struct {
		line string
		err  error
	}
	got := make(chan reply, 1)
	go func() {
		line, err := r.ReadString('\n')
		got <- reply{strings.TrimSuffix(line, "\n"), err}
	}()
	select {
	case res := <-got:
		return res.line, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

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

// fakeClient answers a get for the Running pod labeled
// app.kubernetes.io/name=dashboard, and nothing for any other get. Its forward
// opens $FAKE_HOLD before printing its arguments, then blocks on it.
const fakeClient = `#!/bin/sh
if [ "$3" = get ]; then
  [ "$4 $5 $6 $7" = "pods -l app.kubernetes.io/name=dashboard --field-selector=status.phase=Running" ] && echo pod/gt-dashboard-7f9c2
else exec 3<"$FAKE_HOLD"; printf '%s\n' "$*"; read -r _ <&3
fi`

// TestIntegrationForwardWrapper runs Script under /bin/sh against a fake client:
// its one forward goes to the pod the fake get printed, with the positional
// values, and the wrapper waits for it and then exits non-zero.
func TestIntegrationForwardWrapper(t *testing.T) {
	client := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(client, []byte(fakeClient), 0o700); err != nil {
		t.Fatalf("write the fake client: %v", err)
	}
	hold := filepath.Join(t.TempDir(), "forward.hold")
	if err := syscall.Mkfifo(hold, 0o600); err != nil {
		t.Fatalf("make the FIFO the forward blocks on: %v", err)
	}
	// The test's own read-write open never blocks; closing it ends the forward.
	holdF, err := os.OpenFile(hold, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open the FIFO: %v", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("make the wrapper's stdout pipe: %v", err)
	}
	defer func() { holdF.Close(); stdoutR.Close() }()
	out := bufio.NewReader(stdoutR)

	ctx, cancel := context.WithTimeout(t.Context(), forwardBound)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", Script, "sh", client, "ctx", "ns", "dashboard", "127.0.0.1", "8080")
	cmd.Stdout = stdoutW
	cmd.Env = append(os.Environ(), "FAKE_HOLD="+hold)
	// Its own process group: the cleanup's cancel kills every member of it.
	util.SetProcessGroup(cmd)
	exited := make(chan error, 1)
	go func() {
		err := cmd.Run()
		stdoutW.Close()
		exited <- err
	}()

	forward := "--context=ctx --namespace=ns port-forward --address=127.0.0.1 pod/gt-dashboard-7f9c2 8080:8080"
	if line, err := readLine(ctx, out); err != nil {
		t.Fatalf("the wrapper started no forward: %v", err)
	} else if line != forward {
		t.Errorf("the wrapper started %q, want %q", line, forward)
	}
	// A wrapper that did not wait has ended here, while its forward still runs.
	select {
	case err := <-exited:
		t.Fatalf("the wrapper exited before its forward ended: %v", err)
	default:
	}
	// Ending the forward must end the wrapper non-zero, so KeepAlive restarts it.
	if err := holdF.Close(); err != nil {
		t.Fatalf("end the forward: %v", err)
	}
	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Errorf("the wrapper ended %v; want exit status 1 after its forward ended", err)
		}
	case <-ctx.Done():
		t.Fatalf("the wrapper did not exit after its forward ended: %v", ctx.Err())
	}
	// One forward: nothing more may reach the wrapper's stdout.
	if line, err := readLine(ctx, out); err == nil || !errors.Is(err, io.EOF) {
		t.Errorf("the wrapper's stdout after its one forward = %q, %v; want end of file", line, err)
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

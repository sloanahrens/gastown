//go:build integration

package cmd

import (
	"context"
	"strings"
	"testing"
	"time"
)

// podExecBound ends a child that hangs, so no row here waits on the clock: the
// children below all return at once.
const podExecBound = time.Minute

func podExecContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), podExecBound)
	t.Cleanup(cancel)
	return ctx
}

// TestIntegrationProductionPodExecKeepsStderrApart runs a real child that writes
// to stderr, echoes its stdin and prints a last line: the reply is the stdin and
// that line, with none of the stderr text in it, which is what keeps a kubectl
// warning out of a hold script's one-word reply.
func TestIntegrationProductionPodExecKeepsStderrApart(t *testing.T) {
	t.Parallel()
	stdout, exit, err := productionPodExec(podExecContext(t), []string{"sh", "-c", "echo warning >&2; cat; echo done"}, []byte("in\n"))
	if err != nil || exit != 0 {
		t.Fatalf("productionPodExec = exit %d, err %v; want exit 0 and no error", exit, err)
	}
	if got, want := string(stdout), "in\ndone\n"; got != want {
		t.Errorf("stdout = %q, want %q: stdin, then the last line, no stderr text", got, want)
	}
}

// TestIntegrationProductionPodExecFailureNamesStderr covers a child that exits non-zero and a
// command that cannot start: the first is its status with the stdout so far and an
// error that names its stderr, the second an error naming the command.
func TestIntegrationProductionPodExecFailureNamesStderr(t *testing.T) {
	t.Parallel()
	stdout, exit, err := productionPodExec(podExecContext(t), []string{"sh", "-c", "echo partial; echo refused >&2; exit 3"}, nil)
	if exit != 3 || string(stdout) != "partial\n" || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("productionPodExec = %q, exit %d, err %v; want %q, exit 3 and an error naming refused", stdout, exit, err, "partial\n")
	}
	const missing = "gt-no-such-command-4775kx"
	_, _, err = productionPodExec(podExecContext(t), []string{missing}, nil)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("a command that cannot start gave err %v, want one naming %q", err, missing)
	}
}

// TestIntegrationProductionPodExecRefusals covers the three ends that are errors
// and not an exit status: no argv, a context that is already done, and a child
// killed by a signal (which has no exit status, so -1 must not read as one).
func TestIntegrationProductionPodExecRefusals(t *testing.T) {
	t.Parallel()
	if _, _, err := productionPodExec(podExecContext(t), nil, nil); err == nil {
		t.Error("an empty argv gave no error")
	}
	done, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := productionPodExec(done, []string{"sh", "-c", "echo x"}, nil); err == nil || !strings.Contains(err.Error(), "sh") {
		t.Errorf("a done context gave err %v, want one naming sh", err)
	}
	stdout, exit, err := productionPodExec(podExecContext(t), []string{"sh", "-c", "echo partial; kill -9 $$"}, nil)
	if err == nil || !strings.Contains(err.Error(), "sh") || exit != 0 || stdout != nil {
		t.Errorf("a signalled child gave %q, exit %d, err %v; want no output, exit 0 and an error naming sh", stdout, exit, err)
	}
}

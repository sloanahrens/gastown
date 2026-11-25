//go:build integration

package cmd

import (
	"context"
	"strings"
	"testing"
)

// TestIntegrationProductionPodExecKeepsStderrApart runs a real child that writes
// to stderr, echoes its stdin and prints a last line: the reply is the stdin and
// that line, with none of the stderr text in it, which is what keeps a kubectl
// warning out of a hold script's one-word reply.
func TestIntegrationProductionPodExecKeepsStderrApart(t *testing.T) {
	t.Parallel()
	stdout, exit, err := productionPodExec(context.Background(), []string{"sh", "-c", "echo warning >&2; cat; echo done"}, []byte("in\n"))
	if err != nil || exit != 0 {
		t.Fatalf("productionPodExec = exit %d, err %v; want exit 0 and no error", exit, err)
	}
	if got, want := string(stdout), "in\ndone\n"; got != want {
		t.Errorf("stdout = %q, want %q: stdin, then the last line, no stderr text", got, want)
	}
}

// TestIntegrationProductionPodExecExit covers the two failures: a child that
// exits non-zero is its status with the stdout so far and no error, and a command
// that cannot start is an error naming it.
func TestIntegrationProductionPodExecExit(t *testing.T) {
	t.Parallel()
	stdout, exit, err := productionPodExec(context.Background(), []string{"sh", "-c", "echo partial; exit 3"}, nil)
	if err != nil || exit != 3 || string(stdout) != "partial\n" {
		t.Errorf("productionPodExec = %q, exit %d, err %v; want %q, exit 3, no error", stdout, exit, err, "partial\n")
	}
	const missing = "gt-no-such-command-4775kx"
	_, _, err = productionPodExec(context.Background(), []string{missing}, nil)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("a command that cannot start gave err %v, want one naming %q", err, missing)
	}
}

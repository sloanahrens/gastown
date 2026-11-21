package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// controlClusterExecCall runs the landing worker's control-cluster seam once,
// as the gate run recorder reads the runner's cpu.stat through it.
func controlClusterExecCall(t *testing.T, d *Daemon) (string, error) {
	t.Helper()
	return d.controlClusterExec()(context.Background(), runnerThrottleArgs...)
}

// TestControlClusterExecNamesTheFailingCause: a kubectl run that fails keeps
// the first line of its stderr in the error, so the gate run recorder's log
// line names the cause, and only the first line enters it.
func TestControlClusterExecNamesTheFailingCause(t *testing.T) {
	t.Parallel()
	const first = "Error from server (Forbidden): pods is forbidden"
	const second = "the second stderr line must not reach the error"
	cli := newFakeCLIFor(func(cliCall) cliReply {
		return cliReply{stderr: first + "\n" + second + "\n", code: 1}
	})
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))

	got, err := controlClusterExecCall(t, d)
	if err == nil {
		t.Fatalf("controlClusterExec = %q, want an error from the failed kubectl", got)
	}
	if !strings.Contains(err.Error(), "kubectl on the control cluster: exit status 1: "+first) {
		t.Errorf("error = %q, want it to carry kubectl's first stderr line %q", err, first)
	}
	if strings.Contains(err.Error(), second) {
		t.Errorf("error = %q, want only the first stderr line", err)
	}
}

// TestControlClusterExecCutsLongStderrOnARuneBoundary: a stderr first line
// longer than 200 bytes is cut at the last rune start at or before byte 200,
// so a multi-byte character is never split in the error.
func TestControlClusterExecCutsLongStderrOnARuneBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			// The two-byte character starts at byte 199, so a plain cut at 200
			// would leave its first byte alone in the error.
			name:   "the cut falls inside a rune",
			stderr: strings.Repeat("a", 199) + "é" + strings.Repeat("b", 50),
			want:   strings.Repeat("a", 199),
		},
		{
			// The cut lands exactly between runes, so all 200 bytes are kept.
			name:   "the cut falls on a rune boundary",
			stderr: strings.Repeat("a", 200) + "é" + strings.Repeat("b", 50),
			want:   strings.Repeat("a", 200),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli := newFakeCLIFor(func(cliCall) cliReply {
				return cliReply{stderr: tt.stderr + "\n", code: 1}
			})
			d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))

			_, err := controlClusterExecCall(t, d)
			if err == nil {
				t.Fatal("controlClusterExec: want an error from the failed kubectl, got none")
			}
			if !strings.HasSuffix(err.Error(), "kubectl on the control cluster: exit status 1: "+tt.want) {
				t.Errorf("error = %q, want it to end with the first %d bytes of stderr", err, len(tt.want))
			}
			if strings.Contains(err.Error(), "é") {
				t.Errorf("error = %q, want the multi-byte character cut off whole", err)
			}
		})
	}
}

// TestControlClusterExecKeepsItsOtherAnswers: a successful run returns
// kubectl's stdout, and a town with no control cluster refuses before running
// kubectl, with the same error it returned before the stderr line was kept.
func TestControlClusterExecKeepsItsOtherAnswers(t *testing.T) {
	t.Parallel()
	const stat = "nr_periods 40\nnr_throttled 7\nthrottled_usec 3100\n"
	cli := newFakeCLIFor(func(cliCall) cliReply { return cliReply{stdout: stat} })
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
	got, err := controlClusterExecCall(t, d)
	if err != nil {
		t.Fatalf("controlClusterExec: %v", err)
	}
	if got != stat {
		t.Errorf("controlClusterExec = %q, want kubectl's stdout %q", got, stat)
	}

	t.Run("an empty context is refused", func(t *testing.T) {
		t.Parallel()
		cli := newFakeCLIFor(func(cliCall) cliReply { return cliReply{} })
		d := nodeDiskDaemon(t, cli, nodeDiskPatrol(""))
		if _, err := controlClusterExecCall(t, d); !errors.Is(err, errCICacheTrimNoContext) {
			t.Errorf("error = %v, want %v", err, errCICacheTrimNoContext)
		}
		if calls := cli.recorded(); len(calls) != 0 {
			t.Errorf("kubectl ran %d times, want none", len(calls))
		}
	})

	t.Run("no kubernetes runner is refused", func(t *testing.T) {
		t.Parallel()
		cli := newFakeCLIFor(func(cliCall) cliReply { return cliReply{} })
		d := nodeDiskDaemon(t, cli, nil)
		_, err := controlClusterExecCall(t, d)
		if err == nil || !strings.Contains(err.Error(), "no kubernetes runner names a control cluster") {
			t.Errorf("error = %v, want the no-control-cluster refusal", err)
		}
		if calls := cli.recorded(); len(calls) != 0 {
			t.Errorf("kubectl ran %d times, want none", len(calls))
		}
	})
}

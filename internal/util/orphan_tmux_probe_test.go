//go:build !windows

package util

import (
	"errors"
	"testing"
)

// exitCodeErr is an error that carries a tmux exit status, as *exec.ExitError
// does, without starting a process.
type exitCodeErr struct{ code int }

func (e exitCodeErr) Error() string { return "exit status 1" }
func (e exitCodeErr) ExitCode() int { return e.code }

// With no tmux server, the probe's answer is errTmuxNoServer, which the sweep
// reports as nothing (gt-r2kvf).
func TestClassifyTmuxProbeNoServerIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, out := range []string{
		"no server running on /private/tmp/tmux-501/default\n",
		"error connecting to /private/tmp/tmux-501/default (No such file or directory)\n",
	} {
		err := classifyTmuxProbe([]byte(out), exitCodeErr{1})
		if !errors.Is(err, errTmuxNoServer) {
			t.Errorf("classifyTmuxProbe(%q) = %v, want errTmuxNoServer", out, err)
		}
	}
}

// A tmux failure that is not "no server" is still reported, and so is a probe
// that succeeded.
func TestClassifyTmuxProbeOtherFailuresStillReport(t *testing.T) {
	t.Parallel()
	other := exitCodeErr{1}
	if err := classifyTmuxProbe([]byte("permission denied\n"), other); err == nil || errors.Is(err, errTmuxNoServer) {
		t.Errorf("other failure = %v, want it reported", err)
	}
	if err := classifyTmuxProbe(nil, nil); err != nil {
		t.Errorf("successful probe = %v, want nil", err)
	}
}

// A tmux binary that is missing has no exit status, so it is reported.
func TestClassifyTmuxProbeMissingBinaryStillReports(t *testing.T) {
	t.Parallel()
	err := classifyTmuxProbe(nil, errors.New("exec: \"tmux\": executable file not found in $PATH"))
	if err == nil || errors.Is(err, errTmuxNoServer) {
		t.Errorf("missing binary = %v, want it reported", err)
	}
}

// The sweep's guard turns no server into an empty protected set and no
// warning, and any other probe failure into the "tmux not available" error.
func TestSweepPaneGuardNoServerIsSilent(t *testing.T) {
	t.Parallel()
	d := sweepDeps{
		tmuxPanes: func() map[int]bool { return nil },
		tmuxProbe: func() error { return errTmuxNoServer },
	}
	pids, err := sweepPaneGuard(d)
	if err != nil || pids != nil {
		t.Errorf("guard with no server = %v, %v; want nil, nil", pids, err)
	}

	d.tmuxProbe = func() error { return errors.New("binary gone") }
	if _, err := sweepPaneGuard(d); err == nil {
		t.Error("guard with a real probe failure returned no error")
	}
}

//go:build !windows

// The sweep is unix-only, like orphan.go it tests, so this file takes the same
// constraint.

package util

import (
	"errors"
	"testing"
)

// exitCodeErr is an error that carries a tmux exit status, as *exec.ExitError
// does. It stands in for tmux's own error without starting a process: the
// classifier reads the status through that method, not the concrete type.
type exitCodeErr struct{ code int }

func (e exitCodeErr) Error() string { return "exit status 1" }
func (e exitCodeErr) ExitCode() int { return e.code }

// With no tmux server, the probe's answer is ErrTmuxNoServer, which the sweep
// reports as nothing (gt-r2kvf).
func TestClassifyTmuxProbeNoServerIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, out := range []string{
		"no server running on /private/tmp/tmux-501/default\n",
		"error connecting to /private/tmp/tmux-501/default (No such file or directory)\n",
	} {
		err := classifyTmuxProbe([]byte(out), exitCodeErr{1})
		if !errors.Is(err, ErrTmuxNoServer) {
			t.Errorf("classifyTmuxProbe(%q) = %v, want ErrTmuxNoServer", out, err)
		}
	}
}

// A tmux failure that is not "no server" is still reported, and so is a probe
// that succeeded.
func TestClassifyTmuxProbeOtherFailuresStillReport(t *testing.T) {
	t.Parallel()
	if err := classifyTmuxProbe([]byte("permission denied\n"), exitCodeErr{1}); err == nil || errors.Is(err, ErrTmuxNoServer) {
		t.Errorf("other failure = %v, want it reported", err)
	}
	if err := classifyTmuxProbe(nil, nil); err != nil {
		t.Errorf("successful probe = %v, want nil", err)
	}
}

// A tmux binary that is missing has no exit status, so it is reported.
func TestClassifyTmuxProbeMissingBinaryStillReports(t *testing.T) {
	t.Parallel()
	err := classifyTmuxProbe(nil, errors.New(`exec: "tmux": executable file not found in $PATH`))
	if err == nil || errors.Is(err, ErrTmuxNoServer) {
		t.Errorf("missing binary = %v, want it reported", err)
	}
}

// With no server the sweep refuses to signal and says so with the sentinel; a
// healthy empty town is not refused, and any other probe failure is an error.
func TestSweepPaneGuardNoServerRefusesWithTheSentinel(t *testing.T) {
	t.Parallel()
	d := sweepDeps{
		tmuxPanes: func() map[int]bool { return nil },
		tmuxProbe: func() error { return ErrTmuxNoServer },
	}
	pids, err := sweepPaneGuard(d)
	if pids != nil || !errors.Is(err, ErrTmuxNoServer) {
		t.Errorf("guard with no server = %v, %v; want nil and ErrTmuxNoServer", pids, err)
	}

	d.tmuxProbe = func() error { return nil }
	if pids, err := sweepPaneGuard(d); pids != nil || err != nil {
		t.Errorf("guard with a healthy empty town = %v, %v; want nil, nil", pids, err)
	}

	d.tmuxProbe = func() error { return errors.New("binary gone") }
	if _, err := sweepPaneGuard(d); err == nil || errors.Is(err, ErrTmuxNoServer) {
		t.Errorf("guard with a real probe failure = %v, want it reported", err)
	}
}

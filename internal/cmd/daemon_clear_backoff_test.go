package cmd

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// The three notes clear-backoff can print. They are spelled out here rather than
// read from daemonReloadPlan, so a change to the wording is a test change the
// author has to look at.
const (
	reloadedNote      = "daemon reloaded"
	notRunningNote    = "daemon not running, will take effect on next start"
	noPidToSignalNote = "daemon reported no pid to signal; the change takes effect when the daemon restarts"
)

// fakeDaemon stands in for the daemon clear-backoff reads and signals. It
// records the pids it was asked to find and signal, so a test can assert the
// caller's pid gate held rather than only that the plan said so.
type fakeDaemon struct {
	running  bool
	pid      int
	stateErr error

	findErr   error
	signalErr error

	found    []int
	signaled []int
}

func (f *fakeDaemon) isRunning() (bool, int, error) {
	return f.running, f.pid, f.stateErr
}

func (f *fakeDaemon) findProcess(pid int) (*os.Process, error) {
	f.found = append(f.found, pid)
	if f.findErr != nil {
		return nil, f.findErr
	}
	return &os.Process{Pid: pid}, nil
}

func (f *fakeDaemon) signal(process *os.Process) error {
	f.signaled = append(f.signaled, process.Pid)
	return f.signalErr
}

func (f *fakeDaemon) deps() daemonReloadDeps {
	return daemonReloadDeps{isRunning: f.isRunning, findProcess: f.findProcess, signal: f.signal}
}

// TestClearBackoffReload pins what the clear-backoff caller does for each daemon
// state. A running daemon can report pid 0 or below — a container town's daemon,
// or a host town whose pid file cannot be read — and 0 names this process group,
// so the signal must be skipped there rather than attempted and failed after the
// backoff is already cleared.
func TestClearBackoffReload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		fake         *fakeDaemon
		wantErr      string
		wantNote     string
		wantFound    []int
		wantSignaled []int
	}{
		{
			name:         "running with a pid is signaled",
			fake:         &fakeDaemon{running: true, pid: 4242},
			wantNote:     reloadedNote,
			wantFound:    []int{4242},
			wantSignaled: []int{4242},
		},
		{
			name:     "running with pid 0 is neither looked up nor signaled",
			fake:     &fakeDaemon{running: true, pid: 0},
			wantNote: noPidToSignalNote,
		},
		{
			name:     "running with a negative pid is neither looked up nor signaled",
			fake:     &fakeDaemon{running: true, pid: -1},
			wantNote: noPidToSignalNote,
		},
		{
			name:     "not running ignores a stale pid and is not signaled",
			fake:     &fakeDaemon{running: false, pid: 4242},
			wantNote: notRunningNote,
		},
		{
			name:    "an unreadable daemon state is an error",
			fake:    &fakeDaemon{stateErr: errors.New("lock unreadable")},
			wantErr: "checking daemon status: lock unreadable",
		},
		{
			name:      "a failed process lookup is an error and signals nothing",
			fake:      &fakeDaemon{running: true, pid: 4242, findErr: errors.New("no such process")},
			wantErr:   "finding daemon process: no such process",
			wantFound: []int{4242},
		},
		{
			name:         "a failed signal is an error",
			fake:         &fakeDaemon{running: true, pid: 4242, signalErr: errors.New("operation not permitted")},
			wantErr:      "signaling daemon to reload: operation not permitted",
			wantFound:    []int{4242},
			wantSignaled: []int{4242},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			err := clearBackoffReload(&out, "gastown/polecats/slit", tt.fake.deps())

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("clearBackoffReload() error = %v, want nil", err)
				}
				for _, want := range []string{"gastown/polecats/slit", tt.wantNote} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("output = %q, want it to contain %q", out.String(), want)
					}
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("clearBackoffReload() error = %v, want %q", err, tt.wantErr)
				}
				if out.Len() != 0 {
					t.Errorf("output = %q, want nothing printed when the reload fails", out.String())
				}
			}

			if !slices.Equal(tt.fake.found, tt.wantFound) {
				t.Errorf("looked-up pids = %v, want %v", tt.fake.found, tt.wantFound)
			}
			if !slices.Equal(tt.fake.signaled, tt.wantSignaled) {
				t.Errorf("signaled pids = %v, want %v", tt.fake.signaled, tt.wantSignaled)
			}
		})
	}
}

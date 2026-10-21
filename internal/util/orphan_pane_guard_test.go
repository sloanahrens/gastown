//go:build !windows

package util

import (
	"errors"
	"syscall"
	"testing"
)

// sweepWorld is the canned machine a sweep runs against: its state files, and
// a record of every signal the sweep sent.
type sweepWorld struct {
	signals []int
	state   map[string]map[int]signalState
}

// fakeSweepDeps builds sweep dependencies over a canned process table and a
// sequence of tmux pane sets: call N uses paneSets[N], and the last set
// repeats. A sweep reads the pane set once to scan and again for the TOCTOU
// re-check, so a second set that is empty models tmux going away in between.
// Nothing here runs ps, tmux or lsof, reads the real state files, or sends a
// real signal.
func fakeSweepDeps(table string, probeErr error, paneSets ...map[int]bool) (sweepDeps, *sweepWorld) {
	entries := parseProcessTable(table)
	calls := 0
	w := &sweepWorld{state: make(map[string]map[int]signalState)}
	d := sweepDeps{
		snapshot: func() ([]processEntry, error) { return entries, nil },
		tmuxPanes: func() map[int]bool {
			if len(paneSets) == 0 {
				return nil
			}
			i := calls
			if i >= len(paneSets) {
				i = len(paneSets) - 1
			}
			calls++
			return paneSets[i]
		},
		tmuxProbe: func() error { return probeErr },
		townRoot:  func(int) string { return "/fake/gt" },
		isIDE:     func(int) bool { return false },
		processTTY: func(int) (string, error) {
			return "??", nil
		},
		signal: func(pid int, _ syscall.Signal) error {
			w.signals = append(w.signals, pid)
			return nil
		},
		loadState: func(file string) map[int]signalState {
			if s, ok := w.state[file]; ok {
				return s
			}
			return make(map[int]signalState)
		},
		saveState: func(file string, state map[int]signalState) error {
			w.state[file] = state
			return nil
		},
	}
	return d, w
}

// strandedClaudeTable is one claude reparented to init with no TTY and an age
// well past minOrphanAge: a genuine orphan whenever the pane set is non-empty.
func strandedClaudeTable() string {
	return psTable(
		"  4100     1 ??       claude   01:00:00",
	)
}

// TestSweepsRefuseEmptyPaneSet is the gt-4ah8h regression. With no tmux pane
// PIDs, the orphan and zombie sweeps must refuse: they signal nothing, and a
// stranded claude is not reported.
func TestSweepsRefuseEmptyPaneSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		probeErr  error
		wantError bool
	}{
		{name: "tmux down", probeErr: errors.New("no server running"), wantError: true},
		{name: "tmux up with no panes", probeErr: nil, wantError: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, w := fakeSweepDeps(strandedClaudeTable(), tt.probeErr, nil)

			orphans, err := findOrphanedClaudeProcesses(d)
			if (err != nil) != tt.wantError {
				t.Errorf("findOrphanedClaudeProcesses() error = %v, wantError %v", err, tt.wantError)
			}
			if len(orphans) != 0 {
				t.Errorf("findOrphanedClaudeProcesses() = %d processes, want 0 (empty pane set must refuse)", len(orphans))
			}

			zombies, err := findZombieClaudeProcesses(d)
			if (err != nil) != tt.wantError {
				t.Errorf("findZombieClaudeProcesses() error = %v, wantError %v", err, tt.wantError)
			}
			if len(zombies) != 0 {
				t.Errorf("findZombieClaudeProcesses() = %d processes, want 0 (empty pane set must refuse)", len(zombies))
			}

			if _, err := cleanupOrphanedClaudeProcesses(d); (err != nil) != tt.wantError {
				t.Errorf("cleanupOrphanedClaudeProcesses() error = %v, wantError %v", err, tt.wantError)
			}
			if _, err := cleanupZombieClaudeProcesses(d); (err != nil) != tt.wantError {
				t.Errorf("cleanupZombieClaudeProcesses() error = %v, wantError %v", err, tt.wantError)
			}

			if len(w.signals) != 0 {
				t.Errorf("sweeps signaled PIDs %v, want none on an empty pane set", w.signals)
			}
		})
	}
}

// TestCleanupRefusesWhenPaneSetEmptiesBeforeSignal drives the TOCTOU re-check
// itself: the scan sees a pane set that does not protect the stranded claude,
// then tmux goes away before the signal, so the re-check's own empty-set
// refusal must stop it. Both cleanup paths run the same re-check.
func TestCleanupRefusesWhenPaneSetEmptiesBeforeSignal(t *testing.T) {
	t.Parallel()
	d, w := fakeSweepDeps(strandedClaudeTable(), nil, map[int]bool{9999: true}, nil)
	if _, err := cleanupOrphanedClaudeProcesses(d); err != nil {
		t.Fatalf("cleanupOrphanedClaudeProcesses() error = %v", err)
	}
	if len(w.signals) != 0 {
		t.Errorf("orphan cleanup signaled PIDs %v, want none after the pane set emptied", w.signals)
	}

	d, w = fakeSweepDeps(strandedClaudeTable(), nil, map[int]bool{9999: true}, nil)
	if _, err := cleanupZombieClaudeProcesses(d); err != nil {
		t.Fatalf("cleanupZombieClaudeProcesses() error = %v", err)
	}
	if len(w.signals) != 0 {
		t.Errorf("zombie cleanup signaled PIDs %v, want none after the pane set emptied", w.signals)
	}
}

// TestIsProcessStillOrphanedRefusesEmptyPaneSet is the table form of the same
// refusal: the TOCTOU helper itself answers "not safe to signal" whenever the
// pane set is unusable, and keeps answering for the live pane set otherwise.
func TestIsProcessStillOrphanedRefusesEmptyPaneSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		probeErr error
		paneSet  map[int]bool
		want     bool
	}{
		{name: "tmux down", probeErr: errors.New("no server running"), want: false},
		{name: "tmux up with no panes", want: false},
		{name: "pane set does not protect the pid", paneSet: map[int]bool{9999: true}, want: true},
		{name: "pane set protects the pid", paneSet: map[int]bool{4100: true}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := fakeSweepDeps(strandedClaudeTable(), tt.probeErr, tt.paneSet)
			if got := isProcessStillOrphaned(d, 4100); got != tt.want {
				t.Errorf("isProcessStillOrphaned(4100) = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSweepsStillSignalStrandedClaudeWithPanes is the control for the refusal:
// a non-empty pane set that does not protect the stranded claude leaves the
// candidate filter unchanged, so both sweeps still find and signal it.
func TestSweepsStillSignalStrandedClaudeWithPanes(t *testing.T) {
	t.Parallel()
	d, w := fakeSweepDeps(strandedClaudeTable(), nil, map[int]bool{9999: true})

	orphans, err := findOrphanedClaudeProcesses(d)
	if err != nil {
		t.Fatalf("findOrphanedClaudeProcesses() error = %v", err)
	}
	if len(orphans) != 1 || orphans[0].PID != 4100 {
		t.Fatalf("findOrphanedClaudeProcesses() = %+v, want PID 4100", orphans)
	}

	zombies, err := findZombieClaudeProcesses(d)
	if err != nil {
		t.Fatalf("findZombieClaudeProcesses() error = %v", err)
	}
	if len(zombies) != 1 || zombies[0].PID != 4100 {
		t.Fatalf("findZombieClaudeProcesses() = %+v, want PID 4100", zombies)
	}

	if _, err := cleanupOrphanedClaudeProcesses(d); err != nil {
		t.Fatalf("cleanupOrphanedClaudeProcesses() error = %v", err)
	}
	if len(w.signals) != 1 || w.signals[0] != 4100 {
		t.Errorf("orphan cleanup signals = %v, want [4100]", w.signals)
	}

	d, w = fakeSweepDeps(strandedClaudeTable(), nil, map[int]bool{9999: true})
	if _, err := cleanupZombieClaudeProcesses(d); err != nil {
		t.Fatalf("cleanupZombieClaudeProcesses() error = %v", err)
	}
	if len(w.signals) != 1 || w.signals[0] != 4100 {
		t.Errorf("zombie cleanup signals = %v, want [4100]", w.signals)
	}
}

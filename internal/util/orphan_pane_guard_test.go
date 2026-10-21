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

// fakeSweepDeps builds sweep dependencies over a canned process table, tmux
// pane set and tmux probe result. Nothing it returns runs ps or tmux, reads
// the state directory, or sends a real signal.
func fakeSweepDeps(table string, panes map[int]bool, probeErr error) (sweepDeps, *sweepWorld) {
	entries := parseProcessTable(table)
	w := &sweepWorld{state: make(map[string]map[int]signalState)}
	d := sweepDeps{
		snapshot:   func() ([]processEntry, error) { return entries, nil },
		tmuxPanes:  func() map[int]bool { return panes },
		tmuxProbe:  func() error { return probeErr },
		townRoot:   func(int) string { return "/fake/gt" },
		isIDE:      func(int) bool { return false },
		processTTY: func(int) (string, error) { return "??", nil },
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
// PIDs, the orphan and zombie sweeps and their TOCTOU re-check must all
// refuse: they signal nothing, and a stranded claude is not reported.
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
			t.Parallel()
			d, w := fakeSweepDeps(strandedClaudeTable(), nil, tt.probeErr)

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

// TestSweepsStillSignalStrandedClaudeWithPanes is the control for the refusal:
// a non-empty pane set that does not protect the stranded claude leaves the
// candidate filter unchanged, so the sweeps still find and signal it.
func TestSweepsStillSignalStrandedClaudeWithPanes(t *testing.T) {
	t.Parallel()
	d, w := fakeSweepDeps(strandedClaudeTable(), map[int]bool{9999: true}, nil)

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
		t.Errorf("signals = %v, want [4100] once with a non-empty pane set", w.signals)
	}
}

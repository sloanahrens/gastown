package cmd

import (
	"slices"
	"testing"
)

// fakePaneProcs models one pane's process tree and records which PIDs a kill
// would signal, so a test asserts on the kill set instead of on real processes.
type fakePaneProcs struct {
	tree   []string // pane process first, then its descendants
	killed []string
}

func (f *fakePaneProcs) KillPaneProcesses(string) error {
	f.killed = append(f.killed, f.tree...)
	return nil
}

func (f *fakePaneProcs) KillPaneProcessesExcluding(_ string, exclude []string) error {
	for _, pid := range f.tree {
		if !slices.Contains(exclude, pid) {
			f.killed = append(f.killed, pid)
		}
	}
	return nil
}

// The respawn path runs gt crew at from inside the pane whose runtime exited:
// the pane shell (100) parents gt (102) beside a leftover runtime (101).
func TestCrewAtRespawnSparesCallerPane(t *testing.T) {
	t.Parallel()
	const paneID = "%7"
	callerChain := []string{"102", "100"} // gt, then the pane process
	cases := []struct {
		name       string
		callerPane string
		wantKilled []string
	}{
		{"caller inside the pane keeps its own chain", paneID, []string{"101"}},
		{"caller outside the pane sweeps the whole tree", "%9", []string{"100", "101", "102"}},
		{"caller outside tmux sweeps the whole tree", "", []string{"100", "101", "102"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakePaneProcs{tree: []string{"100", "101", "102"}}
			if err := killPaneForRespawn(f, paneID, tc.callerPane, callerChain); err != nil {
				t.Fatalf("killPaneForRespawn: %v", err)
			}
			if !slices.Equal(f.killed, tc.wantKilled) {
				t.Errorf("kill set = %v, want %v", f.killed, tc.wantKilled)
			}
		})
	}
}

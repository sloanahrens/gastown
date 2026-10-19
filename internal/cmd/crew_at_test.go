package cmd

import (
	"errors"
	"slices"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

// The respawn path runs gt crew at from inside the pane whose runtime exited:
// the pane shell (100) parents gt (102) beside a leftover runtime (101).
func TestCrewAtRespawnSparesCallerPane(t *testing.T) {
	t.Parallel()
	const session = "gt-crew-x" // the fake addresses a pane by its session
	chain := func() ([]string, error) { return []string{"102", "100"}, nil }
	chainErr := func() ([]string, error) { return nil, errors.New("ps failed") }
	cases := []struct {
		name       string
		callerPane string
		callerPIDs func() ([]string, error)
		wantKilled []string
		wantErr    bool
	}{
		{"caller inside the pane keeps its own chain", session, chain, []string{"101"}, false},
		{"caller outside the pane sweeps the whole tree", "%9", chain, []string{"102", "101", "100"}, false},
		{"caller outside tmux sweeps the whole tree", "", chain, []string{"102", "101", "100"}, false},
		{"unreadable ancestry inside the pane kills nothing", session, chainErr, nil, true},
		{"unreadable ancestry is irrelevant outside the pane", "%9", chainErr, []string{"102", "101", "100"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := tmuxfake.New(clockwork.NewFakeClock())
			if err := srv.NewSession(session, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			srv.SetPaneProcesses(session, "100", "102", "101")
			err := killPaneForRespawn(srv, session, tc.callerPane, tc.callerPIDs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("killPaneForRespawn error = %v, wantErr %v", err, tc.wantErr)
			}
			if got := srv.Killed(session); !slices.Equal(got, tc.wantKilled) {
				t.Errorf("kill set = %v, want %v", got, tc.wantKilled)
			}
		})
	}
}

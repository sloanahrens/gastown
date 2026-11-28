package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

const daemonFormulaSyncTown = "/town" // the town root the fake update must receive

func TestDaemonFormulaSync(t *testing.T) {
	t.Parallel()
	updateErr := errors.New("formulas: writing broken.formula.toml: permission denied")

	tests := []struct {
		name      string
		entries   []formula.SyncEntry
		updateErr error
		wantLogs  []string
	}{
		{
			name: "the synced line counts and names every formula it wrote",
			entries: []formula.SyncEntry{
				{Name: "updated.formula.toml", Action: formula.SyncUpdate},
				{Name: "reinstalled.formula.toml", Action: formula.SyncReinstall},
				{Name: "installed.formula.toml", Action: formula.SyncInstall},
				{Name: "drifted.formula.toml", Action: formula.SyncReplaceDrift},
			},
			wantLogs: []string{"formulas synced: 4 changed; updated: updated.formula.toml; reinstalled: reinstalled.formula.toml; installed: installed.formula.toml; drift replaced: drifted.formula.toml"},
		},
		{
			name: "orphaned and unowned files are named and are not an error",
			entries: []formula.SyncEntry{
				{Name: "retired.formula.toml", Action: formula.SyncOrphaned},
				{Name: "handwritten.formula.toml", Action: formula.SyncUnowned},
				{Name: "backup/", Action: formula.SyncUnowned},
			},
			wantLogs: []string{
				"formulas synced: 0 changed",
				"formulas orphaned: this binary no longer ships them and they stay on disk: retired.formula.toml",
				"formulas unowned: gt did not write them and this binary does not ship them: handwritten.formula.toml, backup/",
			},
		},
		{
			name:      "an update error is returned wrapped and nothing is logged as synced",
			updateErr: updateErr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var logs []string
			var gotTown string
			update := func(beadsPath string) (*formula.SyncPlan, error) {
				gotTown = beadsPath
				if tc.updateErr != nil {
					return nil, tc.updateErr
				}
				return &formula.SyncPlan{Entries: tc.entries}, nil
			}
			logf := func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}

			err := syncDaemonFormulas(update, daemonFormulaSyncTown, logf)

			if gotTown != daemonFormulaSyncTown {
				t.Errorf("update ran for town %q, want %q", gotTown, daemonFormulaSyncTown)
			}
			if tc.updateErr != nil {
				if !errors.Is(err, tc.updateErr) {
					t.Errorf("error = %v, want it to wrap %v", err, tc.updateErr)
				}
				if err == nil || !strings.Contains(err.Error(), daemonFormulaSyncTown) {
					t.Errorf("error = %v, want it to name the town root %s", err, daemonFormulaSyncTown)
				}
			} else if err != nil {
				t.Errorf("syncDaemonFormulas: %v, want nil", err)
			}
			if !slices.Equal(logs, tc.wantLogs) {
				t.Errorf("logs = %q, want %q", logs, tc.wantLogs)
			}
		})
	}
}

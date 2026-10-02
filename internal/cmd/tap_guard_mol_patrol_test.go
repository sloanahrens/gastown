package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// TestTapGuardMolPatrolIn covers the guard's identity check: inside an agent
// context only the mayor may run patrol. The agent context is forced with a
// cwd under a polecats layout so the role check — not the earlier "is this an
// agent at all" return — is what decides each verdict. GT_ROLE is that check
// because a mayor session carries GT_ROLE=mayor; GT_MAYOR, which the guard
// used to read, is set nowhere (gt-y3pgh.2.9).
func TestTapGuardMolPatrolIn(t *testing.T) {
	t.Parallel()
	polecatCwd := filepath.Join(t.TempDir(), "gastown", "polecats", "toast", "gastown")
	outsideCwd := t.TempDir()

	tests := []struct {
		name     string
		env      map[string]string
		cwd      string
		wantExit int // 0 means allowed
	}{
		{
			name:     "mayor allowed in agent context",
			env:      map[string]string{EnvGTRole: constants.RoleMayor},
			cwd:      polecatCwd,
			wantExit: 0,
		},
		{
			name:     "compound mayor role allowed in agent context",
			env:      map[string]string{EnvGTRole: "gastown/mayor"},
			cwd:      polecatCwd,
			wantExit: 0,
		},
		{
			name:     "polecat blocked",
			env:      map[string]string{EnvGTRole: "gastown/polecats/toast", "GT_POLECAT": "toast"},
			cwd:      polecatCwd,
			wantExit: 2,
		},
		{
			name:     "roleless agent context blocked",
			env:      map[string]string{"GT_POLECAT": "toast"},
			cwd:      polecatCwd,
			wantExit: 2,
		},
		{
			name:     "outside agent context allowed",
			env:      map[string]string{EnvGTRole: "gastown/polecats/toast"},
			cwd:      outsideCwd,
			wantExit: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(name string) string { return tt.env[name] }
			getwd := func() (string, error) { return tt.cwd, nil }

			var stderr bytes.Buffer
			err := tapGuardMolPatrolIn(getenv, getwd, &stderr)
			got, _ := IsSilentExit(err)
			if got != tt.wantExit {
				t.Fatalf("exit code = %d (err %v), want %d", got, err, tt.wantExit)
			}
			if tt.wantExit != 0 && !strings.Contains(stderr.String(), "MOL PATROL BLOCKED") {
				t.Errorf("blocked verdict wrote %q, want the block notice", stderr.String())
			}
			if tt.wantExit == 0 && stderr.Len() != 0 {
				t.Errorf("allowed verdict wrote %q, want nothing", stderr.String())
			}
		})
	}
}

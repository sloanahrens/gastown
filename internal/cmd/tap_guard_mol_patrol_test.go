package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestTapGuardMolPatrolIn covers the guard's identity check: every Gas Town
// agent context is blocked, whatever its role, and only a caller outside the
// agent tree is allowed. The agent context is forced with a cwd under a
// polecats layout so the block comes from the role/agent check and not from
// an accident of the working directory.
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
			name:     "deacon role blocked",
			env:      map[string]string{EnvGTRole: "deacon"},
			cwd:      polecatCwd,
			wantExit: 2,
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

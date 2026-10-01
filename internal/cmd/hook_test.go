package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/session"
)

// TestHookPolecatEnvCheck verifies that the polecat guard in runHook uses
// GT_ROLE as the authoritative check, so coordinators with a stale GT_POLECAT
// in their environment are not blocked from hooking (GH #1707).
func TestHookPolecatEnvCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		role      string
		polecat   string
		wantBlock bool
	}{
		{
			name:      "bare polecat role is blocked",
			role:      "polecat",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "compound polecat role is blocked",
			role:      "gastown/polecats/Toast",
			polecat:   "Toast",
			wantBlock: true,
		},
		{
			name:      "mayor with stale GT_POLECAT is NOT blocked",
			role:      "mayor",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound witness with stale GT_POLECAT is NOT blocked",
			role:      "gastown/witness",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "crew with stale GT_POLECAT is NOT blocked",
			role:      "crew",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound crew with stale GT_POLECAT is NOT blocked",
			role:      "gastown/crew/den",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "no GT_ROLE with GT_POLECAT set is blocked",
			role:      "",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "no GT_ROLE and no GT_POLECAT is not blocked",
			role:      "",
			polecat:   "",
			wantBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := hookPolecatRefusal(envMap(map[string]string{"GT_ROLE": tt.role, "GT_POLECAT": tt.polecat}))
			blocked := err != nil && strings.Contains(err.Error(), "polecats cannot hook")

			if blocked != tt.wantBlock {
				if tt.wantBlock {
					t.Errorf("expected polecat block but was not blocked (GT_ROLE=%q GT_POLECAT=%q)", tt.role, tt.polecat)
				} else {
					t.Errorf("unexpected polecat block with GT_ROLE=%q GT_POLECAT=%q", tt.role, tt.polecat)
				}
			}
		})
	}
}

// TestHookRejectsNonBeadArg pins down GH#3701: when cobra fails to match a
// subcommand and falls through to the bead-id positional, args that don't
// look like bead IDs should produce a clear error pointing at --help rather
// than the misleading "bead 'set' not found" emitted by bd show.
func TestHookRejectsNonBeadArg(t *testing.T) {
	t.Parallel()
	tests := []string{"set", "list", "delete", "nonexistentword12345"}
	for _, arg := range tests {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			err := hookBeadArgError(arg)
			if err == nil {
				t.Fatalf("hookBeadArgError(%q) returned nil, want error", arg)
			}
			if !strings.Contains(err.Error(), "is not a bead ID") {
				t.Errorf("hookBeadArgError(%q) error = %q, want substring %q", arg, err.Error(), "is not a bead ID")
			}
			if !strings.Contains(err.Error(), "--help") {
				t.Errorf("hookBeadArgError(%q) error = %q, want it to point at --help", arg, err.Error())
			}
		})
	}
}

func TestNormalizeHookShowTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "shorthand polecat path resolves",
			target: "gastown/toast",
			want:   "gastown/polecats/toast",
		},
		{
			name:   "canonical polecat path stays canonical",
			target: "gastown/polecats/toast",
			want:   "gastown/polecats/toast",
		},
		{
			name:   "unknown target stays unchanged",
			target: "this-is-not-an-agent-path",
			want:   "this-is-not-an-agent-path",
		},
		{
			name:   "path traversal shorthand stays unchanged",
			target: "../toast",
			want:   "../toast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeHookShowTarget(session.NewPrefixRegistry(), tt.target)
			if got != tt.want {
				t.Fatalf("normalizeHookShowTarget(%q) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestCloseCompletedHookedMoleculeUsesBdCmdEnv(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	beadsDir := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"hookdb"}`), 0644); err != nil {
		t.Fatal(err)
	}

	var calls []beads.BDCall
	run := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
		calls = append(calls, c)
		return nil, nil, nil
	}
	if err := closeCompletedHookedMoleculeVia(run, workDir, "gt-old", "ses-hook-test"); err != nil {
		t.Fatalf("closeCompletedHookedMolecule: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("bd calls = %d, want 1", len(calls))
	}
	wantArgs := []string{"close", "gt-old", "--force", "--reason=Auto-replaced by gt hook (molecule complete)", "--session=ses-hook-test"}
	if got := calls[0].Args; strings.Join(got, "|") != strings.Join(wantArgs, "|") {
		t.Fatalf("args = %q, want %q", got, wantArgs)
	}
	env := envSlice(calls[0].Env)
	for k, want := range map[string]string{
		"BEADS_DIR":                  beadsDir,
		"BEADS_DOLT_SERVER_DATABASE": "hookdb",
		"BD_READONLY":                "",
		"BD_DOLT_AUTO_COMMIT":        "on",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}

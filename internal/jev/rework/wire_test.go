package rework

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// The rework wire reads the rig's git base from its shared .repo.git only. A rig
// with a legacy mayor/rig clone and no .repo.git is refused, never read from the
// clone.
func TestRigRepoReadsOnlyTheBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, rigRoot string)
		want  bool
	}{
		{
			name: "a shared bare repo is the base",
			setup: func(t *testing.T, rigRoot string) {
				if err := os.MkdirAll(constants.RigBareRepoPath(rigRoot), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			name: "a mayor/rig clone with no bare repo is refused",
			setup: func(t *testing.T, rigRoot string) {
				if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a rig with neither is refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigRoot := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, rigRoot)
			}

			repo, err := rigRepo(rigRoot)
			if !tc.want {
				want := "rig " + rigRoot + " has no .repo.git"
				if err == nil || err.Error() != want {
					t.Fatalf("rigRepo err = %v, want %q", err, want)
				}
				return
			}
			if err != nil || repo != constants.RigBareRepoPath(rigRoot) {
				t.Fatalf("rigRepo = %q, %v; want the bare repo path", repo, err)
			}
		})
	}
}

// Numstat, the call site, refuses a rig whose only git directory is a mayor/rig
// clone: the diff never runs there.
func TestTownRigsNumstatRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rigRoot := filepath.Join(town, "gastown")
	if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigs := &townRigs{townRoot: town}

	_, err := rigs.Numstat(context.Background(), "gastown", "main", "abc1234")

	want := "rig " + rigRoot + " has no .repo.git"
	if err == nil || err.Error() != want {
		t.Fatalf("Numstat err = %v, want %q", err, want)
	}
}

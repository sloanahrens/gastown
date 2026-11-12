package scorecard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

// The finding battery reads the rig's git base from its shared .repo.git only. A
// rig with a legacy mayor/rig clone and no .repo.git is refused, never read from
// the clone.
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

// ChangedLines refuses a rig whose only git directory is a mayor/rig clone, and
// names the bare path it wanted.
func TestChangedLinesRefusesARigWithoutBareRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rigRoot := filepath.Join(town, dedupe.RigName)
	if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := (rigDiffs{town: town}).ChangedLines(context.Background(), "abc1234")

	want := "rig " + rigRoot + " has no .repo.git"
	if err == nil || err.Error() != want {
		t.Fatalf("ChangedLines err = %v, want %q", err, want)
	}
}

// PrefetchLines makes no git call and records nothing for a rig with only a
// mayor/rig clone: the commits are left to ChangedLines, which refuses.
func TestPrefetchLinesDoesNotReadAMayorClone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rigRoot := filepath.Join(town, dedupe.RigName)
	if err := os.MkdirAll(filepath.Join(rigRoot, "mayor", "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls int
	sizes := &diffSizes{}
	d := rigDiffs{town: town, sizes: sizes, run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, os.ErrPermission
	}}

	d.PrefetchLines(context.Background(), []string{"abc1234"})

	if calls != 0 {
		t.Errorf("PrefetchLines made %d git calls, want none", calls)
	}
	if len(sizes.lines) != 0 || len(sizes.absent) != 0 {
		t.Errorf("PrefetchLines recorded %v and %v, want nothing", sizes.lines, sizes.absent)
	}
}

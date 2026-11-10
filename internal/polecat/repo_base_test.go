package polecat

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
)

// repoBase is the git base every worktree operation uses: the rig's shared
// .repo.git only. A rig with a legacy mayor/rig clone and no .repo.git is an
// error naming the bare path, not a fallback to the clone.
func TestRepoBaseReadsOnlyTheBareRepo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, w *world, rigPath string)
		want  bool
	}{
		{
			name: "a shared bare repo is the base",
			setup: func(t *testing.T, w *world, rigPath string) {
				w.InitBare(t, constants.RigBareRepoPath(rigPath))
			},
			want: true,
		},
		{
			name: "a mayor/rig clone with no bare repo is refused",
			setup: func(t *testing.T, w *world, rigPath string) {
				w.InitRepo(t, filepath.Join(rigPath, "mayor", "rig"))
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath := t.TempDir()
			w := newWorld()
			tc.setup(t, w, rigPath)
			m := newTestManager(&rig.Rig{Name: "rig", Path: rigPath}, w, nil, newPolecatDB())

			_, err := m.repoBase()
			if tc.want {
				if err != nil {
					t.Fatalf("repoBase: %v", err)
				}
				return
			}
			want := "no .repo.git under " + rigPath
			if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "mayor/rig") {
				t.Fatalf("repoBase err = %v, want an error naming %q and not mayor/rig", err, want)
			}
		})
	}
}

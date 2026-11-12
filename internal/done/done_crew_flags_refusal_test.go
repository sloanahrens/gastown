package done

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestCrewFlagsRefusals: each refusal on the --branch/--head path stops the
// submission before the bead, the rig's clone or origin is touched. Each
// case names the caller's environment and the text the refusal carries.
func TestCrewFlagsRefusals(t *testing.T) {
	t.Parallel()
	fullHead := strings.Repeat("a1", 20)
	townRoot := t.TempDir()
	absent := filepath.Join(townRoot, "gastown", "crew", "operator")
	baseEnv := map[string]string{
		"BD_ACTOR":     crewTestActor,
		"GT_TOWN_ROOT": townRoot,
		"GT_RIG":       "gastown",
	}
	withEnv := func(kv map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range baseEnv {
			env[k] = v
		}
		for k, v := range kv {
			env[k] = v
		}
		return env
	}
	for _, tc := range []struct {
		name string
		opts Options
		env  map[string]string
		want string
	}{
		{
			name: "branch without head",
			opts: Options{Branch: crewTestBranch, Bead: "bd-source", Getwd: absentDir(absent)},
			env:  baseEnv,
			want: "go together",
		},
		{
			name: "git env override",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent)},
			env:  withEnv(map[string]string{"GIT_DIR": "/elsewhere/.git"}),
			want: "unset GIT_DIR",
		},
		{
			name: "current directory unavailable",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source",
				Getwd: func() (string, error) { return "", errors.New("cwd removed") }},
			env:  baseEnv,
			want: "current directory unavailable",
		},
		{
			name: "no town and no GT_TOWN_ROOT",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(t.TempDir())},
			env:  withEnv(map[string]string{"GT_TOWN_ROOT": ""}),
			want: "GT_TOWN_ROOT is unset",
		},
		{
			name: "branch names no bead",
			opts: Options{Branch: "crew/operator/misc", Head: fullHead, Getwd: absentDir(absent)},
			env:  baseEnv,
			want: "names no bead",
		},
		{
			name: "no rig for the bead",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent)},
			env:  withEnv(map[string]string{"GT_RIG": ""}),
			want: "no rig names bead bd-source",
		},
		{
			name: "rig name is a path",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent)},
			env:  withEnv(map[string]string{"GT_RIG": "../elsewhere"}),
			want: "no rig names bead bd-source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runDoneCrewFromFlags(tc.opts, crewEnv(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runDoneCrewFromFlags err = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// absentDir is a Getwd that names dir without requiring it to exist.
func absentDir(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// TestCrewFlagsSubmitRefusals: each refusal on the absent-worktree submit
// stops before any comment or label is written, so the bead is unchanged.
func TestCrewFlagsSubmitRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(h *submitHarness)
		want  string
	}{
		{
			name:  "run names the default branch",
			setup: func(h *submitHarness) { h.r.branch = "main" },
			want:  "cannot submit the main",
		},
		{
			name:  "run names no identity",
			setup: func(h *submitHarness) { h.r.sender = "" },
			want:  "needs BD_ACTOR",
		},
		{
			name: "source bead unreadable",
			setup: func(h *submitHarness) {
				h.r.deps.source = func(string) (*beads.Issue, beads.Client, error) {
					return nil, nil, errors.New("bd unavailable")
				}
			},
			want: "source issue validation failed",
		},
		{
			name: "no beads client for the bead",
			setup: func(h *submitHarness) {
				h.r.deps.source = func(id string) (*beads.Issue, beads.Client, error) {
					issue, err := h.bd.Show(id)
					return issue, nil, err
				}
			},
			want: "no beads client",
		},
		{
			name:  "target is another polecat's branch",
			setup: func(h *submitHarness) { h.r.defaultBranch = "polecat/other/gt-x" },
			want:  "another polecat's branch",
		},
		{
			name: "bead has unchecked acceptance",
			setup: func(h *submitHarness) {
				h.r.deps.source = func(id string) (*beads.Issue, beads.Client, error) {
					issue, err := h.bd.Show(id)
					if err != nil {
						return nil, nil, err
					}
					issue.AcceptanceCriteria = "- [ ] not yet done"
					return issue, h.client, nil
				}
			},
			want: "unchecked acceptance criteria",
		},
		{
			name:  "head commit authorship unreadable",
			setup: func(h *submitHarness) { h.repo.authorshipErr = errors.New("object missing") },
			want:  "cannot be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := absentCrewRun(t)
			tc.setup(h)
			err := submitCrewAbsentForLanding(h.r, crewTestHead)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("submitCrewAbsentForLanding err = %v, want a refusal containing %q", err, tc.want)
			}
			if comments, cerr := h.bd.Comments("bd-source"); cerr != nil || len(comments) != 0 {
				t.Errorf("comments = %v (%v); a refused submission records nothing", comments, cerr)
			}
			if beads.HasLabel(h.source(t), "gt:ready-to-land") {
				t.Error("refused submission marked the bead ready to land")
			}
		})
	}
}

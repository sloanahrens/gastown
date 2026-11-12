package done

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// flagsTown is a town with a mayor directory, the marker workspace.Find reads,
// and the process directory a flags submission starts in.
func flagsTown(t *testing.T) (townRoot, processDir string) {
	t.Helper()
	townRoot = t.TempDir()
	processDir = filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		t.Fatalf("mkdir process dir: %v", err)
	}
	return townRoot, processDir
}

// TestCrewFlagsRefusals: each refusal on the --branch/--head path stops the
// submission before the bead, the rig's clone or origin is touched. Each
// case names the caller's environment and the text the refusal carries.
func TestCrewFlagsRefusals(t *testing.T) {
	t.Parallel()
	fullHead := strings.Repeat("a1", 20)
	_, processDir := flagsTown(t)
	absent := filepath.Join(t.TempDir(), "gastown", "crew", "operator")
	baseEnv := map[string]string{"BD_ACTOR": crewTestActor}
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
			opts: Options{Branch: crewTestBranch, Bead: "bd-source", Getwd: absentDir(absent), ProcessDir: processDir},
			env:  baseEnv,
			want: "go together",
		},
		{
			name: "git env override",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent), ProcessDir: processDir},
			env:  withEnv(map[string]string{"GIT_DIR": "/elsewhere/.git"}),
			want: "unset GIT_DIR",
		},
		{
			name: "process directory unavailable",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent)},
			env:  baseEnv,
			want: "working directory is unavailable",
		},
		{
			name: "no town above the process directory",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent), ProcessDir: t.TempDir()},
			env:  baseEnv,
			want: "no Gas Town workspace above this process's directory",
		},
		{
			name: "branch names no bead",
			opts: Options{Branch: "crew/operator/misc", Head: fullHead, Getwd: absentDir(absent), ProcessDir: processDir},
			env:  baseEnv,
			want: "names no bead",
		},
		{
			name: "no rig for the bead",
			opts: Options{Branch: crewTestBranch, Head: fullHead, Bead: "bd-source", Getwd: absentDir(absent), ProcessDir: processDir},
			env:  baseEnv,
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

// TestCrewFlagsIgnoreCallerSuppliedTownAndRig: the town and the rig do not
// come from variables the caller sets, nor from the caller's directory. A
// town named by GT_TOWN_ROOT, a rig named by GT_RIG and a caller directory
// inside a town all lead nowhere when the process directory has no town above
// it, and a GT_RIG naming a path adds nothing to a bead no route names.
func TestCrewFlagsIgnoreCallerSuppliedTownAndRig(t *testing.T) {
	t.Parallel()
	fullHead := strings.Repeat("a1", 20)
	callerTown, _ := flagsTown(t)
	env := map[string]string{
		"BD_ACTOR":     crewTestActor,
		"GT_TOWN_ROOT": callerTown,
		"GT_RIG":       "gastown",
	}
	err := runDoneCrewFromFlags(Options{
		Branch: crewTestBranch, Head: fullHead, Bead: "bd-source",
		Getwd:      absentDir(filepath.Join(callerTown, "gastown", "crew", "operator")),
		ProcessDir: t.TempDir(),
	}, crewEnv(env))
	if err == nil || !strings.Contains(err.Error(), "no Gas Town workspace above this process's directory") {
		t.Fatalf("err = %v, want the process-directory town refusal, whatever GT_TOWN_ROOT and the caller's directory name", err)
	}

	_, processDir := flagsTown(t)
	env["GT_RIG"] = "../elsewhere"
	err = runDoneCrewFromFlags(Options{
		Branch: crewTestBranch, Head: fullHead, Bead: "bd-source",
		Getwd:      absentDir(filepath.Join(callerTown, "gastown", "crew", "operator")),
		ProcessDir: processDir,
	}, crewEnv(env))
	if err == nil || !strings.Contains(err.Error(), "no rig names bead bd-source") {
		t.Fatalf("err = %v, want the routed-rig refusal, whatever GT_RIG names", err)
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

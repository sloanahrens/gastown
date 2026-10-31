package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCrewWorktreePlan pins the path and branch a topic produces, and the
// names it refuses: the topic is joined into a path and a branch, so a name
// git or the filesystem would read as something else is refused here.
func TestCrewWorktreePlan(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "town", "gastown", "crew", "dunlin")
	crewDir := filepath.Join(string(filepath.Separator), "town", "gastown", "crew")

	cases := []struct {
		name       string
		agent      string
		topic      string
		wantPath   string
		wantBranch string
		wantErr    bool
	}{
		{
			name:       "a good topic",
			agent:      "dunlin",
			topic:      "fix-flake",
			wantPath:   filepath.Join(crewDir, "dunlin-fix-flake"),
			wantBranch: "crew/dunlin/fix-flake",
		},
		{
			name:       "digits and dots",
			agent:      "dunlin",
			topic:      "3.2-fix",
			wantPath:   filepath.Join(crewDir, "dunlin-3.2-fix"),
			wantBranch: "crew/dunlin/3.2-fix",
		},
		{
			name:    "an empty topic",
			agent:   "dunlin",
			topic:   "",
			wantErr: true,
		},
		{
			name:    "a topic with a slash",
			agent:   "dunlin",
			topic:   "a/b",
			wantErr: true,
		},
		{
			name:    "a topic of two dots",
			agent:   "dunlin",
			topic:   "..",
			wantErr: true,
		},
		{
			name:    "a topic opening with a parent segment",
			agent:   "dunlin",
			topic:   "../x",
			wantErr: true,
		},
		{
			name:    "an uppercase topic",
			agent:   "dunlin",
			topic:   "Fix-Flake",
			wantErr: true,
		},
		{
			name:    "a topic opening with a dot",
			agent:   "dunlin",
			topic:   ".hidden",
			wantErr: true,
		},
		{
			name:    "a topic opening with a hyphen",
			agent:   "dunlin",
			topic:   "-x",
			wantErr: true,
		},
		{
			name:    "an empty crew name",
			agent:   "",
			topic:   "fix-flake",
			wantErr: true,
		},
		{
			name:    "a crew name with a slash",
			agent:   "a/b",
			topic:   "fix-flake",
			wantErr: true,
		},
		{
			name:    "a crew name of two dots",
			agent:   "..",
			topic:   "fix-flake",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, branch, err := crewWorktreePlan(root, tc.agent, tc.topic)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("crewWorktreePlan(%q, %q) = %q, %q, nil; want a refusal", tc.agent, tc.topic, path, branch)
				}
				return
			}
			if err != nil {
				t.Fatalf("crewWorktreePlan(%q, %q): %v", tc.agent, tc.topic, err)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if branch != tc.wantBranch {
				t.Errorf("branch = %q, want %q", branch, tc.wantBranch)
			}
		})
	}
}

// crewWorktreeFake is the git surface a crew worktree test drives: the answers
// the two existence questions give, the errors a step returns, and the calls
// the command made.
type crewWorktreeFake struct {
	pathExists   func() (bool, error)
	branchExists func() (bool, error)
	fetchErr     error
	addErr       error

	fetched bool
	created [][3]string // path, branch, start point
}

func (f *crewWorktreeFake) Fetch(time.Duration) error {
	f.fetched = true
	return f.fetchErr
}

func (f *crewWorktreeFake) PathExists(string) (bool, error) {
	if f.pathExists == nil {
		return false, nil
	}
	return f.pathExists()
}

func (f *crewWorktreeFake) BranchExists(string) (bool, error) {
	if f.branchExists == nil {
		return false, nil
	}
	return f.branchExists()
}

func (f *crewWorktreeFake) WorktreeAddFromRef(path, branch, startPoint string) error {
	f.created = append(f.created, [3]string{path, branch, startPoint})
	return f.addErr
}

// crewCloneFixture is a crew clone inside a temporary town, with every lookup
// the command makes answered from the fixture, so no test reads the host or
// the live town.
type crewCloneFixture struct {
	root string // the crew clone the command runs in
	role RoleInfo
	g    crewWorktreeGit
}

func newCrewCloneFixture(t *testing.T, role RoleInfo, g crewWorktreeGit) *crewCloneFixture {
	t.Helper()
	town := t.TempDir()
	return &crewCloneFixture{
		root: filepath.Join(town, "gastown", "crew", "dunlin"),
		role: role,
		g:    g,
	}
}

// run drives the production crewWorktree from the fixture's clone.
func (f *crewCloneFixture) run(topic string) (string, error) {
	deps := crewWorktreeDeps{
		getwd:    func() (string, error) { return f.root, nil },
		findTown: func() (string, error) { return filepath.Dir(filepath.Dir(filepath.Dir(f.root))), nil },
		role:     func(string, string) (RoleInfo, error) { return f.role, nil },
		repo:     func(string) crewWorktreeGit { return f.g },
		timeout:  time.Second,
	}
	var out bytes.Buffer
	err := crewWorktree(&out, topic, deps)
	return out.String(), err
}

// worktree is the path the plan names for topic: the clone's sibling.
func (f *crewCloneFixture) worktree(topic string) string {
	return filepath.Join(filepath.Dir(f.root), "dunlin-"+topic)
}

// crewRole is the role a crew clone reports for member dunlin.
var crewRole = RoleInfo{Role: RoleCrew, Rig: "gastown", Polecat: "dunlin"}

// TestCrewWorktreeRefusesExistingPathOrBranch pins that neither an existing
// path nor an existing branch is reused: the command refuses before the fetch,
// so the refusal costs no network call and creates nothing.
func TestCrewWorktreeRefusesExistingPathOrBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		g          *crewWorktreeFake
		wantErrSub string
	}{
		{
			name:       "the path exists",
			g:          &crewWorktreeFake{pathExists: func() (bool, error) { return true, nil }},
			wantErrSub: "already exists",
		},
		{
			name:       "the branch exists",
			g:          &crewWorktreeFake{branchExists: func() (bool, error) { return true, nil }},
			wantErrSub: "branch crew/dunlin/fix-flake already exists",
		},
		{
			name:       "the path cannot be read",
			g:          &crewWorktreeFake{pathExists: func() (bool, error) { return false, errors.New("permission denied") }},
			wantErrSub: "check whether",
		},
		{
			name:       "the branch cannot be read",
			g:          &crewWorktreeFake{branchExists: func() (bool, error) { return false, errors.New("show-ref failed") }},
			wantErrSub: "check whether branch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := newCrewCloneFixture(t, crewRole, tc.g).run("fix-flake")
			if err == nil {
				t.Fatalf("crewWorktree() = nil error; want a refusal naming %q", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErrSub)
			}
			if tc.g.fetched {
				t.Error("the clone was fetched for a name that was already taken")
			}
			if len(tc.g.created) != 0 {
				t.Errorf("WorktreeAddFromRef calls = %v, want none", tc.g.created)
			}
			if out != "" {
				t.Errorf("printed %q, want nothing on a refusal", out)
			}
		})
	}
}

// TestCrewWorktreeRefusesOutsideACrewClone pins that the crew name comes from
// the detected role: a session that is not a named crew member has no name to
// build the worktree from, so it is refused before anything is asked of git.
func TestCrewWorktreeRefusesOutsideACrewClone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		role RoleInfo
	}{
		{name: "a polecat", role: RoleInfo{Role: RolePolecat, Rig: "gastown", Polecat: "coma"}},
		{name: "no role", role: RoleInfo{Role: RoleUnknown}},
		{name: "a crew member with no name", role: RoleInfo{Role: RoleCrew, Rig: "gastown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &crewWorktreeFake{}
			out, err := newCrewCloneFixture(t, tc.role, g).run("fix-flake")
			if err == nil {
				t.Fatal("crewWorktree() = nil error; want a refusal")
			}
			if len(g.created) != 0 {
				t.Errorf("WorktreeAddFromRef calls = %v, want none", g.created)
			}
			if out != "" {
				t.Errorf("printed %q, want nothing on a refusal", out)
			}
		})
	}
}

// TestCrewWorktreeCreatesTheWorktree pins the stream: the path and branch the
// plan named, cut from origin/main, and the printed path a caller cd's into.
func TestCrewWorktreeCreatesTheWorktree(t *testing.T) {
	t.Parallel()
	fx := newCrewCloneFixture(t, crewRole, &crewWorktreeFake{})
	g := fx.g.(*crewWorktreeFake)

	out, err := fx.run("fix-flake")
	if err != nil {
		t.Fatalf("crewWorktree(): %v", err)
	}
	if !g.fetched {
		t.Error("the clone was not fetched before the worktree was cut")
	}
	want := [3]string{fx.worktree("fix-flake"), "crew/dunlin/fix-flake", "origin/main"}
	if len(g.created) != 1 || g.created[0] != want {
		t.Errorf("WorktreeAddFromRef calls = %v, want [%v]", g.created, want)
	}
	if got := strings.TrimSpace(out); got != fx.worktree("fix-flake") {
		t.Errorf("printed %q, want %q", got, fx.worktree("fix-flake"))
	}
}

// TestCrewWorktreeReportsAFailedStep pins that a fetch or a creation that
// fails is an error, not a path a caller would cd into.
func TestCrewWorktreeReportsAFailedStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		g          *crewWorktreeFake
		wantErrSub string
	}{
		{
			name:       "the fetch fails",
			g:          &crewWorktreeFake{fetchErr: errors.New("no such remote")},
			wantErrSub: "fetch origin",
		},
		{
			name:       "the worktree cannot be created",
			g:          &crewWorktreeFake{addErr: errors.New("disk full")},
			wantErrSub: "create the worktree",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := newCrewCloneFixture(t, crewRole, tc.g).run("fix-flake")
			if err == nil {
				t.Fatalf("crewWorktree() = nil error; want %q", tc.wantErrSub)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErrSub)
			}
			if out != "" {
				t.Errorf("printed %q, want nothing after a failed step", out)
			}
		})
	}
}

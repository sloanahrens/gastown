package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/done"
)

func TestResolveDonePolecatWorktreeAcceptsOwnWorktree(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		layout   string
		subdir   string
		gtRole   string
		wantRoot func(townRoot string) string
	}{
		{
			name:   "nested root",
			layout: "nested",
			gtRole: "gastown/polecats/shiny",
			wantRoot: func(townRoot string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny", "gastown")
			},
		},
		{
			name:   "nested subdir",
			layout: "nested",
			subdir: filepath.Join("internal", "cmd"),
			gtRole: "gastown/polecats/shiny",
			wantRoot: func(townRoot string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny", "gastown")
			},
		},
		{
			name:   "legacy root",
			layout: "legacy",
			gtRole: "polecat",
			wantRoot: func(townRoot string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny")
			},
		},
		{
			name:   "legacy subdir",
			layout: "legacy",
			subdir: filepath.Join("internal", "cmd"),
			gtRole: "gastown/shiny",
			wantRoot: func(townRoot string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot, repoRoot := newDoneGuardWorktree(t, tt.layout, "shiny")
			cwd := repoRoot
			if tt.subdir != "" {
				cwd = filepath.Join(repoRoot, tt.subdir)
				if err := os.MkdirAll(cwd, 0755); err != nil {
					t.Fatalf("mkdir subdir: %v", err)
				}
			}
			env := doneGuardEnv(townRoot, "gastown", "shiny", tt.gtRole)

			got, err := done.ResolvePolecatWorktreeIn(cwd, envMap(env), markerGitTopLevel)
			if err != nil {
				t.Fatalf("resolveDonePolecatWorktreeIn: %v", err)
			}
			if got.TownRoot != townRoot {
				t.Fatalf("townRoot = %q, want %q", got.TownRoot, townRoot)
			}
			if got.Cwd != done.CanonicalPath(tt.wantRoot(townRoot)) {
				t.Fatalf("cwd = %q, want %q", got.Cwd, done.CanonicalPath(tt.wantRoot(townRoot)))
			}
			if got.RigName != "gastown" || got.PolecatName != "shiny" || got.Actor != "gastown/polecats/shiny" {
				t.Fatalf("identity = %#v, want gastown/polecats/shiny", got)
			}
		})
	}
}

func TestResolveDonePolecatWorktreeRejectsUnsafePaths(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		cwd    func(townRoot, ownRepo string) string
		gitDir bool
	}{
		{
			name: "town root",
			cwd:  func(townRoot, ownRepo string) string { return townRoot },
		},
		{
			name: "mayor rig",
			cwd: func(townRoot, ownRepo string) string {
				return filepath.Join(townRoot, "mayor", "rig")
			},
			gitDir: true,
		},
		{
			name: "rig root",
			cwd: func(townRoot, ownRepo string) string {
				return filepath.Join(townRoot, "gastown")
			},
		},
		{
			name: "other polecat",
			cwd: func(townRoot, ownRepo string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "other", "gastown")
			},
			gitDir: true,
		},
		{
			name: "nested parent without git root",
			cwd: func(townRoot, ownRepo string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny")
			},
		},
		{
			name: "nested parent with git root",
			cwd: func(townRoot, ownRepo string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny")
			},
			gitDir: true,
		},
		{
			name: "missing worktree",
			cwd: func(townRoot, ownRepo string) string {
				return filepath.Join(townRoot, "gastown", "polecats", "shiny", "missing")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot, ownRepo := newDoneGuardWorktree(t, "nested", "shiny")
			cwd := tt.cwd(townRoot, ownRepo)
			if tt.gitDir {
				initDoneGuardGitRepo(t, cwd)
			} else if tt.name != "missing worktree" {
				if err := os.MkdirAll(cwd, 0755); err != nil {
					t.Fatalf("mkdir cwd: %v", err)
				}
			}
			env := doneGuardEnv(townRoot, "gastown", "shiny", "gastown/polecats/shiny")
			env["GT_POLECAT_PATH"] = ownRepo

			if _, err := done.ResolvePolecatWorktreeIn(cwd, envMap(env), markerGitTopLevel); err == nil {
				t.Fatalf("done.ResolvePolecatWorktreeIn(%q) succeeded, want rejection", cwd)
			}
		})
	}
}

func TestResolveDonePolecatWorktreeRejectsIdentityMismatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		actor   string
		gtRole  string
		gtRig   string
		polecat string
	}{
		{name: "non polecat actor", actor: "gastown/crew/shiny", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "malformed actor", actor: "gastown/polecats", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "actor mismatch", actor: "gastown/polecats/other", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "role mismatch", actor: "gastown/polecats/shiny", gtRole: "gastown/polecats/other", gtRig: "gastown", polecat: "shiny"},
		{name: "rig mismatch", actor: "gastown/polecats/shiny", gtRole: "gastown/polecats/shiny", gtRig: "other", polecat: "shiny"},
		{name: "polecat mismatch", actor: "gastown/polecats/shiny", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "other"},
		{name: "non polecat role", actor: "gastown/polecats/shiny", gtRole: "gastown/crew/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "missing role", actor: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "actor traversal rig", actor: "../polecats/shiny", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "actor traversal polecat", actor: "gastown/polecats/..", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "actor backslash polecat", actor: "gastown/polecats/shiny\\other", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: "shiny"},
		{name: "env traversal rig", actor: "gastown/polecats/shiny", gtRole: "gastown/polecats/shiny", gtRig: "..", polecat: "shiny"},
		{name: "env traversal polecat", actor: "gastown/polecats/shiny", gtRole: "gastown/polecats/shiny", gtRig: "gastown", polecat: ".."},
		{name: "role extra components", actor: "gastown/polecats/shiny", gtRole: "gastown/polecats/shiny/extra", gtRig: "gastown", polecat: "shiny"},
		{name: "role traversal rig", actor: "gastown/polecats/shiny", gtRole: "../polecats/shiny", gtRig: "gastown", polecat: "shiny"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot, repoRoot := newDoneGuardWorktree(t, "nested", "shiny")
			env := map[string]string{"GT_TOWN_ROOT": townRoot, "GT_ROOT": townRoot,
				"BD_ACTOR": tt.actor, "GT_ROLE": tt.gtRole, "GT_RIG": tt.gtRig, "GT_POLECAT": tt.polecat}

			if _, err := done.ResolvePolecatWorktreeIn(repoRoot, envMap(env), markerGitTopLevel); err == nil {
				t.Fatal("resolveDonePolecatWorktreeIn succeeded, want identity rejection")
			}
		})
	}
}

func TestResolveDonePolecatWorktreeRejectsTownRootMismatch(t *testing.T) {
	t.Parallel()
	townRoot, repoRoot := newDoneGuardWorktree(t, "nested", "shiny")
	env := doneGuardEnv(townRoot, "gastown", "shiny", "gastown/polecats/shiny")
	env["GT_TOWN_ROOT"] = filepath.Join(t.TempDir(), "other-town")

	if _, err := done.ResolvePolecatWorktreeIn(repoRoot, envMap(env), markerGitTopLevel); err == nil || !strings.Contains(err.Error(), "town root mismatch") {
		t.Fatalf("resolveDonePolecatWorktreeIn error = %v, want town root mismatch", err)
	}
}

func TestResolveDonePolecatWorktreeIgnoresGTRootAlias(t *testing.T) {
	t.Parallel()
	townRoot, repoRoot := newDoneGuardWorktree(t, "nested", "shiny")
	env := doneGuardEnv(townRoot, "gastown", "shiny", "gastown/polecats/shiny")
	// GT_ROOT is the bd alias, not a town-root name gt validates (gt-syhch).
	env["GT_ROOT"] = filepath.Join(t.TempDir(), "other-town")

	if _, err := done.ResolvePolecatWorktreeIn(repoRoot, envMap(env), markerGitTopLevel); err != nil {
		t.Fatalf("resolveDonePolecatWorktreeIn error = %v, want the GT_ROOT alias ignored", err)
	}
}

func TestResolveDonePolecatWorktreeRejectsGitWorkTreeSpoof(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		envName string
		value   func(repoRoot string) string
	}{
		{envName: "GIT_DIR", value: func(repoRoot string) string { return filepath.Join(repoRoot, ".git") }},
		{envName: "GIT_WORK_TREE", value: func(repoRoot string) string { return repoRoot }},
	} {
		t.Run(tt.envName, func(t *testing.T) {
			t.Parallel()
			townRoot, repoRoot := newDoneGuardWorktree(t, "nested", "shiny")
			env := doneGuardEnv(townRoot, "gastown", "shiny", "gastown/polecats/shiny")
			env[tt.envName] = tt.value(repoRoot)

			if _, err := done.ResolvePolecatWorktreeIn(townRoot, envMap(env), markerGitTopLevel); err == nil || !strings.Contains(err.Error(), "unset "+tt.envName) {
				t.Fatalf("resolveDonePolecatWorktreeIn error = %v, want git env override rejection", err)
			}
		})
	}
}

func TestIsDoneCommand(t *testing.T) {
	t.Parallel()
	done := &cobra.Command{Use: "done"}
	root := &cobra.Command{Use: "gt"}
	root.AddCommand(done)
	if !isDoneCommand(done) {
		t.Fatal("done command should be detected")
	}
	if isDoneCommand(root) {
		t.Fatal("root command should not be detected as done")
	}

	// Subcommands that happen to be named "done" (gt dog done,
	// gt mol step done) must NOT trip the polecat-only guard (gt-lt7).
	dog := &cobra.Command{Use: "dog"}
	dogDone := &cobra.Command{Use: "done [name]"}
	dog.AddCommand(dogDone)
	root.AddCommand(dog)
	if isDoneCommand(dogDone) {
		t.Fatal("nested 'dog done' command should not be detected as gt done")
	}
	if isDoneCommand(dog) {
		t.Fatal("'dog' command should not be detected as done")
	}
}

// TestPersistentPreRunDoneRejectsBeforeRegistryFallback: the guard
// persistentPreRun runs before any shared write refuses a polecat's gt done
// from the town root, and lets every other command through.
func TestPersistentPreRunDoneRejectsBeforeRegistryFallback(t *testing.T) {
	t.Parallel()
	townRoot, _ := newDoneGuardWorktree(t, "nested", "shiny")
	initDoneGuardGitRepo(t, townRoot)
	env := envMap(doneGuardEnv(townRoot, "gastown", "shiny", "gastown/polecats/shiny"))

	// Model the real command tree: gt done is a direct child of the root.
	done := &cobra.Command{Use: "done"}
	status := &cobra.Command{Use: "status"}
	testRoot := &cobra.Command{Use: "gt"}
	testRoot.AddCommand(done, status)
	err := donePolecatGuard(done, env, townRoot, markerGitTopLevel)
	if err == nil || !strings.Contains(err.Error(), "assigned polecat worktree") {
		t.Fatalf("donePolecatGuard error = %v, want assigned worktree rejection", err)
	}
	if err := donePolecatGuard(status, env, townRoot, markerGitTopLevel); err != nil {
		t.Fatalf("donePolecatGuard(gt status) = %v, want no guard", err)
	}
}

// newDoneGuardWorktree lays out a town with polecat polecatName's worktree
// (nested under <polecat>/<rig> or the legacy <polecat> layout) and returns
// the town root and the worktree's repo root. The session's variables go in
// doneGuardEnv.
func newDoneGuardWorktree(t *testing.T, layout, polecatName string) (string, string) {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}

	polecatRoot := filepath.Join(townRoot, "gastown", "polecats", polecatName)
	repoRoot := polecatRoot
	if layout == "nested" {
		repoRoot = filepath.Join(polecatRoot, "gastown")
	}
	initDoneGuardGitRepo(t, repoRoot)
	return townRoot, repoRoot
}

// initDoneGuardGitRepo marks dir as a git work tree root for
// markerGitTopLevel.
func initDoneGuardGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("mkdir git repo: %v", err)
	}
}

// markerGitTopLevel stands in for git rev-parse --show-toplevel: the nearest
// directory at or above dir holding a .git entry.
func markerGitTopLevel(dir string) (string, error) {
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p, nil
		}
		if filepath.Dir(p) == p {
			return "", errors.New("not a git repository")
		}
	}
}

// doneGuardEnv is the environment of polecat rig/polecatName's session in
// townRoot.
func doneGuardEnv(townRoot, rig, polecatName, gtRole string) map[string]string {
	return map[string]string{
		"GT_TOWN_ROOT": townRoot,
		"GT_ROOT":      townRoot,
		"BD_ACTOR":     rig + "/polecats/" + polecatName,
		"GT_ROLE":      gtRole,
		"GT_RIG":       rig,
		"GT_POLECAT":   polecatName,
		"GT_SESSION":   "gt-" + polecatName,
	}
}

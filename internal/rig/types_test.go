package rig

import (
	"os"
	"path/filepath"
	"testing"
)

// mkClone makes path look like a regular git clone: a working tree root with a
// .git directory.
func mkClone(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0755); err != nil {
		t.Fatalf("creating clone at %s: %v", path, err)
	}
}

// mkLinkedWorktree makes path look like a git worktree: a working tree root
// whose .git is a file pointing at the shared git directory.
func mkLinkedWorktree(t *testing.T, path, gitDir string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("creating worktree at %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+gitDir+"\n"), 0644); err != nil {
		t.Fatalf("writing .git for %s: %v", path, err)
	}
}

func TestBeadsPath_AlwaysReturnsRigRoot(t *testing.T) {
	t.Parallel()

	// BeadsPath should always return the rig root path, regardless of HasMayor.
	// The redirect system at <rig>/.beads/redirect handles finding the actual
	// beads location (either local at <rig>/.beads/ or tracked at mayor/rig/.beads/).
	//
	// This ensures:
	// 1. We don't write files to the user's repo clone (mayor/rig/)
	// 2. The redirect architecture is respected
	// 3. All code paths use the same beads resolution logic

	tests := []struct {
		name     string
		rig      Rig
		wantPath string
	}{
		{
			name: "rig with mayor only",
			rig: Rig{
				Name:     "testrig",
				Path:     "/home/user/gt/testrig",
				HasMayor: true,
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with witness only",
			rig: Rig{
				Name:       "testrig",
				Path:       "/home/user/gt/testrig",
				HasWitness: true,
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with refinery only",
			rig: Rig{
				Name:        "testrig",
				Path:        "/home/user/gt/testrig",
				HasRefinery: true,
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with no agents",
			rig: Rig{
				Name: "testrig",
				Path: "/home/user/gt/testrig",
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with mayor and witness",
			rig: Rig{
				Name:       "testrig",
				Path:       "/home/user/gt/testrig",
				HasMayor:   true,
				HasWitness: true,
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with mayor and refinery",
			rig: Rig{
				Name:        "testrig",
				Path:        "/home/user/gt/testrig",
				HasMayor:    true,
				HasRefinery: true,
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with witness and refinery",
			rig: Rig{
				Name:        "testrig",
				Path:        "/home/user/gt/testrig",
				HasWitness:  true,
				HasRefinery: true,
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with all agents",
			rig: Rig{
				Name:        "fullrig",
				Path:        "/tmp/gt/fullrig",
				HasMayor:    true,
				HasWitness:  true,
				HasRefinery: true,
			},
			wantPath: "/tmp/gt/fullrig",
		},
		{
			name: "rig with polecats",
			rig: Rig{
				Name:     "testrig",
				Path:     "/home/user/gt/testrig",
				HasMayor: true,
				Polecats: []string{"polecat1", "polecat2"},
			},
			wantPath: "/home/user/gt/testrig",
		},
		{
			name: "rig with crew",
			rig: Rig{
				Name:     "testrig",
				Path:     "/home/user/gt/testrig",
				HasMayor: true,
				Crew:     []string{"crew1", "crew2"},
			},
			wantPath: "/home/user/gt/testrig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.rig.BeadsPath()
			if got != tt.wantPath {
				t.Errorf("BeadsPath() = %q, want %q", got, tt.wantPath)
			}
		})
	}
}

func TestDefaultBranch_FallsBackToMain(t *testing.T) {
	t.Parallel()

	// DefaultBranch should return "main" when config cannot be loaded
	rig := Rig{
		Name: "testrig",
		Path: "/nonexistent/path",
	}

	got := rig.DefaultBranch()
	if got != "main" {
		t.Errorf("DefaultBranch() = %q, want %q", got, "main")
	}
}

func TestRepoPath(t *testing.T) {
	t.Parallel()

	t.Run("mayor clone when rig root is not a repo", func(t *testing.T) {
		t.Parallel()

		rigPath := filepath.Join(t.TempDir(), "gastown")
		mkClone(t, filepath.Join(rigPath, "mayor", "rig"))

		r := Rig{Name: "gastown", Path: rigPath}
		want := filepath.Join(rigPath, "mayor", "rig")
		if got := r.RepoPath(); got != want {
			t.Errorf("RepoPath() = %q, want %q", got, want)
		}
	})

	t.Run("refinery clone when mayor clone is absent", func(t *testing.T) {
		t.Parallel()

		rigPath := filepath.Join(t.TempDir(), "gastown")
		mkClone(t, filepath.Join(rigPath, "refinery", "rig"))

		r := Rig{Name: "gastown", Path: rigPath}
		want := filepath.Join(rigPath, "refinery", "rig")
		if got := r.RepoPath(); got != want {
			t.Errorf("RepoPath() = %q, want %q", got, want)
		}
	})

	t.Run("rig checked out in place wins over the clones", func(t *testing.T) {
		t.Parallel()

		rigPath := filepath.Join(t.TempDir(), "gastown")
		mkClone(t, rigPath)
		mkClone(t, filepath.Join(rigPath, "mayor", "rig"))

		r := Rig{Name: "gastown", Path: rigPath}
		if got := r.RepoPath(); got != rigPath {
			t.Errorf("RepoPath() = %q, want %q", got, rigPath)
		}
	})

	// The regression this method exists to prevent: a rig directory inside a
	// git-tracked town root satisfies `git rev-parse --git-dir` by upward
	// search, so a looser check would hand callers the town repository — and
	// `git rm --cached` would then untrack files in the wrong repo.
	t.Run("rig root inside a git-tracked town root is not the repo", func(t *testing.T) {
		t.Parallel()

		townRoot := t.TempDir()
		mkClone(t, townRoot)

		rigPath := filepath.Join(townRoot, "gastown")
		if err := os.MkdirAll(rigPath, 0755); err != nil {
			t.Fatalf("creating rig root: %v", err)
		}
		mkClone(t, filepath.Join(rigPath, "mayor", "rig"))

		r := Rig{Name: "gastown", Path: rigPath}
		want := filepath.Join(rigPath, "mayor", "rig")
		if got := r.RepoPath(); got != want {
			t.Errorf("RepoPath() = %q, want %q (town root must not leak through)", got, want)
		}
	})

	t.Run("linked worktree counts as a working tree", func(t *testing.T) {
		t.Parallel()

		rigPath := filepath.Join(t.TempDir(), "gastown")
		bareDir := filepath.Join(rigPath, ".repo.git", "worktrees", "mayor")
		if err := os.MkdirAll(bareDir, 0755); err != nil {
			t.Fatalf("creating bare git dir: %v", err)
		}
		mkLinkedWorktree(t, filepath.Join(rigPath, "mayor", "rig"), bareDir)

		r := Rig{Name: "gastown", Path: rigPath}
		want := filepath.Join(rigPath, "mayor", "rig")
		if got := r.RepoPath(); got != want {
			t.Errorf("RepoPath() = %q, want %q", got, want)
		}
	})

	t.Run("bare repo is not a working tree", func(t *testing.T) {
		t.Parallel()

		rigPath := filepath.Join(t.TempDir(), "gastown")
		if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0755); err != nil {
			t.Fatalf("creating bare repo: %v", err)
		}

		r := Rig{Name: "gastown", Path: rigPath}
		if got := r.RepoPath(); got != "" {
			t.Errorf("RepoPath() = %q, want %q", got, "")
		}
	})

	t.Run("no repository returns empty", func(t *testing.T) {
		t.Parallel()

		rigPath := filepath.Join(t.TempDir(), "gastown")
		if err := os.MkdirAll(rigPath, 0755); err != nil {
			t.Fatalf("creating rig root: %v", err)
		}

		r := Rig{Name: "gastown", Path: rigPath}
		if got := r.RepoPath(); got != "" {
			t.Errorf("RepoPath() = %q, want %q", got, "")
		}
	})
}

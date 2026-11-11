package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tailDeployTown makes a town root with a gastown source checkout under
// mayor/rig, the marker GetRepoRootForTown looks for, and no bare repo. The
// checkout is plain files: nothing here runs git.
func tailDeployTown(t *testing.T) (townRoot, checkout string) {
	t.Helper()
	townRoot = t.TempDir()
	checkout = filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(checkout, "cmd", "gt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "cmd", "gt", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot, checkout
}

// TestTailDeployRepo_UsesTheBareRepoWhenItIsThere: the rig's bare repo answers
// for the deploy tracker and the main picture, not the checkout beside it.
func TestTailDeployRepo_UsesTheBareRepoWhenItIsThere(t *testing.T) {
	t.Parallel()
	townRoot, _ := tailDeployTown(t)
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	repo, fellBack, err := tailDeployRepo(townRoot)
	if err != nil {
		t.Fatalf("tailDeployRepo: %v", err)
	}
	if repo != bare {
		t.Errorf("repo = %q, want the bare repo %q", repo, bare)
	}
	if fellBack != "" {
		t.Errorf("fellBack = %q, want none when the bare repo answers", fellBack)
	}
}

// TestTailDeployRepo_FallsBackToTheCheckoutWithoutABareRepo: a town with no
// .repo.git reads the source checkout, and the reason names both the absent
// bare repo and the checkout that answered.
func TestTailDeployRepo_FallsBackToTheCheckoutWithoutABareRepo(t *testing.T) {
	t.Parallel()
	townRoot, checkout := tailDeployTown(t)

	repo, fellBack, err := tailDeployRepo(townRoot)
	if err != nil {
		t.Fatalf("tailDeployRepo: %v", err)
	}
	if repo != checkout {
		t.Errorf("repo = %q, want the checkout %q", repo, checkout)
	}
	for _, want := range []string{"gastown/.repo.git is absent", checkout} {
		if !strings.Contains(fellBack, want) {
			t.Errorf("fellBack = %q, want it to name %q", fellBack, want)
		}
	}
}

// TestTailDeployRepo_RefusesWhatItCannotRead: a bare path that is not a
// directory, and a town with neither a bare repo nor a checkout, are errors.
// Neither is a fallback, so neither answers from the wrong repository.
func TestTailDeployRepo_RefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{
			name: "the bare path is a file",
			setup: func(t *testing.T) string {
				townRoot, _ := tailDeployTown(t)
				if err := os.WriteFile(filepath.Join(townRoot, "gastown", ".repo.git"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return townRoot
			},
		},
		{
			// The checkout is readable, so a fallback on any stat error would
			// answer from it; only the error keeps the stale checkout out.
			name: "the bare path is a symlink loop",
			setup: func(t *testing.T) string {
				townRoot, _ := tailDeployTown(t)
				bare := filepath.Join(townRoot, "gastown", ".repo.git")
				if err := os.Symlink(bare, bare); err != nil {
					t.Fatal(err)
				}
				return townRoot
			},
		},
		{
			name:  "no bare repo and no checkout",
			setup: func(t *testing.T) string { return t.TempDir() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, fellBack, err := tailDeployRepo(tc.setup(t))
			if err == nil {
				t.Fatalf("tailDeployRepo = (%q, %q, nil), want an error", repo, fellBack)
			}
			if repo != "" {
				t.Errorf("repo = %q alongside the error, want none", repo)
			}
		})
	}
}

// TestDashWhyLogDeployRepo_LogsOnlyAChangeOfRepo: the fallback is logged once
// and not on every poll, and its return to the bare repo is logged once too.
// A nil log records nothing.
func TestDashWhyLogDeployRepo_LogsOnlyAChangeOfRepo(t *testing.T) {
	t.Parallel()
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	why.deployRepo("absent; reading the checkout")
	why.deployRepo("absent; reading the checkout")
	why.deployRepo("")
	why.deployRepo("")

	if len(lines) != 2 {
		t.Fatalf("logged %d lines %q, want the fallback and the return once each", len(lines), lines)
	}
	if want := "deploy repo: absent; reading the checkout"; lines[0] != want {
		t.Errorf("fallback line = %q, want %q", lines[0], want)
	}
	if lines[1] != "deploy repo is the rig's bare repo again" {
		t.Errorf("return line = %q, want the bare repo's return", lines[1])
	}
	var nilLog *dashWhyLog
	nilLog.deployRepo("absent") // must not panic
}

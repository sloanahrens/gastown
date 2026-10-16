package rig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/git"
)

// RepoFileReader is the part of *git.Git a rig's tracked-file read goes
// through (docs/testing.md, "Seams for external tools"): *git.Git in
// production, gitfake in the unit tier.
type RepoFileReader interface {
	RefExists(ref string) (bool, error)
	ShowFileAtRev(ref, path string) (string, error)
}

// RepoFileOpener opens the repository at a path as ReadRepoFile reads it.
type RepoFileOpener func(repoPath string) RepoFileReader

// openRepoFile is the production RepoFileOpener.
func openRepoFile(repoPath string) RepoFileReader { return git.NewGit(repoPath) }

// RepoFile is a repo-relative file a rig holds: its contents, where they came
// from, and whether anything has the file at all.
type RepoFile struct {
	Data   []byte
	Source string
	Found  bool
}

// ReadRepoFile returns relPath as the rig's repo main has it, falling back to
// the mayor/rig clone's file; a repo neither copy can be read from is an error
// naming the rig.
//
// main is the rig repository's own origin/<default_branch> ref, not its
// refs/heads/<default_branch>, which no fetch updates. Nothing here fetches:
// the ref is as fresh as the rig's last fetch, which is the same state every
// other read of the rig's main sees.
func ReadRepoFile(open RepoFileOpener, townRoot, rigName, relPath string) (RepoFile, error) {
	if townRoot == "" || rigName == "" || relPath == "" {
		return RepoFile{}, nil
	}

	rigPath := filepath.Join(townRoot, rigName)
	repoDir := filepath.Join(rigPath, ".repo.git")
	if info, err := os.Stat(repoDir); err == nil && info.IsDir() {
		branch := defaultBranch(rigPath)
		ref := "refs/remotes/origin/" + branch
		repo := open(repoDir)
		unreadable := func(err error) (RepoFile, error) {
			return RepoFile{}, fmt.Errorf("rig %s: reading %s at origin/%s in %s: %w", rigName, relPath, branch, repoDir, err)
		}
		exists, err := repo.RefExists(ref)
		if err != nil {
			return unreadable(err)
		}
		// A ref that is not there is main having nothing to prefer, not an
		// error: the rig has not fetched this repository yet.
		if exists {
			data, err := repo.ShowFileAtRev(ref, relPath)
			if err == nil {
				return RepoFile{Data: []byte(data), Source: "origin/" + branch + " in " + repoDir, Found: true}, nil
			}
			// ErrNotAtRef: the file is not on main, so the clone's copy below is
			// the only one there is. Any other failure is a repo this run cannot
			// read, which no fallback may paper over.
			if !errors.Is(err, git.ErrNotAtRef) {
				return unreadable(err)
			}
		}
	}

	path := filepath.Join(rigPath, "mayor", "rig", relPath)
	data, err := os.ReadFile(path) //nolint:gosec // G304: the rig's own clone
	switch {
	case err == nil:
		return RepoFile{Data: data, Source: path, Found: true}, nil
	case os.IsNotExist(err):
		return RepoFile{}, nil
	default:
		return RepoFile{}, fmt.Errorf("rig %s: reading %s: %w", rigName, path, err)
	}
}

// defaultBranch is a rig's default branch, "main" when its config names none.
func defaultBranch(rigPath string) string {
	cfg, err := LoadRigConfigIfPresent(rigPath)
	if err != nil {
		WarnRigConfigOnce(rigPath, err)
	}
	if cfg != nil && cfg.DefaultBranch != "" {
		return cfg.DefaultBranch
	}
	return "main"
}

// repoFileWarnedRigs holds rigs WarnRepoFileUnreadable has already reported, so
// the per-polecat callers of ResolveMergeQueueConfig print one line, not one
// per polecat.
var repoFileWarnedRigs sync.Map

// WarnRepoFileUnreadable reports, once per rig, that a repo-committed tier was
// dropped rather than resolved: a rig repo this run cannot read has no main to
// follow, and the stale clone copy is what gt-yj6g1 exists to stop trusting.
func WarnRepoFileUnreadable(rigName string, err error) {
	if err == nil {
		return
	}
	if _, dup := repoFileWarnedRigs.LoadOrStore(rigName, struct{}{}); dup {
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: rig %s: ignoring an unreadable repo-committed tier: %v\n", rigName, err)
}

// RepoFileUnreadableWarned reports whether WarnRepoFileUnreadable has reported
// rigName, so tests can assert the dropped tier is reported rather than
// silently ignored.
func RepoFileUnreadableWarned(rigName string) bool {
	_, ok := repoFileWarnedRigs.Load(rigName)
	return ok
}

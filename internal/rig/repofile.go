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

// OpenRepoFile is the production RepoFileOpener for a caller outside this
// package that resolves a rig's settings through the same git seam (the
// dashboard's Forgejo panes, through ResolveForgejoConfigWithError).
func OpenRepoFile(repoPath string) RepoFileReader { return openRepoFile(repoPath) }

// RepoFileFor is ReadRepoFile with the production git reader, for a caller
// outside this package that reads a rig's tracked file through a seam of its
// own (the metrics snapshot's Env, gt-16efl).
func RepoFileFor(townRoot, rigName, relPath string) (RepoFile, error) {
	return ReadRepoFile(openRepoFile, townRoot, rigName, relPath)
}

// RepoFile is a repo-relative file as the rig repo's main has it: its contents,
// where they came from, and whether main has the file at all.
type RepoFile struct {
	Data   []byte
	Source string
	Found  bool
}

// ReadRepoFile returns relPath as the rig repo's main has it. main is the
// rig repository's own origin/<default_branch> ref, not its
// refs/heads/<default_branch>, which no fetch updates. Nothing here fetches:
// the ref is as fresh as the rig's last fetch, which is the same state every
// other read of the rig's main sees.
//
// A rig with no .repo.git, a repo with no fetched main, and a main that does
// not carry the file each answer not found with no error: none of them has a
// copy of the file to serve, and the rig's working copy is never read in their
// place (gt-yj6g1). A repo this run cannot read is an error naming the rig.
func ReadRepoFile(open RepoFileOpener, townRoot, rigName, relPath string) (RepoFile, error) {
	if townRoot == "" || rigName == "" || relPath == "" {
		return RepoFile{}, nil
	}

	rigPath := filepath.Join(townRoot, rigName)
	repoDir := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(repoDir); err != nil {
		if os.IsNotExist(err) {
			return RepoFile{}, nil
		}
		return RepoFile{}, fmt.Errorf("rig %s: reading %s: %w", rigName, repoDir, err)
	}

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
	if !exists {
		return RepoFile{}, nil
	}
	data, err := repo.ShowFileAtRev(ref, relPath)
	switch {
	case err == nil:
		return RepoFile{
			Data:   []byte(data),
			Source: "origin/" + branch + " in " + repoDir,
			Found:  true,
		}, nil
	case errors.Is(err, git.ErrNotAtRef):
		// main answered for this path, and its answer is that it holds no such
		// file.
		return RepoFile{}, nil
	default:
		// Any other failure is a repo this run cannot read, which no answer
		// may paper over.
		return unreadable(err)
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

// WarnRepoFileUnreadable reports once per rig, until repoFileReadable clears
// it, that the repo-committed tier was dropped: its repo cannot be read, so
// there is no main to follow (gt-yj6g1), or its file reads but does not parse
// (gt-mvbbe).
func WarnRepoFileUnreadable(rigName string, err error) {
	if err == nil {
		return
	}
	if _, dup := repoFileWarnedRigs.LoadOrStore(rigName, struct{}{}); dup {
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: rig %s: dropping the repo-committed tier: %v\n", rigName, err)
}

// RepoFileUnreadableWarned reports whether WarnRepoFileUnreadable has reported
// rigName, so tests can assert the dropped tier is reported rather than
// silently ignored.
func RepoFileUnreadableWarned(rigName string) bool {
	_, ok := repoFileWarnedRigs.Load(rigName)
	return ok
}

// repoFileReadable forgets rigName's report. Only a resolution that no longer
// drops the tier calls it: a malformed file answers every read, so clearing in
// ReadRepoFile would reprint the report on every daemon patrol (gt-mvbbe).
func repoFileReadable(rigName string) {
	repoFileWarnedRigs.Delete(rigName)
}

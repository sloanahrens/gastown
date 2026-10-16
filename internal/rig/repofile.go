package rig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// Fallback is why Data is the mayor/rig clone's copy rather than the repo's
	// own main, and is empty when main answered or the rig has no repo whose
	// main could. A caller that must follow main — the editorial check reading
	// the rubric — reads it to tell a file main no longer carries from one only
	// the clone ever had (gt-yj6g1).
	Fallback string
}

// ReadRepoFile returns relPath as the rig's repo main has it, falling back to
// the mayor/rig clone's file; a repo neither copy can be read from is an error
// naming the rig.
//
// main is the rig repository's own origin/<default_branch> ref, not its
// refs/heads/<default_branch>, which no fetch updates. Nothing here fetches:
// the ref is as fresh as the rig's last fetch, which is the same state every
// other read of the rig's main sees.
//
// The clone answers only when main does not: the repo has no fetched main yet,
// or main carries no such file. Both are the clone's copy rather than main's,
// never silent — RepoFile.Fallback names which, WarnRepoFileCloneFallback
// reports it, and a caller that means to follow main can refuse the answer.
func ReadRepoFile(open RepoFileOpener, townRoot, rigName, relPath string) (RepoFile, error) {
	if townRoot == "" || rigName == "" || relPath == "" {
		return RepoFile{}, nil
	}

	rigPath := filepath.Join(townRoot, rigName)
	repoDir := filepath.Join(rigPath, ".repo.git")
	fallback := ""
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
		// A ref read that answered is a repo this run can read.
		repoFileReadable(rigName)
		switch {
		case !exists:
			fallback = fmt.Sprintf("the repo has no origin/%s ref", branch)
		default:
			data, err := repo.ShowFileAtRev(ref, relPath)
			switch {
			case err == nil:
				repoFileReadable(rigName)
				return RepoFile{
					Data:   []byte(data),
					Source: "origin/" + branch + " in " + repoDir,
					Found:  true,
				}, nil
			case errors.Is(err, git.ErrNotAtRef):
				// main answered for this path, and its answer is that it holds
				// no such file: the clone's copy is the stale data gt-yj6g1
				// exists to stop serving under main's name.
				fallback = fmt.Sprintf("origin/%s does not carry it", branch)
			default:
				// Any other failure is a repo this run cannot read, which no
				// fallback may paper over.
				return unreadable(err)
			}
		}
	}

	path := filepath.Join(rigPath, "mayor", "rig", relPath)
	data, err := os.ReadFile(path) //nolint:gosec // G304: the rig's own clone
	switch {
	case err == nil:
		WarnRepoFileCloneFallback(rigName, relPath, path, fallback)
		return RepoFile{Data: data, Source: path, Found: true, Fallback: fallback}, nil
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

// WarnRepoFileUnreadable reports that a repo-committed tier was dropped rather
// than resolved: a rig repo this run cannot read has no main to follow, and the
// stale clone copy is what gt-yj6g1 exists to stop trusting. repoFileReadable
// clears the report once the repo answers again, so a failure that follows a
// good read is news rather than a repeat of one already printed.
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

// repoFileReadable forgets rigName's report of an unreadable repo, which a read
// that answered proves is readable again.
func repoFileReadable(rigName string) {
	repoFileWarnedRigs.Delete(rigName)
}

// repoFileCloneFallbacks holds the clone-sourced answers WarnRepoFileCloneFallback
// has reported, keyed by rig, path and reason, so a caller that reads the same
// file on every dispatch prints one line rather than one per dispatch.
var repoFileCloneFallbacks sync.Map

// WarnRepoFileCloneFallback reports that a read answered with the mayor/rig
// clone's copy because the rig repo's main did not answer for the path, reason
// naming which of the two it was. The read still returns that copy — a rig
// whose settings live only in its clone keeps resolving — but a caller that
// means to follow main can see the difference (gt-yj6g1).
func WarnRepoFileCloneFallback(rigName, relPath, path, reason string) {
	if reason == "" {
		return
	}
	key := rigName + "\x00" + relPath + "\x00" + reason
	if _, dup := repoFileCloneFallbacks.LoadOrStore(key, struct{}{}); dup {
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: rig %s: reading %s from the mayor/rig clone at %s: %s\n", rigName, relPath, path, reason)
}

// RepoFileCloneFallbackWarned reports whether WarnRepoFileCloneFallback
// reported a clone-sourced answer for rigName's relPath, so tests can assert
// the fallback is reported rather than silently served.
func RepoFileCloneFallbackWarned(rigName, relPath string) bool {
	prefix := rigName + "\x00" + relPath + "\x00"
	warned := false
	repoFileCloneFallbacks.Range(func(key, _ any) bool {
		if strings.HasPrefix(key.(string), prefix) {
			warned = true
			return false
		}
		return true
	})
	return warned
}

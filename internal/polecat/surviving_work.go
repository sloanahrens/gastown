package polecat

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// ErrNoRigRepo means the rig has neither a shared bare repo nor a mayor/rig
// clone, so there is no git state to judge surviving work from.
var ErrNoRigRepo = errors.New("rig has no git repo (neither .repo.git nor mayor/rig)")

// workSurvivalFetchTimeout bounds every remote call the predicate makes
// (ls-remote and fetch); a timeout makes the answer unknown. A variable so
// tests can shorten it.
var workSurvivalFetchTimeout = git.RemoteQueryTimeout

// WorkSurvival is the one "does this bead's polecat work survive?" predicate
// (gt-vm5g4, gt-7evi4). Every path that releases a hooked bead — nuke, polecat
// removal, sling rollback, the witness orphan reset, sling's re-sling guard —
// asks it before giving the bead back, so work that exists only on a polecat
// branch is never silently handed to a fresh polecat starting from main.
//
// Work survives for bead B iff a generated polecat branch for B exists (local
// in the rig repo, or on origin) AND that branch has at least one commit whose
// patch is on NEITHER origin/<default> NOR any origin integration/* branch.
// Patch identity comes from `git cherry <upstream> <branch>` ('+' lines),
// which stays correct under rebase merges and single-commit squashes, where an
// ancestry check reports merged work as unmerged. A multi-commit squash does
// not match any single commit's patch, so such a branch reads as surviving —
// the safe direction: the hook is kept, never wrongly released. A branch equal
// to its base, fully merged (into main or its epic's integration branch), or
// empty does not survive.
//
// One WorkSurvival serves many beads in one rig: the origin listings and the
// base-branch refreshes are done at most once, and every fetch is bounded.
type WorkSurvival struct {
	g             *git.Git
	defaultBranch string

	basesReady bool
	bases      []string // origin/<default> first, then origin/integration/*
	basesErr   error

	originListed bool
	origin       []string
	originHash   map[string]string
	originErr    error
}

// NewWorkSurvival prepares the predicate for one rig. It returns ErrNoRigRepo
// when the rig has no git repo.
func NewWorkSurvival(rigRoot string) (*WorkSurvival, error) {
	root := rigGitRepo(rigRoot)
	if root == "" {
		return nil, ErrNoRigRepo
	}
	var g *git.Git
	if filepath.Base(root) == ".repo.git" {
		g = git.NewGitWithDir(root, "")
	} else {
		g = git.NewGit(root)
	}
	defaultBranch := "main"
	if cfg, err := rig.LoadRigConfig(rigRoot); err == nil && cfg.DefaultBranch != "" {
		defaultBranch = cfg.DefaultBranch
	}
	return &WorkSurvival{g: g, defaultBranch: defaultBranch}, nil
}

// SurvivingWorkForIssue is NewWorkSurvival(rigRoot).ForIssue(issueID).
func SurvivingWorkForIssue(rigRoot, issueID string) (string, error) {
	w, err := NewWorkSurvival(rigRoot)
	if err != nil {
		return "", err
	}
	return w.ForIssue(issueID)
}

// ForIssue returns the newest polecat branch for issueID that carries work
// found on no base branch, or "" when no such branch exists. A non-nil error
// means the answer is unknown (origin unreachable or timed out, a base branch
// missing, a branch that could not be listed or compared); callers decide
// which way to fail.
func (w *WorkSurvival) ForIssue(issueID string) (string, error) {
	if issueID == "" {
		return "", nil
	}

	var unknown error
	local, localErr := w.g.ListBranches("polecat/*")
	if localErr != nil {
		unknown = errors.Join(unknown, fmt.Errorf("listing local polecat branches: %w", localErr))
	}
	localMatches := MatchSurvivingBranches(local, issueID)
	w.listOrigin()
	if w.originErr != nil {
		unknown = errors.Join(unknown, fmt.Errorf("listing origin polecat branches: %w", w.originErr))
	}
	originMatches := MatchSurvivingBranches(w.origin, issueID)
	if len(localMatches) == 0 && len(originMatches) == 0 {
		return "", unknown
	}

	bases, err := w.baseRefs()
	if err != nil {
		return "", errors.Join(unknown, err)
	}

	// Candidate refs, newest branch first; for one name the local ref (which
	// may hold commits never pushed) is judged before the origin copy.
	isLocal := make(map[string]bool, len(localMatches))
	for _, b := range localMatches {
		isLocal[b] = true
	}
	onOrigin := make(map[string]bool, len(originMatches))
	for _, b := range originMatches {
		onOrigin[b] = true
	}
	names := MatchSurvivingBranches(append(append([]string(nil), localMatches...), originMatches...), issueID)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		var refs []string
		if isLocal[name] {
			refs = append(refs, "refs/heads/"+name)
		}
		if onOrigin[name] {
			if err := w.ensureOriginRef(name); err != nil {
				unknown = errors.Join(unknown, err)
			} else {
				refs = append(refs, "refs/remotes/origin/"+name)
			}
		}
		for _, ref := range refs {
			survives, err := w.unmergedOnAllBases(bases, ref)
			if err != nil {
				unknown = errors.Join(unknown, err)
				continue
			}
			if survives {
				return name, nil
			}
		}
	}
	return "", unknown
}

// unmergedOnAllBases reports whether ref has a commit whose patch is on none of
// bases: the intersection of the '+' sets of `git cherry <base> <ref>`.
func (w *WorkSurvival) unmergedOnAllBases(bases []string, ref string) (bool, error) {
	var survivors map[string]bool
	for _, base := range bases {
		out, err := w.g.Cherry(base, ref)
		if err != nil {
			return false, fmt.Errorf("git cherry %s %s: %w", base, ref, err)
		}
		plus := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "+" {
				plus[fields[1]] = true
			}
		}
		if survivors == nil {
			survivors = plus
		} else {
			for sha := range survivors {
				if !plus[sha] {
					delete(survivors, sha)
				}
			}
		}
		if len(survivors) == 0 {
			return false, nil
		}
	}
	return len(survivors) > 0, nil
}

func (w *WorkSurvival) listOrigin() {
	if w.originListed {
		return
	}
	w.originListed = true
	refs, err := w.g.ListRemoteRefsWithHashesTimeout("origin", "refs/heads/polecat/", workSurvivalFetchTimeout)
	if err != nil {
		w.originErr = err
		return
	}
	w.originHash = make(map[string]string, len(refs))
	for _, r := range refs {
		name := strings.TrimPrefix(r.Name, "refs/heads/")
		w.origin = append(w.origin, name)
		w.originHash[name] = r.Hash
	}
}

// baseRefs refreshes and returns the bases work is judged against:
// origin/<default>, then every origin integration/* branch (an epic's polecat
// branches start from and merge into its integration branch). A stale base can
// only make merged work look unmerged (keep the hook), never the reverse.
func (w *WorkSurvival) baseRefs() ([]string, error) {
	if w.basesReady {
		return w.bases, w.basesErr
	}
	w.basesReady = true

	if err := w.fetch(w.defaultBranch); err != nil {
		w.basesErr = fmt.Errorf("refreshing origin/%s: %w", w.defaultBranch, err)
		return nil, w.basesErr
	}
	w.bases = []string{"origin/" + w.defaultBranch}

	integration, err := w.g.ListRemoteRefsWithHashesTimeout("origin", "refs/heads/integration/", workSurvivalFetchTimeout)
	if err != nil {
		w.basesErr = fmt.Errorf("listing origin integration branches: %w", err)
		return nil, w.basesErr
	}
	for _, r := range integration {
		name := strings.TrimPrefix(r.Name, "refs/heads/")
		if err := w.ensureRef(name, r.Hash); err != nil {
			w.basesErr = err
			return nil, w.basesErr
		}
		w.bases = append(w.bases, "refs/remotes/origin/"+name)
	}
	return w.bases, nil
}

// ensureOriginRef makes refs/remotes/origin/<branch> match the tip ls-remote
// reported, fetching it when the rig repo lacks it or holds another commit.
func (w *WorkSurvival) ensureOriginRef(branch string) error {
	return w.ensureRef(branch, w.originHash[branch])
}

func (w *WorkSurvival) ensureRef(branch, wantHash string) error {
	ref := "refs/remotes/origin/" + branch
	if have, err := w.g.Rev(ref); err == nil && wantHash != "" && strings.TrimSpace(have) == wantHash {
		return nil
	}
	if err := w.fetch(branch); err != nil {
		return fmt.Errorf("fetching origin %s: %w", branch, err)
	}
	return nil
}

// fetch updates refs/remotes/origin/<branch>, bounded by
// workSurvivalFetchTimeout.
func (w *WorkSurvival) fetch(branch string) error {
	return w.g.FetchRefspecWithTimeout("origin",
		"+refs/heads/"+branch+":refs/remotes/origin/"+branch, workSurvivalFetchTimeout)
}

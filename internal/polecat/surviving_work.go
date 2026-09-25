package polecat

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// ErrNoRigRepo means the rig has neither a shared bare repo nor a mayor/rig
// clone, so there is no git state to judge surviving work from.
var ErrNoRigRepo = errors.New("rig has no git repo (neither .repo.git nor mayor/rig)")

// WorkSurvival is the one "does this bead's polecat work survive?" predicate
// (gt-vm5g4, gt-7evi4). Every path that releases a hooked bead — nuke, polecat
// removal, the witness orphan reset, sling's re-sling guard — asks it before
// giving the bead back, so work that exists only on a polecat branch is never
// silently handed to a fresh polecat starting from main.
//
// Work survives for bead B iff a generated polecat branch for B exists (local
// in the rig repo, or on origin) AND that branch has at least one commit whose
// patch is not on the rig's default branch. Patch identity comes from
// `git cherry origin/<default> <branch>` ('+' lines), which stays correct under
// rebase and squash merges where an ancestry check reports merged work as
// unmerged. A branch equal to main, fully merged, or empty does not survive.
//
// One WorkSurvival serves many beads in one rig: the origin listing and the
// base-branch refresh are done at most once.
type WorkSurvival struct {
	g             *git.Git
	defaultBranch string

	baseReady bool
	baseErr   error

	originListed bool
	origin       []string
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

// ForIssue returns the newest polecat branch for issueID that carries work not
// on the default branch, or "" when no such branch exists. A non-nil error
// means the answer is unknown (origin unreachable, base branch missing, a
// branch that could not be compared); callers decide which way to fail.
func (w *WorkSurvival) ForIssue(issueID string) (string, error) {
	if issueID == "" {
		return "", nil
	}

	local, localErr := w.g.ListBranches("polecat/*")
	localMatches := MatchSurvivingBranches(local, issueID)
	w.listOrigin()
	originMatches := MatchSurvivingBranches(w.origin, issueID)

	if len(localMatches) == 0 && len(originMatches) == 0 {
		switch {
		case localErr != nil:
			return "", fmt.Errorf("listing local polecat branches: %w", localErr)
		case w.originErr != nil:
			return "", fmt.Errorf("listing origin polecat branches: %w", w.originErr)
		}
		return "", nil
	}

	base, err := w.base()
	if err != nil {
		return "", err
	}

	// Candidate refs, newest branch first; for one name the local ref (which
	// may hold commits never pushed) is judged before the origin copy.
	type candidate struct{ name, ref string }
	var candidates []candidate
	onOrigin := make(map[string]bool, len(originMatches))
	for _, b := range originMatches {
		onOrigin[b] = true
	}
	isLocal := make(map[string]bool, len(localMatches))
	for _, b := range localMatches {
		isLocal[b] = true
	}
	names := MatchSurvivingBranches(append(append([]string(nil), localMatches...), originMatches...), issueID)
	seen := make(map[string]bool, len(names))
	for _, b := range names {
		if seen[b] {
			continue
		}
		seen[b] = true
		if isLocal[b] {
			candidates = append(candidates, candidate{b, "refs/heads/" + b})
		}
		if onOrigin[b] {
			candidates = append(candidates, candidate{b, "refs/remotes/origin/" + b})
		}
	}

	var evalErr error
	for _, c := range candidates {
		if c.ref != "refs/heads/"+c.name {
			if err := w.ensureOriginRef(c.name); err != nil {
				evalErr = errors.Join(evalErr, err)
				continue
			}
		}
		out, err := w.g.Cherry(base, c.ref)
		if err != nil {
			evalErr = errors.Join(evalErr, fmt.Errorf("git cherry %s %s: %w", base, c.ref, err))
			continue
		}
		if git.CountCherryUnmergedCommits(out) > 0 {
			return c.name, nil
		}
	}
	if evalErr != nil {
		return "", evalErr
	}
	if w.originErr != nil {
		// Every local candidate is merged, but origin could not be asked.
		return "", fmt.Errorf("listing origin polecat branches: %w", w.originErr)
	}
	return "", nil
}

func (w *WorkSurvival) listOrigin() {
	if w.originListed {
		return
	}
	w.originListed = true
	refs, err := w.g.ListRemoteRefsWithHashes("origin", "refs/heads/polecat/")
	if err != nil {
		w.originErr = err
		return
	}
	for _, r := range refs {
		w.origin = append(w.origin, trimHeads(r.Name))
	}
}

// base refreshes and returns origin/<default>. A stale base can only make
// merged work look unmerged (keep the hook), never the reverse.
func (w *WorkSurvival) base() (string, error) {
	base := "origin/" + w.defaultBranch
	if w.baseReady {
		return base, w.baseErr
	}
	w.baseReady = true
	_ = w.g.FetchBranch("origin", "+refs/heads/"+w.defaultBranch+":refs/remotes/origin/"+w.defaultBranch)
	if ok, err := w.g.RefExists("refs/remotes/origin/" + w.defaultBranch); err != nil || !ok {
		w.baseErr = fmt.Errorf("base branch %s not available in the rig repo", base)
	}
	return base, w.baseErr
}

// ensureOriginRef makes refs/remotes/origin/<branch> present locally, fetching
// it when the rig repo has not seen it yet.
func (w *WorkSurvival) ensureOriginRef(branch string) error {
	ref := "refs/remotes/origin/" + branch
	if ok, err := w.g.RefExists(ref); err == nil && ok {
		return nil
	}
	if err := w.g.FetchBranch("origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return fmt.Errorf("fetching origin %s: %w", branch, err)
	}
	return nil
}

func trimHeads(name string) string {
	const p = "refs/heads/"
	if len(name) > len(p) && name[:len(p)] == p {
		return name[len(p):]
	}
	return name
}

package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
)

// localPruneRepo is the git surface of one rig's local polecat branch sweep.
// It is an interface so the sweep's rules are unit-testable without running
// git; realLocalPruneRepo is the production implementation and the
// integration tier drives it against temp repos.
type localPruneRepo interface {
	// LocalPolecatBranches lists the rig repo's local polecat/* branches.
	LocalPolecatBranches() ([]string, error)
	// BaseRef returns the ref a branch must already be merged into to be
	// deleted, erroring when it cannot be resolved.
	BaseRef() (string, error)
	// IsAncestor reports whether ancestor is merged into descendant.
	IsAncestor(ancestor, descendant string) (bool, error)
	// HeldBranches returns every branch a live checkout holds: those checked
	// out in a worktree, plus the repo's own HEAD branch.
	HeldBranches() (map[string]bool, error)
	// DeleteBranch removes a branch; the implementation never forces.
	DeleteBranch(name string) error
}

// realLocalPruneRepo is localPruneRepo over a rig's real repo.
type realLocalPruneRepo struct{ git *git.Git }

func (r realLocalPruneRepo) LocalPolecatBranches() ([]string, error) {
	return r.git.ListBranches("polecat/*")
}

// BaseRef resolves origin/<default branch> and reports a rig whose remote
// default cannot be resolved as an error: ancestry against a missing ref has
// no answer, and the caller must then leave that rig alone.
func (r realLocalPruneRepo) BaseRef() (string, error) {
	ref := "origin/" + r.git.RemoteDefaultBranch()
	exists, err := r.git.RefExists(ref)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", ref, err)
	}
	if !exists {
		return "", fmt.Errorf("%s does not exist", ref)
	}
	return ref, nil
}

func (r realLocalPruneRepo) IsAncestor(ancestor, descendant string) (bool, error) {
	return r.git.IsAncestor(ancestor, descendant)
}

// HeldBranches reads every worktree of the repo, not just this process's
// HEAD: a branch held by a polecat's worktree is live work even though
// nothing about the bare repo says so. The repo's own HEAD goes in too,
// because a bare repo's worktree entry carries no branch line, so the branch
// its HEAD points at — which `git branch -d` will happily delete, leaving
// HEAD dangling — is invisible to the worktree list alone (gt-y3d7h).
func (r realLocalPruneRepo) HeldBranches() (map[string]bool, error) {
	worktrees, err := r.git.WorktreeList()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(worktrees)+1)
	for _, w := range worktrees {
		if w.Branch != "" {
			out[w.Branch] = true
		}
	}
	if head := r.git.DefaultBranch(); head != "" {
		out[head] = true
	}
	return out, nil
}

// DeleteBranch passes force=false. -d is a second gate, not the main one: it
// refuses a branch merged into neither HEAD nor its own upstream, so a branch
// merged into its upstream passes it even when that work is nowhere near
// origin/<default>. The ancestry check above it is what actually decides, and
// -D would delete whatever it is handed.
func (r realLocalPruneRepo) DeleteBranch(name string) error {
	return r.git.DeleteBranch(name, false)
}

// localPruneRigResult is one rig's sweep outcome. A non-empty SkippedReason
// means nothing in that rig was touched; the three lists hold branch names
// and are always fully populated, so the report can name every branch it
// kept as well as every branch it deleted.
type localPruneRigResult struct {
	Deleted       []string // deleted, or on a dry run what would be deleted
	Held          []string // kept: checked out in a worktree, or the repo's HEAD
	Unmerged      []string // kept: not merged into the base ref
	Failures      []string // per-branch errors, already formatted
	SkippedReason string   // non-empty: rig skipped, all lists empty
}

// pruneLocalPolecatBranches sweeps one rig: merged, non-checked-out polecat
// branches go, and everything else is reported. apply=false is the dry run,
// which reaches the same decisions and deletes nothing.
//
// Nothing is deleted in a rig whose base ref or worktree list cannot be read,
// because both are what make a delete safe here — an unreadable base ref
// would make every branch look unmerged, and an unreadable worktree list
// would make a live polecat's branch look abandoned.
func pruneLocalPolecatBranches(repo localPruneRepo, apply bool) localPruneRigResult {
	var res localPruneRigResult

	branches, err := repo.LocalPolecatBranches()
	if err != nil {
		res.SkippedReason = fmt.Sprintf("listing local branches: %v", err)
		return res
	}
	base, err := repo.BaseRef()
	if err != nil {
		res.SkippedReason = fmt.Sprintf("no base ref to compare against: %v", err)
		return res
	}
	held, err := repo.HeldBranches()
	if err != nil {
		res.SkippedReason = fmt.Sprintf("listing worktrees: %v", err)
		return res
	}

	sort.Strings(branches)
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		// Held first: that a branch is also merged does not make it safe to
		// delete while a worktree has it checked out.
		if held[branch] {
			res.Held = append(res.Held, branch)
			continue
		}
		merged, err := repo.IsAncestor(branch, base)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", branch, err))
			continue
		}
		if !merged {
			res.Unmerged = append(res.Unmerged, branch)
			continue
		}
		if apply {
			if err := repo.DeleteBranch(branch); err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("deleting %s: %v", branch, err))
				continue
			}
		}
		res.Deleted = append(res.Deleted, branch)
	}
	return res
}

// localPruneTarget is one rig in the sweep: its name, and the repo holding
// its polecat branches. RepoDir is empty for a rig with neither .repo.git
// nor mayor/rig.
type localPruneTarget struct {
	Name    string
	RepoDir string
}

// localPruneOpener opens one target's repo. It is a parameter of the sweep so
// tests can drive the rules with fakes, and a non-empty RepoDir is the only
// thing the sweep needs to try.
type localPruneOpener func(repoDir string) localPruneRepo

// openRealLocalPruneRepo opens a rig repo for the production sweep, in the
// shape rigPruneRepoDir found it: a bare .repo.git is its own git dir and
// takes no working tree, while a legacy mayor/rig is an ordinary clone whose
// .git sits inside it.
func openRealLocalPruneRepo(repoDir string) localPruneRepo {
	if filepath.Base(repoDir) == ".repo.git" {
		return realLocalPruneRepo{git: git.NewGitWithDir(repoDir, "")}
	}
	return realLocalPruneRepo{git: git.NewGit(repoDir)}
}

// localPruneTotals is the whole-run sum of every rig's counts.
type localPruneTotals struct {
	Deleted  int
	Held     int
	Unmerged int
	Failed   int
	Skipped  int
}

func (t *localPruneTotals) add(res localPruneRigResult) {
	t.Deleted += len(res.Deleted)
	t.Held += len(res.Held)
	t.Unmerged += len(res.Unmerged)
	t.Failed += len(res.Failures)
	if res.SkippedReason != "" {
		t.Skipped++
	}
}

// pruneLocalPolecatBranchBacklog sweeps every target in order, reporting each
// rig as it finishes and then the run's totals. One rig failing to open
// never stops the others (gt-y3d7h).
func pruneLocalPolecatBranchBacklog(out io.Writer, targets []localPruneTarget, open localPruneOpener, apply bool) localPruneTotals {
	var totals localPruneTotals
	for _, target := range targets {
		var res localPruneRigResult
		if target.RepoDir == "" {
			res.SkippedReason = "no git repo (neither .repo.git nor mayor/rig)"
		} else {
			res = pruneLocalPolecatBranches(open(target.RepoDir), apply)
		}
		reportLocalPolecatPrune(out, target.Name, res, apply)
		totals.add(res)
	}
	reportLocalPolecatPruneTotals(out, totals, apply)
	return totals
}

// reportLocalPolecatPrune prints one rig's sweep result. It names every
// branch it kept as well as every one it deleted: the sweep's whole point is
// a list an operator can read before trusting a delete.
func reportLocalPolecatPrune(out io.Writer, rigName string, res localPruneRigResult, apply bool) {
	if res.SkippedReason != "" {
		fmt.Fprintf(out, "  %s  %s skipped: %s\n", style.Bold.Render(rigName), style.Warning.Render("⚠"), res.SkippedReason)
		return
	}
	fmt.Fprintf(out, "  %s\n", style.Bold.Render(rigName))
	fmt.Fprintf(out, "    %s: %d\n", localPruneDeleteVerb(apply), len(res.Deleted))
	writeLocalPruneBranchNames(out, res.Deleted)
	fmt.Fprintf(out, "    kept, checked out: %d\n", len(res.Held))
	writeLocalPruneBranchNames(out, res.Held)
	fmt.Fprintf(out, "    kept, unmerged: %d\n", len(res.Unmerged))
	writeLocalPruneBranchNames(out, res.Unmerged)
	if len(res.Failures) > 0 {
		fmt.Fprintf(out, "    %s: %d\n", style.Error.Render("errors"), len(res.Failures))
		writeLocalPruneBranchNames(out, res.Failures)
	}
}

func reportLocalPolecatPruneTotals(out io.Writer, totals localPruneTotals, apply bool) {
	fmt.Fprintf(out, "\nTotal: %s %d, kept %d (%d checked out, %d unmerged), %d error(s), %d rig(s) skipped\n",
		localPruneDeleteVerb(apply), totals.Deleted,
		totals.Held+totals.Unmerged, totals.Held, totals.Unmerged,
		totals.Failed, totals.Skipped)
}

func localPruneDeleteVerb(apply bool) string {
	if apply {
		return "deleted"
	}
	return "would delete"
}

func writeLocalPruneBranchNames(out io.Writer, names []string) {
	for _, name := range names {
		fmt.Fprintf(out, "      %s\n", name)
	}
}

// runPolecatPruneAllRigs resolves every rig in the town config and sweeps
// them. A rig it cannot read is reported and skipped without failing the run;
// only an unreadable rig list, or a branch the sweep decided to delete and
// could not, exits non-zero, so a caller can tell a complete sweep from a
// partial one.
func runPolecatPruneAllRigs(out io.Writer, apply bool) error {
	targets, err := resolveLocalPruneTargets()
	if err != nil {
		return err
	}
	mode := " (dry run — nothing is deleted without --apply)"
	if apply {
		mode = " (--apply: merged branches are deleted)"
	}
	fmt.Fprintf(out, "Local polecat branch sweep%s:\n\n", mode)
	totals := pruneLocalPolecatBranchBacklog(out, targets, openRealLocalPruneRepo, apply)
	return localPruneRunError(totals)
}

// localPruneRunError turns a run's totals into the command's exit status. A
// branch the sweep decided to delete and could not leaves the backlog only
// partly swept, which a caller has to be able to detect; a skipped rig is the
// designed outcome for a rig that cannot be read, and stays success so one
// repo-less rig does not fail every scheduled sweep (gt-y3d7h).
func localPruneRunError(totals localPruneTotals) error {
	if totals.Failed == 0 {
		return nil
	}
	return fmt.Errorf("%d polecat branch(es) could not be swept; see the report above", totals.Failed)
}

// resolveLocalPruneTargets resolves every rig in the town config, in name
// order so two runs report their rigs alike.
func resolveLocalPruneTargets() ([]localPruneTarget, error) {
	rigs, err := getAllRigs()
	if err != nil {
		return nil, err
	}
	targets := make([]localPruneTarget, 0, len(rigs))
	for _, r := range rigs {
		targets = append(targets, localPruneTarget{Name: r.Name, RepoDir: rigPruneRepoDir(r.Path)})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

// rigPruneRepoDir picks the repo holding a rig's polecat branches, the same
// choice `gt polecat prune <rig>` makes: the shared bare .repo.git, else the
// legacy mayor/rig clone. An empty result means neither exists, which the
// sweep reports as a skipped rig.
func rigPruneRepoDir(rigRoot string) string {
	bare := filepath.Join(rigRoot, ".repo.git")
	if info, err := os.Stat(bare); err == nil && info.IsDir() {
		return bare
	}
	legacy := filepath.Join(rigRoot, "mayor", "rig")
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	return ""
}

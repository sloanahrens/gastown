package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// The git_hygiene patrol cleans each rig's repository (gitHygieneRepoPath):
// it deletes merged local branches, local agent branches whose remote branch
// is gone, and merged agent branches on origin, then runs git gc. It was the
// git-hygiene run.sh plugin (gt-4k3fj.8.5). Two things the script did are
// gone: it cleared every stash, which drops whatever uncommitted work someone
// parked there, and it deleted remote branches through `gh api`; the delete is
// now a git push guarded by the hash the merge check saw.
const defaultGitHygieneInterval = 12 * time.Hour

// gitHygieneProtected are branch names the patrol never deletes, besides the
// default and checked-out branches.
var gitHygieneProtected = map[string]bool{"main": true, "master": true, "refinery-patrol": true}

// gitHygieneLocalOrphanPrefixes name the local agent branches deleted, merged
// or not, once origin no longer has them.
var gitHygieneLocalOrphanPrefixes = []string{"polecat/", "dog/", "fix/", "pr-", "integration/", "worktree-agent-"}

// gitHygieneRemotePrefixes name the origin branches deleted once merged into
// the default branch.
var gitHygieneRemotePrefixes = []string{"polecat/", "fix/", "pr-", "integration/", "worktree-agent-"}

// gitHygieneResult counts one repository's cleanup.
type gitHygieneResult struct {
	merged, orphan, remote int
	gc                     bool
}

func gitHygieneInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.GitHygiene != nil {
		if d, err := time.ParseDuration(config.Patrols.GitHygiene.Interval); err == nil && d > 0 {
			return d
		}
	}
	return defaultGitHygieneInterval
}

// triggerGitHygiene runs a git_hygiene cycle on its own goroutine when the
// patrol is due: fetches and gc across every rig take minutes, and inline they
// would stall the heartbeat.
func (d *Daemon) triggerGitHygiene() {
	if !d.isPatrolActive("git_hygiene") {
		return
	}
	dec := evaluatePatrolDue(d.config.TownRoot, "git_hygiene", time.Time{}, d.clk().Now(), gitHygieneInterval(d.patrolConfig))
	if !dec.due {
		return
	}
	if !d.gitHygieneRunning.CompareAndSwap(false, true) {
		return
	}
	if dec.warn != "" {
		d.logger.Printf("git_hygiene: WARNING: %s — %s", dec.warn, dec.note)
	}
	go func() {
		defer d.gitHygieneRunning.Store(false)
		d.runGitHygiene()
	}()
}

// gitHygieneRepoPath returns the repository the patrol cleans for r: the
// rig's shared bare repo (<rig>/.repo.git) when it has one, else the working
// clone Rig.RepoPath reports ("" when it has neither).
//
// Cleaning the bare repo keeps the patrol's fetch out of the clone
// install-gt.sh merges into after a landing (gt-ckoau), and it is the
// repository polecat.Manager.repoBase and `gt polecat prune` already work in:
// the refs every polecat worktree shares.
func gitHygieneRepoPath(r *rig.Rig) string {
	bare := filepath.Join(r.Path, ".repo.git")
	if info, err := os.Stat(bare); err == nil && info.IsDir() {
		return bare
	}
	return r.RepoPath()
}

// runGitHygiene cleans every known rig's repository and records the run.
func (d *Daemon) runGitHygiene() {
	cycle := d.startDogCycle("git_hygiene")
	defer cycle.close()
	defer func() {
		if err := savePatrolLastRun(d.config.TownRoot, "git_hygiene", d.clk().Now()); err != nil {
			d.logger.Printf("git_hygiene: WARNING: cannot persist last-run time (%v)", err)
		}
	}()

	var repos []string
	for _, name := range d.getKnownRigs() {
		r := &rig.Rig{Name: name, Path: filepath.Join(d.config.TownRoot, name)}
		if p := gitHygieneRepoPath(r); p != "" {
			repos = append(repos, p)
		}
	}
	// No repository to clean is the failure the script guarded against
	// (gt-chqi): a run over nothing must not read as a clean run.
	if len(repos) == 0 {
		cycle.failStep("repos", "no rig has a repository (mayor/rigs.json lists none, or none has a .repo.git or a working clone)")
		return
	}

	var total gitHygieneResult
	for _, repo := range repos {
		res := d.cleanRigRepo(repo)
		total.merged += res.merged
		total.orphan += res.orphan
		total.remote += res.remote
		d.logger.Printf("git_hygiene: %s: %d merged, %d orphan, %d remote, gc=%t", repo, res.merged, res.orphan, res.remote, res.gc)
	}
	summary := fmt.Sprintf("%d repo(s): %d merged, %d orphan, %d remote branch(es) deleted", len(repos), total.merged, total.orphan, total.remote)
	if total.merged+total.orphan+total.remote == 0 {
		cycle.skipStep("branches", summary)
		return
	}
	d.logger.Printf("git_hygiene: %s", summary)
	cycle.closeStep("branches")
}

// cleanRigRepo cleans one repository. Every git failure is logged and the
// rest of the cleanup goes on, as the script's `|| true` did — including the
// branch delete git refuses for a branch a worktree still has checked out.
// That refusal protects only a live polecat: a polecat whose session died
// before gt done pushed leaves its branch ref in the shared bare repo with no
// worktree, so an orphan polecat/* branch is kept while it holds commits
// origin lacks (gt-a0jxl).
func (d *Daemon) cleanRigRepo(repo string) gitHygieneResult {
	var res gitHygieneResult
	g := d.gitAt(repo)
	logf := func(format string, args ...interface{}) {
		d.logger.Printf("git_hygiene: %s: "+format, append([]interface{}{repo}, args...)...)
	}

	if err := g.FetchPrune("origin"); err != nil {
		logf("fetch --prune: %v", err)
	}
	def := g.RemoteDefaultBranch()
	cur, _ := g.CurrentBranch()
	keep := func(b string) bool {
		return b == "" || b == def || b == cur || gitHygieneProtected[b] || strings.HasPrefix(b, "merge/")
	}

	// origin's branches, from ls-remote. When origin cannot be read, no
	// branch can be called orphaned or merged on origin, so only merged
	// local branches are deleted.
	remoteRefs, err := g.ListRemoteRefsWithHashes("origin", "refs/heads/")
	remoteOK := err == nil
	if !remoteOK {
		logf("ls-remote origin: %v", err)
	}
	onRemote := map[string]string{}
	for _, ref := range remoteRefs {
		onRemote[strings.TrimPrefix(ref.Name, "refs/heads/")] = ref.Hash
	}

	branches, err := g.ListBranches("")
	if err != nil {
		logf("list branches: %v", err)
	}
	for _, b := range branches {
		if keep(b) {
			continue
		}
		if merged, _ := g.IsAncestor(b, def); merged {
			if err := g.DeleteBranch(b, false); err != nil {
				logf("delete merged %s: %v", b, err)
				continue
			}
			res.merged++
			continue
		}
		if _, ok := onRemote[b]; !remoteOK || ok || !hasAnyPrefix(b, gitHygieneLocalOrphanPrefixes) {
			continue
		}
		if strings.HasPrefix(b, "polecat/") {
			holds, err := holdsUnpushedWork(g, def, b, onRemote)
			if err != nil {
				logf("keep orphan %s: cannot tell whether it holds unpushed commits: %v", b, err)
				continue
			}
			if holds {
				logf("keep orphan %s: it holds commits that are not on origin", b)
				continue
			}
		}
		if err := g.DeleteBranch(b, true); err != nil {
			logf("delete orphan %s: %v", b, err)
			continue
		}
		res.orphan++
	}

	for b, hash := range onRemote {
		if keep(b) || !hasAnyPrefix(b, gitHygieneRemotePrefixes) {
			continue
		}
		if merged, _ := g.IsAncestor(hash, "origin/"+def); !merged {
			continue
		}
		if err := g.DeleteRemoteBranchIfAt("origin", b, hash); err != nil {
			logf("delete origin/%s: %v", b, err)
			continue
		}
		res.remote++
	}

	if err := g.GC(); err != nil {
		logf("gc: %v", err)
	} else {
		res.gc = true
	}
	return res
}

// holdsUnpushedWork reports whether local branch b has a commit whose patch is
// on neither origin's default branch nor any of origin's integration branches,
// the test polecat.Manager.PlanStaleBranches applies (gt-429ue). Patch identity
// keeps a rebased or squash-landed branch from reading as unpushed work. An
// error means the answer is unknown, and callers keep the branch.
func holdsUnpushedWork(g daemonGit, def, b string, onRemote map[string]string) (bool, error) {
	bases := []string{"origin/" + def}
	var integration []string
	for name := range onRemote {
		if strings.HasPrefix(name, "integration/") {
			integration = append(integration, "origin/"+name)
		}
	}
	sort.Strings(integration)
	return polecat.UnmergedOnAllBases(g.Cherry, append(bases, integration...), "refs/heads/"+b)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

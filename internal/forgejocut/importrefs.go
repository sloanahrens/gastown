package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejoresync"
	"github.com/steveyegge/gastown/internal/util"
)

// Cutover is one cutover run: the options it was parsed with, the git it reads and
// writes through, and where it logs.
type Cutover struct {
	Opts *Options
	// Git runs one git argv and returns its stdout; nil runs the git binary, each
	// call bounded as forgejoresync bounds its own.
	Git forgejoresync.Runner
	// Log writes one line to the run's stderr; nil writes nothing.
	Log func(string)
	// MkdirTemp and RemoveAll make and remove the scratch directory the import's
	// repository sits in; nil means os.MkdirTemp and os.RemoveAll.
	MkdirTemp func(dir, pattern string) (string, error)
	RemoveAll func(path string) error
	// ProvisionRun is step 3's provisioning run; nil runs forgejoprov.Run in this
	// process. A test replaces it to count the call and to force its exit code.
	ProvisionRun ProvisionFunc
	// ProvisionEnv is what that run reads: the environment the admin token resolves
	// from, the token file reader and the Forgejo transport. Provision replaces its
	// Log with the run's own, so a progress line reaches the operator the way every
	// other step's lines do.
	ProvisionEnv forgejoprov.Env
}

// errRefsMissing is the final comparison's refusal: Forgejo does not hold every
// GitHub ref after the push, so a later mirror sync would prune GitHub.
var errRefsMissing = errors.New("Forgejo is missing GitHub refs after the import")

// forgejoRefspecs fetch Forgejo's refs into the scratch repository under
// refs/forgejo/*, beside GitHub's refs/*. The import pushes ref by ref, so the
// push can never send this side: fetching both into one repository is what lets
// the plan read one sha against the other.
var forgejoRefspecs = []string{"+refs/heads/*:refs/forgejo/heads/*", "+refs/tags/*:refs/forgejo/tags/*"}

// ImportRefs is step 2b: it imports every GitHub branch and tag Forgejo lacks or
// is behind on, and stops on a ref whose two sides diverged unless the run accepts
// skipping it.
//
// The push is plain — no '+', no --force, no --prune — because a rig already cut
// over has Forgejo ahead of a frozen GitHub, and a forced push would discard every
// commit landed since (gt-fn9e6.51).
//
// Every URL this run prints goes through util.RedactURL first: an operator-supplied
// remote can carry a credential in its userinfo, and the log is read and pasted
// into a bead.
func (c *Cutover) ImportRefs() error {
	o := c.Opts
	c.logf("importing every GitHub ref through %s without force", util.RedactURL(o.AdminGitURL))

	githubBefore, err := c.listRefs(o.GitHubURL)
	if err != nil {
		return err
	}
	forgejoBefore, err := c.listRefs(o.AdminGitURL)
	if err != nil {
		return err
	}

	scratch, err := c.makeScratch()
	if err != nil {
		return err
	}
	defer c.removeScratch(scratch)
	if err := c.fetchBoth(scratch); err != nil {
		return err
	}

	plan, err := Plan(githubBefore, forgejoBefore, ExecRefGit{Dir: scratch, Run: c.git()})
	if err != nil {
		return err
	}
	specs, diverged := c.importSpecs(plan)
	if len(diverged) > 0 && !o.AcceptDiverged {
		c.logf("ERROR: %d ref(s) have diverged between GitHub and Forgejo:", len(diverged))
		for _, ref := range diverged {
			c.logf("  %s", ref)
		}
		return errors.New("reconcile the diverged refs, or re-run with --accept-diverged to skip them")
	}

	if len(specs) == 0 {
		c.logf("no GitHub ref needs importing")
	} else {
		c.logf("+ git -C %s push --quiet -- %s %s", scratch, util.RedactURL(o.AdminGitURL), strings.Join(specs, " "))
		if !o.DryRun {
			if err := pushRefs(c.git(), scratch, o.AdminGitURL, specs); err != nil {
				return err
			}
		}
	}
	// A dry run stops here: the refs it would import were printed, and listing
	// both sides again would only say the push never happened.
	if o.DryRun {
		return nil
	}

	return c.checkEveryRefArrived()
}

// importSpecs logs the plan's line for every ref and returns the refspecs to push
// and the refs that diverged. A refspec is the plain ref-to-itself form: the plan
// already settled that a push of it is a creation or a fast-forward.
func (c *Cutover) importSpecs(plan []Decision) (specs, diverged []string) {
	for _, d := range plan {
		c.logf("%s", d.Line)
		switch d.Action {
		case ActionPush, ActionFastForward:
			specs = append(specs, d.Ref+":"+d.Ref)
		case ActionDiverged:
			diverged = append(diverged, d.Ref)
		}
	}
	return specs, diverged
}

// checkEveryRefArrived re-lists both sides and refuses a Forgejo copy that lacks a
// GitHub ref name. A mirror has no branch filter and prunes, so a name Forgejo
// lacks is deleted on GitHub at the next sync.
func (c *Cutover) checkEveryRefArrived() error {
	github, err := c.listRefs(c.Opts.GitHubURL)
	if err != nil {
		return err
	}
	forgejo, err := c.listRefs(c.Opts.AdminGitURL)
	if err != nil {
		return err
	}
	if missing := missingNames(github, forgejo); len(missing) > 0 {
		c.logf("ERROR: Forgejo is missing GitHub refs after the import; the mirror would prune them")
		for _, name := range missing {
			c.logf("%s", name)
		}
		return errRefsMissing
	}
	c.logf("imported every GitHub ref into %s (%d refs there)", c.Opts.Repo, len(forgejo))
	return nil
}

// missingNames is the refs github holds and forgejo does not, in ref-name order.
func missingNames(github, forgejo []forgejoresync.Ref) []string {
	have := make(map[string]bool, len(forgejo))
	for _, ref := range forgejo {
		have[ref.Name] = true
	}
	var missing []string
	for _, ref := range github {
		if !have[ref.Name] {
			missing = append(missing, ref.Name)
		}
	}
	return missing
}

// listRefs logs and reads one side's branch and tag listing, sorted by ref name.
func (c *Cutover) listRefs(url string) ([]forgejoresync.Ref, error) {
	c.logf("+ git ls-remote --heads --tags --refs %s", util.RedactURL(url))
	out, err := forgejoresync.ExecGit{Run: c.git()}.LsRemote(url)
	if err != nil {
		return nil, fmt.Errorf("could not list the refs at %s: %w", util.RedactURL(url), err)
	}
	refs, err := forgejoresync.ParseRefs(out)
	if err != nil {
		return nil, fmt.Errorf("could not read the ref listing at %s: %w", util.RedactURL(url), err)
	}
	return refs, nil
}

// fetchBoth fills the scratch repository with both sides' objects, so the plan can
// ask one side's sha against the other's. A dry run fetches too: these writes stay
// inside the scratch directory, and the plan is unreadable without them.
func (c *Cutover) fetchBoth(scratch string) error {
	git := forgejoresync.ExecGit{Run: c.git()}
	c.logf("+ git init --bare --quiet %s", scratch)
	if err := git.InitBare(scratch); err != nil {
		return fmt.Errorf("could not make the scratch repository %s: %w", scratch, err)
	}
	for _, side := range []struct {
		url      string
		refspecs []string
	}{
		{url: c.Opts.AdminGitURL, refspecs: forgejoRefspecs},
		{url: c.Opts.GitHubURL, refspecs: forgejoresync.Refspecs},
	} {
		c.logf("+ git -C %s fetch --quiet -- %s %s", scratch, util.RedactURL(side.url), strings.Join(side.refspecs, " "))
		if err := git.Fetch(scratch, side.url, side.refspecs...); err != nil {
			return fmt.Errorf("could not fetch %s into the scratch repository: %w", util.RedactURL(side.url), err)
		}
	}
	return nil
}

// pushRefs sends specs from the scratch repository to url as a plain push, so a
// ref Forgejo is ahead on cannot move. The cutover installs no signal handler, so
// the push runs under the background context (gt-csjwi).
func pushRefs(git forgejoresync.Runner, scratch, url string, specs []string) error {
	argv := append([]string{"git", "-C", scratch, "push", "--quiet", "--", url}, specs...)
	if _, err := git(context.Background(), argv); err != nil {
		return fmt.Errorf("the push of %d ref(s) to the admin base failed: %w", len(specs), err)
	}
	return nil
}

// makeScratch creates the scratch directory and returns the bare repository inside
// it, as the script's $WORK/import.git.
func (c *Cutover) makeScratch() (string, error) {
	mkdir := c.MkdirTemp
	if mkdir == nil {
		mkdir = os.MkdirTemp
	}
	work, err := mkdir("", "forgejo-cutover.")
	if err != nil {
		return "", fmt.Errorf("could not make the scratch directory: %w", err)
	}
	return filepath.Join(work, "import.git"), nil
}

// removeScratch removes the scratch directory makeScratch returned the repository
// of. A directory left behind is litter, not a failed step.
func (c *Cutover) removeScratch(scratch string) {
	rm := c.RemoveAll
	if rm == nil {
		rm = os.RemoveAll
	}
	_ = rm(filepath.Dir(scratch))
}

// git is the runner the cutover's git calls go through.
func (c *Cutover) git() forgejoresync.Runner {
	if c.Git != nil {
		return c.Git
	}
	return forgejoresync.RunExec
}

// logf writes one line of the run's log, when the run has one.
func (c *Cutover) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

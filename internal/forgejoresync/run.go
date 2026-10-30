package forgejoresync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// errRefsDiffer is the verify stage's refusal: Forgejo still differs from GitHub after the push.
var errRefsDiffer = errors.New("refs differ after the push")

// Run runs the stages in order and restores main's protection afterwards. The restore is
// deferred once the admin token resolves, so every exit past that point runs it: a failed
// stage, a panic, and success alike. A canceled run context (a signal in the command) stops
// the stages and returns ExitFailure once the restore has run; the restore keeps its own
// context, so the cancellation cannot skip it. A failed restore forces ExitFailure even when
// the stages succeeded, because main is left unprotected until someone reruns the provisioner
// by hand. The scratch directory goes after the restore, on every path that made one.
func (r *Resync) Run() (rc int) {
	var token forgejoprov.Secret
	ready := false
	defer r.removeWork()
	if err := r.runContext().Err(); err != nil {
		r.logf("run canceled: %v", err)
		return ExitFailure
	}
	defer func() {
		if !ready {
			return
		}
		if err := r.restore(token); err != nil {
			r.logf("%v", err)
			r.logf("ERROR: could not restore %s protection on %s; run the provisioner by hand", r.Opts.MainBranch, r.Opts.Repo)
			rc = ExitFailure
		}
	}()
	token, err := forgejoprov.ResolveAdminToken(r.getenv(), r.readFile(), r.Opts.AdminTokenFile)
	if err != nil {
		if errors.Is(err, forgejoprov.ErrNoAdminToken) {
			r.logf("%s", noAdminTokenLine(r.Opts.AdminTokenFile))
		} else {
			r.logf("%v", err)
		}
		return ExitFailure
	}
	ready = true
	if r.Client == nil {
		r.Client = &forgejoprov.Client{API: r.Opts.APIURL, HTTP: r.HTTP, Token: token, DryRun: r.Opts.DryRun}
	}
	if err := r.runSteps(); err != nil {
		r.logf("%v", err)
		return ExitFailure
	}
	// A signal can arrive as the last stage ends: the restore still runs, and the
	// run reports the cancellation rather than a convergence it did not see.
	if err := r.runContext().Err(); err != nil {
		r.logf("run canceled: %v", err)
		return ExitFailure
	}
	return ExitOK
}

// noAdminTokenLine is the refusal for a run with no admin token. It names this command's
// broker rule, which the provisioner's own refusal would not.
func noAdminTokenLine(tokenFile string) string {
	standIn := ""
	if tokenFile != "" {
		standIn = fmt.Sprintf(" (or FORGEJO_ADMIN_TOKEN= in %s)", tokenFile)
	}
	return fmt.Sprintf("no admin token: run this command through the auth broker (sudo -u gt-forge gt-approve run forgejo-resync -- <its arguments>), or set FORGEJO_ADMIN_TOKEN%s for a stand-in", standIn)
}

// runSteps runs each stage in order and stops at the first error. A stage the
// run's context was canceled before does not start: a signal must not lift main's
// protection on its way out.
func (r *Resync) runSteps() error {
	steps := r.Steps
	if steps == nil {
		steps = r.productionSteps()
	}
	for _, step := range steps {
		if err := r.runContext().Err(); err != nil {
			return fmt.Errorf("%s not run: %w", step.Name, err)
		}
		if err := step.Run(); err != nil {
			return fmt.Errorf("%s failed: %w", step.Name, err)
		}
	}
	return nil
}

// productionSteps are the five stages of a real run, in the script's order: list both
// sides and refuse a plan that would empty the Forgejo copy, fetch GitHub into the scratch
// repository, lift main's rule, push the mirror with --prune, then compare the two lists.
func (r *Resync) productionSteps() []Step {
	return []Step{
		{Name: "list", Run: r.listStage},
		{Name: "fetch", Run: r.fetchStage},
		{Name: "lift", Run: r.LiftProtection},
		{Name: "push", Run: r.pushStage},
		{Name: "verify", Run: r.verifyStage},
	}
}

// listStage reads both ref lists and logs the plan the push will carry out. It refuses a
// plan that would empty the Forgejo copy before the lift takes main's protection away.
func (r *Resync) listStage() error {
	github, err := r.lsRemote(r.Opts.GitHubURL)
	if err != nil {
		return err
	}
	forgejo, err := r.lsRemote(r.Opts.ForgejoURL)
	if err != nil {
		return err
	}
	push, del := Plan(github, forgejo)
	for _, ref := range push {
		r.logf("will push %s", ref)
	}
	for _, ref := range del {
		r.logf("will delete %s (absent from GitHub)", ref)
	}
	return guardPlan(github, del, r.Opts.MainBranch)
}

// guardPlan refuses a plan the pruning push must not carry out. A GitHub listing with no refs,
// or without the main branch, would prune the whole Forgejo copy, so the run stops before the
// lift. The check on the delete list cannot fire on a plan Plan built from a listing that holds
// main; it stays so a later change to Plan cannot let main go.
func guardPlan(github []Ref, del []string, branch string) error {
	main := "refs/heads/" + branch
	switch {
	case len(github) == 0:
		return errors.New("GitHub's ref listing is empty; refusing to prune the Forgejo copy to nothing")
	case !slices.ContainsFunc(github, func(ref Ref) bool { return ref.Name == main }):
		return fmt.Errorf("GitHub's ref listing lacks %s; refusing to prune it from the Forgejo copy", main)
	case slices.Contains(del, main):
		return fmt.Errorf("the plan deletes %s; refusing the pruning push", main)
	}
	return nil
}

// fetchStage imports every GitHub branch and tag into a scratch repository, so the push
// sends them all. A dry run logs the two git calls and makes nothing.
func (r *Resync) fetchStage() error {
	if !r.Opts.DryRun {
		work, err := r.makeWork()
		if err != nil {
			return fmt.Errorf("make the scratch dir: %w", err)
		}
		r.work = work
	}
	scratch := r.scratchPath()
	r.logf("+ git init --bare --quiet %s", scratch)
	r.logf("+ git -C %s fetch --quiet -- %s %s", scratch, r.Opts.GitHubURL, strings.Join(Refspecs, " "))
	if r.Opts.DryRun {
		return nil
	}
	if err := r.git().InitBare(scratch); err != nil {
		return err
	}
	return r.git().Fetch(scratch, r.Opts.GitHubURL, Refspecs...)
}

// pushStage sends the scratch repository's refs to Forgejo with --prune, so a branch GitHub
// dropped is deleted there. A dry run logs the push and sends nothing.
func (r *Resync) pushStage() error {
	scratch := r.scratchPath()
	r.logf("+ git -C %s push --prune --quiet -- %s %s", scratch, r.Opts.ForgejoURL, strings.Join(Refspecs, " "))
	if r.Opts.DryRun {
		return nil
	}
	return r.git().PushPrune(scratch, r.Opts.ForgejoURL, Refspecs...)
}

// verifyStage compares the two ref lists after the push. A difference means Forgejo does not
// hold every GitHub ref, and a later mirror sync would prune GitHub, so the run fails and
// names the refs it disagrees on.
func (r *Resync) verifyStage() error {
	if r.Opts.DryRun {
		r.logf("dry run: no write was sent")
		return nil
	}
	github, err := r.lsRemote(r.Opts.GitHubURL)
	if err != nil {
		return err
	}
	forgejo, err := r.lsRemote(r.Opts.ForgejoURL)
	if err != nil {
		return err
	}
	if !Equal(github, forgejo) {
		r.logf("ERROR: Forgejo and GitHub refs differ after the push; a mirror sync would prune GitHub")
		for _, line := range diffRefs(github, forgejo) {
			r.logf("%s", line)
		}
		return errRefsDiffer
	}
	r.logf("refs match: Forgejo holds every GitHub ref (%d refs)", len(forgejo))
	r.logf("resync complete")
	return nil
}

// lsRemote logs the listing it reads and returns the refs at url, sorted by name.
func (r *Resync) lsRemote(url string) ([]Ref, error) {
	r.logf("+ git ls-remote --heads --tags --refs %s", url)
	out, err := r.git().LsRemote(url)
	if err != nil {
		return nil, err
	}
	return ParseRefs(out)
}

// diffRefs names each ref the two listings disagree on, with the sha each side holds
// ("absent" where a side lacks the ref), in ref-name order.
func diffRefs(github, forgejo []Ref) []string {
	shas := func(refs []Ref) map[string]string {
		m := make(map[string]string, len(refs))
		for _, ref := range refs {
			m[ref.Name] = ref.SHA
		}
		return m
	}
	gh, fj := shas(github), shas(forgejo)
	names := make([]string, 0, len(gh)+len(fj))
	for name := range gh {
		names = append(names, name)
	}
	for name := range fj {
		if _, ok := gh[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	show := func(m map[string]string, name string) string {
		if sha, ok := m[name]; ok {
			return sha
		}
		return "absent"
	}
	var lines []string
	for _, name := range names {
		if show(gh, name) != show(fj, name) {
			lines = append(lines, fmt.Sprintf("  %s: GitHub %s, Forgejo %s", name, show(gh, name), show(fj, name)))
		}
	}
	return lines
}

// scratchPath is the bare repository the mirror is assembled in. A dry run makes no scratch
// directory, so it logs a placeholder in its place.
func (r *Resync) scratchPath() string {
	if r.work == "" {
		return "<scratch>"
	}
	return filepath.Join(r.work, "refs.git")
}

// makeWork creates the scratch directory the mirror is built in, through the MkdirTemp seam
// when a test sets one.
func (r *Resync) makeWork() (string, error) {
	if r.MkdirTemp != nil {
		return r.MkdirTemp("", "forgejo-resync.")
	}
	return os.MkdirTemp("", "forgejo-resync.")
}

// removeWork removes the scratch directory the fetch stage made, if it made one.
func (r *Resync) removeWork() {
	if r.work == "" {
		return
	}
	remove := r.RemoveAll
	if remove == nil {
		remove = os.RemoveAll
	}
	if err := remove(r.work); err != nil {
		r.logf("could not remove the scratch dir %s: %v", r.work, err)
	}
	r.work = ""
}

// git returns the git seam, or the git binary when none is set.
func (r *Resync) git() Git {
	if r.Git != nil {
		return r.Git
	}
	return NewExecGit(r.runContext())
}

// runContext never returns nil, so a stage's context cannot panic on it.
func (r *Resync) runContext() context.Context {
	if r.Context != nil {
		return r.Context
	}
	return context.Background()
}

package forgejocut

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// Action is what step 2's import does with one GitHub ref. The import never
// forces: only a ref Forgejo lacks or is behind on is pushed.
type Action int

// The five outcomes of the import, as scripts/forgejo-cutover.sh settles them.
const (
	// ActionSame leaves the ref alone: both sides hold the same sha.
	ActionSame Action = iota
	// ActionPush creates the ref from GitHub, its only side.
	ActionPush
	// ActionFastForward pushes GitHub's sha over Forgejo's, which is an ancestor
	// of it.
	ActionFastForward
	// ActionKeep leaves Forgejo's sha alone: GitHub's is an ancestor of it, so a
	// push would rewind the ref.
	ActionKeep
	// ActionDiverged leaves the ref to the operator: neither sha contains the
	// other, and only a human can reconcile them.
	ActionDiverged
)

// objectIDPattern is what a sha from a remote's ref listing may be: git's 40 hex
// digits, or 64 under SHA-256. Anything else never reaches a git argv, so a remote
// cannot smuggle an option in through the sha it reports.
var objectIDPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// RefGit is the git read the ref plan needs, answered from the scratch repository
// that holds both sides' objects: whether one sha is an ancestor of another, and
// how many commits one sha has that another lacks. Production binds ExecRefGit to
// that repository's directory; a test binds a fake.
type RefGit interface {
	// IsAncestor reports whether a is b, or an ancestor of b.
	IsAncestor(a, b string) bool
	// CommitsAhead is the number of commits b has that a does not, "?" when git
	// cannot answer.
	CommitsAhead(a, b string) string
}

// ExecRefGit is RefGit backed by the git binary, run through Run in the scratch
// repository at Dir.
type ExecRefGit struct {
	Dir string               // the scratch repository holding both sides' objects
	Run forgejoresync.Runner // runs one git argv; forgejoresync.RunExec in production
}

// IsAncestor reports whether a is b, or an ancestor of b.
func (g ExecRefGit) IsAncestor(a, b string) bool {
	if !objectIDPattern.MatchString(a) || !objectIDPattern.MatchString(b) {
		return false
	}
	// merge-base --is-ancestor answers by exit status, so a false answer and a
	// failed read are both an error here, as the script's is_ancestor read them.
	_, err := g.Run([]string{"git", "-C", g.Dir, "merge-base", "--is-ancestor", a, b})
	return err == nil
}

// CommitsAhead is the number of commits b has that a does not, "?" when git cannot
// answer.
func (g ExecRefGit) CommitsAhead(a, b string) string {
	if !objectIDPattern.MatchString(a) || !objectIDPattern.MatchString(b) {
		return "?"
	}
	out, err := g.Run([]string{"git", "-C", g.Dir, "rev-list", "--count", a + ".." + b})
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

// Classify is the import's decision for one ref: the sha GitHub holds and the one
// Forgejo holds, "" when Forgejo has no copy. ref is the name the decision is
// about and does not change the answer, since a tag follows the same rules as a
// branch. ancestry answers whether its first sha is the second or an ancestor of
// it; an ancestry neither side's sha satisfies is ActionDiverged, where an
// annotated tag whose sides differ lands because the ancestry checks do not hold
// for a tag object.
func Classify(ref, github, forgejo string, ancestry func(a, b string) bool) Action {
	switch {
	case forgejo == "":
		return ActionPush
	case github == forgejo:
		return ActionSame
	case ancestry(forgejo, github):
		return ActionFastForward
	case ancestry(github, forgejo):
		return ActionKeep
	}
	return ActionDiverged
}

// Decision is the plan's answer for one GitHub ref.
type Decision struct {
	Ref     forgejoresync.Ref // the GitHub ref: its name and the sha GitHub holds
	Forgejo string            // the sha Forgejo holds, "" when it has no copy
	Action  Action            // what the import does with it
	Line    string            // the log line the plan reports for it
}

// Plan is step 2a: one Decision per GitHub ref, sorted by ref name, each carrying
// the line the run logs for it. A ref only Forgejo holds is not in the plan; the
// import never deletes one.
func Plan(github, forgejo []forgejoresync.Ref, refs RefGit) []Decision {
	have := make(map[string]string, len(forgejo))
	for _, r := range forgejo {
		have[r.Name] = r.SHA
	}
	plan := make([]Decision, 0, len(github))
	for _, r := range github {
		d := Decision{Ref: r, Forgejo: have[r.Name]}
		d.Action = Classify(r.Name, r.SHA, d.Forgejo, refs.IsAncestor)
		d.Line = d.line(refs)
		plan = append(plan, d)
	}
	slices.SortFunc(plan, func(a, b Decision) int { return strings.Compare(a.Ref.Name, b.Ref.Name) })
	return plan
}

// line is the line the run logs for d, in the script's wording. The ahead count is
// read only for a kept ref, the one line that reports it, so a converged plan
// runs no rev-list at all.
func (d Decision) line(refs RefGit) string {
	switch d.Action {
	case ActionPush:
		return fmt.Sprintf("will import %s (Forgejo has no copy)", d.Ref.Name)
	case ActionSame:
		return fmt.Sprintf("up to date: %s is %s on both", d.Ref.Name, d.Ref.SHA)
	case ActionFastForward:
		return fmt.Sprintf("will import %s (fast-forward)", d.Ref.Name)
	case ActionKeep:
		// The count and both shas are the rewind note: they say what a push would
		// discard.
		return fmt.Sprintf("kept: %s — Forgejo is ahead of GitHub by %s commits (forgejo %s, github %s)",
			d.Ref.Name, refs.CommitsAhead(d.Ref.SHA, d.Forgejo), d.Forgejo, d.Ref.SHA)
	}
	// An action outside the five reads as diverged, the one outcome that stops the
	// run rather than moving a ref.
	return fmt.Sprintf("diverged: %s — Forgejo and GitHub have diverged (forgejo %s, github %s)",
		d.Ref.Name, d.Forgejo, d.Ref.SHA)
}

package forgejocut

import (
	"context"
	"errors"
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
	// ActionSame leaves the ref alone: both sides name the same commit.
	ActionSame Action = iota
	// ActionPush creates the ref from GitHub, its only side.
	ActionPush
	// ActionFastForward pushes GitHub's commit over Forgejo's, which is an ancestor
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
// that holds both sides' objects: the commit a sha names, whether one commit is an
// ancestor of another, and how many commits one has that another lacks. Production
// binds ExecRefGit to that repository's directory; a test binds a fake.
type RefGit interface {
	// Peel returns the commit sha names: the sha itself, or the commit an
	// annotated tag object points at.
	Peel(sha string) (string, error)
	// IsAncestor reports whether a is b, or an ancestor of b. An error is a read
	// git could not complete, which is not an answer.
	IsAncestor(a, b string) (bool, error)
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

// run runs one git argv. The cutover installs no signal handler, so every call
// runs under the background context (gt-csjwi).
func (g ExecRefGit) run(argv []string) ([]byte, error) {
	return g.Run(context.Background(), argv)
}

// Peel returns the commit sha names: the sha itself, or the commit an annotated
// tag object points at.
func (g ExecRefGit) Peel(sha string) (string, error) {
	if !objectIDPattern.MatchString(sha) {
		return "", errors.New("ref plan: rev-parse --verify: sha is not an object id")
	}
	out, err := g.run([]string{"git", "-C", g.Dir, "rev-parse", "--verify", sha + "^{commit}"})
	if err != nil {
		return "", fmt.Errorf("ref plan: rev-parse --verify: %w", err)
	}
	commit := strings.TrimSpace(string(out))
	if !objectIDPattern.MatchString(commit) {
		return "", errors.New("ref plan: rev-parse --verify printed no object id")
	}
	return commit, nil
}

// IsAncestor reports whether a is b, or an ancestor of b. merge-base answers by
// exit status: 1 is git saying no, and every other failure is a read that never
// happened, which the plan reports rather than reads as a divergence.
func (g ExecRefGit) IsAncestor(a, b string) (bool, error) {
	if !objectIDPattern.MatchString(a) || !objectIDPattern.MatchString(b) {
		return false, errors.New("ref plan: merge-base --is-ancestor: sha is not an object id")
	}
	_, err := g.run([]string{"git", "-C", g.Dir, "merge-base", "--is-ancestor", a, b})
	if err == nil {
		return true, nil
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) && coded.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("ref plan: merge-base --is-ancestor: %w", err)
}

// CommitsAhead is the number of commits b has that a does not, "?" when git cannot
// answer.
func (g ExecRefGit) CommitsAhead(a, b string) string {
	if !objectIDPattern.MatchString(a) || !objectIDPattern.MatchString(b) {
		return "?"
	}
	out, err := g.run([]string{"git", "-C", g.Dir, "rev-list", "--count", a + ".." + b})
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

// Ancestry answers whether its first sha is the second, or an ancestor of it. An
// error is a read git could not complete, never a no: a no means the second names
// a commit the first lacks.
type Ancestry func(a, b string) (bool, error)

// Classify is the import's decision for one ref from the commits each side names,
// "" for a side with no copy. An error is an ancestry read that failed: the Action
// beside it is not a decision, and the plan reports the error rather than answering
// diverged.
func Classify(github, forgejo string, ancestry Ancestry) (Action, error) {
	switch {
	case forgejo == "":
		return ActionPush, nil
	case github == forgejo:
		return ActionSame, nil
	}
	behind, err := ancestry(forgejo, github)
	if err != nil {
		return 0, err
	}
	if behind {
		return ActionFastForward, nil
	}
	ahead, err := ancestry(github, forgejo)
	if err != nil {
		return 0, err
	}
	if ahead {
		return ActionKeep, nil
	}
	return ActionDiverged, nil
}

// Decision is the plan's answer for one GitHub ref: the commit each side names,
// what the import does with it, and the line the run logs for it.
type Decision struct {
	Ref     string // the GitHub ref's name
	Github  string // the commit GitHub's ref names, or the sha it lists when the sides are not compared
	Forgejo string // the commit Forgejo's ref names, "" when Forgejo has no copy
	Action  Action // what the import does with it
	Line    string // the log line the plan reports for it
}

// Plan is step 2a: one Decision per GitHub ref, sorted by ref name, each carrying
// the line the run logs for it. A ref only Forgejo holds is not in the plan; the
// import never deletes one. An error is a read git could not answer: the plan
// reports it rather than deciding any ref on a read that never happened.
func Plan(github, forgejo []forgejoresync.Ref, refs RefGit) ([]Decision, error) {
	have := make(map[string]string, len(forgejo))
	for _, r := range forgejo {
		have[r.Name] = r.SHA
	}
	plan := make([]Decision, 0, len(github))
	for _, r := range github {
		d, err := decide(r, have[r.Name], refs)
		if err != nil {
			return nil, fmt.Errorf("plan %s: %w", r.Name, err)
		}
		plan = append(plan, d)
	}
	slices.SortFunc(plan, func(a, b Decision) int { return strings.Compare(a.Ref, b.Ref) })
	return plan, nil
}

// decide is the plan's row for one listed ref. A tag's sha names a tag object, so
// two sides at a tag are compared by the commit they peel to: a tag re-created on
// one side would otherwise read as a divergence. A branch's sha is already a
// commit, and a tag with no Forgejo copy is pushed without a comparison.
func decide(g forgejoresync.Ref, forgejo string, refs RefGit) (Decision, error) {
	d := Decision{Ref: g.Name, Github: g.SHA, Forgejo: forgejo}
	if forgejo != "" && strings.HasPrefix(g.Name, "refs/tags/") {
		var err error
		if d.Github, err = refs.Peel(d.Github); err != nil {
			return Decision{}, err
		}
		if d.Forgejo, err = refs.Peel(d.Forgejo); err != nil {
			return Decision{}, err
		}
	}
	action, err := Classify(d.Github, d.Forgejo, refs.IsAncestor)
	if err != nil {
		return Decision{}, err
	}
	d.Action = action
	d.Line = d.line(refs)
	return d, nil
}

// line is the line the run logs for d, in the script's wording. The ahead count is
// read only for a kept ref, the one line that reports it, so a converged plan
// runs no rev-list at all.
func (d Decision) line(refs RefGit) string {
	switch d.Action {
	case ActionPush:
		return fmt.Sprintf("will import %s (Forgejo has no copy)", d.Ref)
	case ActionSame:
		return fmt.Sprintf("up to date: %s is %s on both", d.Ref, d.Github)
	case ActionFastForward:
		return fmt.Sprintf("will import %s (fast-forward)", d.Ref)
	case ActionKeep:
		// The count and both commits are the rewind note: they say what a push
		// would discard.
		return fmt.Sprintf("kept: %s — Forgejo is ahead of GitHub by %s commits (forgejo %s, github %s)",
			d.Ref, refs.CommitsAhead(d.Github, d.Forgejo), d.Forgejo, d.Github)
	}
	// An action outside the five reads as diverged, the one outcome that stops the
	// run rather than moving a ref.
	return fmt.Sprintf("diverged: %s — Forgejo and GitHub have diverged (forgejo %s, github %s)",
		d.Ref, d.Forgejo, d.Github)
}

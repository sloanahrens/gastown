package landworker

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// LabelRedMain marks a bead the red-main owner filed for one package that
// stayed red on main after a rerun.
const LabelRedMain = "red-main"

// redMainNoPackage stands in for the package when a red run named no failing
// Go package (a build or shell-test failure, a test budget overrun).
const redMainNoPackage = "(no Go package named)"

// RedMainBeads is what the red-main owner reads and writes beads through.
type RedMainBeads interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	AddComment(id, text string) error
	CloseWithReason(reason string, ids ...string) error
}

// RedMain owns a rig's red main (gt-v4ssj.4): the PostLandRunner hands it
// every post-landing verdict. On red it reruns each failing package once at
// the same commit; a package that fails again gets one open bead (a new
// red-main bead, or a comment on the one already open for it). On green it
// closes the open beads for every package that passed. Each verdict ends in
// one status line, which is the signal: never an expiring nudge.
//
// Reverting a single culprit landing through Land() is gt-v4ssj.4.1.
type RedMain struct {
	Rig   string
	Beads RedMainBeads
	// Rerun runs one package's tests at pl.Commit for the tier cmd ran.
	Rerun func(ctx context.Context, cmd, pkg string, pl PostLand) PostLandResult
	// Status records the rig's one-line main status.
	Status func(line string)
	Logf   func(format string, args ...any)
}

func (r *RedMain) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf("landing_worker: "+r.Rig+": red-main: "+format, args...)
	}
}

func (r *RedMain) status(line string) {
	r.logf("status: %s", line)
	if r.Status != nil {
		r.Status(line)
	}
}

// RedMainTitle is the title of the red-main bead for pkg on rig. It is the
// key an open bead is found by, so it must not change for a package.
func RedMainTitle(rig, pkg string) string {
	return fmt.Sprintf("red main (%s): %s", rig, pkg)
}

// failingPackages is each package res reported as FAIL, once, in order.
func failingPackages(res PostLandResult) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range res.Packages {
		if !p.Passed && !seen[p.Package] {
			seen[p.Package] = true
			out = append(out, p.Package)
		}
	}
	return out
}

// Red handles a red run of cmd at pl.Commit.
func (r *RedMain) Red(ctx context.Context, cmd string, pl PostLand, res PostLandResult) {
	pkgs := failingPackages(res)
	var stillRed, flaky []string
	tails := map[string]string{}
	if len(pkgs) == 0 {
		// Nothing to rerun: the whole command is the failing unit.
		stillRed = []string{redMainNoPackage}
		tails[redMainNoPackage] = res.Tail
	}
	for _, pkg := range pkgs {
		if ctx.Err() != nil {
			return
		}
		rr := r.Rerun(ctx, cmd, pkg, pl)
		switch {
		case rr.Err != nil:
			if ctx.Err() != nil {
				return
			}
			r.logf("rerun of %s at %s could not run (%v); counting it red", pkg, short(pl.Commit), rr.Err)
			stillRed = append(stillRed, pkg)
			tails[pkg] = res.Tail
		case rr.ExitCode == 0:
			r.logf("%s passed its rerun at %s: flaky, no bead", pkg, short(pl.Commit))
			flaky = append(flaky, pkg)
		default:
			stillRed = append(stillRed, pkg)
			tails[pkg] = rr.Tail
		}
	}
	open := r.openBeads()
	var filed []string
	for _, pkg := range stillRed {
		id := r.fileOrComment(open, cmd, pkg, pl, tails[pkg])
		filed = append(filed, fmt.Sprintf("%s [%s]", pkg, id))
	}
	// A package that passed in this run (or on its rerun) is no longer red.
	r.closePassed(open, passedPackages(res, flaky), pl)
	line := fmt.Sprintf("main GREEN at %s after rerun (landed by %s)", short(pl.Commit), pl.BeadID)
	if len(filed) > 0 {
		line = fmt.Sprintf("main RED at %s (landed by %s): %s", short(pl.Commit), pl.BeadID, strings.Join(filed, ", "))
	}
	if len(flaky) > 0 {
		line += "; flaky (passed on rerun): " + strings.Join(flaky, ", ")
	}
	r.status(line)
}

// Green handles a green run of cmd at pl.Commit: every open red-main bead is
// closed, the one for a red run that named no package included.
func (r *RedMain) Green(_ context.Context, _ string, pl PostLand, _ PostLandResult) {
	open := r.openBeads()
	all := map[string]bool{}
	for pkg := range open {
		all[pkg] = true
	}
	r.closePassed(open, all, pl)
	r.status(fmt.Sprintf("main GREEN at %s (landed by %s)", short(pl.Commit), pl.BeadID))
}

func passedPackages(res PostLandResult, flaky []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range res.Packages {
		if p.Passed {
			out[p.Package] = true
		}
	}
	for _, p := range flaky {
		out[p] = true
	}
	return out
}

// openBeads maps package to the open red-main bead filed for it on this rig.
// A failed read returns nil, so a red package files a duplicate rather than
// going unreported.
func (r *RedMain) openBeads() map[string]string {
	issues, err := r.Beads.List(beads.ListOptions{Status: "open", Label: LabelRedMain, Priority: -1, Limit: 0})
	if err != nil {
		r.logf("listing open %s beads: %v", LabelRedMain, err)
		return nil
	}
	prefix := RedMainTitle(r.Rig, "")
	out := map[string]string{}
	for _, is := range issues {
		if pkg, ok := strings.CutPrefix(is.Title, prefix); ok && pkg != "" {
			out[pkg] = is.ID
		}
	}
	return out
}

func (r *RedMain) fileOrComment(open map[string]string, cmd, pkg string, pl PostLand, tail string) string {
	detail := fmt.Sprintf("%s at %s (landed by %s) via %q; failed again on a rerun of the package. Last lines:\n%s",
		pkg, pl.Commit, pl.BeadID, cmd, lastLines(tail, postLandTailLines))
	if pkg == redMainNoPackage {
		detail = fmt.Sprintf("%q failed at %s (landed by %s) without naming a failing Go package, so nothing was rerun. Last lines:\n%s",
			cmd, pl.Commit, pl.BeadID, lastLines(tail, postLandTailLines))
	}
	if id, ok := open[pkg]; ok {
		if err := r.Beads.AddComment(id, "still red: "+detail); err != nil {
			r.logf("commenting on %s: %v", id, err)
		}
		return id
	}
	is, err := r.Beads.Create(beads.CreateOptions{
		Title:    RedMainTitle(r.Rig, pkg),
		Labels:   []string{LabelRedMain},
		Priority: 1,
		Description: "The daemon's post-landing run found main red (gt-v4ssj.4). " + detail +
			"\n\nFix it on main. The red-main owner closes this bead on the first post-landing run in which the package passes.",
	})
	if err != nil {
		r.logf("filing the red-main bead for %s: %v", pkg, err)
		return "not filed"
	}
	return is.ID
}

func (r *RedMain) closePassed(open map[string]string, passed map[string]bool, pl PostLand) {
	for pkg, id := range open {
		if !passed[pkg] {
			continue
		}
		reason := fmt.Sprintf("green on main at %s (landed by %s)", pl.Commit, pl.BeadID)
		if err := r.Beads.CloseWithReason(reason, id); err != nil {
			r.logf("closing %s: %v", id, err)
		}
	}
}

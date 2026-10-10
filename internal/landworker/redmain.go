package landworker

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/promote"
)

// LabelRedMain marks a bead the red-main owner filed for one package that
// stayed red on main after a rerun.
const LabelRedMain = "red-main"

// redMainNoPackage stands in for the package when a red run named no failing
// Go package (a build or shell-test failure, a test budget overrun).
const redMainNoPackage = "(no Go package named)"

// RedMainBeads is what the red-main owner reads and writes beads through.
type RedMainBeads interface {
	Show(id string) (*beads.Issue, error)
	Update(id string, opts beads.UpdateOptions) error
	AppendNotes(id, note string) error
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
// With State, Landings, Revert and Diff set it also reverts a culprit
// (gt-v4ssj.4.1): when the red commit's landing was built on the last green
// commit, that landing is the only change between green and red, and the
// diff can have moved what failed, a revert of it is filed as a work bead and
// landed through Land by the worker. With more than one change in between, or
// a failure the diff cannot have moved, the red-main beads stand alone.
type RedMain struct {
	Rig   string
	Beads RedMainBeads
	// Rerun runs one package's tests at pl.Commit for the tier cmd ran.
	Rerun func(ctx context.Context, cmd, pkg string, pl PostLand) PostLandResult
	// Status records the rig's one-line main status.
	Status func(line string)
	Logf   func(format string, args ...any)
	// State remembers the last green commit; nil turns reverts off.
	State MainStateStore
	// Landings finds the red commit's landing record.
	Landings Landings
	// Revert builds and pushes a revert branch.
	Revert RevertBuilder
	// Diff lists the paths a landing changed, so a revert is only filed when
	// that landing's diff can have moved what failed (gt-40so9). A nil Diff,
	// or an error, skips the revert: an unattributable red main is a human's
	// call, never a landing spent on a guess.
	Diff func(ctx context.Context, rec land.LandingRecord) ([]string, error)
	// Promote fast-forwards the rig's GitHub main on a green verdict
	// (gt-fn9e6.37). nil leaves the verdict with no promotion path at all: a
	// rig with no promote_target, or one whose tier sweep owns the promotion
	// and promotes from its own fully green cycle instead (gt-fn9e6.38).
	Promote *promote.Promoter
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
			r.superseded(pl, res)
			return
		}
		rr := r.Rerun(ctx, cmd, pkg, pl)
		if ctx.Err() != nil {
			// Killed by the stop, not by the test: no verdict either way.
			r.superseded(pl, res)
			return
		}
		switch {
		case rr.Err != nil:
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
	r.recordVerdict(pl, len(stillRed) == 0)
	open := r.openBeads()
	var filed []string
	for _, pkg := range stillRed {
		id := r.fileOrComment(open, cmd, pkg, pl, res, tails[pkg])
		filed = append(filed, fmt.Sprintf("%s [%s]", pkg, id))
	}
	// A package that passed in this run (or on its rerun) is no longer red.
	r.closePassed(open, passedPackages(res, flaky), pl)
	line := fmt.Sprintf("main GREEN at %s after rerun (%s)", short(pl.Commit), pl.by())
	if len(filed) > 0 {
		line = fmt.Sprintf("main RED at %s (%s): %s", short(pl.Commit), pl.by(), strings.Join(filed, ", "))
	}
	if len(flaky) > 0 {
		line += "; flaky (passed on rerun): " + strings.Join(flaky, ", ")
	}
	if len(filed) > 0 {
		if did := r.maybeRevert(ctx, pl, redBlame(stillRed, res.ShellFailures)); did != "" {
			line += "; " + did
		}
	}
	r.status(line)
}

// superseded records a red run whose reruns the daemon's stop cut short
// (gt-f2voh). Nothing is filed and no verdict is recorded, so the restarted
// worker runs the post-landing command again from the last verdict to the
// tip; that run's verdict stands for this one.
func (r *RedMain) superseded(pl PostLand, res PostLandResult) {
	r.logf("RED at %s (%s) superseded: the daemon stopped before the reruns reached a verdict; the restarted worker reruns the post-landing command at the untested tip%s",
		short(pl.Commit), pl.by(), fullLog(res.LogPath))
}

// Green handles a green run of cmd at pl.Commit: GitHub main is advanced to it
// when this rig promotes, and every open red-main bead is closed, the one for
// a red run that named no package included.
func (r *RedMain) Green(_ context.Context, _ string, pl PostLand, _ PostLandResult) {
	r.recordVerdict(pl, true)
	r.promote(pl.Commit)
	open := r.openBeads()
	all := map[string]bool{}
	for pkg := range open {
		all[pkg] = true
	}
	r.closePassed(open, all, pl)
	r.status(fmt.Sprintf("main GREEN at %s (%s)", short(pl.Commit), pl.by()))
}

// promote advances GitHub main to commit and records the promotion beside the
// other main state. A failure is already recorded in the returned state, so
// the verdict's own work (beads, status) goes on.
func (r *RedMain) promote(commit string) {
	if r.Promote == nil {
		return
	}
	st, ok := r.loadMainState()
	if !ok {
		return
	}
	promoted := r.Promote.Promote(st.State, commit)
	if r.State == nil {
		return
	}
	// The push above runs for seconds against the network while the tier
	// sweep and another verdict write this same record: re-read it and carry
	// over only the promotion State, so a verdict written during the push
	// survives this save (gt-8iq4h).
	if st, ok = r.loadMainState(); !ok {
		return
	}
	st.State = promoted
	if err := r.State.Save(st); err != nil {
		r.logf("saving the promotion state: %v", err)
	}
}

// loadMainState reads the rig's main state, reporting false when it cannot.
func (r *RedMain) loadMainState() (MainState, bool) {
	if r.State == nil {
		return MainState{}, true
	}
	st, err := r.State.Load()
	if err != nil {
		r.logf("reading the main state: %v", err)
		return MainState{}, false
	}
	return st, true
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
	open, err := openBeadsByTitle(r.Beads, LabelRedMain, RedMainTitle(r.Rig, ""))
	if err != nil {
		r.logf("listing open %s beads: %v", LabelRedMain, err)
		return nil
	}
	return open
}

// openBeadsByTitle maps the rest of each open bead's title after prefix
// (the key it was filed under) to its id, for the beads labeled label.
func openBeadsByTitle(bd interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
}, label, prefix string) (map[string]string, error) {
	issues, err := bd.List(beads.ListOptions{Status: "open", Label: label, Priority: -1, Limit: 0})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, is := range issues {
		if key, ok := strings.CutPrefix(is.Title, prefix); ok && key != "" {
			out[key] = is.ID
		}
	}
	return out, nil
}

// fileOrComment comments on openID when a bead is already open for the
// same key, else files create. It returns the bead's id.
func fileOrComment(bd interface {
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	AddComment(id, text string) error
}, openID, comment string, create beads.CreateOptions) (string, error) {
	if openID != "" {
		if err := bd.AddComment(openID, comment); err != nil {
			return openID, fmt.Errorf("commenting on %s: %w", openID, err)
		}
		return openID, nil
	}
	is, err := bd.Create(create)
	if err != nil {
		return "", fmt.Errorf("filing %q: %w", create.Title, err)
	}
	return is.ID, nil
}

func (r *RedMain) fileOrComment(open map[string]string, cmd, pkg string, pl PostLand, res PostLandResult, tail string) string {
	detail := fmt.Sprintf("%s at %s (%s) via %q; failed again on a rerun of the package. Last lines:\n%s",
		pkg, pl.Commit, pl.by(), cmd, lastLines(tail, postLandTailLines))
	if pkg == redMainNoPackage {
		detail = fmt.Sprintf("%q exited %d at %s (%s) without naming a failing Go package (a build or shell-test failure, a timeout or a kill), so nothing was rerun. Last lines:\n%s",
			cmd, res.ExitCode, pl.Commit, pl.by(), lastLines(tail, postLandTailLines))
	}
	if res.LogPath != "" {
		detail += "\n\nFull log: " + res.LogPath
	}
	if pl.Direct {
		detail += fmt.Sprintf("\n\nThe commit reached main by a direct push, not a landing: suspect range %s..%s.", pl.From, pl.Commit)
	}
	id, err := fileOrComment(r.Beads, open[pkg], "still red: "+detail, beads.CreateOptions{
		Title:    RedMainTitle(r.Rig, pkg),
		Labels:   []string{LabelRedMain},
		Priority: 1,
		Description: "The daemon's post-landing run found main red (gt-v4ssj.4). " + detail +
			"\n\nFix it on main. The red-main owner closes this bead on the first post-landing run in which the package passes.",
	})
	if err != nil {
		r.logf("red-main bead for %s: %v", pkg, err)
		if id == "" {
			return "not filed"
		}
	}
	return id
}

func (r *RedMain) closePassed(open map[string]string, passed map[string]bool, pl PostLand) {
	for pkg, id := range open {
		if !passed[pkg] {
			continue
		}
		reason := fmt.Sprintf("green on main at %s (%s)", pl.Commit, pl.by())
		if err := r.Beads.CloseWithReason(reason, id); err != nil {
			r.logf("closing %s: %v", id, err)
		}
	}
}

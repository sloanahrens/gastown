package landworker

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// LabelRevert marks a work bead the red-main owner filed to revert one
// landing that turned main red (gt-v4ssj.4.1).
const LabelRevert = "gt:revert"

// RevertNoteMarker opens the notes block that names what a revert bead
// reverts. The landing worker reads it after the revert lands to reopen the
// culprit.
const RevertNoteMarker = "REVERT OF"

// MainState is what the red-main owner remembers about a rig's main between
// verdicts and across daemon restarts.
type MainState struct {
	// LastGreen is the newest commit a post-landing run passed at.
	LastGreen string `json:"last_green,omitempty"`
	// LastRun is the newest commit a post-landing run reached a verdict at.
	// The worker reads it to tell a direct push from main it has tested.
	LastRun string `json:"last_run,omitempty"`
}

// MainStateStore persists a rig's MainState. The daemon keeps it beside the
// red-main status line (.runtime/red-main/<rig>.json).
type MainStateStore interface {
	Load() (MainState, error)
	Save(MainState) error
}

// MemoryMainState is a MainStateStore that lives only as long as the process.
type MemoryMainState struct {
	mu sync.Mutex
	st MainState
}

func (m *MemoryMainState) Load() (MainState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st, nil
}

func (m *MemoryMainState) Save(st MainState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st = st
	return nil
}

// RevertBuilder commits the revert of rec.LandedCommit on a branch named
// branch, pushes it to origin, and returns the pushed head.
type RevertBuilder func(ctx context.Context, rec land.LandingRecord, branch string) (string, error)

// RevertTitle is the title of the revert bead for culprit. It is the key an
// open revert is found by, so one culprit is never reverted twice at once.
func RevertTitle(rig, culprit string) string {
	return fmt.Sprintf("revert (%s): %s", rig, culprit)
}

// RevertBranch is the branch a revert of culprit's landing is pushed to.
func RevertBranch(culprit, landed string) string {
	return "revert/" + culprit + "-" + short(landed)
}

// FormatRevertNote renders the REVERT OF block for a revert of rec.
func FormatRevertNote(rec land.LandingRecord) string {
	return fmt.Sprintf("%s\nculprit: %s\nlanded_commit: %s", RevertNoteMarker, land.NoteField(rec.BeadID), land.NoteField(rec.LandedCommit))
}

// ParseRevertNote reads the last REVERT OF block in notes.
func ParseRevertNote(notes string) (culprit, landed string, ok bool) {
	idx := strings.LastIndex(notes, RevertNoteMarker+"\n")
	if idx < 0 {
		return "", "", false
	}
	lines := strings.SplitN(notes[idx+len(RevertNoteMarker)+1:], "\n", 3)
	for _, line := range lines[:min(2, len(lines))] {
		key, value, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(key) {
		case "culprit":
			culprit = strings.TrimSpace(value)
		case "landed_commit":
			landed = strings.TrimSpace(value)
		}
	}
	return culprit, landed, culprit != "" && landed != ""
}

// by names who put pl.Commit on main, for status lines and bead text.
func (pl PostLand) by() string {
	if pl.Direct {
		return fmt.Sprintf("direct push %s..%s", short(pl.From), short(pl.Commit))
	}
	return "landed by " + pl.BeadID
}

// recordVerdict remembers that a run at pl.Commit was green or red.
func (r *RedMain) recordVerdict(pl PostLand, green bool) {
	if r.State == nil {
		return
	}
	st, err := r.State.Load()
	if err != nil {
		r.logf("reading the main state: %v", err)
		return
	}
	st.LastRun = pl.Commit
	if green {
		st.LastGreen = pl.Commit
	}
	if err := r.State.Save(st); err != nil {
		r.logf("saving the main state: %v", err)
	}
}

// maybeRevert files a revert of pl's landing when it is the only landing
// between the last green commit and the red one, and its diff can have moved
// what failed: then it is the culprit. It returns what it did, for the status
// line, or "" when a revert is not this owner's call (a direct push, revert
// tracking off).
func (r *RedMain) maybeRevert(ctx context.Context, pl PostLand, b blame) string {
	if pl.Direct || pl.BeadID == "" || r.State == nil || r.Landings == nil || r.Revert == nil {
		return ""
	}
	st, err := r.State.Load()
	if err != nil {
		r.logf("reading the main state: %v", err)
		return "no revert: main state unreadable"
	}
	if st.LastGreen == "" {
		return "no revert: no green commit recorded yet"
	}
	rec, found, err := r.Landings.LatestForBead(pl.BeadID)
	if err != nil || !found || rec.LandedCommit != pl.Commit {
		r.logf("no landing record of %s at %s (found %v, err %v); not reverting", pl.BeadID, short(pl.Commit), found, err)
		return "no revert: no landing record for the red commit"
	}
	if rec.Base != st.LastGreen {
		return fmt.Sprintf("no revert: more than one change since the last green %s", short(st.LastGreen))
	}
	culprit, err := r.Beads.Show(pl.BeadID)
	if err != nil {
		r.logf("reading %s: %v", pl.BeadID, err)
		return "no revert: culprit bead unreadable"
	}
	if beads.HasLabel(culprit, LabelRevert) {
		return "no revert: the culprit is itself a revert"
	}
	if id := r.openRevert(pl.BeadID); id != "" {
		return fmt.Sprintf("revert of %s already open as %s", pl.BeadID, id)
	}
	if why := r.unattributableFromDiff(ctx, rec, b); why != "" {
		return "no revert: " + why
	}
	if ctx.Err() != nil {
		return ""
	}
	branch := RevertBranch(pl.BeadID, rec.LandedCommit)
	head, err := r.Revert(ctx, rec, branch)
	if err != nil {
		r.logf("building the revert of %s (%s): %v", pl.BeadID, short(rec.LandedCommit), err)
		return fmt.Sprintf("no revert: building it failed (%v)", land.NoteField(err.Error()))
	}
	id, err := r.fileRevert(rec, branch, head)
	if err != nil {
		r.logf("filing the revert of %s: %v", pl.BeadID, err)
		return fmt.Sprintf("no revert: filing it failed (%v)", land.NoteField(err.Error()))
	}
	return fmt.Sprintf("reverting %s as %s", pl.BeadID, id)
}

// blame is what one red post-landing run held responsible: the Go packages
// still red after their rerun, and the scripts the run itself named. An empty
// kind is one the run named none of; a run that named neither (a build the
// merged tree failed) has no unit a diff can be checked against.
type blame struct {
	packages []string
	scripts  []string
}

// redBlame reads a run's failing units: stillRed holds redMainNoPackage when
// the run named no Go package, and the scripts the run named then stand for
// the whole-command failure that key describes.
func redBlame(stillRed, scripts []string) blame {
	b := blame{scripts: scripts}
	for _, p := range stillRed {
		if p != redMainNoPackage {
			b.packages = append(b.packages, p)
		}
	}
	return b
}

var (
	// goVerdictInputsRE is the paths whose change can move a Go test's
	// verdict: the sources, the module files, the Makefile whose targets run
	// the tier, and the test policy that decides which packages pass.
	goVerdictInputsRE = regexp.MustCompile(`(\.go$|go\.mod$|go\.sum$|Makefile$|^internal/testpolicy/)`)
	// shellVerdictInputsRE is the same rule for the shell tier, the tier's
	// own (land.ShellTierInputs).
	shellVerdictInputsRE = regexp.MustCompile(land.ShellTierInputs)
)

// unattributable names a unit a red run blamed that a landing with those
// changed paths cannot have moved, or "" when it can have moved every blamed
// unit: a failing script only by a shell-tier input, a failing Go package only
// by a Go input. Every Go input counts for every package (a package's
// dependencies span the module), so the check over-approximates "downstream"
// instead of guessing an import graph: only a landing that changed no Go code
// at all is ruled out of a Go failure (gt-40so9).
func unattributable(changed []string, b blame) string {
	if len(b.packages) > 0 && !anyMatch(changed, goVerdictInputsRE) {
		return fmt.Sprintf("%s: the landing changed no Go input (%s)", strings.Join(b.packages, ", "), pathsBrief(changed))
	}
	if len(b.scripts) > 0 && !anyMatch(changed, shellVerdictInputsRE) {
		return fmt.Sprintf("%s: the landing changed no shell-tier input (%s)", strings.Join(b.scripts, ", "), pathsBrief(changed))
	}
	return ""
}

// unattributableFromDiff is unattributable over the landing of rec. A red run
// that blamed no unit keeps the revert it always had (nothing names what the
// landing would have to have moved), and a diff this cannot read is no
// attribution.
func (r *RedMain) unattributableFromDiff(ctx context.Context, rec land.LandingRecord, b blame) string {
	if len(b.packages) == 0 && len(b.scripts) == 0 {
		return ""
	}
	if r.Diff == nil {
		return "the landing's changed paths are unreadable"
	}
	changed, err := r.Diff(ctx, rec)
	if err != nil {
		r.logf("reading what %s changed (%s..%s): %v", rec.BeadID, short(rec.Base), short(rec.LandedCommit), err)
		return "the landing's changed paths are unreadable"
	}
	return unattributable(changed, b)
}

func anyMatch(paths []string, re *regexp.Regexp) bool {
	for _, p := range paths {
		if re.MatchString(strings.TrimSpace(p)) {
			return true
		}
	}
	return false
}

// pathsBrief is up to namesBrief changed paths, for a log line.
func pathsBrief(paths []string) string {
	if len(paths) == 0 {
		return "nothing"
	}
	const namesBrief = 5
	if len(paths) > namesBrief {
		return fmt.Sprintf("%s, ... (%d more)", strings.Join(paths[:namesBrief], ", "), len(paths)-namesBrief)
	}
	return strings.Join(paths, ", ")
}

// openRevert is the open revert bead for culprit on this rig, or "".
func (r *RedMain) openRevert(culprit string) string {
	issues, err := r.Beads.List(beads.ListOptions{Label: LabelRevert, Priority: -1, Limit: 0})
	if err != nil {
		r.logf("listing %s beads: %v", LabelRevert, err)
		return ""
	}
	title := RevertTitle(r.Rig, culprit)
	for _, is := range issues {
		if is.Title == title && !beads.IssueStatus(strings.TrimSpace(is.Status)).IsTerminal() {
			return is.ID
		}
	}
	return ""
}

// fileRevert creates the revert's work bead and hands it to the landing
// worker, which lands it through Land like any other work. The ready label
// goes on last, so the worker never sees the bead without its request.
func (r *RedMain) fileRevert(rec land.LandingRecord, branch, head string) (string, error) {
	is, err := r.Beads.Create(beads.CreateOptions{
		Title:    RevertTitle(r.Rig, rec.BeadID),
		Labels:   []string{LabelRevert},
		Priority: 0,
		Description: fmt.Sprintf("The daemon's post-landing run found main red at %s, and %s's landing was the only change since the last green commit %s, so it is reverted (gt-v4ssj.4.1). "+
			"This bead lands the revert through the landing worker; when it lands, %s is reopened with the %s label.",
			rec.LandedCommit, rec.BeadID, rec.Base, rec.BeadID, land.LabelRework),
	})
	if err != nil {
		return "", err
	}
	work := land.Work{Branch: branch, Head: head, Target: rec.Target, Submitted: time.Now().UTC()}
	if err := r.Beads.AppendNotes(is.ID, land.FormatReadyNote(work)+"\n\n"+FormatRevertNote(rec)); err != nil {
		return is.ID, err
	}
	if err := r.Beads.Update(is.ID, beads.UpdateOptions{AddLabels: []string{land.LabelReadyToLand}}); err != nil {
		return is.ID, err
	}
	return is.ID, nil
}

// revertOf reads the bead a landing worker just landed or rejected and
// returns what it reverts, when it is one of this owner's reverts.
func (r *RedMain) revertOf(beadID string) (culprit, landed string, ok bool) {
	is, err := r.Beads.Show(beadID)
	if err != nil || is == nil || !beads.HasLabel(is, LabelRevert) {
		return "", "", false
	}
	culprit, landed, ok = ParseRevertNote(is.Notes)
	if ok && is.Title != RevertTitle(r.Rig, culprit) {
		return "", "", false
	}
	return culprit, landed, ok
}

// RevertLanded finishes a revert once the landing worker has landed it: the
// culprit is commented on and reopened with the rework label. Any other
// landing is ignored.
func (r *RedMain) RevertLanded(_ context.Context, work land.Work, res land.Result) {
	culprit, landed, ok := r.revertOf(work.BeadID)
	if !ok {
		return
	}
	msg := fmt.Sprintf("Reverted by %s (%s): main went red at %s and this landing was the only change since the last green commit. "+
		"Reopened for rework. The reverted commits are still in main's history, so a plain re-merge of the old branch brings nothing back: "+
		"start from main, re-apply the change (git revert %s), fix it, and submit again.",
		work.BeadID, res.LandedCommit, landed, res.LandedCommit)
	if err := r.Beads.AddComment(culprit, msg); err != nil {
		r.logf("commenting on %s: %v", culprit, err)
	}
	open, unassigned := string(beads.StatusOpen), ""
	if err := r.Beads.Update(culprit, beads.UpdateOptions{
		Status:    &open,
		Assignee:  &unassigned,
		AddLabels: []string{land.LabelRework},
		Force:     true,
	}); err != nil {
		r.logf("reopening %s for rework: %v", culprit, err)
	}
	r.status(fmt.Sprintf("main reverted %s's landing %s as %s; %s reopened for rework", culprit, short(landed), short(res.LandedCommit), culprit))
}

// RevertRejected handles a rework rejection of a revert: no polecat reworks
// a revert, so the bead is closed and the red-main beads stand. It reports
// whether work was a revert.
func (r *RedMain) RevertRejected(work land.Work, rej *land.Rejection) bool {
	culprit, _, ok := r.revertOf(work.BeadID)
	if !ok {
		return false
	}
	reason := fmt.Sprintf("revert of %s did not land (%s): %s; the red-main beads stand", culprit, rej.Kind, land.NoteField(rej.Reason))
	if err := r.Beads.Update(work.BeadID, beads.UpdateOptions{RemoveLabels: []string{land.LabelRework}}); err != nil {
		r.logf("removing %s from %s: %v", land.LabelRework, work.BeadID, err)
	}
	if err := r.Beads.CloseWithReason(reason, work.BeadID); err != nil {
		r.logf("closing %s: %v", work.BeadID, err)
	}
	r.status("main still red: " + reason)
	return true
}

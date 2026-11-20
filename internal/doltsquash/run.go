package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrRunMarkerHeld is what Marker.Hold returns while another run holds the
// marker. Run refuses such a run before it sends a statement.
var ErrRunMarkerHeld = errors.New("another run holds the run marker")

// Marker is the run marker: one run holds it for its whole length, so a second
// run that finds it held is refused instead of writing beside the first.
//
// Hold tries once and does not wait, so a run that finds the marker held is
// refused rather than queued. Run releases the marker on every way out, so a
// failed run does not keep the next check from running.
type Marker interface {
	// Hold takes the marker, or returns ErrRunMarkerHeld when another run holds
	// it.
	Hold() error
	// Release gives the marker back.
	Release() error
}

// RunInput is what one run reads: the Dolt seam, the run marker, the clock and
// sleeper the steps wait on, and the cut a due check chose.
type RunInput struct {
	Exec    Exec
	Marker  Marker
	Clock   Clock
	Sleeper Sleeper
	// Cut is the instant a commit must predate to be squashed, from Decide.
	Cut time.Time
}

// RecoverInput is what RecoverOnly reads: the Dolt seam and the cut the check
// chose.
type RecoverInput struct {
	Exec Exec
	// Cut is the instant a commit must predate to be squashed, as for Run.
	Cut time.Time
}

// RecoverOnly recovers the branches a dead or failed run left, without starting
// a squash: it calls Recover with the cut and returns its Recovery (nil when the
// database holds nothing to recover) or its error. Run recovers only at its own
// start, and the 24-hour gate keeps a failed run's database from reaching Run, so
// a caller that must recover without running uses this (gt-an57lb). It does not
// hold the run marker: that is gt-2x4rx5, and nothing calls this function before
// then.
func RecoverOnly(ctx context.Context, in RecoverInput) (*Recovery, error) {
	return Recover(ctx, in.Exec, in.Cut)
}

// RunSummary is what one run did, for the ledger line the design records per
// run.
type RunSummary struct {
	// CommitsBefore is main's commit count in the BEFORE record.
	CommitsBefore int64
	// CommitsAfter is main's commit count read after the swap. AfterRead says
	// whether it was read: a run that stopped before the swap leaves it zero,
	// and that zero is a missing reading, not a count.
	CommitsAfter int64
	// AfterRead reports that CommitsAfter was read.
	AfterRead bool
	// Rebase, Catchup and Swap are how long the three steps took on the run's
	// clock.
	Rebase, Catchup, Swap time.Duration
	// Tail is how many commits the old main held after the verified tip.
	Tail int
	// Retries is how many waits the catch-up and the swap spent on picks a
	// writer's uncommitted working set blocked.
	Retries int
}

// RunResult is what each step of one run did, one field per step in the order
// the steps run. "Design step N", here and in the package's other comments, is
// step N of "The run" in docs/plans/2026-11-15-dolt-squash-design.md: read that
// list before changing a step's order or its statements.
type RunResult struct {
	// Before is design step 3's record of main, read before the rebase.
	Before Before
	// Rebase is what design step 4 did.
	Rebase RebaseResult
	// Catchup is what design step 5 did.
	Catchup CatchupResult
	// Verified is design step 3's record read again at the last picked tip: the
	// state of main that design step 6 compares compact-work with. It is zero
	// when the run stopped before that record.
	Verified Before
	// Verify is what design step 6 found.
	Verify VerifyResult
	// Swap is what design steps 7 and 8 did.
	Swap SwapResult
	// Recovered is the dead run this start found and recovered before it ran a
	// step of its own, nil when the database held no leftover branch.
	Recovered *Recovery
	// Held reports that the run sent no statement of its own: the branches say
	// a swap stands with its old history kept for the retention check, so
	// Summary is zero and no step ran. It is not an error, because nothing is
	// at risk and a step would only repeat a swap the retention check holds.
	Held bool
	// Summary counts what the run did.
	Summary RunSummary
}

// RecoveryState names the row of the design's recovery table a run found its
// database in.
type RecoveryState string

const (
	// RecoveryBeforeSwap is a run that died in steps 3 to 6: main was never
	// renamed, and the dead run's work branches are left beside it.
	RecoveryBeforeSwap RecoveryState = "before the swap"
	// RecoveryFirstRename is a run that died between the swap's two renames:
	// main-pre-squash holds the old main and no branch is named main.
	RecoveryFirstRename RecoveryState = "after the first rename, before the second"
	// RecoverySwapped is a run that died after the swap with the old main
	// holding commits the new main does not and nothing on the new main to
	// lose: the two branches are renamed back, so main is the old main again
	// and the run redoes the work.
	RecoverySwapped RecoveryState = "after the swap, the new main lacks commits the old main holds"
	// RecoverySwappedKept is the swap left standing: the new main holds a
	// commit the old main does not, so renaming back would move it off main, or
	// it holds every commit the old main has, so the swap is finished and the
	// old history stays as main-pre-squash for the retention check. A run that
	// completed its swap leaves the same catalog, so this is also the row the
	// next start finds after a finished run.
	RecoverySwappedKept RecoveryState = "after the swap, the new main stands"
)

// Recovery is a dead run a start found, and what the recovery did about it.
type Recovery struct {
	// State is the row of the design's recovery table the database was in.
	State RecoveryState
	// Dropped names the branches the recovery dropped, in the order it sent
	// the statements.
	Dropped []string
	// Kept names the branches the recovery left beside main: either one a
	// delete would have destroyed because it holds a commit main does not, or,
	// after the swap, the branches the retention check owns. A run must not
	// start beside a kept branch.
	Kept []string
	// Landed names the commits newer than the cut that are why a branch was
	// kept: on the swap's branches, a commit that landed on the new main and
	// main-pre-squash does not hold; on a work branch, a commit main does not
	// hold at all. Each is the author date and message a rebase preserves, so
	// an item raised from it names a commit across the rename.
	Landed []string
	// Unreplayed names the commits newer than the cut that main-pre-squash
	// holds and the new main does not, when State is RecoverySwappedKept: the
	// swap's reconcile had not replayed them, and they stay reachable on
	// main-pre-squash.
	Unreplayed []string
	// Reason is the log line.
	Reason string
}

// Complete reports that the recovery found a finished swap: the new main holds
// every commit the old main has, both branches stay for the retention check,
// and the run has no step to take. It is not a failure, because no commit is at
// risk and a step could only repeat a swap the retention check still holds.
func (r *Recovery) Complete() bool {
	return r != nil && r.State == RecoverySwappedKept && len(r.Unreplayed) == 0 && len(r.Kept) > 0
}

// branchesStatement lists the branches a recovery reads. The tips come with the
// names so a recovery that renamed main back can read the old tip it landed
// there before it drops anything.
const branchesStatement = "SELECT name, hash FROM dolt_branches WHERE name IN ('compact-base', 'compact-work', 'main', 'main-pre-squash') ORDER BY name"

// Recover applies the design's recovery table to a database a dead run left
// branches on, before a run sends a statement of its own. It returns nil when
// no branch is left.
//
// The state is told from the branch catalog alone, because a dead run leaves
// nothing else behind: with no main-pre-squash the swap never began, so main
// was never renamed and the work branches are dropped; with no main the swap
// stopped between its two renames, so main-pre-squash is renamed back; with
// both the swap finished, and the commits newer than cut decide the row.
//
// After the swap the two branches are compared by the author date and message a
// rebase preserves. The old main holding a commit the new main does not means
// the reconcile had not replayed it: with nothing on the new main the old main
// lacks, renaming the two back loses nothing and the run redoes the swap; with
// commits on both sides, renaming back would move a writer's commit off main,
// so both branches are kept and named. The new main holding every commit the
// old main does means the swap stands, and that row is kept rather than undone:
// the same catalog is what a run that completed its swap leaves for the
// retention check, which branches alone cannot tell from a death after the
// second rename, and undoing a finished squash is a loss the design's fourth row
// does not ask for.
//
// Only commits newer than cut are compared: the rebase's squashed commit is at
// or before the cut and may carry a message the rewrite joined, so it is not an
// identity the two branches are expected to share. A commit that landed with an
// author date older than the cut is invisible to this comparison, which is the
// one hole the design's row leaves.
//
// No branch is dropped on an unproven state. Before a work branch is deleted,
// the recovery reads main's commits newer than cut and the branch's and keeps
// the branch when it holds one main does not, because a commit only it holds is
// history a delete would destroy. A recovery that renamed main back reads the
// catalog first and requires main to hold the old main's tip, and a recovery
// that moved or dropped a branch reads the catalog again afterwards and
// requires main plus exactly the branches it kept. A state no row of the table
// names stops with an error and touches nothing.
func Recover(ctx context.Context, ex Exec, cut time.Time) (*Recovery, error) {
	if ex == nil {
		return nil, errors.New("doltsquash: the recovery has no Exec, so it cannot read the branches")
	}
	if cut.IsZero() {
		return nil, errors.New("doltsquash: the recovery has no cut, so it cannot tell a commit that landed on the new main from the squashed history")
	}
	s := &designStep{ex: ex}
	tips, err := s.branchTips(ctx)
	if err != nil {
		return nil, fmt.Errorf("doltsquash: reading the branches a dead run may have left: %w", err)
	}
	_, hasMain := tips["main"]
	preTip, hasPre := tips["main-pre-squash"]
	_, hasWork := tips["compact-work"]
	_, hasBase := tips["compact-base"]
	switch {
	case !hasPre && !hasWork && !hasBase:
		if !hasMain {
			return nil, fmt.Errorf("doltsquash: the database lists no branch at all, which no row of the recovery table names: the run will not touch it")
		}
		return nil, nil
	case !hasPre:
		if !hasMain {
			return nil, fmt.Errorf("doltsquash: the database holds %s but no main, which no row of the recovery table names: the run will not touch it", branchNames(tips))
		}
		return s.recoverBeforeSwap(ctx, cut, hasWork, hasBase)
	case !hasMain:
		if !hasWork {
			return nil, fmt.Errorf("doltsquash: the database holds %s, with the old main at main-pre-squash and nothing else a swap leaves, which no row of the recovery table names: nothing was touched", branchNames(tips))
		}
		return s.recoverFirstRename(ctx, cut, preTip, hasBase)
	default:
		if hasWork {
			return nil, fmt.Errorf("doltsquash: main, main-pre-squash and compact-work all exist (%s), which no row of the recovery table names: nothing was touched", branchNames(tips))
		}
		return s.recoverSwapped(ctx, cut, preTip, hasBase)
	}
}

// recoverBeforeSwap drops the work branches of a run that died before the swap.
// main was never renamed, so it is the original history and the branches beside
// it are the only leftovers, and each is dropped only after main is proved to
// hold every commit it has after the cut.
func (s *designStep) recoverBeforeSwap(ctx context.Context, cut time.Time, hasWork, hasBase bool) (*Recovery, error) {
	if _, err := s.exec(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return nil, err
	}
	rec := &Recovery{State: RecoveryBeforeSwap}
	if err := s.dropWorkBranches(ctx, cut, hasWork, hasBase, rec); err != nil {
		return nil, err
	}
	rec.Reason = fmt.Sprintf("recovered a run that died before the swap: main was never renamed, %d work branches were dropped%s",
		len(rec.Dropped), keptClause(rec))
	if err := s.proveRecovery(ctx, "", rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// recoverFirstRename puts the old main back after a run died between the swap's
// two renames, then drops each work branch main is proved to hold every commit
// of.
func (s *designStep) recoverFirstRename(ctx context.Context, cut time.Time, preTip string, hasBase bool) (*Recovery, error) {
	if _, err := s.exec(ctx, renameStatement("main-pre-squash", "main")); err != nil {
		return nil, err
	}
	if _, err := s.exec(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return nil, err
	}
	tips, err := s.branchTips(ctx)
	if err != nil {
		return nil, fmt.Errorf("doltsquash: main-pre-squash was renamed to main but the tip it moved could not be read back, so no branch was dropped: %w", err)
	}
	if tips["main"] != preTip {
		return nil, fmt.Errorf("doltsquash: main holds %q after the rename back, want the old main's tip %s, so no branch was dropped", tips["main"], preTip)
	}
	rec := &Recovery{State: RecoveryFirstRename}
	if err := s.dropWorkBranches(ctx, cut, true, hasBase, rec); err != nil {
		return nil, err
	}
	rec.Reason = fmt.Sprintf("recovered a run that died between the swap's two renames: main-pre-squash is main again at %s, %d work branches were dropped%s",
		preTip, len(rec.Dropped), keptClause(rec))
	if err := s.proveRecovery(ctx, preTip, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// recoverSwapped settles a database a run left after the swap. The new main
// holding every commit the old main has after the cut means the swap stands, so
// both branches stay and the run has nothing to do; the old main holding one the
// new main does not means the reconcile had not replayed it, and the two
// branches are renamed back when nothing on the new main would be lost by it.
func (s *designStep) recoverSwapped(ctx context.Context, cut time.Time, preTip string, hasBase bool) (*Recovery, error) {
	mainNewer, err := s.newerIdentities(ctx, "main", cut)
	if err != nil {
		return nil, err
	}
	oldNewer, err := s.newerIdentities(ctx, "main-pre-squash", cut)
	if err != nil {
		return nil, err
	}
	landed := commitsOnlyOnMain(oldNewer, mainNewer)
	unreplayed := commitsOnlyOnMain(mainNewer, oldNewer)
	if len(unreplayed) == 0 || len(landed) > 0 {
		return keptSwap(landed, unreplayed, hasBase), nil
	}
	// The comparison and the renames are separate statements, so a commit can
	// land between them. Renaming main away keeps it where it is, and the
	// commits the renamed branch holds are read back: one that the old main did
	// not hold before the rename is a writer's, and the rename is undone so it
	// stays on main.
	if _, err := s.exec(ctx, renameStatement("main", "compact-work")); err != nil {
		return nil, err
	}
	movedNewer, err := s.newerIdentities(ctx, "compact-work", cut)
	if err != nil {
		return nil, fmt.Errorf("doltsquash: main was renamed to compact-work but the commits it holds could not be read back, so no branch was dropped and the next check finds the work branches beside main: %w", err)
	}
	if late := commitsOnlyOnMain(oldNewer, movedNewer); len(late) > 0 {
		if _, err := s.exec(ctx, renameStatement("compact-work", "main")); err != nil {
			return nil, fmt.Errorf("doltsquash: a commit reached the new main while the two branches were being renamed back, and renaming it to main failed, so the next check finds the work branches beside main: %w", err)
		}
		return keptSwap(late, unreplayed, hasBase), nil
	}
	if _, err := s.exec(ctx, renameStatement("main-pre-squash", "main")); err != nil {
		return nil, err
	}
	if _, err := s.exec(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return nil, err
	}
	// Nothing is dropped until main is back at the old main's tip: a drop
	// before this check could delete the only copy of a history main does not
	// hold.
	tips, err := s.branchTips(ctx)
	if err != nil {
		return nil, fmt.Errorf("doltsquash: the two branches were renamed back but main's tip could not be read, so no branch was dropped: %w", err)
	}
	if tips["main"] != preTip {
		return nil, fmt.Errorf("doltsquash: main holds %q after the rename back, want the old main's tip %s, so no branch was dropped", tips["main"], preTip)
	}
	rec := &Recovery{State: RecoverySwapped, Unreplayed: unreplayed}
	if err := s.dropWorkBranches(ctx, cut, true, hasBase, rec); err != nil {
		return nil, err
	}
	rec.Reason = fmt.Sprintf("recovered a run that died after the swap with the new main lacking %d of the old main's commits: the two branches are renamed back, main holds %s again, %d work branches were dropped%s",
		len(unreplayed), preTip, len(rec.Dropped), keptClause(rec))
	if err := s.proveRecovery(ctx, preTip, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// keptSwap is the result of a recovery that found the new main cannot simply be
// renamed away: either it holds every commit the old main has, so the swap
// stands, or it holds a commit the old main does not, so renaming back would
// move that commit off main. Both branches stay, and the reason names what is
// where.
func keptSwap(landed, unreplayed []string, hasBase bool) *Recovery {
	kept := []string{"main-pre-squash"}
	if hasBase {
		kept = append(kept, "compact-base")
	}
	slices.Sort(kept)
	rec := &Recovery{State: RecoverySwappedKept, Kept: kept, Landed: landed, Unreplayed: unreplayed}
	landedClause := "no commit landed on the new main since the swap"
	if len(landed) > 0 {
		landedClause = fmt.Sprintf("commits landed on the new main since the swap: %d (%s)", len(landed), strings.Join(landed, ", "))
	}
	if len(unreplayed) == 0 {
		rec.Reason = fmt.Sprintf("kept the new main: it holds every commit main-pre-squash has after the cut, so the swap stands and main-pre-squash stays for the retention check; %s",
			landedClause)
		return rec
	}
	rec.Reason = fmt.Sprintf("kept both branches: %s; main-pre-squash holds commits the new main does not: %d (%s)",
		landedClause, len(unreplayed), strings.Join(unreplayed, ", "))
	return rec
}

// dropWorkBranches drops the work branches a dead run left, each only after
// proving main holds every commit newer than the cut that the branch holds. A
// branch that holds one main does not is left beside main and named: the design
// never deletes a dead run's branches blindly, and a commit only it holds is
// history a delete would destroy.
func (s *designStep) dropWorkBranches(ctx context.Context, cut time.Time, hasWork, hasBase bool, rec *Recovery) error {
	names := make([]string, 0, 2)
	if hasWork {
		names = append(names, "compact-work")
	}
	if hasBase {
		names = append(names, "compact-base")
	}
	if len(names) == 0 {
		return nil
	}
	mainNewer, err := s.newerIdentities(ctx, "main", cut)
	if err != nil {
		return fmt.Errorf("doltsquash: reading main's commits after the cut, so a work branch it does not hold is never dropped: %w", err)
	}
	for _, name := range names {
		branchNewer, err := s.newerIdentities(ctx, name, cut)
		if err != nil {
			return fmt.Errorf("doltsquash: reading %s's commits after the cut, so it is never dropped until main is proved to hold them: %w", name, err)
		}
		if only := commitsOnlyOnMain(mainNewer, branchNewer); len(only) > 0 {
			rec.Kept = append(rec.Kept, name)
			rec.Landed = append(rec.Landed, only...)
			continue
		}
		if err := s.dropBranch(ctx, name, rec); err != nil {
			return err
		}
	}
	return nil
}

// keptClause renders the suffix a recovery's reason carries when it left a work
// branch beside main, naming the branch and the commits that are why; empty when
// nothing was kept.
func keptClause(rec *Recovery) string {
	if len(rec.Kept) == 0 {
		return ""
	}
	return fmt.Sprintf("; kept %s, which holds %s, newer than the cut and not on main",
		strings.Join(rec.Kept, " and "), strings.Join(rec.Landed, ", "))
}

// branchTips reads the branch catalog and returns the tip of each branch the
// recovery reads.
func (s *designStep) branchTips(ctx context.Context) (map[string]string, error) {
	rows, err := s.exec(ctx, branchesStatement)
	if err != nil {
		return nil, err
	}
	tips := make(map[string]string, len(rows.Data))
	for i, record := range rows.Data {
		if len(record) != 2 {
			return nil, fmt.Errorf("doltsquash: statement %d (%s) returned record %d of %d values, want a name and a tip",
				s.n, shortStatement(branchesStatement), i, len(record))
		}
		if err := checkCommitHash("branch "+record[0]+"'s tip", record[1]); err != nil {
			return nil, err
		}
		tips[record[0]] = record[1]
	}
	return tips, nil
}

// newerIdentities reads the commits on branch newer than cut, by the author
// date a rebase keeps.
func (s *designStep) newerIdentities(ctx context.Context, branch string, cut time.Time) ([]commitIdentity, error) {
	return s.identities(ctx, fmt.Sprintf(
		"SELECT author_date, message FROM dolt_log(%s) WHERE author_date > %s ORDER BY author_date",
		quoteLiteral(branch), quoteLiteral(formatCut(cut))))
}

// commitsOnlyOnMain returns the commits current holds that old does not, by the
// author date and message a rebase preserves. A commit only the new main holds
// landed there from a writer after the swap; one only a work branch holds is
// history a delete would destroy.
func commitsOnlyOnMain(old, current []commitIdentity) []string {
	var landed []string
	for _, difference := range compareNewer(old, current) {
		if difference.Kind == KindExtraCommit {
			landed = append(landed, difference.Commit)
		}
	}
	return landed
}

// dropBranch deletes one of a dead run's work branches and records it. The
// caller read the branch's existence first, and a recovery that renamed main
// back proved the old main's tip is there, so the delete is never sent for a
// branch that is not there or while the original history is not the one main
// holds.
func (s *designStep) dropBranch(ctx context.Context, name string, rec *Recovery) error {
	if _, err := s.exec(ctx, "CALL DOLT_BRANCH('-D', "+quoteLiteral(name)+")"); err != nil {
		return err
	}
	rec.Dropped = append(rec.Dropped, name)
	return nil
}

// proveRecovery reads the branches again and refuses a recovery that left a
// branch it did not keep, or moved main. wantMain, when set, is the tip main
// must hold: the old main's tip a rename restored. It is empty for a recovery
// that never renamed main, where main keeps moving with the town's writes and
// only its presence is the proof.
func (s *designStep) proveRecovery(ctx context.Context, wantMain string, rec *Recovery) error {
	tips, err := s.branchTips(ctx)
	if err != nil {
		return fmt.Errorf("doltsquash: the recovery ran but the branches could not be read back, so a branch it meant to drop is not ruled out: %w", err)
	}
	mainTip, hasMain := tips["main"]
	if !hasMain {
		return fmt.Errorf("doltsquash: the recovery left %s, want main and the branches it kept (%s)", branchNames(tips), strings.Join(rec.Kept, ", "))
	}
	if wantMain != "" && mainTip != wantMain {
		return fmt.Errorf("doltsquash: the recovery left main at %s, want the old main's tip %s", mainTip, wantMain)
	}
	kept := make(map[string]bool, len(rec.Kept))
	for _, name := range rec.Kept {
		kept[name] = true
		if _, ok := tips[name]; !ok {
			return fmt.Errorf("doltsquash: the recovery reports keeping %s, which the catalog no longer holds, so the commits it was kept for are not accounted for", name)
		}
	}
	for name := range tips {
		if name != "main" && !kept[name] {
			return fmt.Errorf("doltsquash: the recovery left %s, want main and the branches it kept (%s)", branchNames(tips), strings.Join(rec.Kept, ", "))
		}
	}
	return nil
}

// branchNames renders the branches a recovery read as one line, in name order.
func branchNames(tips map[string]string) string {
	if len(tips) == 0 {
		return "no branch"
	}
	names := make([]string, 0, len(tips))
	for name := range tips {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// Run runs the squash over one database in the design's order: the BEFORE
// record, the rebase, the catch-up, verification, and the swap with its
// reconcile.
//
// The run holds the marker for its whole length and releases it on every way
// out, so a second run is refused before it sends a statement and a failed run
// does not keep the next check from running. The marker is held before the
// recovery for the reason the marker exists: the branches a recovery drops
// belong to a run that no longer holds it, and a live run's branches must never
// be read as a dead run's.
//
// Before its first step the run recovers the branches a dead run left, by the
// state the design's recovery table names. A recovery that keeps a branch
// beside main stops the run before a statement of its own, because a kept
// branch is either a commit main does not hold or a swap the retention check
// owns. A kept catalog whose new main holds every commit of the old main is a
// finished swap, and the run reports it as held rather than failed: there is no
// commit at risk and no step it could take until the retention check drops the
// branches. Every other kept catalog stops the run with an error so the caller
// raises the item. A recovery that drops what it proved safe leaves main as the
// old main and the run proceeds; the result carries what it recovered for the
// item the caller raises.
//
// Verification compares compact-work with main at the tip the catch-up stopped
// at, so main's state there is recorded after the catch-up: the BEFORE record
// describes the tip the rebase branched at, and the commits that landed since
// then are on compact-work. A record that reads main at another tip stops the
// run before verification, because the swap replays from the tip verification
// proved. A run that stops before the swap leaves main and the work branches as
// the stopped step left them.
//
// A step that fails, or a verification difference, stops the run and returns
// the result the steps reached. A swap that leaves a conflict is not an error:
// SwapResult.Conflict names the commits, and the caller raises the item. The
// summary reports the steps the run reached, and CommitsAfter only when the
// swap ran, so its zero is never read as a count.
func Run(ctx context.Context, in RunInput) (result RunResult, err error) {
	switch {
	case in.Exec == nil:
		return result, errors.New("doltsquash: the run has no Exec, so no step could read or write Dolt")
	case in.Marker == nil:
		return result, errors.New("doltsquash: the run has no marker, so a second run could start beside it")
	case in.Clock == nil:
		return result, errors.New("doltsquash: the run has no clock, so the catch-up cannot tell a quiet tip")
	case in.Sleeper == nil:
		return result, errors.New("doltsquash: the run has no sleeper, so the catch-up cannot wait out a quiet tip or a held working set")
	case in.Cut.IsZero():
		return result, errors.New("doltsquash: the run has no cut, so the rebase would mark no commit for squashing")
	}
	if err = ctx.Err(); err != nil {
		return result, fmt.Errorf("doltsquash: the run did not start: %w", err)
	}
	if err = in.Marker.Hold(); err != nil {
		return result, fmt.Errorf("doltsquash: the run marker: %w", err)
	}
	defer func() {
		if releaseErr := in.Marker.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("doltsquash: releasing the run marker: %w", releaseErr))
		}
	}()
	if result.Recovered, err = Recover(ctx, in.Exec, in.Cut); err != nil {
		return result, err
	}
	if result.Recovered != nil && len(result.Recovered.Kept) > 0 {
		if result.Recovered.Complete() {
			result.Held = true
			return result, nil
		}
		return result, fmt.Errorf("doltsquash: the run did not start: %s", result.Recovered.Reason)
	}
	if result.Before, _, err = timed(in.Clock, func() (Before, error) {
		return RecordBefore(ctx, in.Exec)
	}); err != nil {
		return result, err
	}
	result.Summary.CommitsBefore = result.Before.Commits
	if result.Rebase, result.Summary.Rebase, err = timed(in.Clock, func() (RebaseResult, error) {
		return RebaseHistory(ctx, in.Exec, result.Before, in.Cut)
	}); err != nil {
		return result, err
	}
	if result.Catchup, result.Summary.Catchup, err = timed(in.Clock, func() (CatchupResult, error) {
		return Catchup(ctx, in.Exec, result.Before.Tip, in.Clock, in.Sleeper)
	}); err != nil {
		return result, err
	}
	if result.Verified, err = RecordBefore(ctx, in.Exec); err != nil {
		return result, err
	}
	// Verification proves compact-work equal to main at the recorded tip, and the
	// swap replays the old main's tail from the tip it verified against. A record
	// that read a tip the catch-up never stopped at would leave both steps reading
	// different states of main, so the run refuses instead of proving one state and
	// swapping from another.
	if result.Verified.Tip != result.Catchup.Tip {
		return result, fmt.Errorf(
			"doltsquash: the record at the last picked tip read main at %s, but the catch-up stopped at %s, so the run cannot verify and swap at the tip the picks reached",
			result.Verified.Tip, result.Catchup.Tip)
	}
	if result.Verify, err = Verify(ctx, in.Exec, result.Verified, in.Cut); err != nil {
		return result, err
	}
	if !result.Verify.OK() {
		return result, fmt.Errorf("doltsquash: compact-work does not verify against main at %s, so main is untouched: %s",
			result.Verified.Tip, describeDifferences(result.Verify.Differences))
	}
	result.Swap, result.Summary.Swap, err = timed(in.Clock, func() (SwapResult, error) {
		return Swap(ctx, in.Exec, result.Verified.Tip, in.Sleeper)
	})
	result.Summary.Tail = result.Swap.Tail
	result.Summary.Retries = result.Catchup.Retries + result.Swap.Retries
	if err != nil {
		return result, err
	}
	if result.Summary.CommitsAfter, err = mainCommits(ctx, in.Exec); err != nil {
		return result, err
	}
	result.Summary.AfterRead = true
	return result, nil
}

// timed runs one step and returns its result with how long it took on the run's
// clock, so the summary reports the three durations the design's ledger line
// records.
func timed[T any](clk Clock, step func() (T, error)) (T, time.Duration, error) {
	start := clk.Now()
	value, err := step()
	return value, clk.Now().Sub(start), err
}

// mainCommits reads main's commit count for the summary's after figure. It is
// read after the swap, when main is the branch the run leaves in place.
func mainCommits(ctx context.Context, ex Exec) (int64, error) {
	return (&designStep{ex: ex}).number(ctx, "SELECT COUNT(*) FROM dolt_log('main')")
}

// describeDifferences renders every difference verification found as one line.
func describeDifferences(differences []Difference) string {
	lines := make([]string, len(differences))
	for i, difference := range differences {
		lines[i] = difference.String()
	}
	return strings.Join(lines, "; ")
}

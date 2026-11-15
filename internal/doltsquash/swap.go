package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SwapConflict is the pick that left conflicts while the reconcile replayed
// the old main's tail onto the new main.
type SwapConflict struct {
	// Commit is the tail commit whose pick left conflicts.
	Commit string
	// Unpicked holds the tail commits that are not on the new main, oldest
	// first: Commit and every commit after it. A run that keeps the new main
	// names them, because main-pre-squash is the branch that holds them.
	Unpicked []string
	// Landed holds the commits that reached the new main from a writer since
	// the swap, oldest first. A non-empty Landed is why the branches were kept:
	// renaming back would move those commits off main onto compact-work.
	Landed []string
}

// String renders the conflict as the line the item raised from it carries.
func (c SwapConflict) String() string {
	landed := "no commit landed on the new main since the swap"
	if len(c.Landed) > 0 {
		landed = fmt.Sprintf("%d commits landed on the new main since the swap (%s)",
			len(c.Landed), strings.Join(c.Landed, ", "))
	}
	return fmt.Sprintf("the pick of %s left conflicts, %d tail commits are not on the new main (%s), and %s",
		c.Commit, len(c.Unpicked), strings.Join(c.Unpicked, ", "), landed)
}

// SwapResult is what design steps 7 and 8 did.
type SwapResult struct {
	// SwappedTip is the tip the new main held at the swap: compact-work's tip,
	// read before either rename.
	SwappedTip string
	// OldTip is the tip main-pre-squash holds, read after the swap: the old
	// main's last tip.
	OldTip string
	// Tail is how many commits the old main held after the verified tip.
	Tail int
	// Replayed is how many of the tail's picks the replay attempted, the one a
	// conflict stopped included.
	Replayed int
	// Retries is how many waits the replay spent on picks that reported
	// uncommitted changes.
	Retries int
	// Conflict is the pick that left conflicts, nil when every pick applied.
	Conflict *SwapConflict
	// RenamedBack reports that a conflict renamed the two branches back, so
	// main is the old main again and the squashed branch is compact-work.
	RenamedBack bool
	// Reason is the run's log line for steps 7 and 8.
	Reason string
}

// Swap runs design steps 7 and 8: it renames main to main-pre-squash and
// compact-work to main, both forced, then replays the commits the old main
// gained after verifiedTip onto the new main one pick at a time, retrying a
// pick a writer's uncommitted working set blocks as the catch-up does.
//
// The session must be on compact-work, the branch that becomes main, and
// verifiedTip is the old main's commit that branch was verified equal to: the
// tip the catch-up stopped at.
//
// A pick that leaves conflicts ends the replay. With no commit landed on the
// new main since the swap, the two branches are renamed back and RenamedBack
// reports it. With one landed, both branches stay as the swap left them and
// Conflict names the commits, because renaming back would move that commit off
// main onto compact-work. A commit that lands between the check and the renames
// is caught by reading back the tip the renames moved: the renames are undone
// and both branches stay as the swap left them, with that commit named. Both
// outcomes return a nil error: the caller raises the item and the recovery
// table takes over from the state the result describes.
func Swap(ctx context.Context, ex Exec, verifiedTip string, sleep Sleeper) (SwapResult, error) {
	if err := checkCommitHash("the verified tip", verifiedTip); err != nil {
		return SwapResult{}, err
	}
	s := &designStep{ex: ex}
	// The check before the renames refuses a run whose picks would land on a
	// branch that does not become main: it fails with no state to recover. The
	// check after them proves the session followed its branch's rename, so the
	// reconcile's picks land on the new main and not on the only copy of the
	// old history.
	if err := s.onBranch(ctx, "compact-work", "a pick must land on the branch that becomes main"); err != nil {
		return SwapResult{}, err
	}
	swappedTip, err := s.scalar(ctx, "SELECT dolt_hashof('compact-work')")
	if err != nil {
		return SwapResult{}, err
	}
	if err := checkCommitHash("compact-work's tip", swappedTip); err != nil {
		return SwapResult{}, err
	}
	occupied, err := s.number(ctx, mainPreSquashCount)
	if err != nil {
		return SwapResult{}, err
	}
	if occupied != 0 {
		return SwapResult{}, fmt.Errorf(
			"doltsquash: statement %d (%s) counted %d branches named main-pre-squash, want none: the swap's forced rename would overwrite it",
			s.n, shortStatement(mainPreSquashCount), occupied)
	}
	for _, rename := range swapOrder {
		if _, err := s.exec(ctx, renameStatement(rename[0], rename[1])); err != nil {
			return SwapResult{}, err
		}
	}
	result := SwapResult{SwappedTip: swappedTip}
	if err := s.onBranch(ctx, "main", "a pick must land on the new main, and a pick on main-pre-squash would rewrite the old history"); err != nil {
		return result, err
	}
	oldTip, err := s.scalar(ctx, "SELECT dolt_hashof('main-pre-squash')")
	if err != nil {
		return result, err
	}
	if err := checkCommitHash("main-pre-squash's tip", oldTip); err != nil {
		return result, err
	}
	result.OldTip = oldTip
	if oldTip == verifiedTip {
		result.Reason = "swapped: no commit reached the old main after the verified tip"
		return result, nil
	}
	statement := tailStatement(verifiedTip, oldTip)
	commits, err := s.column(ctx, statement)
	if err != nil {
		return result, err
	}
	if len(commits) == 0 {
		return result, fmt.Errorf(
			"doltsquash: statement %d (%s) listed no commit though main-pre-squash's tip %s is not %s, the verified tip: a commit would be left unpicked",
			s.n, shortStatement(statement), oldTip, verifiedTip)
	}
	for _, commit := range commits {
		if err := checkCommitHash("a commit in the old main's tail", commit); err != nil {
			return result, err
		}
	}
	result.Tail = len(commits)
	// The replay's own commits are how the reconcile tells its picks from a
	// writer's commits on the new main, so each successful pick's hash is kept.
	ours := make(map[string]bool, len(commits))
	for i, commit := range commits {
		result.Replayed++
		outcome, err := s.pick(ctx, commit, sleep)
		if err != nil {
			var conflict *pickConflictError
			if !errors.As(err, &conflict) {
				return result, err
			}
			if err := s.resolveConflict(ctx, &result, commits[i:], ours); err != nil {
				return result, err
			}
			return result, nil
		}
		result.Retries += outcome.retries
		if outcome.commit != "" {
			ours[outcome.commit] = true
		}
	}
	result.Reason = fmt.Sprintf("swapped and replayed %d commits", result.Replayed)
	return result, nil
}

// onBranch refuses a session that is not on want, so a pick cannot land on a
// branch the step did not choose.
func (s *designStep) onBranch(ctx context.Context, want, why string) error {
	branch, err := s.scalar(ctx, "SELECT active_branch()")
	if err != nil {
		return err
	}
	if branch != want {
		return fmt.Errorf("doltsquash: statement %d (SELECT active_branch()) answered %q, want %s: %s",
			s.n, branch, want, why)
	}
	return nil
}

// resolveConflict answers a pick that left conflicts. It renames the two
// branches back only when no commit has reached the new main from a writer
// since the swap: the replay's own picks do not count, and renaming back keeps
// them as compact-work. A landed commit would move off main onto compact-work
// with the new history, so the branches stay and the caller raises the item.
// The check and the renames are separate statements, so a commit can land in
// between: the tip the renames moved is read back, and a tip that moved undoes
// the renames and names that commit, leaving both branches as the swap left
// them. A read it cannot trust stops the run: one before the renames leaves the
// branches as the swap left them for recovery by state, and one after them says
// the branches are renamed back, because a refusal is not a recovery.
func (s *designStep) resolveConflict(ctx context.Context, result *SwapResult, unpicked []string, ours map[string]bool) error {
	landed, checkedTip, err := s.landedSince(ctx, result.SwappedTip, ours)
	if err != nil {
		return err
	}
	result.Conflict = &SwapConflict{
		Commit:   unpicked[0],
		Unpicked: slices.Clone(unpicked),
		Landed:   landed,
	}
	if len(landed) > 0 {
		result.Reason = "kept both branches: " + result.Conflict.String()
		return nil
	}
	for _, rename := range renameBackOrder {
		if _, err := s.exec(ctx, renameStatement(rename[0], rename[1])); err != nil {
			return err
		}
	}
	// The renames carry whatever main held, so this tip is the one the check
	// read unless a commit landed in the window between them.
	movedTip, err := s.scalar(ctx, "SELECT dolt_hashof('compact-work')")
	if err != nil {
		return fmt.Errorf("doltsquash: the two branches are renamed back but the tip they moved could not be read, so a commit that landed between the check and the renames is not ruled out: %w", err)
	}
	if err := checkCommitHash("the tip the rename back moved", movedTip); err != nil {
		return fmt.Errorf("doltsquash: the two branches are renamed back but the tip they moved cannot be trusted, so a commit that landed between the check and the renames is not ruled out: %w", err)
	}
	if movedTip == checkedTip {
		result.RenamedBack = true
		result.Reason = "renamed the two back: " + result.Conflict.String()
		return nil
	}
	// A commit landed before the renames and now sits on compact-work. Rename
	// forth again so both branches sit as the swap left them, then name it.
	for _, rename := range swapOrder {
		if _, err := s.exec(ctx, renameStatement(rename[0], rename[1])); err != nil {
			return err
		}
	}
	statement := tailStatement(checkedTip, movedTip)
	commits, err := s.column(ctx, statement)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return fmt.Errorf(
			"doltsquash: statement %d (%s) listed no commit though compact-work's tip %s is not %s, the tip the check read: a landed commit would be missed",
			s.n, shortStatement(statement), movedTip, checkedTip)
	}
	landed = make([]string, 0, len(commits))
	for _, commit := range commits {
		if err := checkCommitHash("a commit that landed between the check and the renames", commit); err != nil {
			return err
		}
		landed = append(landed, commit)
	}
	result.Conflict.Landed = landed
	result.Reason = "kept both branches: " + result.Conflict.String()
	return nil
}

// landedSince returns the commits on the new main that the replay did not make,
// oldest first: what is left when the log from the tip the swap gave it to its
// tip now drops every hash the replay's picks returned. A tip equal to the
// swapped tip means nothing has landed. The tip it read is returned too, so the
// caller can tell the tip the rename back moved from one a commit moved. A log
// that lists nothing while the tip moved is refused rather than read as empty,
// because renaming back against a commit it could not name would move that
// commit off main unseen.
func (s *designStep) landedSince(ctx context.Context, swappedTip string, ours map[string]bool) ([]string, string, error) {
	tip, err := s.scalar(ctx, "SELECT dolt_hashof('main')")
	if err != nil {
		return nil, "", err
	}
	if err := checkCommitHash("the new main's tip", tip); err != nil {
		return nil, "", err
	}
	if tip == swappedTip {
		return nil, tip, nil
	}
	statement := tailStatement(swappedTip, tip)
	commits, err := s.column(ctx, statement)
	if err != nil {
		return nil, "", err
	}
	if len(commits) == 0 {
		return nil, "", fmt.Errorf(
			"doltsquash: statement %d (%s) listed no commit though the new main's tip %s is not %s, the tip the swap gave it: a landed commit would be missed",
			s.n, shortStatement(statement), tip, swappedTip)
	}
	var landed []string
	for _, commit := range commits {
		if err := checkCommitHash("a commit in the new main's log", commit); err != nil {
			return nil, "", err
		}
		if !ours[commit] {
			landed = append(landed, commit)
		}
	}
	return landed, tip, nil
}

// mainPreSquashCount counts the branches named main-pre-squash. The swap's
// first rename is forced, so it would overwrite a branch that already holds
// that name; the count must be zero before the renames.
const mainPreSquashCount = "SELECT COUNT(*) FROM dolt_branches WHERE name = 'main-pre-squash'"

// swapOrder is the swap's renames in order; renameBackOrder is their reverse,
// which a conflict sends when nothing landed on the new main.
var (
	swapOrder       = [][2]string{{"main", "main-pre-squash"}, {"compact-work", "main"}}
	renameBackOrder = [][2]string{{"main", "compact-work"}, {"main-pre-squash", "main"}}
)

// renameStatement renders one of the swap's branch renames. --force is what
// lets the rename through while another session is on the branch, which Dolt
// refuses without it, and it also lets the rename overwrite a branch that
// already holds the target name.
func renameStatement(from, to string) string {
	return fmt.Sprintf("CALL DOLT_BRANCH('-m', '--force', %s, %s)", quoteLiteral(from), quoteLiteral(to))
}

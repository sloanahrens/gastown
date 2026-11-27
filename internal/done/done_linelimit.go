package done

import (
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// noLimitLine is what the check prints when the bead declares no added-line
// limit, so the skipped check is visible rather than silent (gt-wxpd0y).
const noLimitLine = "no limit declared in the bead's Size or Constraints: the added-line check did not run"

// overLimitLine is what a refusal past twice the limit tells a finished seat to
// do (gt-wxpd0y, gt-8f4y1k): push the commit first so it outlives the seat
// (gt-574kps), report the measured added lines and the limit, and stop for the
// overseer to decide whether to raise the limit or split the bead.
const overLimitLine = "The measured added lines are over twice the limit the gate applies. Push the branch first with a plain git push origin HEAD:refs/heads/<your branch> (never --force), read git ls-remote origin <your branch> and check the tip is your HEAD, then comment both shas, the measured added lines and the limit on the bead and stop: the overseer decides whether to raise the limit or split the bead. Do not propose a split and do not run gt done again with the same change."

// lineLimitVerdict decides whether added is too many for the limit the gate
// applies — the larger of the limits the bead declares (the largest of several
// numbers) and floor, the band median specdispatch.LineFloor reads for the
// bead's estimate (gt-3mcn7b) — and returns the lines to print: the refusal
// past twice the limit, a warning past the limit, or the no-limit line
// (gt-wxpd0y). A bead that declares no limit skips the check, floor or not.
func lineLimitVerdict(limits []int, floor, added int) (refuse bool, lines []string) {
	if len(limits) == 0 {
		return false, []string{noLimitLine}
	}
	declared := limits[0]
	for _, l := range limits[1:] {
		if l > declared {
			declared = l
		}
	}
	limit := declared
	stated := ""
	if len(limits) > 1 {
		parts := make([]string, len(limits))
		for i, l := range limits {
			parts[i] = fmt.Sprint(l)
		}
		stated = "the bead states " + strings.Join(parts, " and ") + "; "
	}
	if floor > declared {
		limit = floor
		stated += fmt.Sprintf("the sp-minutes band floor of %d is the larger; ", floor)
	}
	line := fmt.Sprintf("Change exceeds the %d-added-line constraint: %d added lines against %d declared (%sthe refusal is at twice the limit).",
		limit, added, declared, stated)
	if added > 2*limit {
		return true, []string{line, overLimitLine}
	}
	if added > limit {
		return false, []string{line}
	}
	return false, nil
}

// sizeRefusalError is the refusal of a polecat past twice the declared limit,
// apart from the unreadable added-line count that also stops a run: a caller
// that acts on the refusal alone asks errors.As for it.
type sizeRefusalError string

func (e sizeRefusalError) Error() string { return string(e) }

// pushedRefusal ends a size refusal with the line naming where origin now holds
// a sandbox seat's work, so the seat and the overseer read the sha (gt-s0kxmj).
func pushedRefusal(refusal sizeRefusalError, sha, branch string) sizeRefusalError {
	return sizeRefusalError(string(refusal) + "\n" + fmt.Sprintf("your work is pushed at %s on %s", ShortSHA(sha), branch))
}

// unpushedRefusal ends a size refusal with the line saying the work is not
// pushed and the error that stopped the push, so the seat and the overseer know
// the sandbox holds the only copy (gt-96m2m0). The push error can span several
// lines, so it is joined into one: the shape of the refusal is its last line.
func unpushedRefusal(refusal sizeRefusalError, pushErr error) sizeRefusalError {
	reason := strings.Join(strings.Fields(pushErr.Error()), " ")
	return sizeRefusalError(string(refusal) + "\n" + fmt.Sprintf("your work is NOT pushed: %s; the sandbox holds the only copy", reason))
}

// crewWarningPrefix marks a crew's over-limit lines apart from a polecat refusal (gt-zqg4m2).
const crewWarningPrefix = "warning (crew branch, not refused): "

// reportLineLimit prints the added-line check's verdict and returns the refusal
// for a polecat past twice the limit the gate applies, which is the bead's
// declared limit or the band floor labels read, whichever is larger (gt-3mcn7b);
// a crew branch is warned on errOut instead, an unreadable count is an error,
// and no declared limit skips the count and the floor alike.
func reportLineLimit(out, errOut io.Writer, count func() (int, error), description string, labels []string, polecat bool) error {
	limits := DeclaredLineLimits(description)
	if len(limits) == 0 {
		_, lines := lineLimitVerdict(limits, 0, 0)
		fmt.Fprintln(out, lines[0])
		return nil
	}
	var floor int
	if f, ok := specdispatch.LineFloor(labels); ok {
		floor = f
	}
	added, err := count()
	if err != nil {
		return fmt.Errorf("added-line check: %w", err)
	}
	refuse, lines := lineLimitVerdict(limits, floor, added)
	if refuse && !polecat {
		for _, line := range lines {
			fmt.Fprintln(errOut, crewWarningPrefix+line)
		}
		return nil
	}
	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	if refuse {
		return sizeRefusalError(strings.Join(lines, "\n"))
	}
	return nil
}

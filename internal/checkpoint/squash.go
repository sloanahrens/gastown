package checkpoint

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/util"
)

// WIPCommitPrefix is the commit message prefix used by checkpoint_dog auto-commits.
const WIPCommitPrefix = "WIP: checkpoint (auto)"

// AutoSaveCommitPrefix is the commit message prefix used by the gt done
// safety-net auto-commit (gt-pvx).
const AutoSaveCommitPrefix = "fix: auto-save uncommitted implementation work"

// IsAutoSaveSubject reports whether a commit subject is machine-generated —
// either a checkpoint_dog WIP commit or a gt-pvx safety-net auto-save.
func IsAutoSaveSubject(subject string) bool {
	return strings.HasPrefix(subject, WIPCommitPrefix) || strings.HasPrefix(subject, AutoSaveCommitPrefix)
}

// HasAutoSaveCommits reports whether any commit between the merge-base of
// baseRef and headRef, and headRef itself, has a machine-generated subject
// (see IsAutoSaveSubject). It is read-only — it never checks anything out or
// changes HEAD — so it is safe to call against a ref other than the current
// branch, e.g. from a merge process staged on a different target (gt-rswr).
func HasAutoSaveCommits(workDir, baseRef, headRef string) (bool, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, headRef)
	if err != nil {
		return false, fmt.Errorf("finding merge-base: %w", err)
	}

	logOut, err := gitOutput(workDir, "log", "--format=%s", mergeBase+".."+headRef)
	if err != nil {
		return false, fmt.Errorf("listing commits: %w", err)
	}
	if logOut == "" {
		return false, nil
	}

	for _, line := range strings.Split(logOut, "\n") {
		if IsAutoSaveSubject(line) {
			return true, nil
		}
	}
	return false, nil
}

// AutoSaveTip describes where a branch's machine-generated commits sit
// relative to its tip, which decides whether a caller can fold them away
// without moving the tip off real work (gt-iki6).
type AutoSaveTip struct {
	Subject  string // subject line of headRef's tip commit
	AutoSave bool   // whether Subject is machine-generated (see IsAutoSaveSubject)
	Trailing int    // consecutive machine-generated commits at the tip; 0 when the tip is real
	Ahead    int    // commits in merge-base(baseRef, headRef)..headRef
	// BeneathIsMerge reports that headRef~Trailing — the commit the run would
	// be folded into — is a merge, so amending it would bury the branch's own
	// message under the merge's subject. False when the commit could not be read.
	BeneathIsMerge bool
}

// InspectAutoSaveTip reports the machine-generated shape of headRef's tip. It
// is read-only, so it is safe against a ref other than the current branch.
func InspectAutoSaveTip(workDir, baseRef, headRef string) (AutoSaveTip, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, headRef)
	if err != nil {
		return AutoSaveTip{}, fmt.Errorf("finding merge-base: %w", err)
	}

	// %x1e terminates each record: a bare newline cannot separate them, because
	// gitOutput trims the trailing one and an empty tip subject (git commit
	// --allow-empty-message) would then be swallowed, shifting every index by
	// one and naming the commit below the tip.
	logOut, err := gitOutput(workDir, "log", "--format=%s%x1e", mergeBase+".."+headRef)
	if err != nil {
		return AutoSaveTip{}, fmt.Errorf("listing commits: %w", err)
	}
	subjects := parseSubjectRecords(logOut)
	if len(subjects) == 0 {
		return AutoSaveTip{}, nil
	}

	// git log lists newest first, so index 0 is the tip. The run of generated
	// commits at the tip is single-parent by construction — a merge subject is
	// never generated — so the index also counts first-parent steps, which is
	// what HEAD~Trailing means to a caller.
	tip := AutoSaveTip{
		Subject:  subjects[0],
		AutoSave: IsAutoSaveSubject(subjects[0]),
		Ahead:    len(subjects),
	}
	for _, subject := range subjects {
		if !IsAutoSaveSubject(subject) {
			break
		}
		tip.Trailing++
	}

	if tip.Trailing < tip.Ahead {
		if parents, parentsErr := gitOutput(workDir, "log", "-1", "--format=%p", fmt.Sprintf("%s~%d", headRef, tip.Trailing)); parentsErr == nil {
			tip.BeneathIsMerge = len(strings.Fields(parents)) > 1
		}
	}
	return tip, nil
}

// parseSubjectRecords splits `git log --format=%s%x1e` output into one subject
// per commit, newest first.
func parseSubjectRecords(logOut string) []string {
	if logOut == "" {
		return nil
	}
	records := strings.Split(logOut, "\x1e")
	// Whatever follows the final sentinel is not a record.
	records = records[:len(records)-1]

	subjects := make([]string, 0, len(records))
	for _, record := range records {
		subjects = append(subjects, strings.TrimSpace(record))
	}
	return subjects
}

// CountWIPCommits returns the number of WIP checkpoint commits between
// the merge-base of baseRef and HEAD.
func CountWIPCommits(workDir, baseRef string) (int, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return 0, fmt.Errorf("finding merge-base: %w", err)
	}

	// List commit subjects from merge-base..HEAD
	logOut, err := gitOutput(workDir, "log", "--format=%s", mergeBase+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("listing commits: %w", err)
	}

	if logOut == "" {
		return 0, nil
	}

	count := 0
	for _, line := range strings.Split(logOut, "\n") {
		if strings.HasPrefix(line, WIPCommitPrefix) {
			count++
		}
	}
	return count, nil
}

// SquashWIPCommits collapses all commits from merge-base..HEAD into a single
// commit, preserving non-WIP commit messages in the body. Returns the number
// of WIP commits that were squashed.
//
// This is safe because Refinery squash-merges polecat branches anyway —
// individual commit history on polecat branches is not preserved.
func SquashWIPCommits(workDir, baseRef string) (int, error) {
	isWIP := func(subject string) bool { return strings.HasPrefix(subject, WIPCommitPrefix) }
	return squashMatchingCommits(workDir, baseRef, isWIP, "squashed WIP checkpoint commits")
}

// SquashAutoSaveCommits collapses the branch into a single commit when any
// machine-generated commit (checkpoint_dog WIP or gt-pvx auto-save) exists in
// merge-base..HEAD (gt-3wf). Non-generated subjects are preserved: the first
// becomes the squash title and the rest go in the body. When every commit is
// machine-generated, fallbackTitle (typically derived from the source issue
// title) becomes the subject so merge history stays descriptive. Returns the
// number of machine-generated commits that were squashed.
func SquashAutoSaveCommits(workDir, baseRef, fallbackTitle string) (int, error) {
	if strings.TrimSpace(fallbackTitle) == "" {
		fallbackTitle = "squashed auto-save checkpoint commits"
	}
	return squashMatchingCommits(workDir, baseRef, IsAutoSaveSubject, fallbackTitle)
}

// squashMatchingCommits soft-resets merge-base..HEAD into one commit when any
// subject matches. Non-matching subjects are kept: first as title, rest as
// body bullets. allMatchedTitle is used when every commit matched.
func squashMatchingCommits(workDir, baseRef string, matches func(string) bool, allMatchedTitle string) (int, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return 0, fmt.Errorf("finding merge-base: %w", err)
	}

	// List commit subjects from merge-base..HEAD
	logOut, err := gitOutput(workDir, "log", "--format=%s", mergeBase+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("listing commits: %w", err)
	}

	if logOut == "" {
		return 0, nil // No commits to squash
	}

	subjects := strings.Split(logOut, "\n")

	wipCount := 0
	var nonWIPSubjects []string
	for _, subj := range subjects {
		if matches(subj) {
			wipCount++
		} else if subj != "" {
			nonWIPSubjects = append(nonWIPSubjects, subj)
		}
	}

	if wipCount == 0 {
		return 0, nil // Nothing to squash
	}

	// Soft-reset to merge-base (preserves all changes as staged)
	if _, err := gitOutput(workDir, "reset", "--soft", mergeBase); err != nil {
		return 0, fmt.Errorf("soft reset: %w", err)
	}

	// Build combined commit message
	var msg strings.Builder
	if len(nonWIPSubjects) > 0 {
		// Use first non-WIP subject as the title
		msg.WriteString(nonWIPSubjects[0])
		if len(nonWIPSubjects) > 1 {
			msg.WriteString("\n")
			for _, subj := range nonWIPSubjects[1:] {
				msg.WriteString("\n- ")
				msg.WriteString(subj)
			}
		}
	} else {
		// All commits matched — use the caller-supplied title
		msg.WriteString(allMatchedTitle)
	}

	// Commit with combined message
	if _, err := gitOutput(workDir, "commit", "-m", msg.String()); err != nil {
		return 0, fmt.Errorf("squash commit: %w", err)
	}

	return wipCount, nil
}

// gitOutput runs a git command and returns trimmed stdout.
func gitOutput(workDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	util.SetDetachedProcessGroup(cmd)

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return "", fmt.Errorf("%s: %s", err, stderr)
			}
		}
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
	"github.com/steveyegge/gastown/internal/rig"
)

// editorialCoverageState is the runtime baseline for the editorial-coverage
// check, persisted at <rig>/.runtime/editorial-coverage.json. It records the
// head sha this check last walked to, so each run only re-verifies ranges
// landed since the previous run.
type editorialCoverageState struct {
	LastCheckedSHA string `json:"last_checked_sha"`
}

// EditorialCoverageCheck verifies that every commit landed on a rig's
// default branch since the last check carries an approve note (refs/notes/om)
// whose patch-id matches the commit's own diff from its first parent. A rig
// only enforces this when merge_queue.editorial.required is set — the check
// walks nothing and reports OK for rigs that never opted in.
type EditorialCoverageCheck struct {
	BaseCheck
}

// NewEditorialCoverageCheck creates a new editorial-coverage check.
func NewEditorialCoverageCheck() *EditorialCoverageCheck {
	return &EditorialCoverageCheck{
		BaseCheck: BaseCheck{
			CheckName:        "editorial-coverage",
			CheckDescription: "Verify every landed merge carries a matching om approve note",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run walks origin/<default-branch> first-parent from the recorded baseline
// to head and verifies each landed commit has a matching approve note.
func (c *EditorialCoverageCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified",
		}
	}

	mq := rig.ResolveMergeQueueConfig(ctx.TownRoot, ctx.RigName)
	if mq == nil || mq.Editorial == nil || !mq.Editorial.Required {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "editorial review not required for this rig",
		}
	}

	mayorRig := filepath.Join(rigPath, "mayor", "rig")
	g := git.NewGit(mayorRig)

	if err := g.Fetch("origin"); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not fetch origin",
			Details: []string{err.Error()},
		}
	}

	target := g.RemoteDefaultBranch()
	head, err := g.Rev("origin/" + target)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: fmt.Sprintf("unknown: could not resolve origin/%s", target),
			Details: []string{err.Error()},
		}
	}

	statePath := filepath.Join(rigPath, ".runtime", "editorial-coverage.json")
	state, hadBaseline := readEditorialCoverageState(statePath)
	if !hadBaseline {
		if err := writeEditorialCoverageState(statePath, editorialCoverageState{LastCheckedSHA: head}); err != nil {
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusSkipped,
				Message: "unknown: no baseline, and could not write one",
				Details: []string{err.Error()},
			}
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: no baseline",
			Details: []string{fmt.Sprintf("recorded head %s as the starting baseline for future runs", shortSHA(head))},
		}
	}

	if _, err := g.Rev("refs/notes/" + editorial.NotesRef); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: notes ref absent",
			Details: []string{fmt.Sprintf("refs/notes/%s does not exist in %s", editorial.NotesRef, mayorRig)},
		}
	}

	landed, err := firstParentRange(mayorRig, state.LastCheckedSHA, head)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not walk first-parent history",
			Details: []string{err.Error()},
		}
	}

	var uncovered []string
	for _, r := range landed {
		note, err := editorial.ReadNote(g, r.commit)
		switch {
		case err == git.ErrNoNote:
			uncovered = append(uncovered, fmt.Sprintf("%s: no note", shortSHA(r.commit)))
			continue
		case err != nil:
			uncovered = append(uncovered, fmt.Sprintf("%s: could not read note (%v)", shortSHA(r.commit), err))
			continue
		}
		if note.Verdict != "approve" {
			uncovered = append(uncovered, fmt.Sprintf("%s: verdict is %q, not approve", shortSHA(r.commit), note.Verdict))
			continue
		}
		patchID, err := g.PatchID(r.parent, r.commit)
		if err != nil {
			uncovered = append(uncovered, fmt.Sprintf("%s: could not compute patch-id (%v)", shortSHA(r.commit), err))
			continue
		}
		if note.PatchID != patchID {
			uncovered = append(uncovered, fmt.Sprintf("%s: note patch-id does not match commit", shortSHA(r.commit)))
		}
	}

	// The baseline advances to the head just walked regardless of outcome:
	// coverage is reported per run, not accumulated, so a failing range
	// flagged this run is not re-flagged on the next one once fixed forward.
	if err := writeEditorialCoverageState(statePath, editorialCoverageState{LastCheckedSHA: head}); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not advance baseline",
			Details: []string{err.Error()},
		}
	}

	total := len(landed)
	covered := total - len(uncovered)
	message := fmt.Sprintf("covered %d/%d", covered, total)
	if len(uncovered) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: message,
			Details: uncovered,
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: message,
	}
}

// landedRange is one first-parent step in the walked history: commit is the
// landed sha, parent is its own first parent (commit^1), the pair PatchID is
// computed against.
type landedRange struct {
	commit string
	parent string
}

// firstParentRange lists the first-parent commits in (from, to] along with
// each commit's own first parent, using the rig repo at repoDir. from ==
// "" is treated as the repository root (walk everything reachable from to).
func firstParentRange(repoDir, from, to string) ([]landedRange, error) {
	rangeSpec := to
	if from != "" {
		rangeSpec = from + ".." + to
	}
	cmd := exec.Command("git", "-C", repoDir, "log", "--first-parent", "--pretty=%H %P", rangeSpec)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git log --first-parent %s: %w: %s", rangeSpec, err, strings.TrimSpace(string(out)))
	}

	var ranges []landedRange
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			// Root commit: no parent to diff against, nothing to review.
			continue
		}
		ranges = append(ranges, landedRange{commit: fields[0], parent: fields[1]})
	}
	// git log lists newest-first; walk oldest-first so results read
	// chronologically.
	for i, j := 0, len(ranges)-1; i < j; i, j = i+1, j-1 {
		ranges[i], ranges[j] = ranges[j], ranges[i]
	}
	return ranges, nil
}

// readEditorialCoverageState reads the baseline state file. The second
// return value is false when no usable baseline exists yet (missing or
// unparseable file) — distinct from a zero-value LastCheckedSHA the check
// must never invent.
func readEditorialCoverageState(path string) (editorialCoverageState, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return editorialCoverageState{}, false
	}
	var state editorialCoverageState
	if err := json.Unmarshal(data, &state); err != nil || state.LastCheckedSHA == "" {
		return editorialCoverageState{}, false
	}
	return state, true
}

// writeEditorialCoverageState persists the baseline state file, creating
// its .runtime parent directory if needed.
func writeEditorialCoverageState(path string, state editorialCoverageState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// shortSHA returns the first 12 hex characters of sha, matching the
// convention finding ids elsewhere in the om-gate work use.
func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

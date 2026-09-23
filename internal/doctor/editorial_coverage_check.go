package doctor

import (
	"encoding/json"
	"errors"
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
// default branch since the last check is proven: it carries an approve note
// (refs/notes/om) whose patch-id matches the commit's own diff from its first
// parent, or — reported as covered-by-patch-id, with the backfill that moves
// the proof — a note on another sha proves the same diff. A rig only enforces
// this when merge_queue.editorial.required is set — the check walks nothing
// and reports OK for rigs that never opted in.
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

	// mayorRig is a plain clone: a bare `git fetch origin` above only pulls
	// refs/heads/*, never refs/notes/*. Reviews land their approve/reject
	// note (and push it to origin) from a different clone entirely
	// (refinery/rig), so without an explicit notes fetch this ref is empty
	// here even when every landed commit is genuinely covered.
	if err := g.FetchNotes("origin", editorial.NotesRef); err != nil && !errors.Is(err, git.ErrNoRemoteNotes) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not fetch notes",
			Details: []string{err.Error()},
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

	ix := &coverageNoteIndex{g: g}
	var uncovered []string
	var parkedCommits []parkedCommit
	for _, r := range landed {
		note, err := editorial.ReadNote(g, r.commit)
		switch {
		case err == git.ErrNoNote:
			p, ok, lerr := ix.parked(r)
			if lerr != nil {
				return &CheckResult{
					Name:    c.Name(),
					Status:  StatusSkipped,
					Message: "unknown: could not list notes",
					Details: []string{lerr.Error()},
				}
			}
			if !ok {
				uncovered = append(uncovered, fmt.Sprintf("%s: no note", shortSHA(r.commit)))
				continue
			}
			parkedCommits = append(parkedCommits, p)
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

	// A commit proven by a note on another sha counts as covered: its diff is
	// what was reviewed, and the backfill only moves the proof, not the
	// verdict. It is still named in the details, because the proof is not
	// where this check reads and a later run that re-walks the commit would
	// report it again until the note is re-keyed.
	total := len(landed)
	covered := total - len(uncovered)
	message := fmt.Sprintf("covered %d/%d", covered, total)
	if len(parkedCommits) > 0 {
		message += fmt.Sprintf(" (%d by patch-id, no note on the landed commit)", len(parkedCommits))
	}
	details := make([]string, 0, len(uncovered)+2*len(parkedCommits))
	details = append(details, uncovered...)
	for _, p := range parkedCommits {
		details = append(details, p.finding(), p.fix())
	}
	if len(uncovered) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: message,
			Details: details,
		}
	}
	if len(parkedCommits) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: message,
			Details: details,
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

// parkedCommit is a landed commit whose own diff is proven by an approve note
// attached to a different sha. The merge queue parks a verdict there whenever
// the note is keyed to a head the landing discarded — a rehearsal head, or the
// branch tip a non-fast-forward merge left behind — so the landed commit has
// no note while the diff it carries does (gt-8jwn).
type parkedCommit struct {
	// commit is the walked first-parent commit that carries no note.
	commit string
	// noteOn is the commit the covering note is attached to.
	noteOn string
	// mr is the MR that note names; the backfill looks the note up by it.
	mr string
	// secondParent is set when the merge's second parent — the polecat head
	// it brought in — carries the same diff, so the backfill must stamp both.
	secondParent bool
}

// finding states the covered-by-patch-id case in the detail list.
func (p parkedCommit) finding() string {
	return fmt.Sprintf("%s: covered-by-patch-id — MR %s's approve note on %s proves this diff",
		shortSHA(p.commit), p.mr, shortSHA(p.noteOn))
}

// fix is the exact backfill that moves the proof onto the landed commit.
// RekeyNote refuses rather than guesses, but every patch-id it checks is one
// this check already matched, so the command printed here is one that runs
// rather than refuses. The MR id is single-quoted because it is read out of a
// notes ref every writer shares, and a line handed to an operator to paste
// should carry no syntax but the command's own.
func (p parkedCommit) fix() string {
	cmd := fmt.Sprintf("gt mq rekey-note '%s' --landed %s", p.mr, shortSHA(p.commit))
	if p.secondParent {
		cmd += " --second-parent"
	}
	return fmt.Sprintf("  fix: %s --reason \"<why the copy is legitimate>\"", cmd)
}

// coverageNoteIndex answers "which note proves this diff?" from one listing of
// the notes ref. It is loaded on first miss: a fully covered range never pays
// for it.
type coverageNoteIndex struct {
	g      *git.Git
	loaded bool
	err    error
	// byPatch is keyed by patch-id and holds one note per key, the lowest
	// annotated sha, so a diff several notes prove reports the same MR on
	// every run.
	byPatch map[string]coveringNote
}

// coveringNote is an approve note that proves a diff, with the commit it is
// attached to and the MR it names.
type coveringNote struct {
	commit string
	mr     string
}

// parked reports whether r's own diff is proven by an approve note attached to
// another sha, and describes the backfill that moves that proof onto r. ok is
// false when nothing proves the diff — the genuinely uncovered case, which
// stays an error; err reports a notes ref that could not be read at all.
func (ix *coverageNoteIndex) parked(r landedRange) (parkedCommit, bool, error) {
	patchID, err := ix.g.PatchID(r.parent, r.commit)
	if err != nil {
		// A diff that cannot be patch-id'd cannot be matched to a note; leave
		// it to the uncovered branch rather than invent a proof for it.
		return parkedCommit{}, false, nil
	}
	note, ok, err := ix.covering(patchID)
	if err != nil || !ok {
		return parkedCommit{}, false, err
	}
	return parkedCommit{
		commit:       r.commit,
		noteOn:       note.commit,
		mr:           note.mr,
		secondParent: secondParentCovers(ix.g, r.commit, patchID),
	}, true, nil
}

// covering returns the approve note proving patchID, if the notes ref holds
// one.
func (ix *coverageNoteIndex) covering(patchID string) (coveringNote, bool, error) {
	if !ix.loaded {
		ix.loaded = true
		ix.err = ix.load()
	}
	if ix.err != nil {
		return coveringNote{}, false, ix.err
	}
	note, ok := ix.byPatch[patchID]
	return note, ok, nil
}

// load reads refs/notes/om once. Notes that do not parse are skipped: the ref
// is shared with every writer that ever touched it, so an unreadable note
// elsewhere must not hide the proof for this diff.
func (ix *coverageNoteIndex) load() error {
	entries, err := ix.g.NotesList(editorial.NotesRef)
	if err != nil {
		return err
	}
	ix.byPatch = make(map[string]coveringNote, len(entries))
	for _, e := range entries {
		var n editorial.Note
		if err := json.Unmarshal([]byte(e.Content), &n); err != nil {
			continue
		}
		// Only an approve verdict is proof, and an empty patch-id proves
		// nothing.
		if n.Verdict != "approve" || n.PatchID == "" {
			continue
		}
		// The remedy names the MR in a command the operator pastes, so an empty
		// id, or one that would escape that command's quoting, backs no remedy
		// this check can hand over. Bead ids are always safe; a value read out
		// of a ref every writer shares is not assumed to be one.
		if n.MR == "" || strings.ContainsAny(n.MR, "'\n") {
			continue
		}
		if cur, ok := ix.byPatch[n.PatchID]; !ok || e.Annotated < cur.commit {
			ix.byPatch[n.PatchID] = coveringNote{commit: e.Annotated, mr: n.MR}
		}
	}
	return nil
}

// secondParentCovers reports whether commit is a merge whose second parent
// carries the same diff. RekeyNote's --second-parent stamps that parent too,
// and refuses when the two patch-ids differ, so the flag is offered only where
// the command would accept it.
func secondParentCovers(g *git.Git, commit, patchID string) bool {
	parents, err := g.Parents(commit)
	if err != nil || len(parents) < 2 {
		return false
	}
	second := parents[1]
	secondParents, err := g.Parents(second)
	if err != nil || len(secondParents) == 0 {
		return false
	}
	secondPatchID, err := g.PatchID(secondParents[0], second)
	if err != nil {
		return false
	}
	return secondPatchID == patchID
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

package doctor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery/editorial"
)

// coverageFixture is a rig with a real mayor/rig clone of a bare origin, with
// merge_queue.editorial.required set in the rig-root config.json.
type coverageFixture struct {
	rigPath    string
	originPath string
	mayorRig   string
	g          *git.Git
}

func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newCoverageFixture sets up <tmpDir>/<rigName>/mayor/rig as a clone of a
// bare origin, with editorial review required, and a single root commit on
// main pushed to origin.
func newCoverageFixture(t *testing.T, tmpDir, rigName string) *coverageFixture {
	t.Helper()
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := `{"merge_queue":{"editorial":{"required":true}}}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	origin := filepath.Join(tmpDir, rigName+"-origin.git")
	runGitIn(t, tmpDir, "init", "--bare", origin)

	mayorRig := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(filepath.Dir(mayorRig), 0755); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, tmpDir, "clone", origin, mayorRig)
	runGitIn(t, mayorRig, "config", "user.email", "test@test.com")
	runGitIn(t, mayorRig, "config", "user.name", "Test User")
	runGitIn(t, mayorRig, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(mayorRig, "README.md"), []byte("root\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, mayorRig, "add", ".")
	runGitIn(t, mayorRig, "commit", "-m", "root commit")
	runGitIn(t, mayorRig, "push", "-u", "origin", "main")

	return &coverageFixture{
		rigPath:    rigPath,
		originPath: origin,
		mayorRig:   mayorRig,
		g:          git.NewGit(mayorRig),
	}
}

// commit writes name=content, commits, and pushes to origin main. Returns
// the new commit sha.
func (f *coverageFixture) commit(t *testing.T, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.mayorRig, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, f.mayorRig, "add", ".")
	runGitIn(t, f.mayorRig, "commit", "-m", message)
	runGitIn(t, f.mayorRig, "push", "origin", "main")
	sha, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return sha
}

// approveNote writes a matching approve note on commit (parent must be its
// actual git parent, for a correct patch-id).
func (f *coverageFixture) approveNote(t *testing.T, parent, commit string) {
	t.Helper()
	f.verdictNote(t, parent, commit, "", "approve")
}

// verdictNote writes a note on commit claiming the patch-id of base..commit,
// for mr, under the given verdict. base is the commit the note's diff is taken
// against, so a note can be written on a commit other than the one that landed
// and still prove the landed diff.
func (f *coverageFixture) verdictNote(t *testing.T, base, commit, mr, verdict string) {
	t.Helper()
	patchID, err := f.g.PatchID(base, commit)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	note := editorial.Note{
		OMVersion:  "1.0.0",
		Rig:        "testrig",
		MR:         mr,
		BaseSHA:    base,
		HeadSHA:    commit,
		PatchID:    patchID,
		Score:      0.9,
		Verdict:    verdict,
		ReviewedAt: time.Now(),
	}
	if err := editorial.WriteNote(f.g, note); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
}

// commitOnly commits name=content on the current branch and returns the new
// sha, without pushing: history a test builds and discards.
func (f *coverageFixture) commitOnly(t *testing.T, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.mayorRig, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, f.mayorRig, "add", ".")
	runGitIn(t, f.mayorRig, "commit", "-m", message)
	sha, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return sha
}

func (f *coverageFixture) writeState(t *testing.T, lastCheckedSHA string) {
	t.Helper()
	dir := filepath.Join(f.rigPath, ".runtime")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(editorialCoverageState{LastCheckedSHA: lastCheckedSHA})
	if err := os.WriteFile(filepath.Join(dir, "editorial-coverage.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func (f *coverageFixture) ctx(tmpDir, rigName string) *CheckContext {
	return &CheckContext{TownRoot: tmpDir, RigName: rigName}
}

func TestEditorialCoverageCheck_NotRequired(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}
	// No config.json at all -> editorial not required.
	check := NewEditorialCoverageCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK when editorial not required, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialCoverageCheck_NoBaseline(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	c1 := f.commit(t, "a.txt", "a\n", "add a")
	f.approveNote(t, root, c1)

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusSkipped {
		t.Fatalf("expected StatusSkipped (unknown) with no baseline, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "no baseline") {
		t.Errorf("expected message to mention 'no baseline', got %q", result.Message)
	}

	statePath := filepath.Join(f.rigPath, ".runtime", "editorial-coverage.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("expected state file to be written: %v", err)
	}
	var state editorialCoverageState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	head, _ := f.g.Rev("origin/main")
	if state.LastCheckedSHA != head {
		t.Errorf("expected baseline %s, got %s", head, state.LastCheckedSHA)
	}
}

func TestEditorialCoverageCheck_FullyCovered(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	c1 := f.commit(t, "a.txt", "a\n", "add a")
	f.approveNote(t, root, c1)
	c2 := f.commit(t, "b.txt", "b\n", "add b")
	f.approveNote(t, c1, c2)
	c3 := f.commit(t, "c.txt", "c\n", "add c")
	f.approveNote(t, c2, c3)

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s\n%v", result.Status, result.Message, result.Details)
	}
	if result.Message != "covered 3/3" {
		t.Errorf("expected 'covered 3/3', got %q", result.Message)
	}

	// Baseline should have advanced to the new head.
	statePath := filepath.Join(f.rigPath, ".runtime", "editorial-coverage.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state editorialCoverageState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.LastCheckedSHA != c3 {
		t.Errorf("expected baseline to advance to %s, got %s", c3, state.LastCheckedSHA)
	}
}

func TestEditorialCoverageCheck_MissingNote(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	c1 := f.commit(t, "a.txt", "a\n", "add a")
	f.approveNote(t, root, c1)
	c2 := f.commit(t, "b.txt", "b\n", "add b")
	// c2 gets no note.
	c3 := f.commit(t, "c.txt", "c\n", "add c")
	f.approveNote(t, c2, c3)

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	if result.Message != "covered 2/3" {
		t.Errorf("expected 'covered 2/3', got %q", result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.HasPrefix(d, shortSHA(c2)) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected details to name uncovered sha %s, got %v", shortSHA(c2), result.Details)
	}
}

func TestEditorialCoverageCheck_NotesRefAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	c1 := f.commit(t, "a.txt", "a\n", "add a")
	f.approveNote(t, root, c1)
	// Delete the notes ref entirely.
	runGitIn(t, f.mayorRig, "update-ref", "-d", "refs/notes/om")

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusSkipped {
		t.Fatalf("expected StatusSkipped (unknown), got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "notes ref absent") {
		t.Errorf("expected message to mention 'notes ref absent', got %q", result.Message)
	}
}

// TestEditorialCoverageCheck_NoteReachableOnlyViaOriginFetch reproduces the
// real deployment split: gt mq review writes and pushes its approve note
// from a different clone (refinery/rig) than the one the check reads
// (mayor/rig). Here that writer is modeled as a third clone of the same
// bare origin — mayorRig never sees the note locally until the check fetches
// refs/notes/om from origin itself.
func TestEditorialCoverageCheck_NoteReachableOnlyViaOriginFetch(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	c1 := f.commit(t, "a.txt", "a\n", "add a")

	writerClone := filepath.Join(tmpDir, "writer-clone")
	runGitIn(t, tmpDir, "clone", f.originPath, writerClone)
	writer := git.NewGit(writerClone)
	patchID, err := writer.PatchID(root, c1)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	note := editorial.Note{
		OMVersion:  "1.0.0",
		Rig:        "testrig",
		BaseSHA:    root,
		HeadSHA:    c1,
		PatchID:    patchID,
		Score:      0.9,
		Verdict:    "approve",
		ReviewedAt: time.Now(),
	}
	if err := editorial.WriteNote(writer, note); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
	if err := writer.PushNotes("origin", editorial.NotesRef); err != nil {
		t.Fatalf("PushNotes: %v", err)
	}

	// Sanity check: mayorRig has no local note yet — only origin does.
	if _, err := f.g.NotesShow(editorial.NotesRef, c1); err == nil {
		t.Fatal("expected mayorRig to have no local note before the check runs")
	}

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s\n%v", result.Status, result.Message, result.Details)
	}
	if result.Message != "covered 1/1" {
		t.Errorf("expected 'covered 1/1', got %q", result.Message)
	}
}

// rootSHA returns the sha of the fixture's single root commit.
func (f *coverageFixture) rootSHA(t *testing.T) string {
	t.Helper()
	sha, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return sha
}

// rehearsalHead commits the same change as a commit already made from base,
// on a branch that never lands. The two diffs are byte-identical, so their
// patch-ids match while their shas do not — the shape a merge queue leaves
// behind when a review is keyed to a head the landing discarded (gt-8jwn).
// Leaves the fixture checked out on main.
func (f *coverageFixture) rehearsalHead(t *testing.T, base, name, content string) string {
	t.Helper()
	runGitIn(t, f.mayorRig, "checkout", "-b", "rehearsal", base)
	sha := f.commitOnly(t, name, content, "rehearsal")
	runGitIn(t, f.mayorRig, "checkout", "main")
	return sha
}

// TestEditorialCoverageCheck_CoveredByPatchID covers the parked case: the
// landed commit carries no note, but an approve note on another sha proves its
// diff. The check must report that distinctly, keep counting it as covered,
// and print the backfill that moves the proof onto the landed commit.
func TestEditorialCoverageCheck_CoveredByPatchID(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	landed := f.commit(t, "a.txt", "a\n", "land a")
	rehearsal := f.rehearsalHead(t, root, "a.txt", "a\n")

	landedPatchID, err := f.g.PatchID(root, landed)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	rehearsalPatchID, err := f.g.PatchID(root, rehearsal)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if landedPatchID != rehearsalPatchID {
		t.Fatalf("fixture no longer reproduces gt-8jwn: rehearsal patch-id %s != landed patch-id %s", rehearsalPatchID, landedPatchID)
	}
	f.verdictNote(t, root, rehearsal, "gt-wisp-parked", "approve")

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for a commit proven by a note on another sha, got %v: %s\n%v", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "covered 1/1") {
		t.Errorf("a patch-id match is coverage, so the count must keep it: got %q", result.Message)
	}
	if !strings.Contains(result.Message, "1 by patch-id") {
		t.Errorf("expected the message to separate the patch-id match, got %q", result.Message)
	}

	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "covered-by-patch-id") {
		t.Errorf("expected a covered-by-patch-id detail, got %v", result.Details)
	}
	if !strings.Contains(joined, shortSHA(rehearsal)) {
		t.Errorf("expected the detail to name the note's own commit %s, got %v", shortSHA(rehearsal), result.Details)
	}
	if !strings.Contains(joined, "gt-wisp-parked") {
		t.Errorf("expected the detail to name the note's MR, got %v", result.Details)
	}
	want := "gt mq rekey-note 'gt-wisp-parked' --landed " + shortSHA(landed)
	if !strings.Contains(joined, want) {
		t.Errorf("expected the remedy %q, got %v", want, result.Details)
	}
	if strings.Contains(joined, "--second-parent") {
		t.Errorf("a non-merge landing needs no second-parent stamp, got %v", result.Details)
	}
}

// TestEditorialCoverageCheck_CoveredByPatchID_MergeSuggestsSecondParent covers
// the non-fast-forward landing: the merge commit has no note, its second
// parent (the polecat head) carries one proving the same diff, so the remedy
// must stamp both.
func TestEditorialCoverageCheck_CoveredByPatchID_MergeSuggestsSecondParent(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	runGitIn(t, f.mayorRig, "checkout", "-b", "polecat", root)
	polecat := f.commitOnly(t, "a.txt", "a\n", "add a")
	runGitIn(t, f.mayorRig, "checkout", "main")
	runGitIn(t, f.mayorRig, "merge", "--no-ff", "-m", "Merge polecat", polecat)
	runGitIn(t, f.mayorRig, "push", "origin", "main")
	merge, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	mergePatchID, err := f.g.PatchID(root, merge)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	polecatPatchID, err := f.g.PatchID(root, polecat)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	if mergePatchID != polecatPatchID {
		t.Fatalf("fixture no longer reproduces a second-parent copy: merge patch-id %s != polecat patch-id %s", mergePatchID, polecatPatchID)
	}
	// The note stayed on the polecat head, so the merge commit has none.
	f.verdictNote(t, root, polecat, "gt-wisp-qb0y", "approve")

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s\n%v", result.Status, result.Message, result.Details)
	}
	joined := strings.Join(result.Details, "\n")
	want := "gt mq rekey-note 'gt-wisp-qb0y' --landed " + shortSHA(merge) + " --second-parent"
	if !strings.Contains(joined, want) {
		t.Errorf("expected the remedy %q, got %v", want, result.Details)
	}
}

// TestEditorialCoverageCheck_CoveredByPatchIDKeepsUncoveredAnError keeps the
// two shapes apart: a patch-id match is reported beside the genuinely
// uncovered commit, and does not downgrade it out of an error.
func TestEditorialCoverageCheck_CoveredByPatchIDKeepsUncoveredAnError(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	parked := f.commit(t, "a.txt", "a\n", "land a")
	rehearsal := f.rehearsalHead(t, root, "a.txt", "a\n")
	f.verdictNote(t, root, rehearsal, "gt-wisp-parked", "approve")
	uncovered := f.commit(t, "b.txt", "b\n", "land b")

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusError {
		t.Fatalf("expected StatusError while a commit has no proof anywhere, got %v: %s", result.Status, result.Message)
	}
	if result.Message != "covered 1/2 (1 by patch-id, no note on the landed commit)" {
		t.Errorf("unexpected message %q", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, shortSHA(parked)+": covered-by-patch-id") {
		t.Errorf("expected the parked commit reported distinctly, got %v", result.Details)
	}
	if !strings.Contains(joined, shortSHA(uncovered)+": no note") {
		t.Errorf("expected the uncovered commit to stay an error, got %v", result.Details)
	}
}

// TestEditorialCoverageCheck_NonApproveNoteIsNotCoverage guards the rule that
// only an approve verdict is proof: a request_changes note on the same diff
// must not be reported as covering the landed commit.
func TestEditorialCoverageCheck_NonApproveNoteIsNotCoverage(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	landed := f.commit(t, "a.txt", "a\n", "land a")
	rehearsal := f.rehearsalHead(t, root, "a.txt", "a\n")
	f.verdictNote(t, root, rehearsal, "gt-wisp-rejected", "request_changes")

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusError {
		t.Fatalf("expected StatusError: a rejected diff is not covered, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), shortSHA(landed)+": no note") {
		t.Errorf("expected the landed commit reported uncovered, got %v", result.Details)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "covered-by-patch-id") {
		t.Errorf("a non-approve note must not be offered as a remedy, got %v", result.Details)
	}
}

// TestEditorialCoverageCheck_UnquotableMRBacksNoRemedy guards the printed
// remedy: it is a command the operator pastes, built from an MR id read out of
// the notes ref, so an id that could escape the command's quoting must leave
// the commit uncovered rather than be echoed into it.
func TestEditorialCoverageCheck_UnquotableMRBacksNoRemedy(t *testing.T) {
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := newCoverageFixture(t, tmpDir, rigName)
	root := f.rootSHA(t)
	f.writeState(t, root)

	landed := f.commit(t, "a.txt", "a\n", "land a")
	rehearsal := f.rehearsalHead(t, root, "a.txt", "a\n")
	f.verdictNote(t, root, rehearsal, "gt-wisp-x'; touch /tmp/pwned; echo '", "approve")

	check := NewEditorialCoverageCheck()
	result := check.Run(f.ctx(tmpDir, rigName))
	if result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, shortSHA(landed)+": no note") {
		t.Errorf("expected the landed commit reported uncovered, got %v", result.Details)
	}
	if strings.Contains(joined, "gt mq rekey-note") {
		t.Errorf("expected no pasteable command for an id that cannot be quoted, got %v", result.Details)
	}
}

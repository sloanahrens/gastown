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
	rigPath  string
	mayorRig string
	g        *git.Git
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
		rigPath:  rigPath,
		mayorRig: mayorRig,
		g:        git.NewGit(mayorRig),
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
	patchID, err := f.g.PatchID(parent, commit)
	if err != nil {
		t.Fatalf("PatchID: %v", err)
	}
	note := editorial.Note{
		OMVersion:  "1.0.0",
		Rig:        "testrig",
		BaseSHA:    parent,
		HeadSHA:    commit,
		PatchID:    patchID,
		Score:      0.9,
		Verdict:    "approve",
		ReviewedAt: time.Now(),
	}
	if err := editorial.WriteNote(f.g, note); err != nil {
		t.Fatalf("WriteNote: %v", err)
	}
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

// rootSHA returns the sha of the fixture's single root commit.
func (f *coverageFixture) rootSHA(t *testing.T) string {
	t.Helper()
	sha, err := f.g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	return sha
}

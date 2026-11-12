package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// setupRigWithRubric makes a rig whose repo main carries the rubric, with
// editorialConfig as its config.json when it is not empty, and returns the fake
// world the check reads the repo through.
func setupRigWithRubric(t *testing.T, tmpDir, rigName string, editorialConfig string) *gitfake.Fake {
	t.Helper()
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		t.Fatal(err)
	}
	if editorialConfig != "" {
		if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(editorialConfig), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return rigRepoWithRubric(t, tmpDir, rigName, map[string]string{".om.json": `{"rubric":[]}`})
}

func TestEditorialRequiredCheck_RubricPresentRequiredFalse(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := setupRigWithRubric(t, tmpDir, rigName, `{"merge_queue":{"editorial":{"required":false}}}`)

	check := NewEditorialRequiredCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, f)
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialRequiredCheck_RubricPresentNoConfigAtAll(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := setupRigWithRubric(t, tmpDir, rigName, "")

	check := NewEditorialRequiredCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, f)
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning when required is unset, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialRequiredCheck_RubricPresentRequiredTrue(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	// A rig config.json carries identity as well as merge_queue: with one
	// loader for the file (gt-y3pgh.2.5) an identity-less file no longer
	// decodes, so the fixture is shaped like a real one.
	f := setupRigWithRubric(t, tmpDir, rigName,
		`{"type":"rig","version":1,"name":"testrig","merge_queue":{"editorial":{"required":true}}}`)

	check := NewEditorialRequiredCheck()
	ctx := withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, f)
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s", result.Status, result.Message)
	}
}

func TestEditorialRequiredCheck_NoRubric(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(tmpDir, rigName)
	if err := os.MkdirAll(filepath.Join(rigPath, "mayor", "rig"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewEditorialRequiredCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK when no rubric present, got %v: %s", result.Status, result.Message)
	}
}

// rigRepoWithRubric makes <tmpDir>/<rigName>/.repo.git through gitfake with
// files committed on main and origin/main set to that commit, and returns the
// fake world the check reads the repo through.
func rigRepoWithRubric(t *testing.T, tmpDir, rigName string, files map[string]string) *gitfake.Fake {
	t.Helper()
	f := gitfake.New()
	repo := filepath.Join(tmpDir, rigName, ".repo.git")
	f.InitBare(t, repo)
	f.SetRef(t, repo, "refs/remotes/origin/main", f.Commit(t, repo, "main", "main's tip", files))
	return f
}

// TestEditorialRequiredCheck_RubricOnMainCloneBehind is the gt-yj6g1 case for
// this check: the rubric landed on main and the clone has not caught up. Read
// from the clone alone the rig would report no rubric, and the missing
// enforcement would go unnoticed.
func TestEditorialRequiredCheck_RubricOnMainCloneBehind(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	f := rigRepoWithRubric(t, tmpDir, rigName, map[string]string{".om.json": `{"rubric":[]}`})
	if err := os.MkdirAll(filepath.Join(tmpDir, rigName, "mayor", "rig"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewEditorialRequiredCheck()
	result := check.Run(withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, f))
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for main's rubric with editorial.required unset, got %v: %s", result.Status, result.Message)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "origin/main") {
		t.Errorf("details = %v, want the rubric's source named as main's copy", result.Details)
	}
}

// unreadableRefs is the repo seam a rig repo this run cannot read answers on:
// the two reads ReadRepoFile makes report. Driving it through CheckContext's
// own seam is what the test asserts the check does with a read failure, rather
// than a fake world's accident of a .repo.git directory it has no repository
// for (gt-yj6g1).
type unreadableRefs struct{ Repo }

var errRigRepoUnreadable = errors.New("fatal: not a git repository")

func (unreadableRefs) RefExists(string) (bool, error) { return false, errRigRepoUnreadable }

func (unreadableRefs) ShowFileAtRev(string, string) (string, error) {
	return "", errRigRepoUnreadable
}

// TestEditorialRequiredCheck_UnreadableRepoWarnsNamingRig: a rig repo the run
// cannot read is unknown, not a rig without a rubric (gt-yj6g1).
func TestEditorialRequiredCheck_UnreadableRepoWarnsNamingRig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	// ReadRepoFile only opens the repo when the directory is there, so the
	// unreadability has to be the repository's, not the path's.
	if err := os.MkdirAll(filepath.Join(tmpDir, rigName, ".repo.git"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
	ctx.openGit = func(string, string) Repo { return unreadableRefs{} }

	check := NewEditorialRequiredCheck()
	result := check.Run(ctx)
	if result.Status == StatusOK {
		t.Fatalf("expected a non-OK result when the repo cannot be read, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, rigName) {
		t.Errorf("message %q does not name the rig", result.Message)
	}
}

// writeStaleCloneRubric puts a rubric in the rig's mayor/rig clone, the copy
// main no longer carries, so a check that read the clone would see a rubric.
func writeStaleCloneRubric(t *testing.T, tmpDir, rigName string) {
	t.Helper()
	cloneDir := filepath.Join(tmpDir, rigName, "mayor", "rig")
	if err := os.MkdirAll(cloneDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, ".om.json"), []byte(`{"rubric":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestEditorialRequiredCheck_RubricOnlyInTheCloneIsNoRubric: a rubric deleted on
// main while the clone still holds it is not a rubric the rig has. A rig that
// requires editorial review with no rubric on main warns and names the rig and
// .om.json; the clone's copy does not make the rubric enforced (gt-yj6g1).
func TestEditorialRequiredCheck_RubricOnlyInTheCloneIsNoRubric(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	writeStaleCloneRubric(t, tmpDir, rigName)
	config := `{"type":"rig","version":1,"name":"testrig","merge_queue":{"editorial":{"required":true}}}`
	if err := os.WriteFile(filepath.Join(tmpDir, rigName, "config.json"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	f := rigRepoWithRubric(t, tmpDir, rigName, map[string]string{"README.md": "# testrig\n"})

	check := NewEditorialRequiredCheck()
	result := check.Run(withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, f))
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning for required editorial with no rubric on main, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, rigName) || !strings.Contains(result.Message, ".om.json") {
		t.Errorf("message = %q, want it to name rig %q and .om.json", result.Message, rigName)
	}
}

// TestEditorialRequiredCheck_RubricOnlyInTheCloneRequiredFalseIsOK is the twin
// of the required case: with editorial review not required and no rubric on
// main, the clone's stale copy is ignored and the rig reads as having no rubric.
func TestEditorialRequiredCheck_RubricOnlyInTheCloneRequiredFalseIsOK(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	writeStaleCloneRubric(t, tmpDir, rigName)
	config := `{"type":"rig","version":1,"name":"testrig","merge_queue":{"editorial":{"required":false}}}`
	if err := os.WriteFile(filepath.Join(tmpDir, rigName, "config.json"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	f := rigRepoWithRubric(t, tmpDir, rigName, map[string]string{"README.md": "# testrig\n"})

	check := NewEditorialRequiredCheck()
	result := check.Run(withGit(&CheckContext{TownRoot: tmpDir, RigName: rigName}, f))
	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK when editorial is not required, got %v: %s", result.Status, result.Message)
	}
	if result.Message != "No .om.json rubric in rig repo" {
		t.Errorf("message = %q, want no rubric in the rig repo", result.Message)
	}
}

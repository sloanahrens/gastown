package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The all-rigs check and the per-rig check judge one rig's clones, so they must
// name the same ones. A flat polecats/<name> clone is one the per-rig check has
// always covered.
func TestHooksPathAllRigs_FlatPolecatCloneIsChecked(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	flat := filepath.Join("polecats", "p1")
	nested := filepath.Join("polecats", "p2", rigName)
	ctx := hooksPathRig(t, rigName, flat, nested)

	want := []string{
		filepath.Join(ctx.RigPath(), flat),
		filepath.Join(ctx.RigPath(), nested),
	}
	if got := findRigClones(ctx.RigPath()); !reflect.DeepEqual(got, want) {
		t.Fatalf("findRigClones = %v, want %v", got, want)
	}

	perRig := NewHooksPathConfiguredCheck().Run(ctx)
	allRigs := NewHooksPathAllRigsCheck().Run(ctx)
	wantDetails := []string{
		filepath.Join(rigName, flat),
		filepath.Join(rigName, nested),
	}
	if allRigs.Status != StatusWarning || !reflect.DeepEqual(allRigs.Details, wantDetails) {
		t.Fatalf("all-rigs = %v %v, want Warning %v", allRigs.Status, allRigs.Details, wantDetails)
	}
	if perRig.Status != StatusWarning || len(perRig.Details) != len(wantDetails) {
		t.Fatalf("per-rig = %v %v, want Warning over the same %d clones", perRig.Status, perRig.Details, len(wantDetails))
	}
}

// Both checks skip an operator's linked crew worktree: it shares the crew
// clone's config, so the clone's verdict covers it.
func TestHooksPathChecks_AgreeOnCrewWorktree(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	crew := filepath.Join("crew", "sloan")
	topic := filepath.Join("crew", "sloan-topic")
	ctx := hooksPathRig(t, rigName, crew, topic)
	// A linked worktree's .git is a file, not a directory.
	topicGit := filepath.Join(ctx.RigPath(), topic, ".git")
	if err := os.RemoveAll(topicGit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(topicGit, []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	want := []string{filepath.Join(ctx.RigPath(), crew)}
	if got := findRigClones(ctx.RigPath()); !reflect.DeepEqual(got, want) {
		t.Fatalf("findRigClones = %v, want %v", got, want)
	}
	if got := rigCloneDirs(ctx.RigPath()); !reflect.DeepEqual(got, want) {
		t.Fatalf("rigCloneDirs = %v, want %v", got, want)
	}
}

// A polecat worktree is found under whatever directory it is nested in, not only
// the one named after the rig, so both checks see the same clone.
func TestHooksPathAllRigs_PolecatCloneUnderAnyDirIsChecked(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	nested := filepath.Join("polecats", "p1", "renamed-rig")
	ctx := hooksPathRig(t, rigName, nested)

	want := []string{filepath.Join(ctx.RigPath(), nested)}
	if got := findRigClones(ctx.RigPath()); !reflect.DeepEqual(got, want) {
		t.Fatalf("findRigClones = %v, want %v", got, want)
	}
	allRigs := NewHooksPathAllRigsCheck().Run(ctx)
	if allRigs.Status != StatusWarning || len(allRigs.Details) != 1 {
		t.Fatalf("all-rigs = %v %v, want Warning over the one nested clone", allRigs.Status, allRigs.Details)
	}
}

// A polecat can hold several nested clones. Each is judged, and a subdirectory
// with no .git is not, so neither the parent nor the stray directory is added.
func TestHooksPathAllRigs_PolecatWithSeveralNestedClones(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	alpha := filepath.Join("polecats", "p1", "alpha")
	beta := filepath.Join("polecats", "p1", "beta")
	ctx := hooksPathRig(t, rigName, alpha, beta)
	if err := os.MkdirAll(filepath.Join(ctx.RigPath(), "polecats", "p1", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(ctx.RigPath(), alpha),
		filepath.Join(ctx.RigPath(), beta),
	}
	if got := findRigClones(ctx.RigPath()); !reflect.DeepEqual(got, want) {
		t.Fatalf("findRigClones = %v, want %v", got, want)
	}
	assertHooksPathChecksWarn(t, ctx, alpha, beta)
}

// A linked worktree is not a clone: its .git is a file, as for a crew worktree.
// A nested one under a polecat is left out, and so is a flat one.
func TestHooksPathAllRigs_LinkedWorktreeIsNotAClone(t *testing.T) {
	t.Parallel()
	const rigName = "testrig"
	nested := filepath.Join("polecats", "p1", "wt")
	flat := filepath.Join("polecats", "p2")
	ctx := hooksPathRig(t, rigName, nested, flat)
	markLinkedWorktree(t, filepath.Join(ctx.RigPath(), nested))
	markLinkedWorktree(t, filepath.Join(ctx.RigPath(), flat))

	if got := findRigClones(ctx.RigPath()); len(got) != 0 {
		t.Fatalf("findRigClones = %v, want none", got)
	}
	assertHooksPathChecksOK(t, ctx)
}

// markLinkedWorktree turns the clone at dir into a linked worktree: its .git
// becomes a file that points elsewhere, as git writes it.
func markLinkedWorktree(t *testing.T, dir string) {
	t.Helper()
	gitPath := filepath.Join(dir, ".git")
	if err := os.RemoveAll(gitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitPath, []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertHooksPathChecksWarn requires both hooks-path checks to warn over exactly
// the clones at rels, given relative to the rig. The per-rig check names them
// relative to the rig, the all-rigs check relative to the town.
func assertHooksPathChecksWarn(t *testing.T, ctx *CheckContext, rels ...string) {
	t.Helper()
	var allRigs []string
	for _, rel := range rels {
		allRigs = append(allRigs, filepath.Join(ctx.RigName, rel))
	}
	if got := NewHooksPathConfiguredCheck().Run(ctx); got.Status != StatusWarning || !reflect.DeepEqual(got.Details, rels) {
		t.Fatalf("per-rig = %v %v, want Warning over %v", got.Status, got.Details, rels)
	}
	if got := NewHooksPathAllRigsCheck().Run(ctx); got.Status != StatusWarning || !reflect.DeepEqual(got.Details, allRigs) {
		t.Fatalf("all-rigs = %v %v, want Warning over %v", got.Status, got.Details, allRigs)
	}
}

// assertHooksPathChecksOK requires both hooks-path checks to pass: no clone is
// judged, so none is reported.
func assertHooksPathChecksOK(t *testing.T, ctx *CheckContext) {
	t.Helper()
	if got := NewHooksPathConfiguredCheck().Run(ctx); got.Status != StatusOK {
		t.Fatalf("per-rig = %v %v, want OK", got.Status, got.Details)
	}
	if got := NewHooksPathAllRigsCheck().Run(ctx); got.Status != StatusOK {
		t.Fatalf("all-rigs = %v %v, want OK", got.Status, got.Details)
	}
}

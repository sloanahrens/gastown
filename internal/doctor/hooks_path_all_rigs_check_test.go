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
	if got := rigCloneDirs(ctx.RigPath(), rigName); !reflect.DeepEqual(got, want) {
		t.Fatalf("rigCloneDirs = %v, want %v", got, want)
	}
}

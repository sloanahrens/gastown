package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// trackedConfigRig builds a town with one registered rig whose repo main
// carries trackedConfig at .beads/config.yaml, and whose store is the legacy
// mayor/rig/.beads directory. It returns the town root, the fake git world,
// the fake bd and the store's directory.
func trackedConfigRig(t *testing.T, trackedConfig string) (town string, gf *gitfake.Fake, bd *fakeBD, beadsDir string) {
	t.Helper()
	town = t.TempDir()
	const rigName = "alpha"
	writeRigsFixture(t, town, rigName)
	beadsDir = filepath.Join(town, rigName, "mayor", "rig", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gf = gitfake.New()
	repo := filepath.Join(town, rigName, ".repo.git")
	gf.InitBare(t, repo)
	gf.SetRef(t, repo, "refs/remotes/origin/main",
		gf.Commit(t, repo, "main", "tracked config", map[string]string{".beads/config.yaml": trackedConfig}))
	return town, gf, newFakeBD(), beadsDir
}

// TestBeadsConfigDriftCheckWarnsOnDifference: the tracked file says the events
// journal is off and the live store says it is on; the warning names the rig,
// the key and both values.
func TestBeadsConfigDriftCheckWarnsOnDifference(t *testing.T) {
	t.Parallel()
	town, gf, bd, beadsDir := trackedConfigRig(t, "events-journal: false\n")
	if err := bd.db(filepath.Dir(beadsDir)).ConfigSet("events-journal", "true"); err != nil {
		t.Fatal(err)
	}

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want warning: %+v", res.Status, res)
	}
	joined := strings.Join(res.Details, "\n")
	for _, want := range []string{"alpha", "events-journal", `"false"`, `"true"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("details = %q, want %q named", joined, want)
		}
	}
	for _, o := range bd.opened() {
		if env := strings.Join(o.env, " "); !strings.Contains(env, "BEADS_DIR="+beadsDir) {
			t.Errorf("bd in %s ran with env %v, want BEADS_DIR pinned to the store", o.dir, o.env)
		}
	}
}

// TestBeadsConfigDriftCheckMatchingValuesOK: a tracked value the live store
// agrees with is not a difference.
func TestBeadsConfigDriftCheckMatchingValuesOK(t *testing.T) {
	t.Parallel()
	town, gf, bd, beadsDir := trackedConfigRig(t, "events-journal: true\n")
	if err := bd.db(filepath.Dir(beadsDir)).ConfigSet("events-journal", "true"); err != nil {
		t.Fatal(err)
	}

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok: %+v", res.Status, res)
	}
}

// TestBeadsConfigDriftCheckSkipsNestedKeys: a top-level key whose value is a
// mapping (external_projects) is not a scalar setting and is not compared.
func TestBeadsConfigDriftCheckSkipsNestedKeys(t *testing.T) {
	t.Parallel()
	town, gf, bd, beadsDir := trackedConfigRig(t, "external_projects:\n  beads: ../../../beads/mayor/rig\n")
	bd.db(filepath.Dir(beadsDir)).FailWith("config get external_projects", errors.New("must not be read"))

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusOK {
		t.Fatalf("status = %v, want ok: %+v", res.Status, res)
	}
}

// TestBeadsConfigDriftCheckRegistryUnreadable: no rigs registry is an error,
// never a town with no rigs to compare.
func TestBeadsConfigDriftCheckRegistryUnreadable(t *testing.T) {
	t.Parallel()
	ctx := newFakeBD().ctx(t.TempDir())

	res := NewBeadsConfigDriftCheck().Run(ctx)
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
}

// TestBeadsConfigDriftCheckTrackedFileUnreadable: a rig repo this run cannot
// read is an error naming the rig, not a rig with nothing to compare.
func TestBeadsConfigDriftCheckTrackedFileUnreadable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeRigsFixture(t, town, "alpha")
	if err := os.MkdirAll(filepath.Join(town, "alpha", ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &CheckContext{TownRoot: town}
	ctx.openGit = func(string, string) Repo { return unreadableRefs{} }

	res := NewBeadsConfigDriftCheck().Run(ctx)
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message+strings.Join(res.Details, "\n"), "alpha") {
		t.Errorf("result = %+v, want the rig named", res)
	}
}

// TestBeadsConfigDriftCheckUnparseableTrackedFile: a tracked config.yaml that
// does not parse is an error naming the rig, not a file with no settings.
func TestBeadsConfigDriftCheckUnparseableTrackedFile(t *testing.T) {
	t.Parallel()
	town, gf, bd, _ := trackedConfigRig(t, "events-journal: [\n")

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message+strings.Join(res.Details, "\n"), "alpha") {
		t.Errorf("result = %+v, want the rig named", res)
	}
}

// TestBeadsConfigDriftCheckTrackedFileNotMapping: a tracked config.yaml whose
// top level is not a mapping is an error naming the rig, not a file with no
// settings.
func TestBeadsConfigDriftCheckTrackedFileNotMapping(t *testing.T) {
	t.Parallel()
	town, gf, bd, _ := trackedConfigRig(t, "- events-journal\n")

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message+strings.Join(res.Details, "\n"), "alpha") {
		t.Errorf("result = %+v, want the rig named", res)
	}
}

// TestBeadsConfigDriftCheckNoLiveStore: a rig with a tracked file and no store
// directory has nothing to compare and is an error naming the rig, not a
// setting that matches.
func TestBeadsConfigDriftCheckNoLiveStore(t *testing.T) {
	t.Parallel()
	town, gf, bd, beadsDir := trackedConfigRig(t, "events-journal: false\n")
	if err := os.RemoveAll(beadsDir); err != nil {
		t.Fatal(err)
	}

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message+strings.Join(res.Details, "\n"), "alpha") {
		t.Errorf("result = %+v, want the rig named", res)
	}
}

// TestBeadsConfigDriftCheckStoreUnreadable: a live value bd cannot read is an
// error naming the rig and the key, not a difference and not a match.
func TestBeadsConfigDriftCheckStoreUnreadable(t *testing.T) {
	t.Parallel()
	town, gf, bd, beadsDir := trackedConfigRig(t, "events-journal: false\n")
	bd.db(filepath.Dir(beadsDir)).FailWith("config get events-journal", errors.New("database not found"))

	res := NewBeadsConfigDriftCheck().Run(withGit(bd.ctx(town), gf))
	if res.Status != StatusError {
		t.Fatalf("status = %v, want error: %+v", res.Status, res)
	}
	joined := res.Message + strings.Join(res.Details, "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "events-journal") {
		t.Errorf("result = %+v, want the rig and key named", res)
	}
}

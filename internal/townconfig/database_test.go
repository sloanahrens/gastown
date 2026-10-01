package townconfig

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// liveRigDatabases is each live rig's bd dolt_database. gastown's differs
// from the rig name: it kept the "gt" database through the upgrade.
var liveRigDatabases = map[string]string{"beads": "be", "gastown": "gt", "hm": "hm", "mango": "mango", "om": "om"}

// withBDMetadata gives the town at root the bd workspaces the live town has:
// each rig's metadata.json under mayor/rig/.beads with <rig>/.beads a
// redirect to it, and the town's own .beads naming "hq". Values are fake
// apart from the database names.
func withBDMetadata(t *testing.T, root string) string {
	t.Helper()
	for rig, db := range liveRigDatabases {
		write(t, root, rig+"/mayor/rig/.beads/metadata.json",
			`{"backend":"dolt","database":"dolt","dolt_database":"`+db+`","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`)
		write(t, root, rig+"/.beads/redirect", "mayor/rig/.beads\n")
	}
	write(t, root, ".beads/metadata.json", `{"backend":"dolt","dolt_database":"hq","dolt_mode":"server"}`)
	return root
}

func registryDatabases(t *testing.T, root string) map[string]string {
	t.Helper()
	town, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range town.RigNames() {
		db, err := town.RigDatabase(name)
		if err != nil {
			t.Fatal(err)
		}
		if db != "" {
			out[name] = db
		}
	}
	return out
}

// TestMigrateThenAbsorbRecordsLiveRigDatabases is gt config migrate on the
// live town: the layout moves, then every rig's database name lands in its
// registry entry and metadata.json is left for bd.
func TestMigrateThenAbsorbRecordsLiveRigDatabases(t *testing.T) {
	t.Parallel()
	root := withBDMetadata(t, copyLiveTown(t))
	if got := registryDatabases(t, root); len(got) != 0 {
		t.Fatalf("live registry already names databases: %v", got)
	}
	// Before the registry has the names, readers get metadata.json's.
	if db := DatabaseForBeadsDir(filepath.Join(root, "gastown", "mayor", "rig", ".beads")); db != "gt" {
		t.Errorf("DatabaseForBeadsDir(gastown) before absorb = %q, want gt", db)
	}

	plan, err := AbsorbRigDatabases(root, true)
	if err != nil || len(plan) != len(liveRigDatabases) {
		t.Fatalf("dry run = %+v, %v", plan, err)
	}
	if got := registryDatabases(t, root); len(got) != 0 {
		t.Fatalf("dry run wrote the registry: %v", got)
	}

	if _, err := config.MigrateLayout(root); err != nil {
		t.Fatal(err)
	}
	done, err := AbsorbRigDatabases(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(done, plan) {
		t.Errorf("absorbed %+v, dry run said %+v", done, plan)
	}
	if got := registryDatabases(t, root); !reflect.DeepEqual(got, liveRigDatabases) {
		t.Errorf("registry databases = %v, want %v", got, liveRigDatabases)
	}
	for _, s := range done {
		if s.MetadataFile != s.Rig+"/mayor/rig/.beads/metadata.json" || s.Registry != "" {
			t.Errorf("absorbed %+v", s)
		}
	}
	if db, _ := config.RigMetadataDatabase(root, "gastown"); db != "gt" {
		t.Errorf("metadata.json after absorb names %q, want it kept as gt", db)
	}
	if again, err := AbsorbRigDatabases(root, false); err != nil || len(again) != 0 {
		t.Errorf("second absorb = %+v, %v; want nothing to do", again, err)
	}
}

// TestDatabaseForBeadsDirReadsTheRegistry: once recorded, the registry
// answers for both of a rig's workspace paths, even against a drifted
// metadata.json; anything that is not a registered rig's workspace reads its
// own metadata.json.
func TestDatabaseForBeadsDirReadsTheRegistry(t *testing.T) {
	t.Parallel()
	root := withBDMetadata(t, copyLiveTown(t))
	if _, err := AbsorbRigDatabases(root, false); err != nil {
		t.Fatal(err)
	}
	write(t, root, "om/mayor/rig/.beads/metadata.json", `{"dolt_mode":"server","dolt_database":"om_stale"}`)
	write(t, root, "claude/.beads/metadata.json", `{"dolt_mode":"server","dolt_database":"claude"}`)
	cases := map[string]string{
		"gastown/mayor/rig/.beads": "gt",
		"gastown/.beads":           "gt",
		"om/mayor/rig/.beads":      "om",
		".beads":                   "hq",
		"claude/.beads":            "claude",
		"gastown/crew/x/.beads":    "",
		"gastown/mayor/rig":        "",
	}
	for rel, want := range cases {
		if got := DatabaseForBeadsDir(filepath.Join(root, rel)); got != want {
			t.Errorf("DatabaseForBeadsDir(%s) = %q, want %q", rel, got, want)
		}
	}
	if got := RigDatabaseName(root, "gastown"); got != "gt" {
		t.Errorf("RigDatabaseName(gastown) = %q", got)
	}
	if got := RigDatabaseName(root, "claude"); got != "claude" {
		t.Errorf("RigDatabaseName(claude) = %q; an unregistered rig reads its metadata.json", got)
	}

	states, err := RigDatabaseStates(root)
	if err != nil {
		t.Fatal(err)
	}
	var drifted []string
	for _, s := range states {
		if s.Drifted() {
			drifted = append(drifted, s.Rig)
		}
		if s.Absorbable() {
			t.Errorf("%s still absorbable after absorb: %+v", s.Rig, s)
		}
	}
	if !reflect.DeepEqual(drifted, []string{"om"}) {
		t.Errorf("drifted rigs = %v, want [om]", drifted)
	}
	// Absorb never overwrites a recorded name.
	if again, err := AbsorbRigDatabases(root, false); err != nil || len(again) != 0 {
		t.Errorf("absorb over drift = %+v, %v", again, err)
	}
	if got := registryDatabases(t, root)["om"]; got != "om" {
		t.Errorf("registry om = %q after absorb over drift", got)
	}
}

// TestAbsorbOnAFiveFileTownWritesRigsJSON: absorb works before the layout
// move too, through the rigs.json writer.
func TestAbsorbOnAFiveFileTownWritesRigsJSON(t *testing.T) {
	t.Parallel()
	root := withBDMetadata(t, copyLiveTown(t))
	if _, err := AbsorbRigDatabases(root, false); err != nil {
		t.Fatal(err)
	}
	rc, err := config.LoadRigsConfig(filepath.Join(root, FileRigs))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Rigs["gastown"].DoltDatabase != "gt" {
		t.Errorf("rigs.json gastown = %+v", rc.Rigs["gastown"])
	}
	if r, err := config.DetectLayout(root); err != nil || r.Layout != config.LayoutFiveFile {
		t.Errorf("layout after absorb = %+v, %v; absorb must not move files", r, err)
	}
}

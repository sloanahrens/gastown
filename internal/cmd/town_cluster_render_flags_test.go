package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// renderFlagsFixture is a live render's flags with a distinct value in every
// string field and a mixture of true and false booleans, so a record that
// swaps, drops or duplicates two fields fails the round trip rather than
// matching by accident. The six booleans cannot all differ in one fixture, so
// TestRenderFlagsBooleansRoundTripDistinctly gives each its own run.
func renderFlagsFixture() clusterRenderFlags {
	return clusterRenderFlags{
		dir:                   "/repo/gastown",
		out:                   "/repo/manifests",
		namespace:             "gt-control",
		rehearsal:             false,
		cpu:                   "4",
		memory:                "8Gi",
		hostGateway:           "192.168.64.1",
		confirmStopped:        true,
		withHelper:            true,
		proxyImage:            "registry.example.test/gastown/proxy@sha256:" + strings.Repeat("1", 64),
		polecatImage:          "registry.example.test/gastown/polecat@sha256:" + strings.Repeat("2", 64),
		configDir:             "/host/.config/gt",
		openShellConfigDir:    "/host/.config/openshell",
		forgeHost:             "forge.internal.test:3000",
		doltBackupHostPath:    "/host/dolt-backup",
		dashboard:             true,
		dashboardClaudeDir:    "/host/claude-dashboard",
		bdGatewayURL:          "http://127.0.0.1:8080",
		daemonClaudeDir:       "/host/claude-daemon",
		tracker:               true,
		trackerDSNSecret:      "tracker-dsn",
		trackerBackup:         true,
		trackerBackupImage:    "pg@sha256:" + strings.Repeat("3", 64),
		daemonBdGatewayURL:    "http://127.0.0.1:8081",
		daemonBdGatewayURLSet: true,
	}
}

// TestRenderFlagsSaveLoadRoundTrip: the file holds every flag of the render, so
// a replay renders the pod the recorded run rendered.
func TestRenderFlagsSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deploy", "values", "render-flags.json")
	want := renderFlagsFixture()
	if err := saveRenderFlags(path, want); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	got, ok, err := loadRenderFlags(path)
	if err != nil {
		t.Fatalf("loadRenderFlags: %v", err)
	}
	if !ok {
		t.Fatal("loadRenderFlags reported no record after a save")
	}
	if got != want {
		t.Errorf("loaded flags = %+v, want the saved %+v", got, want)
	}
}

// renderFlagBool is one boolean flag of the render: its field name and the
// accessors a test drives it by. The flag struct's fields are unexported, so a
// reflection loop cannot set them.
type renderFlagBool struct {
	name string
	get  func(clusterRenderFlags) bool
	set  func(*clusterRenderFlags, bool)
}

// renderFlagBooleans lists every boolean flag of the render, each with the
// accessors that name it. TestRenderFlagsBooleansNamingIsComplete fails until a
// newly added boolean has a row here.
func renderFlagBooleans() []renderFlagBool {
	return []renderFlagBool{
		{"rehearsal", func(f clusterRenderFlags) bool { return f.rehearsal }, func(f *clusterRenderFlags, v bool) { f.rehearsal = v }},
		{"confirmStopped", func(f clusterRenderFlags) bool { return f.confirmStopped }, func(f *clusterRenderFlags, v bool) { f.confirmStopped = v }},
		{"withHelper", func(f clusterRenderFlags) bool { return f.withHelper }, func(f *clusterRenderFlags, v bool) { f.withHelper = v }},
		{"dashboard", func(f clusterRenderFlags) bool { return f.dashboard }, func(f *clusterRenderFlags, v bool) { f.dashboard = v }},
		{"tracker", func(f clusterRenderFlags) bool { return f.tracker }, func(f *clusterRenderFlags, v bool) { f.tracker = v }},
		{"trackerBackup", func(f clusterRenderFlags) bool { return f.trackerBackup }, func(f *clusterRenderFlags, v bool) { f.trackerBackup = v }},
	}
}

// TestRenderFlagsBooleansNamingIsComplete: the rows above name every boolean
// flag the record holds and nothing else, so the distinctness test cannot leave
// a newly added boolean covered only by the shared fixture.
// daemonBdGatewayURLSet has no row: it is derived on load from the recorded
// URL, which the table already carries, rather than held by the file.
func TestRenderFlagsBooleansNamingIsComplete(t *testing.T) {
	t.Parallel()
	flagType := reflect.TypeOf(clusterRenderFlags{})
	recordType := reflect.TypeOf(renderFlagsRecord{})
	// want is the boolean flags the record holds, so the ones the file carries
	// and a round trip can distinguish.
	want := make(map[string]bool)
	for flagName, recName := range renderFlagRecordNames {
		flag, ok := flagType.FieldByName(flagName)
		if !ok || flag.Type.Kind() != reflect.Bool {
			continue
		}
		if rec, ok := recordType.FieldByName(recName); ok && rec.Type.Kind() == reflect.Bool {
			want[flagName] = true
		}
	}
	rows := renderFlagBooleans()
	named := make(map[string]bool, len(rows))
	for _, b := range rows {
		if named[b.name] {
			t.Errorf("renderFlagBooleans lists %s twice", b.name)
		}
		named[b.name] = true
		if !want[b.name] {
			t.Errorf("renderFlagBooleans names %s, which is not a boolean flag the record holds", b.name)
		}
	}
	for name := range want {
		if !named[name] {
			t.Errorf("clusterRenderFlags.%s is a recorded boolean with no row in renderFlagBooleans", name)
		}
	}
}

// TestRenderFlagsBooleansRoundTripDistinctly gives each boolean flag its own
// run: that flag true and every other boolean false. The shared fixture holds
// six booleans with two values, so a record that confuses two of them compares
// equal there; here a boolean that is dropped, read from another flag's field
// or written to another flag's field loads with the wrong value and fails.
func TestRenderFlagsBooleansRoundTripDistinctly(t *testing.T) {
	t.Parallel()
	bools := renderFlagBooleans()
	for _, b := range bools {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			f := renderFlagsFixture()
			for _, other := range bools {
				other.set(&f, false)
			}
			b.set(&f, true)
			path := filepath.Join(t.TempDir(), "render-flags.json")
			if err := saveRenderFlags(path, f); err != nil {
				t.Fatalf("saveRenderFlags: %v", err)
			}
			got, _, err := loadRenderFlags(path)
			if err != nil {
				t.Fatalf("loadRenderFlags: %v", err)
			}
			if !b.get(got) {
				t.Errorf("%s loaded false, want the true it was saved with", b.name)
			}
			for _, other := range bools {
				if other.name != b.name && other.get(got) {
					t.Errorf("%s loaded true when only %s was recorded, so the record confuses the two", other.name, b.name)
				}
			}
		})
	}
}

// TestRenderFlagsEmptyDaemonGatewayURLLoadsUnset: the file holds the daemon
// gateway URL and not whether the flag was named, so an empty URL loads with
// the "named" bit clear: the flag was never given, and the render's refusal for
// a named-but-empty value does not fire on a replay of that record.
func TestRenderFlagsEmptyDaemonGatewayURLLoadsUnset(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	f := renderFlagsFixture()
	f.daemonBdGatewayURL = ""
	f.daemonBdGatewayURLSet = false
	if err := saveRenderFlags(path, f); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	got, _, err := loadRenderFlags(path)
	if err != nil {
		t.Fatalf("loadRenderFlags: %v", err)
	}
	if got.daemonBdGatewayURL != "" || got.daemonBdGatewayURLSet {
		t.Errorf("daemon gateway URL/Set = %q/%t, want empty and unset", got.daemonBdGatewayURL, got.daemonBdGatewayURLSet)
	}
}

// TestRenderFlagsSaveLeavesNoTempFile guards the temp-file half of the atomic
// write: the values directory holds the record and nothing else.
func TestRenderFlagsSaveLeavesNoTempFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := saveRenderFlags(filepath.Join(dir, "render-flags.json"), renderFlagsFixture()); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "render-flags.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("the values directory holds %v, want only render-flags.json", names)
	}
}

// TestRenderFlagsSaveReplacesTheFile: the write renames a new file over the
// old one instead of rewriting it in place, so a reader never sees a partial
// record.
func TestRenderFlagsSaveReplacesTheFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	first := renderFlagsFixture()
	first.cpu = "1"
	if err := saveRenderFlags(path, first); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	second := renderFlagsFixture()
	second.cpu = "2"
	if err := saveRenderFlags(path, second); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the record was rewritten in place; an atomic write replaces it by a rename")
	}
	got, _, err := loadRenderFlags(path)
	if err != nil {
		t.Fatalf("loadRenderFlags: %v", err)
	}
	if got.cpu != "2" {
		t.Errorf("loaded cpu = %q, want the second save's %q", got.cpu, "2")
	}
}

// TestRenderFlagsSaveRefusesWhenTheDirectoryCannotBeMade: a path under a
// regular file has no directory to write into, and the error comes back rather
// than a silent no-op.
func TestRenderFlagsSaveRefusesWhenTheDirectoryCannotBeMade(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "deploy", "values", "render-flags.json")
	err := saveRenderFlags(path, renderFlagsFixture())
	if err == nil {
		t.Fatal("saveRenderFlags wrote a record under a regular file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
}

// TestRenderFlagsSaveRefusesWhenThePathIsADirectory: the rename onto a path a
// directory occupies fails, so the record cannot replace it and the error names
// the path instead of leaving the old contents to pass as the new record.
func TestRenderFlagsSaveRefusesWhenThePathIsADirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := saveRenderFlags(path, renderFlagsFixture())
	if err == nil {
		t.Fatal("saveRenderFlags replaced a directory with the record")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
}

// TestRenderFlagsMissingFileIsNoneRecorded: no live render has recorded its
// flags yet, which is not a failure.
func TestRenderFlagsMissingFileIsNoneRecorded(t *testing.T) {
	t.Parallel()
	got, ok, err := loadRenderFlags(filepath.Join(t.TempDir(), "render-flags.json"))
	if err != nil {
		t.Fatalf("loadRenderFlags on a missing file: %v", err)
	}
	if ok {
		t.Error("loadRenderFlags reported a record for a missing file")
	}
	if got != (clusterRenderFlags{}) {
		t.Errorf("loaded flags = %+v, want the zero flags", got)
	}
}

// TestRenderFlagsRefusesAnUnreadableFile: a record that exists and cannot be
// read is an error, never an absent record.
func TestRenderFlagsRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	_, ok, err := loadRenderFlags(path)
	if err == nil {
		t.Fatal("loadRenderFlags on an unreadable path succeeded")
	}
	if ok {
		t.Error("loadRenderFlags reported a record for an unreadable path")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
}

// TestRenderFlagsRefusesAnUnknownField: a field this build does not know would
// be dropped by the replay, so the record is refused and the path is named.
func TestRenderFlagsRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.WriteFile(path, []byte(`{"dir": "/repo/gastown", "no_such_flag": "x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := loadRenderFlags(path)
	if err == nil {
		t.Fatal("loadRenderFlags accepted a file with an unknown field")
	}
	if ok {
		t.Error("loadRenderFlags reported a record for a file it refused")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
	if !strings.Contains(err.Error(), "no_such_flag") {
		t.Errorf("error %q does not name the unknown field", err)
	}
}

// TestRenderFlagsRefusesMalformedJSON: a file that does not decode is refused
// with the path, not read as a partial flag set.
func TestRenderFlagsRefusesMalformedJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.WriteFile(path, []byte(`{"dir": `), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := loadRenderFlags(path)
	if err == nil {
		t.Fatal("loadRenderFlags accepted a truncated file")
	}
	if ok {
		t.Error("loadRenderFlags reported a record for a file it refused")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
}

// TestRenderFlagsRefusesATrailingValue: a second JSON value after the record is
// refused, so the file cannot hold a record the loader silently ignores.
func TestRenderFlagsRefusesATrailingValue(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.WriteFile(path, []byte(`{"dir": "/repo/gastown"}{"dir": "/other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := loadRenderFlags(path)
	if err == nil {
		t.Fatal("loadRenderFlags accepted a file with two JSON values")
	}
	if ok {
		t.Error("loadRenderFlags reported a record for a file it refused")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
}

// TestRenderFlagsRefusesNull: a file holding null is refused rather than read
// as a record with every flag empty.
func TestRenderFlagsRefusesNull(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.WriteFile(path, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := loadRenderFlags(path)
	if err == nil {
		t.Fatal("loadRenderFlags accepted a file holding null")
	}
	if ok {
		t.Error("loadRenderFlags reported a record for a file it refused")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %s", err, path)
	}
}

// TestRenderFlagsPathIsBesideTheImageValues: the record sits in the directory
// the running values file sits in, both under the town's gastown directory, so
// a live render's flags and its pair are found together.
func TestRenderFlagsPathIsBesideTheImageValues(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	got := renderFlagsPathIn(town)
	want := filepath.Join(town, "gastown", "deploy", "values", "render-flags.json")
	if got != want {
		t.Errorf("renderFlagsPathIn = %s, want %s", got, want)
	}
	if valuesDir := filepath.Dir(imagevalues.PathIn(filepath.Join(town, "gastown"))); filepath.Dir(got) != valuesDir {
		t.Errorf("the record's directory is %s, want the values file's %s", filepath.Dir(got), valuesDir)
	}
}

// renderFlagRecordNames pairs each field of clusterRenderFlags with the
// renderFlagsRecord field that holds it. The flag struct's fields are
// unexported so it can be a default value verbatim, and the record's are
// exported so encoding/json can marshal them; this table is where the two
// names meet, and a flag absent from it fails TestRenderFlagsRecordCoversTheFlagStruct.
var renderFlagRecordNames = map[string]string{
	"dir":                    "Dir",
	"out":                    "Out",
	"namespace":              "Namespace",
	"rehearsal":              "Rehearsal",
	"cpu":                    "CPU",
	"memory":                 "Memory",
	"hostGateway":            "HostGateway",
	"confirmStopped":         "ConfirmStopped",
	"withHelper":             "WithHelper",
	"proxyImage":             "ProxyImage",
	"polecatImage":           "PolecatImage",
	"configDir":              "ConfigDir",
	"openShellConfigDir":     "OpenShellConfigDir",
	"forgeHost":              "ForgeHost",
	"doltBackupHostPath":     "DoltBackupHostPath",
	"dashboard":              "Dashboard",
	"dashboardClaudeDir":     "DashboardClaudeDir",
	"bdGatewayURL":           "BdGatewayURL",
	"bdGatewayTokenSecret":   "BdGatewayTokenSecret",
	"bdGatewayMaxConcurrent": "BdGatewayMaxConcurrent",
	"daemonClaudeDir":        "DaemonClaudeDir",
	"tracker":                "Tracker",
	"trackerDSNSecret":       "TrackerDSNSecret",
	"trackerBackup":          "TrackerBackup",
	"trackerBackupImage":     "TrackerBackupImage",
	"daemonBdGatewayURL":     "DaemonBdGatewayURL",
}

// TestRenderFlagsRecordCoversTheFlagStruct: every field of the render's flag
// struct is a field of the record, of the same type, or is named here as
// deliberately not recorded. A flag added to the command fails this test until
// it is recorded, so a replay cannot silently drop it.
func TestRenderFlagsRecordCoversTheFlagStruct(t *testing.T) {
	t.Parallel()
	// notRecorded names the flag-struct fields the file does not hold, each
	// with the reason it need not.
	notRecorded := map[string]string{
		"daemonBdGatewayURLSet": "derived: whether the flag was named, which a non-empty recorded URL already answers on load",
	}
	flagType := reflect.TypeOf(clusterRenderFlags{})
	recordType := reflect.TypeOf(renderFlagsRecord{})
	recorded := make(map[string]bool, len(renderFlagRecordNames))
	for i := 0; i < flagType.NumField(); i++ {
		flag := flagType.Field(i)
		if _, ok := notRecorded[flag.Name]; ok {
			continue
		}
		recName, ok := renderFlagRecordNames[flag.Name]
		if !ok {
			t.Errorf("clusterRenderFlags.%s is neither in renderFlagRecordNames nor named as deliberately not recorded, so a replay would drop it", flag.Name)
			continue
		}
		recorded[recName] = true
		rec, ok := recordType.FieldByName(recName)
		if !ok {
			t.Errorf("renderFlagRecordNames pairs clusterRenderFlags.%s with renderFlagsRecord.%s, which does not exist", flag.Name, recName)
			continue
		}
		if rec.Type != flag.Type {
			t.Errorf("renderFlagsRecord.%s has type %s, want the flag's %s", recName, rec.Type, flag.Type)
		}
		if tag := rec.Tag.Get("json"); tag == "" || tag == "-" {
			t.Errorf("renderFlagsRecord.%s has json tag %q, so it would not be written", recName, tag)
		}
	}
	for name := range notRecorded {
		if _, ok := flagType.FieldByName(name); !ok {
			t.Errorf("notRecorded names %s, which clusterRenderFlags no longer has", name)
		}
	}
	// Every record field is a flag's, so the file holds nothing the command
	// cannot take back.
	for i := 0; i < recordType.NumField(); i++ {
		if name := recordType.Field(i).Name; !recorded[name] {
			t.Errorf("renderFlagsRecord.%s holds no clusterRenderFlags field, so it is written and never replayed", name)
		}
	}
}

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// renderFlagsFixture is a live render's flags with a distinct value in every
// string field and a mixture of true and false booleans, so a record that
// swaps, drops or duplicates two fields fails the round trip rather than
// matching by accident. A fixture cannot give every boolean a distinct value,
// so TestRenderFlagsBooleansRoundTripDistinctly gives each its own run.
func renderFlagsFixture() clusterRenderFlags {
	return clusterRenderFlags{
		dir:                     "/repo/gastown",
		out:                     "/repo/manifests",
		namespace:               "gt-control",
		rehearsal:               false,
		cpu:                     "4",
		memory:                  "8Gi",
		hostGateway:             "192.168.64.1",
		confirmStopped:          true,
		withHelper:              true,
		proxyImage:              "registry.example.test/gastown/proxy@sha256:" + strings.Repeat("1", 64),
		gatewayProxyCPU:         "500m",
		registryProxyCPU:        "250m",
		polecatImage:            "registry.example.test/gastown/polecat@sha256:" + strings.Repeat("2", 64),
		configDir:               "/host/.config/gt",
		openShellConfigDir:      "/host/.config/openshell",
		forgeHost:               "forge.internal.test:3000",
		doltBackupHostPath:      "/host/dolt-backup",
		townStateBackupHostPath: "/host/gt-backups/town-state",
		cloudReportsHostPath:    "/host/gt-cloud/reports",
		cloudReleasesHostPath:   "/host/gt-cloud/releases",
		dashboard:               true,
		bdGatewayURL:            "http://127.0.0.1:8080",
		bdGatewayReplicas:       2,
		tracker:                 true,
		trackerDSNSecret:        "tracker-dsn",
		trackerBackup:           true,
		trackerBackupImage:      "pg@sha256:" + strings.Repeat("3", 64),
		daemonBdGatewayURL:      "http://127.0.0.1:8081",
		daemonBdGatewayURLSet:   true,
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
		{"bdGatewayServicePolicyApplied", func(f clusterRenderFlags) bool { return f.bdGatewayServicePolicyApplied }, func(f *clusterRenderFlags, v bool) { f.bdGatewayServicePolicyApplied = v }},
		{"tracker", func(f clusterRenderFlags) bool { return f.tracker }, func(f *clusterRenderFlags, v bool) { f.tracker = v }},
		{"trackerBackup", func(f clusterRenderFlags) bool { return f.trackerBackup }, func(f *clusterRenderFlags, v bool) { f.trackerBackup = v }},
		{"includeStatic", func(f clusterRenderFlags) bool { return f.includeStatic }, func(f *clusterRenderFlags, v bool) { f.includeStatic = v }},
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
// its booleans with only two values, so a record that confuses two of them
// compares equal there; here a boolean that is dropped, read from another
// flag's field or written to another flag's field loads with the wrong value
// and fails.
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
	"dir":                           "Dir",
	"out":                           "Out",
	"namespace":                     "Namespace",
	"rehearsal":                     "Rehearsal",
	"cpu":                           "CPU",
	"memory":                        "Memory",
	"hostGateway":                   "HostGateway",
	"confirmStopped":                "ConfirmStopped",
	"withHelper":                    "WithHelper",
	"proxyImage":                    "ProxyImage",
	"gatewayProxyCPU":               "GatewayProxyCPU",
	"registryProxyCPU":              "RegistryProxyCPU",
	"polecatImage":                  "PolecatImage",
	"configDir":                     "ConfigDir",
	"openShellConfigDir":            "OpenShellConfigDir",
	"forgeHost":                     "ForgeHost",
	"doltBackupHostPath":            "DoltBackupHostPath",
	"townStateBackupHostPath":       "TownStateBackupHostPath",
	"cloudReportsHostPath":          "CloudReportsHostPath",
	"cloudReleasesHostPath":         "CloudReleasesHostPath",
	"dashboard":                     "Dashboard",
	"bdGatewayServicePolicyApplied": "BdGatewayServicePolicyApplied",
	"bdGatewayURL":                  "BdGatewayURL",
	"bdGatewayTokenSecret":          "BdGatewayTokenSecret",
	"bdGatewayMaxConcurrent":        "BdGatewayMaxConcurrent",
	"bdGatewayReplicas":             "BdGatewayReplicas",
	"bdGatewayCPURequest":           "BdGatewayCPURequest",
	"tracker":                       "Tracker",
	"trackerDSNSecret":              "TrackerDSNSecret",
	"trackerBackup":                 "TrackerBackup",
	"trackerBackupImage":            "TrackerBackupImage",
	"daemonBdGatewayURL":            "DaemonBdGatewayURL",
	"includeStatic":                 "IncludeStatic",
	"cluster":                       "Cluster",
	"sbxManifests":                  "SbxManifests",
	"sbxChart":                      "SbxChart",
	"sbxValues":                     "SbxValues",
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
		"dashboardClaudeDir":    "retired: the flag is refused, so a replay never carries it (gt-ja7otj)",
		"daemonClaudeDir":       "retired: the flag is refused, so a replay never carries it (gt-ja7otj)",
		"helm":                  "gt-sbx input: the record holds it once the gt-sbx record bead lands (gt-2bj447)",
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

// TestClusterRenderRelayCPUFlags holds --gateway-proxy-cpu and
// --registry-proxy-cpu to their Values fields and the recorded flags: each flag
// defaults to empty, a named value is trimmed into Values, a recorded share
// replays unless its flag is named, and a record written before the keys exist
// loads both empty.
func TestClusterRenderRelayCPUFlags(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"gateway-proxy-cpu", "registry-proxy-cpu"} {
		if flag := townClusterRenderCmd.Flags().Lookup(name); flag == nil || flag.DefValue != "" {
			t.Errorf("--%s = %v, want a flag whose default is the empty string", name, flag)
		}
	}
	f := clusterRenderFixture(t)
	f.gatewayProxyCPU, f.registryProxyCPU = " 500m ", "1"
	d := clusterRenderDepsAt(t, clusterRenderTestTown(t), io.Discard)
	v, _, _, err := clusterRenderValues(d, f, d.findTown)
	if err != nil {
		t.Fatalf("clusterRenderValues: %v", err)
	}
	if v.GatewayProxyCPU != "500m" || v.RegistryProxyCPU != "1" {
		t.Errorf("Values carries %q/%q, want 500m/1", v.GatewayProxyCPU, v.RegistryProxyCPU)
	}

	// The fixture records both shares; one row per named-flag combination.
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := saveRenderFlags(path, renderFlagsFixture()); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	loaded, ok, err := loadRenderFlags(path)
	if err != nil || !ok {
		t.Fatalf("loadRenderFlags = (%t, %v), want the record", ok, err)
	}
	for _, tc := range []struct {
		named                     []string
		wantGateway, wantRegistry string
	}{
		{nil, "500m", "250m"},
		{[]string{"gateway-proxy-cpu"}, "1", "250m"},
		{[]string{"registry-proxy-cpu"}, "500m", "2"},
	} {
		given := clusterRenderFlags{gatewayProxyCPU: "1", registryProxyCPU: "2"}
		replay, err := given.withRecorded(loaded, func(flag string) bool { return slices.Contains(tc.named, flag) })
		if err != nil {
			t.Fatalf("withRecorded %v: %v", tc.named, err)
		}
		if replay.gatewayProxyCPU != tc.wantGateway || replay.registryProxyCPU != tc.wantRegistry {
			t.Errorf("replay with named %v carries %q/%q, want %q/%q",
				tc.named, replay.gatewayProxyCPU, replay.registryProxyCPU, tc.wantGateway, tc.wantRegistry)
		}
	}
	old := filepath.Join(t.TempDir(), "render-flags.json")
	if err := os.WriteFile(old, []byte(`{"dir": "/repo/gastown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadRenderFlags(old)
	if err != nil || !ok || got.gatewayProxyCPU != "" || got.registryProxyCPU != "" {
		t.Fatalf("the old record = %+v, ok %t, err %v; want both shares empty", got, ok, err)
	}
}

// TestTownClusterRenderFlagsRefuseBadCloudPaths drives each cloud directory
// flag through the render: the flag value reaches Values and the daemon
// render's Build refuses a relative or unclean path there, with an error naming
// the Values field it landed in, so a bad flag value never reaches the pod spec
// (gt-tnl0g8, the validator of gt-9gcfj3).
func TestTownClusterRenderFlagsRefuseBadCloudPaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, value, field, other string
		reports                   bool
	}{
		{"a relative reports path", "gt-cloud/reports", "Values.CloudReportsHostPath", "Values.CloudReleasesHostPath", true},
		{"an unclean releases path", "/Users/Shared/gt-cloud/../reports", "Values.CloudReleasesHostPath", "Values.CloudReportsHostPath", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			if tc.reports {
				f.cloudReportsHostPath = tc.value
			} else {
				f.cloudReleasesHostPath = tc.value
			}
			town := clusterRenderTestTown(t)
			v, _, _, err := clusterRenderValues(clusterRenderDepsAt(t, town, io.Discard), f,
				func() (string, error) { return town, nil })
			if err != nil {
				t.Fatalf("clusterRenderValues: %v", err)
			}
			if _, err := k8sdaemon.Build(v); err == nil {
				t.Fatalf("Build rendered the bad path %q into a pod spec", tc.value)
			} else if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("Build refused %q with %q, want an error naming %s", tc.value, err, tc.field)
			} else if strings.Contains(err.Error(), tc.other) {
				t.Errorf("Build refused %q with %q, which names the other field %s", tc.value, err, tc.other)
			}
		})
	}
}

// TestTownClusterRenderFlagsCloudHostPathsAreRegistered: both cloud directory
// flags exist and are empty by default, so a render that names neither mounts
// nothing (gt-s8yd4x).
func TestTownClusterRenderFlagsCloudHostPathsAreRegistered(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cloud-reports-host-path", "cloud-releases-host-path"} {
		if flag := townClusterRenderCmd.Flags().Lookup(name); flag == nil || flag.DefValue != "" {
			t.Errorf("--%s = %v, want a flag whose default is the empty string", name, flag)
		}
	}
}

// TestTownClusterRenderFlagsCarryTheCloudHostPaths holds the two cloud
// directory flags to the places DoltBackupHostPath reaches, one table of rows over
// one render: Values (in a live and in a rehearsal render, since the daemon mounts
// both directories in either), the recorded-flags mirror and a replay from it. The
// two mirror keys are written only when set, so a render with neither flag writes
// the mirror file it wrote before: its mirror has no cloud_ key at all, and its
// Values leave both fields empty, the input the daemon package's own goldens pin
// (gt-s8yd4x).
func TestTownClusterRenderFlagsCarryTheCloudHostPaths(t *testing.T) {
	t.Parallel()
	const reports = "/Users/Shared/gt-cloud/reports"
	const releases = "/Users/Shared/gt-cloud/releases"
	for _, tc := range []struct {
		name              string
		rehearsal         bool
		reports, releases string
	}{
		{"both, a live render", false, reports, releases},
		{"both, a rehearsal", true, reports, releases},
		{"only the reports directory", false, reports, ""},
		{"neither: the mirror and Values are as before", false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := clusterRenderFixture(t)
			f.rehearsal, f.cloudReportsHostPath, f.cloudReleasesHostPath = tc.rehearsal, tc.reports, tc.releases
			town := clusterRenderTestTown(t)
			got, _, _, err := clusterRenderValues(clusterRenderDepsAt(t, town, io.Discard), f,
				func() (string, error) { return town, nil })
			if err != nil {
				t.Fatalf("clusterRenderValues: %v", err)
			}
			if got.CloudReportsHostPath != tc.reports || got.CloudReleasesHostPath != tc.releases {
				t.Errorf("Values carries %q/%q, want %q/%q", got.CloudReportsHostPath, got.CloudReleasesHostPath, tc.reports, tc.releases)
			}
			// The manifests the Values render: the cloud mounts appear exactly when a
			// path is set, so a render with neither flag carries no cloud volume.
			built := got
			if tc.rehearsal {
				built.Namespace = "gt-rehearsal-test" // a rehearsal names its own namespace; the flags carry none here
			}
			manifests, err := k8sdaemon.Build(built)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			var rendered strings.Builder
			if err := k8sdaemon.Render(&rendered, manifests); err != nil {
				t.Fatalf("Render: %v", err)
			}
			if mounted := strings.Contains(rendered.String(), k8sdaemon.CloudReportsVolume); mounted != (tc.reports != "") {
				t.Errorf("the rendered manifests mount %s = %t for the reports path %q", k8sdaemon.CloudReportsVolume, mounted, tc.reports)
			}
			path := filepath.Join(t.TempDir(), "render-flags.json")
			m := renderFlagsFixture()
			m.cloudReportsHostPath, m.cloudReleasesHostPath = tc.reports, tc.releases
			if err := saveRenderFlags(path, m); err != nil {
				t.Fatalf("saveRenderFlags: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"cloud_reports_host_path": tc.reports, "cloud_releases_host_path": tc.releases} {
				if listed := strings.Contains(string(data), `"`+key+`"`); listed != (want != "") {
					t.Errorf("the mirror lists %s = %t for the value %q:\n%s", key, listed, want, data)
				}
			}
			loaded, ok, err := loadRenderFlags(path)
			if err != nil || !ok {
				t.Fatalf("loadRenderFlags = %v, ok = %t", err, ok)
			}
			replayed, err := clusterRenderFlags{}.withRecorded(loaded, func(string) bool { return false })
			if err != nil {
				t.Fatalf("withRecorded: %v", err)
			}
			if replayed.cloudReportsHostPath != tc.reports || replayed.cloudReleasesHostPath != tc.releases {
				t.Errorf("the replayed flags = %q/%q, want %q/%q", replayed.cloudReportsHostPath, replayed.cloudReleasesHostPath, tc.reports, tc.releases)
			}
		})
	}
}

// TestClusterRenderTownStateBackupHostPath holds --town-state-backup-host-path
// to the daemon's own town-state-backup volume: the flag is registered empty by
// default, its value reaches Values.TownStateBackupHostPath, the rendered
// Deployment carries the Directory hostPath volume and its mount at that path
// (read-write live, read-only in a rehearsal), a render without the flag is the
// same Deployment with that volume and mount taken out, and a relative path is
// refused through the render by the daemon's own
// validateTownStateBackupHostPath (gt-3c4ppq).
func TestClusterRenderTownStateBackupHostPath(t *testing.T) {
	t.Parallel()
	const path = "/Users/Shared/gt-backups/town-state"

	// render draws a Deployment with f changed by mutate; a rehearsal names its
	// own namespace and carries no forge host.
	render := func(t *testing.T, mutate func(*clusterRenderFlags)) k8sdaemon.Deployment {
		t.Helper()
		f := clusterRenderFixture(t)
		mutate(&f)
		if f.rehearsal {
			f.forgeHost, f.confirmStopped = "", false
			f.namespace = "gt-rehearsal-test"
		}
		var out bytes.Buffer
		if err := runClusterRender(clusterRenderTestDeps(t, &out), f); err != nil {
			t.Fatalf("render: %v", err)
		}
		return renderedDeployment(t, f.out, "-deployment-daemon.yaml")
	}
	// hasMount asserts d carries the volume and the mount at path, read-only when
	// wantReadOnly.
	hasMount := func(t *testing.T, d k8sdaemon.Deployment, wantReadOnly bool) {
		t.Helper()
		pod := d.Spec.Template.Spec
		if len(pod.Containers) == 0 {
			t.Fatalf("the render has no container: %+v", d)
		}
		i := slices.IndexFunc(pod.Volumes, func(v k8sdaemon.Volume) bool { return v.Name == k8sdaemon.TownStateBackupVolume })
		if i < 0 || pod.Volumes[i].HostPath == nil || pod.Volumes[i].HostPath.Path != path || pod.Volumes[i].HostPath.Type != "Directory" {
			t.Errorf("volume %s = %+v, want a Directory hostPath %s", k8sdaemon.TownStateBackupVolume, pod.Volumes, path)
		}
		j := slices.IndexFunc(pod.Containers[0].VolumeMounts, func(m k8sdaemon.VolumeMount) bool { return m.MountPath == path })
		if j < 0 || pod.Containers[0].VolumeMounts[j].Name != k8sdaemon.TownStateBackupVolume || pod.Containers[0].VolumeMounts[j].ReadOnly != wantReadOnly {
			t.Errorf("the mount at %s = %+v, want volume %s readOnly %t", path, pod.Containers[0].VolumeMounts, k8sdaemon.TownStateBackupVolume, wantReadOnly)
		}
	}

	t.Run("the flag is registered, empty by default", func(t *testing.T) {
		t.Parallel()
		if flag := townClusterRenderCmd.Flags().Lookup("town-state-backup-host-path"); flag == nil || flag.DefValue != "" {
			t.Errorf("--town-state-backup-host-path = %v, want a flag whose default is the empty string", flag)
		}
	})

	t.Run("a live render mounts the host directory read-write", func(t *testing.T) {
		t.Parallel()
		hasMount(t, render(t, func(f *clusterRenderFlags) { f.townStateBackupHostPath = path }), false)
	})

	t.Run("a rehearsal mounts it read-only", func(t *testing.T) {
		t.Parallel()
		hasMount(t, render(t, func(f *clusterRenderFlags) { f.rehearsal, f.townStateBackupHostPath = true, path }), true)
	})

	t.Run("a render without the flag is unchanged", func(t *testing.T) {
		t.Parallel()
		live, flagless := render(t, func(f *clusterRenderFlags) { f.townStateBackupHostPath = path }), render(t, func(*clusterRenderFlags) {})
		// hasMount makes the comparison meaningful: without the flagged render's
		// volume there would be nothing to take out, and both would match.
		hasMount(t, live, false)
		pod := &live.Spec.Template.Spec
		pod.Volumes = slices.DeleteFunc(pod.Volumes, func(v k8sdaemon.Volume) bool { return v.Name == k8sdaemon.TownStateBackupVolume })
		pod.Containers[0].VolumeMounts = slices.DeleteFunc(pod.Containers[0].VolumeMounts, func(m k8sdaemon.VolumeMount) bool { return m.MountPath == path })
		if !reflect.DeepEqual(live, flagless) {
			t.Errorf("a render without the flag differs from the flagged render minus its town-state entries:\nflagless: %+v\nflagged, town-state entries removed: %+v", flagless, live)
		}
	})

	t.Run("a relative path is refused through the render", func(t *testing.T) {
		t.Parallel()
		f := clusterRenderFixture(t)
		f.townStateBackupHostPath = "gt-backups/town-state"
		var out bytes.Buffer
		err := runClusterRender(clusterRenderTestDeps(t, &out), f)
		if err == nil || !strings.Contains(err.Error(), "Values.TownStateBackupHostPath") {
			t.Fatalf("a render of the relative path %q = %v, want a refusal naming Values.TownStateBackupHostPath", f.townStateBackupHostPath, err)
		}
		if names := dirNames(t, f.out); len(names) != 0 {
			t.Errorf("the refused render wrote %v", names)
		}
	})
}

// TestClusterRenderRecordedFlagsTownStateBackupHostPath holds
// --town-state-backup-host-path to the recorded flags: --record-flags writes
// the path into the file, and a later --use-recorded render of that town, with
// the flag unnamed, mounts the recorded host directory (gt-6iyjli).
func TestClusterRenderRecordedFlagsTownStateBackupHostPath(t *testing.T) {
	t.Parallel()
	const path = "/Users/Shared/gt-backups/town-state"
	f := clusterRenderFixture(t)
	f.townStateBackupHostPath = path
	town := clusterRenderTestTown(t)
	d := clusterRenderDepsAt(t, town, io.Discard)
	d.record = clusterRenderRecordOptions{record: true}
	if err := runClusterRender(d, f); err != nil {
		t.Fatalf("the recording render: %v", err)
	}

	// The save: the file carries the key, rather than the loader answering empty
	// from a default no file holds.
	recordPath := renderFlagsPathIn(town)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(renderedText(t, recordPath)), &raw); err != nil {
		t.Fatalf("decoding the recorded flags: %v", err)
	}
	if got, want := string(raw["town_state_backup_host_path"]), `"`+path+`"`; got != want {
		t.Fatalf("the record's town_state_backup_host_path = %s, want %s: --record-flags dropped --town-state-backup-host-path", got, want)
	}

	// The replay names only a fresh --out; the path is the record's, and the
	// rendered Deployment mounts the recorded host directory.
	replay := clusterRenderFlags{out: filepath.Join(t.TempDir(), "replay")}
	d = clusterRenderDepsAt(t, town, io.Discard)
	d.record = clusterRenderRecordOptions{
		use:   true,
		named: func(flag string) bool { return flag == "out" },
	}
	if err := runClusterRender(d, replay); err != nil {
		t.Fatalf("the replay: %v", err)
	}
	pod := renderedDeployment(t, replay.out, "-deployment-daemon.yaml").Spec.Template.Spec
	i := slices.IndexFunc(pod.Volumes, func(v k8sdaemon.Volume) bool { return v.Name == k8sdaemon.TownStateBackupVolume })
	if i < 0 || pod.Volumes[i].HostPath == nil || pod.Volumes[i].HostPath.Path != path {
		t.Fatalf("the replay's %s volume is not a hostPath %s: --use-recorded dropped the recorded path", k8sdaemon.TownStateBackupVolume, path)
	}
}

// TestClusterRenderFlagsRecordTheSbxClusterAndPaths holds the gt-sbx cluster and
// its three paths to the record: --record-flags saves them under their keys, and
// a replay that names none of them restores them, so a later drift or plan run
// reads the same sandbox render back (gt-u4q8if).
func TestClusterRenderFlagsRecordTheSbxClusterAndPaths(t *testing.T) {
	t.Parallel()
	want := renderFlagsFixture()
	want.cluster = clusterSbx
	want.sbxManifests = "/host/agent-sandbox/manifest.yaml"
	want.sbxChart = "/host/charts/agent-sandbox"
	want.sbxValues = "/host/gt-sbx/values.yaml"
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := saveRenderFlags(path, want); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the recorded flags: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decoding the recorded flags: %v", err)
	}
	for key, value := range map[string]string{
		"cluster":       clusterSbx,
		"sbx_manifests": want.sbxManifests,
		"sbx_chart":     want.sbxChart,
		"sbx_values":    want.sbxValues,
	} {
		if got, wantJSON := string(raw[key]), `"`+value+`"`; got != wantJSON {
			t.Errorf("the record's %s = %s, want %s: --record-flags dropped it", key, got, wantJSON)
		}
	}

	got, ok, err := loadRenderFlags(path)
	if err != nil || !ok {
		t.Fatalf("loadRenderFlags = (%t, %v), want the record", ok, err)
	}
	none := func(string) bool { return false }
	replay, err := clusterRenderFlags{}.withRecorded(got, none)
	if err != nil {
		t.Fatalf("withRecorded: %v", err)
	}
	if replay.cluster != want.cluster || replay.sbxManifests != want.sbxManifests ||
		replay.sbxChart != want.sbxChart || replay.sbxValues != want.sbxValues {
		t.Errorf("replay restored cluster %q and paths %q %q %q, want %q and %q %q %q",
			replay.cluster, replay.sbxManifests, replay.sbxChart, replay.sbxValues,
			want.cluster, want.sbxManifests, want.sbxChart, want.sbxValues)
	}
}

// TestClusterRenderFlagsOldRecordLoadsAsControl: a record written before the
// cluster and the sandbox paths were recorded loads, its own flags keep their
// meaning, and it reads as the control render with no sandbox paths, so an old
// file cannot restore a gt-sbx run (gt-u4q8if).
func TestClusterRenderFlagsOldRecordLoadsAsControl(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	oldShape := `{"dir": "/repo/gastown", "out": "/repo/manifests", "include_static": true}`
	if err := os.WriteFile(path, []byte(oldShape), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadRenderFlags(path)
	if err != nil {
		t.Fatalf("loadRenderFlags on a record of the old shape: %v", err)
	}
	if !ok {
		t.Fatal("loadRenderFlags reported no record for a file that exists")
	}
	if got.dir != "/repo/gastown" || got.out != "/repo/manifests" || !got.includeStatic {
		t.Errorf("the old record's flags = dir %q out %q includeStatic %t, want the values it holds", got.dir, got.out, got.includeStatic)
	}
	if got.cluster != clusterControl {
		t.Errorf("cluster = %q, want %q: a record with no cluster key is the control render", got.cluster, clusterControl)
	}
	if got.sbxManifests != "" || got.sbxChart != "" || got.sbxValues != "" {
		t.Errorf("sandbox paths = %q %q %q, want none", got.sbxManifests, got.sbxChart, got.sbxValues)
	}
}

// TestClusterRenderFlagsOldRecordReplaysTheControlRender: a record written before
// the cluster was recorded replays through --use-recorded as the control render:
// its own flags come back, and no sandbox path rides along (gt-2bj447).
func TestClusterRenderFlagsOldRecordReplaysTheControlRender(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "render-flags.json")
	oldShape := `{"dir": "/repo/gastown", "out": "/repo/manifests", "cpu": "4", "include_static": true}`
	if err := os.WriteFile(path, []byte(oldShape), 0o644); err != nil {
		t.Fatal(err)
	}
	recorded, ok, err := loadRenderFlags(path)
	if err != nil || !ok {
		t.Fatalf("loadRenderFlags = (%t, %v), want the old record", ok, err)
	}
	none := func(string) bool { return false }
	replay, err := clusterRenderFlags{}.withRecorded(recorded, none)
	if err != nil {
		t.Fatalf("withRecorded on the old record: %v", err)
	}
	if replay.cluster != clusterControl {
		t.Errorf("replay cluster = %q, want %q: a record with no cluster key replays the control render", replay.cluster, clusterControl)
	}
	if replay.sbxManifests != "" || replay.sbxChart != "" || replay.sbxValues != "" {
		t.Errorf("replay sandbox paths = %q %q %q, want none", replay.sbxManifests, replay.sbxChart, replay.sbxValues)
	}
	if replay.dir != "/repo/gastown" || replay.out != "/repo/manifests" || replay.cpu != "4" || !replay.includeStatic {
		t.Errorf("replay flags = dir %q out %q cpu %q includeStatic %t, want the values the old record holds",
			replay.dir, replay.out, replay.cpu, replay.includeStatic)
	}
}

// TestClusterRenderFlagsCommandLineOverridesTheRecordedSbx: a flag named on the
// command line wins over the recorded cluster and sandbox paths, and each flag
// not named is still replayed from the record, one by one (gt-2bj447).
func TestClusterRenderFlagsCommandLineOverridesTheRecordedSbx(t *testing.T) {
	t.Parallel()
	recorded := renderFlagsFixture()
	recorded.cluster = clusterSbx
	recorded.sbxManifests = "/host/recorded/manifest.yaml"
	recorded.sbxChart = "/host/recorded/chart"
	recorded.sbxValues = "/host/recorded/values.yaml"
	path := filepath.Join(t.TempDir(), "render-flags.json")
	if err := saveRenderFlags(path, recorded); err != nil {
		t.Fatalf("saveRenderFlags: %v", err)
	}
	loaded, ok, err := loadRenderFlags(path)
	if err != nil || !ok {
		t.Fatalf("loadRenderFlags = (%t, %v), want the record", ok, err)
	}
	// The values the command line parsed: each differs from the record, so a
	// replay that takes the wrong side of a flag fails on that field.
	given := clusterRenderFlags{
		cluster:      clusterControl,
		sbxManifests: "/cli/manifest.yaml",
		sbxChart:     "/cli/chart",
		sbxValues:    "/cli/values.yaml",
	}
	cases := []struct {
		name                                  string
		named                                 []string
		wantCluster, wantManifests, wantChart string
		wantValues                            string
	}{
		{
			name:          "nothing named replays the record",
			named:         nil,
			wantCluster:   recorded.cluster,
			wantManifests: recorded.sbxManifests,
			wantChart:     recorded.sbxChart,
			wantValues:    recorded.sbxValues,
		},
		{
			name:          "cluster named keeps the command line's cluster",
			named:         []string{"cluster"},
			wantCluster:   given.cluster,
			wantManifests: recorded.sbxManifests,
			wantChart:     recorded.sbxChart,
			wantValues:    recorded.sbxValues,
		},
		{
			name:          "one path named keeps its command line value and not the others",
			named:         []string{"sbx-chart"},
			wantCluster:   recorded.cluster,
			wantManifests: recorded.sbxManifests,
			wantChart:     given.sbxChart,
			wantValues:    recorded.sbxValues,
		},
		{
			name:          "every sandbox flag named keeps the command line",
			named:         []string{"cluster", "sbx-manifests", "sbx-chart", "sbx-values"},
			wantCluster:   given.cluster,
			wantManifests: given.sbxManifests,
			wantChart:     given.sbxChart,
			wantValues:    given.sbxValues,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			named := func(flag string) bool { return slices.Contains(tc.named, flag) }
			replay, err := given.withRecorded(loaded, named)
			if err != nil {
				t.Fatalf("withRecorded: %v", err)
			}
			if replay.cluster != tc.wantCluster || replay.sbxManifests != tc.wantManifests ||
				replay.sbxChart != tc.wantChart || replay.sbxValues != tc.wantValues {
				t.Errorf("replay cluster %q paths %q %q %q, want %q and %q %q %q",
					replay.cluster, replay.sbxManifests, replay.sbxChart, replay.sbxValues,
					tc.wantCluster, tc.wantManifests, tc.wantChart, tc.wantValues)
			}
		})
	}
}

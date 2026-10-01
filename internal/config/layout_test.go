package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fiveFileTown copies the live town's town-level files into a temp town.
func fiveFileTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"mayor/town.json", "mayor/rigs.json", "mayor/overseer.json", "mayor/daemon.json", "settings/config.json", "settings/escalation.json"} {
		data, err := os.ReadFile(filepath.Join(liveTown, rel))
		if err != nil {
			t.Fatal(err)
		}
		writeTownFile(t, root, rel, string(data))
	}
	return root
}

func writeTownFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// townValues is everything a caller reads through the legacy paths.
type townValues struct {
	Town       *TownConfig
	Rigs       *RigsConfig
	Overseer   *OverseerConfig
	Daemon     *DaemonPatrolConfig
	Escalation *EscalationConfig
	Settings   *TownSettings
}

func readTownValues(t *testing.T, root string) townValues {
	t.Helper()
	var v townValues
	var err error
	if v.Town, err = LoadTownConfig(filepath.Join(root, "mayor", "town.json")); err != nil {
		t.Fatal(err)
	}
	if v.Rigs, err = LoadRigsConfig(filepath.Join(root, "mayor", "rigs.json")); err != nil {
		t.Fatal(err)
	}
	if v.Overseer, err = LoadOverseerConfig(OverseerConfigPath(root)); err != nil {
		t.Fatal(err)
	}
	if v.Daemon, err = LoadDaemonPatrolConfig(DaemonPatrolConfigPath(root)); err != nil {
		t.Fatal(err)
	}
	if v.Escalation, err = LoadEscalationConfig(EscalationConfigPath(root)); err != nil {
		t.Fatal(err)
	}
	if v.Settings, err = LoadOrCreateTownSettings(TownSettingsPath(root)); err != nil {
		t.Fatal(err)
	}
	// The hosts gain the sections; the comparison is about what the
	// retired paths read.
	v.Town.Registry, v.Town.Overseer = nil, nil
	v.Settings.Daemon, v.Settings.Escalation = nil, nil
	return v
}

func snapshotDir(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrateLayoutRoundTripsTheLiveTown(t *testing.T) {
	t.Parallel()
	root := fiveFileTown(t)
	before := readTownValues(t, root)
	trees := map[string]any{}
	for _, rel := range LegacyConfigFiles() {
		trees[rel] = jsonTree(t, filepath.Join(root, rel))
	}
	if r, err := DetectLayout(root); err != nil || r.Layout != LayoutFiveFile {
		t.Fatalf("DetectLayout before = %+v, %v; want five-file", r, err)
	}

	untouched := snapshotDir(t, root)
	plan, err := PlanLayoutMigration(root)
	if err != nil {
		t.Fatalf("PlanLayoutMigration = %v", err)
	}
	if len(plan) != 4 {
		t.Fatalf("plan = %v, want 4 moves", plan)
	}
	for _, step := range plan {
		if step.Action != ActionMove {
			t.Errorf("step %v, want a move", step)
		}
	}
	if !reflect.DeepEqual(snapshotDir(t, root), untouched) {
		t.Fatal("the dry run wrote to the town")
	}

	steps, err := MigrateLayout(root)
	if err != nil {
		t.Fatalf("MigrateLayout = %v", err)
	}
	if !reflect.DeepEqual(steps, plan) {
		t.Errorf("applied %v, planned %v", steps, plan)
	}
	for _, rel := range LegacyConfigFiles() {
		for _, p := range []string{rel, rel + ".lock"} {
			if _, err := os.Stat(filepath.Join(root, p)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s still on disk (%v)", p, err)
			}
		}
	}
	if r, err := DetectLayout(root); err != nil || r.Layout != LayoutTwoFile {
		t.Fatalf("DetectLayout after = %+v, %v; want two-file", r, err)
	}
	// Compared as JSON: raw-message fields keep their key order in the
	// old file and are re-encoded in the section.
	if after := readTownValues(t, root); !reflect.DeepEqual(roundJSON(t, after), roundJSON(t, before)) {
		a, _ := json.Marshal(after)
		b, _ := json.Marshal(before)
		t.Errorf("values changed across the migration:\nbefore %s\nafter  %s", b, a)
	}
	machine := jsonTree(t, filepath.Join(root, MachineConfigFile)).(map[string]any)
	operator := jsonTree(t, filepath.Join(root, OperatorConfigFile)).(map[string]any)
	for rel, host := range map[string]map[string]any{
		"mayor/rigs.json": machine, "mayor/overseer.json": machine,
		"mayor/daemon.json": operator, "settings/escalation.json": operator,
	} {
		key := map[string]string{"mayor/rigs.json": "registry", "mayor/overseer.json": "overseer", "mayor/daemon.json": "daemon", "settings/escalation.json": "escalation"}[rel]
		if !reflect.DeepEqual(roundJSON(t, host[key]), trees[rel]) {
			t.Errorf("%s section %q is not the old file's JSON", rel, key)
		}
	}

	if _, err := MigrateLayout(root); !errors.Is(err, ErrAlreadyMigrated) {
		t.Errorf("second MigrateLayout = %v, want ErrAlreadyMigrated", err)
	}
	if _, err := PlanLayoutMigration(root); !errors.Is(err, ErrAlreadyMigrated) {
		t.Errorf("PlanLayoutMigration on a two-file town = %v, want ErrAlreadyMigrated", err)
	}
}

func roundJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func migratedTown(t *testing.T) string {
	t.Helper()
	root := fiveFileTown(t)
	if _, err := MigrateLayout(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSourcePathNamesTheFileThatHoldsTheContent: a caller that reports which
// file it read must name the host, not the retired path, or the daemon's
// startup line sends an operator to a file nothing reads (gt-y3pgh.12).
func TestSourcePathNamesTheFileThatHoldsTheContent(t *testing.T) {
	t.Parallel()

	twoFile := migratedTown(t)
	daemonPath := DaemonPatrolConfigPath(twoFile)
	got := SourcePath(daemonPath)
	if !strings.HasPrefix(got, filepath.Join(twoFile, "settings", "config.json")) ||
		!strings.Contains(got, `section "daemon"`) {
		t.Errorf("SourcePath(%s) = %q, want the settings host and its daemon section", daemonPath, got)
	}
	fiveFile := fiveFileTown(t)
	if got, want := SourcePath(DaemonPatrolConfigPath(fiveFile)), DaemonPatrolConfigPath(fiveFile); got != want {
		t.Errorf("SourcePath on the five-file layout = %q, want the retired path itself", got)
	}
	if got := SourcePath(TownSettingsPath(twoFile)); got != TownSettingsPath(twoFile) {
		t.Errorf("SourcePath(settings/config.json) = %q, want the path itself", got)
	}
	if got := SourcePath(DaemonPatrolConfigPath(filepath.Join(t.TempDir(), "empty"))); !strings.HasSuffix(got, "daemon.json") {
		t.Errorf("SourcePath with no hosts = %q, want the path itself", got)
	}
}

func TestWritesFollowRetiredFilesToTheirSections(t *testing.T) {
	t.Parallel()
	root := migratedTown(t)
	rigsPath := filepath.Join(root, "mayor", "rigs.json")
	err := UpdateConfigJSON(rigsPath, 0o600, func(rc *RigsConfig, exists bool) error {
		if !exists {
			t.Error("registry section read as absent")
		}
		rc.Rigs["newrig"] = RigEntry{GitURL: "https://example.com/newrig.git", AddedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), BeadsConfig: &BeadsConfig{Prefix: "nr"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rigsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writing the registry recreated mayor/rigs.json (%v)", err)
	}
	rc, err := LoadRigsConfig(rigsPath)
	if err != nil || rc.Rigs["newrig"].BeadsConfig == nil || len(rc.Rigs) != 6 {
		t.Fatalf("LoadRigsConfig after write = %+v, %v", rc, err)
	}
	town, err := LoadTownConfig(filepath.Join(root, "mayor", "town.json"))
	if err != nil || town.Name != "gt" || town.Dolt == nil || town.Dolt.Port != 3307 {
		t.Fatalf("town identity after a registry write = %+v, %v", town, err)
	}

	// A file the old town never had goes to its section too.
	daemonPath := DaemonPatrolConfigPath(root)
	if err := os.Remove(filepath.Join(root, OperatorConfigFile)); err != nil {
		t.Fatal(err)
	}
	if err := SaveDaemonPatrolConfig(daemonPath, &DaemonPatrolConfig{Type: "daemon-patrol-config", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(daemonPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saving the daemon config recreated mayor/daemon.json (%v)", err)
	}
	s, err := LoadOrCreateTownSettings(TownSettingsPath(root))
	if err != nil || s.Daemon == nil || s.Daemon.Type != "daemon-patrol-config" {
		t.Fatalf("settings daemon section = %+v, %v", s, err)
	}
	if _, err := LoadEscalationConfig(EscalationConfigPath(root)); !errors.Is(err, ErrNotFound) {
		t.Errorf("absent escalation section = %v, want ErrNotFound", err)
	}
}

// TestHostSavesKeepSections: a TownConfig or TownSettings built before the
// migration has no sections; saving it must not drop them.
func TestHostSavesKeepSections(t *testing.T) {
	t.Parallel()
	root := migratedTown(t)
	town := &TownConfig{Type: "town", Version: 2, Name: "renamed"}
	if err := SaveTownConfig(filepath.Join(root, "mayor", "town.json"), town); err != nil {
		t.Fatal(err)
	}
	if rc, err := LoadRigsConfig(filepath.Join(root, "mayor", "rigs.json")); err != nil || len(rc.Rigs) != 5 {
		t.Fatalf("registry after SaveTownConfig = %+v, %v", rc, err)
	}
	if err := SaveTownSettings(TownSettingsPath(root), NewTownSettings()); err != nil {
		t.Fatal(err)
	}
	if d, err := LoadDaemonPatrolConfig(DaemonPatrolConfigPath(root)); err != nil || d.Patrols == nil {
		t.Fatalf("daemon section after SaveTownSettings = %+v, %v", d, err)
	}
}

func TestMigrateLayoutFinishesAPartialMigration(t *testing.T) {
	t.Parallel()
	root := migratedTown(t)
	// A crash before the removal leaves the old file beside its section.
	original, err := os.ReadFile(filepath.Join(liveTown, "mayor", "rigs.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeTownFile(t, root, "mayor/rigs.json", string(original))
	if r, err := DetectLayout(root); err != nil || r.Layout != LayoutPartial || !reflect.DeepEqual(r.Left, []string{"mayor/rigs.json"}) {
		t.Fatalf("DetectLayout = %+v, %v; want partial with rigs.json left", r, err)
	}
	steps, err := MigrateLayout(root)
	if err != nil {
		t.Fatalf("MigrateLayout = %v", err)
	}
	if len(steps) != 1 || steps[0].Action != ActionRetire {
		t.Fatalf("steps = %v, want one retire", steps)
	}
	if r, _ := DetectLayout(root); r.Layout != LayoutTwoFile {
		t.Errorf("layout after finishing = %s", r.Layout)
	}
}

func TestMigrateLayoutRefusesAndWritesNothing(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T) string{
		"old file differs from its section": func(t *testing.T) string {
			root := migratedTown(t)
			writeTownFile(t, root, "mayor/rigs.json", `{"version":1,"rigs":{}}`)
			return root
		},
		"old file does not parse": func(t *testing.T) string {
			root := fiveFileTown(t)
			writeTownFile(t, root, "mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"bogus":true}`)
			return root
		},
		"host does not parse": func(t *testing.T) string {
			root := fiveFileTown(t)
			writeTownFile(t, root, "settings/config.json", `{"type":`)
			return root
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := setup(t)
			before := withoutLocks(snapshotDir(t, root))
			if _, err := MigrateLayout(root); err == nil {
				t.Fatal("MigrateLayout succeeded")
			}
			if after := withoutLocks(snapshotDir(t, root)); !reflect.DeepEqual(after, before) {
				t.Error("a refused migration changed the town")
			}
		})
	}
}

func TestMigrateLayoutWithoutOptionalFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTownFile(t, root, "mayor/town.json", `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`)
	steps, err := MigrateLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Action != ActionCreate {
		t.Fatalf("steps = %v, want one registry create", steps)
	}
	if rc, err := LoadRigsConfig(filepath.Join(root, "mayor", "rigs.json")); err != nil || rc.Rigs == nil || len(rc.Rigs) != 0 {
		t.Fatalf("empty registry = %+v, %v", rc, err)
	}
	if _, err := os.Stat(filepath.Join(root, OperatorConfigFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("settings/config.json written with nothing to hold (%v)", err)
	}
	if r, _ := DetectLayout(root); r.Layout != LayoutTwoFile {
		t.Errorf("layout = %s", r.Layout)
	}
}

func withoutLocks(files map[string]string) map[string]string {
	for rel := range files {
		if strings.HasSuffix(rel, ".lock") {
			delete(files, rel)
		}
	}
	return files
}

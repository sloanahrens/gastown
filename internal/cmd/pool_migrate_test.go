package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// migrateNow is the clock every migration test stamps its backups with, so the
// backup name is known without a sleep.
var migrateNow = time.Date(2026, 10, 19, 14, 30, 5, 0, time.UTC)

const migrateStamp = "20261019T143005Z-pool-migrate"

// writeMigrateTown writes body as the settings file of a new town and returns
// the town root and the file's path.
func writeMigrateTown(t *testing.T, body []byte) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = config.TownSettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: a temp town this test wrote
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// jsonTreeOf decodes JSON into a generic tree, numbers kept as text, so two
// files compare by value and not by key order or indentation.
func jsonTreeOf(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	return v
}

func backupsOf(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// The three old shapes each convert to the golden file, a second run finds
// nothing to do and leaves the file as the first wrote it, and the converted
// file loads through the strict loaders (gt-mukgk).
func TestPoolMigrateGoldens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// wantLines are fragments each reported change must carry.
		wantLines []string
	}{
		{"two-seat", []string{
			`polecat_pool.agent "claude-haiku", max_seats 3 -> polecat_pool.seats.claude-haiku.cap 3`,
			`spec_dispatch hooked_agent "claude-sonnet", max_hooked 2 -> polecat_pool.seats.claude-sonnet.cap 2`,
			"removed role_agents.polecat (claude-haiku)",
		}},
		{"pro-seat", []string{
			`polecat_pool.agent "claude-sonnet", max_seats 2 -> polecat_pool.seats.claude-sonnet.cap 2`,
			"removed polecat_pool.pro_agent (claude-opus)",
			"removed polecat_pool.pro_max (1)",
			"removed polecat_pool.pro_label (needs-pro)",
		}},
		{"deep-review-pins", []string{
			`removed agent claude-sonnet from scheduled sling "deep-review-slice"`,
			`removed agent claude-opus from scheduled sling "deep-review-risk"`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in, err := os.ReadFile(filepath.Join("testdata", "pool_migrate", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "pool_migrate", tc.name+".want.json"))
			if err != nil {
				t.Fatal(err)
			}
			root, path := writeMigrateTown(t, in)

			var out bytes.Buffer
			if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
				t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
			}
			for _, line := range tc.wantLines {
				if !strings.Contains(out.String(), line) {
					t.Errorf("the report has no %q:\n%s", line, out.String())
				}
			}
			if got := jsonTreeOf(t, readFile(t, path)); !reflect.DeepEqual(got, jsonTreeOf(t, want)) {
				t.Errorf("the migrated file differs from the golden:\ngot  %s\nwant %s", readFile(t, path), want)
			}

			// The original is kept beside the file under the migration's name.
			backups := backupsOf(t, path)
			if len(backups) != 1 || !strings.HasSuffix(backups[0], ".bak-"+migrateStamp) {
				t.Fatalf("backups = %v, want one ending .bak-%s", backups, migrateStamp)
			}
			if !bytes.Equal(readFile(t, backups[0]), in) {
				t.Error("the backup is not the file as it was")
			}

			// The result loads: the old keys are gone and the seats resolve.
			ts, err := config.LoadOrCreateTownSettings(path)
			if err != nil {
				t.Fatalf("the migrated file does not load: %v", err)
			}
			if _, err := config.Seats(ts, config.AgentResolver(root, ts)); err != nil {
				t.Errorf("the migrated seats do not load: %v", err)
			}

			// A second run changes nothing, writes no second backup.
			after := readFile(t, path)
			out.Reset()
			if err := runPoolMigrate(&out, root, false, migrateNow.Add(time.Hour)); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if strings.TrimSpace(out.String()) != "nothing to migrate" {
				t.Errorf("second run printed %q, want nothing to migrate", out.String())
			}
			if !bytes.Equal(readFile(t, path), after) {
				t.Error("a second run changed the file")
			}
			if n := len(backupsOf(t, path)); n != 1 {
				t.Errorf("%d backups after a second run, want the first run's alone", n)
			}
		})
	}
}

// --dry-run prints each change and writes nothing: not the file, not a backup.
func TestPoolMigrateDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	in, err := os.ReadFile(filepath.Join("testdata", "pool_migrate", "two-seat.json"))
	if err != nil {
		t.Fatal(err)
	}
	root, path := writeMigrateTown(t, in)

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, true, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate --dry-run: %v", err)
	}
	for _, want := range []string{"polecat_pool.seats.claude-haiku.cap 3", "dry run: nothing written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry run did not print %q:\n%s", want, out.String())
		}
	}
	if !bytes.Equal(readFile(t, path), in) {
		t.Error("the dry run changed the file")
	}
	if backups := backupsOf(t, path); len(backups) != 0 {
		t.Errorf("the dry run left backups: %v", backups)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("the dry run left a temp file: %s", e.Name())
		}
	}
}

// A town on the five-file layout keeps the patrol document in mayor/daemon.json:
// the second seat's keys convert into the settings file and leave the patrol
// file, and each file that changed is backed up (gt-mukgk).
func TestPoolMigrateFiveFileTown(t *testing.T) {
	t.Parallel()
	root, settings := writeMigrateTown(t, []byte(`{"type":"town-settings","version":1,`+
		`"polecat_pool":{"agent":"claude-haiku","max_seats":1}}`))
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	patrolBody := `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,` +
		`"hooked_agent":"claude-sonnet","max_hooked":2,"prefer_hooked":true,"max_per_tick":1},` +
		`"scheduled_slings":{"enabled":true,"entries":[{"name":"r","rig":"gastown","formula":"f","interval":"1h","dispatch":"spec","agent":"claude-opus"}]}}}`
	if err := os.WriteFile(patrol, []byte(patrolBody), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	var ts config.TownSettings
	if err := config.DecodeJSONFile(settings, readFile(t, settings), &ts); err != nil {
		t.Fatalf("the settings file does not load: %v", err)
	}
	if got := ts.PolecatPool.Seats; len(got) != 2 || got["claude-haiku"].Cap != 1 || got["claude-sonnet"].Cap != 2 {
		t.Errorf("seats = %+v, want claude-haiku 1 and claude-sonnet 2\n%s", got, out.String())
	}
	var pc config.DaemonPatrolConfig
	if err := config.DecodeJSONFile(patrol, readFile(t, patrol), &pc); err != nil {
		t.Fatalf("the patrol file does not load: %v", err)
	}
	if sd := pc.Patrols.SpecDispatch; sd == nil || sd.MaxPerTick != 1 {
		t.Errorf("spec_dispatch = %+v, want max_per_tick kept", sd)
	}
	if e := pc.Patrols.ScheduledSlings.Entries[0]; e.Agent != "" {
		t.Errorf("the spec entry kept agent %q", e.Agent)
	}
	for _, path := range []string{settings, patrol} {
		if n := len(backupsOf(t, path)); n != 1 {
			t.Errorf("%s has %d backups, want 1", path, n)
		}
		if !strings.Contains(out.String(), "wrote "+config.SourcePathRel(root, path)) {
			t.Errorf("the report does not say it wrote %s:\n%s", path, out.String())
		}
	}

	out.Reset()
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil || strings.TrimSpace(out.String()) != "nothing to migrate" {
		t.Errorf("second run = %q, %v, want nothing to migrate", out.String(), err)
	}
}

// A town whose only retired keys sit in the separate patrol file leaves the
// settings file untouched and unbacked-up: nothing in it changed.
func TestPoolMigrateOnlyThePatrolFileChanged(t *testing.T) {
	t.Parallel()
	settingsBody := []byte(`{"type":"town-settings","version":1,"polecat_pool":{"seats":{"claude-sonnet":{"cap":2}}}}`)
	root, settings := writeMigrateTown(t, settingsBody)
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patrol, []byte(`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"prefer_hooked":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	if !bytes.Equal(readFile(t, settings), settingsBody) {
		t.Error("the settings file was rewritten though nothing in it changed")
	}
	if n := len(backupsOf(t, settings)); n != 0 {
		t.Errorf("settings has %d backups, want none", n)
	}
	var pc config.DaemonPatrolConfig
	if err := config.DecodeJSONFile(patrol, readFile(t, patrol), &pc); err != nil {
		t.Errorf("the patrol file does not load: %v", err)
	}
}

// A file with nothing to convert, and a town with no settings file, are both
// nothing to migrate and are not an error.
func TestPoolMigrateNothingToMigrate(t *testing.T) {
	t.Parallel()
	seatMap := []byte(`{"type":"town-settings","version":1,"polecat_pool":{"seats":{"claude-sonnet":{"cap":2}}}}`)
	root, path := writeMigrateTown(t, seatMap)
	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil || strings.TrimSpace(out.String()) != "nothing to migrate" {
		t.Errorf("seat map file: %q, %v", out.String(), err)
	}
	if !bytes.Equal(readFile(t, path), seatMap) || len(backupsOf(t, path)) != 0 {
		t.Error("a file with nothing to migrate was touched")
	}

	out.Reset()
	if err := runPoolMigrate(&out, t.TempDir(), false, migrateNow); err != nil || strings.TrimSpace(out.String()) != "nothing to migrate" {
		t.Errorf("no settings file: %q, %v", out.String(), err)
	}
}

// A five-file town with no settings file converts its patrol document and writes
// the settings file its second seat goes in (gt-oh50w).
func TestPoolMigratePatrolOnlyTownWithoutSettingsFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	patrolBody := `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,` +
		`"hooked_agent":"claude-sonnet","max_hooked":2,"max_per_tick":1}}}`
	if err := os.WriteFile(patrol, []byte(patrolBody), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	settings := config.TownSettingsPath(root)
	var ts config.TownSettings
	if err := config.DecodeJSONFile(settings, readFile(t, settings), &ts); err != nil {
		t.Fatalf("the new settings file does not load: %v\n%s", err, out.String())
	}
	if ts.PolecatPool == nil || ts.PolecatPool.Seats["claude-sonnet"].Cap != 2 {
		t.Errorf("settings = %+v, want claude-sonnet seat at cap 2\n%s", ts.PolecatPool, out.String())
	}
	if fi, err := os.Stat(settings); err != nil {
		t.Fatalf("stat %s: %v", settings, err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("new settings file mode = %v, want 0600", fi.Mode().Perm())
	}
	var pc config.DaemonPatrolConfig
	if err := config.DecodeJSONFile(patrol, readFile(t, patrol), &pc); err != nil {
		t.Fatalf("the patrol file still holds the retired keys: %v", err)
	}
	if n := len(backupsOf(t, patrol)); n != 1 {
		t.Errorf("patrol file has %d backups, want 1", n)
	}
	if n := len(backupsOf(t, settings)); n != 0 {
		t.Errorf("settings file has %d backups, want none: there was no file to back up", n)
	}
	if !strings.Contains(out.String(), "new file") {
		t.Errorf("the report does not say the settings file is new:\n%s", out.String())
	}

	out.Reset()
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil || strings.TrimSpace(out.String()) != "nothing to migrate" {
		t.Errorf("second run = %q, %v, want nothing to migrate", out.String(), err)
	}
}

// --dry-run on a town with no settings file prints the conversion and creates
// nothing: no settings file, no backup of the patrol document, no temp file.
func TestPoolMigrateDryRunWithoutSettingsFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	patrolBody := []byte(`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"enabled":true,` +
		`"hooked_agent":"claude-sonnet","max_hooked":2}}}`)
	if err := os.WriteFile(patrol, patrolBody, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, true, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate --dry-run: %v", err)
	}
	for _, want := range []string{"polecat_pool.seats.claude-sonnet.cap 2", "dry run: nothing written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the dry run did not print %q:\n%s", want, out.String())
		}
	}
	settings := config.TownSettingsPath(root)
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Errorf("the dry run created the settings file (stat: %v)\n%s", err, out.String())
	}
	if !bytes.Equal(readFile(t, patrol), patrolBody) {
		t.Error("the dry run changed the patrol file")
	}
	if n := len(backupsOf(t, patrol)); n != 0 {
		t.Errorf("the dry run left %d patrol backups, want none", n)
	}
}

// A patrol document whose conversion the loaders would refuse writes nothing
// when there is no settings file to fall back on: the run fails, no file is
// created, and the patrol document keeps its keys.
func TestPoolMigrateWithoutSettingsFileRefusalWritesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	patrolBody := []byte(`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{` +
		`"hooked_agent":"no-such-agent","max_hooked":2}}}`)
	if err := os.WriteFile(patrol, patrolBody, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runPoolMigrate(&out, root, false, migrateNow)
	if err == nil || !strings.Contains(err.Error(), "would not load") || !strings.Contains(err.Error(), "no-such-agent") {
		t.Fatalf("runPoolMigrate = %v, want a refusal naming the agent", err)
	}
	if _, err := os.Stat(config.TownSettingsPath(root)); !os.IsNotExist(err) {
		t.Errorf("a refused migration created the settings file (stat: %v)", err)
	}
	if !bytes.Equal(readFile(t, patrol), patrolBody) || len(backupsOf(t, patrol)) != 0 {
		t.Error("a refused migration wrote something")
	}
}

// A settings file that cannot be created fails the run before the patrol
// document is written, so the town keeps the keys it had rather than losing
// them to a half-run migration.
func TestPoolMigrateWithoutSettingsFileWriteErrorWritesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	patrolBody := []byte(`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{` +
		`"hooked_agent":"claude-sonnet","max_hooked":2}}}`)
	if err := os.WriteFile(patrol, patrolBody, 0o644); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Dir(config.TownSettingsPath(root))
	if err := os.MkdirAll(settingsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the settings directory: the search bit comes back so the temp tree can be removed
	t.Cleanup(func() { _ = os.Chmod(settingsDir, 0o700) })

	var out bytes.Buffer
	err := runPoolMigrate(&out, root, false, migrateNow)
	if err == nil || !strings.Contains(err.Error(), config.TownSettingsPath(root)) {
		t.Fatalf("runPoolMigrate = %v, want the settings creation to fail\n%s", err, out.String())
	}
	if !bytes.Equal(readFile(t, patrol), patrolBody) || len(backupsOf(t, patrol)) != 0 {
		t.Error("a failed creation wrote the patrol document")
	}
}

// The existence check runs before the file's lock, so a settings file can
// appear in between. The link refuses to replace it and the run converts the
// file that is there, reporting it as an update rather than as a new file.
func TestConvertMigratedSettingsUpdatesAFileThatAppeared(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.json")
	appeared := []byte(`{"type":"town-settings","version":1,"role_agents":{"polecat":"claude-haiku"}}`)
	if err := os.WriteFile(path, appeared, 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := convertMigratedSettings(path, false, func(tree map[string]any) (bool, error) {
		tree["role_agents"] = map[string]any{}
		return true, nil
	})
	if err != nil {
		t.Fatalf("convertMigratedSettings: %v", err)
	}
	if created {
		t.Error("the file was reported as created though it appeared before the link")
	}
	if got := readFile(t, path); !strings.Contains(string(got), `"role_agents": {}`) {
		t.Errorf("the appeared file was not converted:\n%s", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want the appeared file's 0644 kept", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

// A patrol document whose only retired key changes nothing in settings writes
// no settings file: the removal is reported and the patrol file alone is
// rewritten.
func TestPoolMigratePatrolChangeWithoutSettingsCreatesNoSettings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	patrol := config.DaemonPatrolConfigPath(root)
	if err := os.MkdirAll(filepath.Dir(patrol), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patrol, []byte(`{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"prefer_hooked":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(config.TownSettingsPath(root)); !os.IsNotExist(err) {
		t.Errorf("a settings file was created though no seat was converted (stat: %v)\n%s", err, out.String())
	}
	var pc config.DaemonPatrolConfig
	if err := config.DecodeJSONFile(patrol, readFile(t, patrol), &pc); err != nil {
		t.Errorf("the patrol file does not load: %v", err)
	}
}

// A pool that names no agent still holds retired keys, and removing them is a
// change: the run reports it and rewrites the file. Otherwise it says there is
// nothing to migrate and leaves a key the strict loaders refuse, so the daemon
// start keeps failing (gt-mmae7).
func TestPoolMigrateAgentlessPoolKeysAreReportedAndRemoved(t *testing.T) {
	t.Parallel()
	in := []byte(`{"type":"town-settings","version":1,"polecat_pool":{"max_seats":3}}`)
	root, path := writeMigrateTown(t, in)

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "removed polecat_pool.max_seats") {
		t.Fatalf("the report does not name the removed key:\n%s", out.String())
	}
	// The written file leaves the loader that refused it now readable.
	var ts config.TownSettings
	if err := config.DecodeJSONFile(path, readFile(t, path), &ts); err != nil {
		t.Errorf("the rewritten settings file does not load: %v\n%s", err, out.String())
	}
	tree, _ := jsonTreeOf(t, readFile(t, path)).(map[string]any)
	pool, _ := tree["polecat_pool"].(map[string]any)
	if _, present := pool["max_seats"]; present {
		t.Errorf("polecat_pool.max_seats survived:\n%s", readFile(t, path))
	}
	if n := len(backupsOf(t, path)); n != 1 {
		t.Errorf("settings file has %d backups, want 1", n)
	}

	out.Reset()
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil || strings.TrimSpace(out.String()) != "nothing to migrate" {
		t.Errorf("second run = %q, %v, want nothing to migrate", out.String(), err)
	}
}

// A conversion whose result the loaders would refuse writes nothing, so a run
// never leaves a town worse than it found it: an agent the town cannot launch
// names no seat.
func TestPoolMigrateRefusesAResultThatWouldNotLoad(t *testing.T) {
	t.Parallel()
	in := []byte(`{"type":"town-settings","version":1,"polecat_pool":{"agent":"no-such-agent","max_seats":2}}`)
	root, path := writeMigrateTown(t, in)

	var out bytes.Buffer
	err := runPoolMigrate(&out, root, false, migrateNow)
	if err == nil || !strings.Contains(err.Error(), "would not load") || !strings.Contains(err.Error(), "no-such-agent") {
		t.Fatalf("runPoolMigrate = %v, want a refusal naming the agent", err)
	}
	if !bytes.Equal(readFile(t, path), in) || len(backupsOf(t, path)) != 0 {
		t.Error("a refused migration wrote something")
	}
}

// The conversion rules the goldens do not reach: the defaults an unset key
// stood for, the aliases of the older spellings, the seats the dispatcher kept
// one of per agent, and a seat the map already holds.
func TestMigratePoolKeys(t *testing.T) {
	t.Parallel()
	tree := func(t *testing.T, doc string) map[string]any {
		t.Helper()
		m, ok := jsonTreeOf(t, []byte(doc)).(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object", doc)
		}
		return m
	}
	seats := func(m map[string]any) map[string]int {
		out := map[string]int{}
		pool, _ := m["polecat_pool"].(map[string]any)
		for agent, v := range pool["seats"].(map[string]any) {
			n, _ := intOf(v.(map[string]any)["cap"])
			out[agent] = n
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		doc     string
		hooked  *legacySeat
		prefer  bool
		want    map[string]int
		wantErr string
	}{
		{
			name: "an unset max_seats keeps the dispatcher's default of 2",
			doc:  `{"polecat_pool":{"agent":"claude-haiku"}}`,
			want: map[string]int{"claude-haiku": poolMigrateDefaultSeatCap},
		},
		{
			name: "the older spellings read as the keys they became",
			doc:  `{"polecat_pool":{"overflow_agent":"claude-haiku","max_overflow":4}}`,
			want: map[string]int{"claude-haiku": 4},
		},
		{
			name:   "the second seat keeps nothing when the pool names its agent first",
			doc:    `{"polecat_pool":{"agent":"claude-haiku","max_seats":3}}`,
			hooked: &legacySeat{agent: "claude-haiku", cap: 1, from: "second"},
			want:   map[string]int{"claude-haiku": 3},
		},
		{
			name:   "prefer_hooked makes the second seat the one an agent keeps",
			doc:    `{"polecat_pool":{"agent":"claude-haiku","max_seats":3}}`,
			hooked: &legacySeat{agent: "claude-haiku", cap: 1, from: "second"},
			prefer: true,
			want:   map[string]int{"claude-haiku": 1},
		},
		{
			name: "a seat already in the map is never overwritten",
			doc:  `{"polecat_pool":{"agent":"claude-haiku","max_seats":3,"seats":{"claude-haiku":{"cap":9}}}}`,
			want: map[string]int{"claude-haiku": 9},
		},
		{
			name:    "a seats key that is not an object is refused",
			doc:     `{"polecat_pool":{"agent":"claude-haiku","seats":[]}}`,
			wantErr: "polecat_pool.seats is not an object",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := tree(t, tc.doc)
			_, err := migratePoolKeys(m, tc.hooked, tc.prefer)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("migratePoolKeys = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := seats(m); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("seats = %v, want %v", got, tc.want)
			}
			pool := m["polecat_pool"].(map[string]any)
			for _, gone := range []string{"agent", "max_seats", "overflow_agent", "max_overflow"} {
				if _, present := pool[gone]; present {
					t.Errorf("polecat_pool.%s survived", gone)
				}
			}
		})
	}
}

// The line a pool that names no agent reports, whole: the keys in the order
// the migration reads them, each value as JSON, and the advice to add a seat
// only when no seat exists.
func TestMigratePoolKeysAgentlessReportLine(t *testing.T) {
	t.Parallel()
	const remove = "removed polecat_pool."
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "every retired key, in the fixed order, with values as JSON",
			doc:  `{"polecat_pool":{"max_overflow":4,"overflow_agent":"","max_seats":3,"agent":""}}`,
			want: remove + `agent (""), max_seats (3), overflow_agent (""), max_overflow (4): they named no agent, so no seat was made; add one with gt pool set <agent>=<cap>`,
		},
		{
			name: "only the overflow keys",
			doc:  `{"polecat_pool":{"max_overflow":4}}`,
			want: remove + `max_overflow (4): they named no agent, so no seat was made; add one with gt pool set <agent>=<cap>`,
		},
		{
			name: "a seat already in the map is not told to add one",
			doc:  `{"polecat_pool":{"max_overflow":4,"seats":{"claude-haiku":{"cap":2}}}}`,
			want: remove + `max_overflow (4): they named no agent, so no seat was made`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := jsonTreeOf(t, []byte(tc.doc)).(map[string]any)
			lines, err := migratePoolKeys(m, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lines, []string{tc.want}) {
				t.Errorf("lines = %q, want %q", lines, []string{tc.want})
			}
		})
	}
}

// The second seat's keys on their own: max_hooked with no agent runs the
// dispatcher's default agent, an agent with no cap was a closed seat, and keys
// that named no seat are removed without making one.
func TestTakePatrolSeatKeys(t *testing.T) {
	t.Parallel()
	doc := func(spec string) map[string]any {
		m, _ := jsonTreeOf(t, []byte(`{"patrols":{"spec_dispatch":`+spec+`}}`)).(map[string]any)
		return m
	}
	for _, tc := range []struct {
		name       string
		spec       string
		wantAgent  string
		wantCap    int
		wantNoSeat bool
	}{
		{name: "a cap with no agent", spec: `{"max_hooked":2}`, wantAgent: poolMigrateDefaultHookedAgent, wantCap: 2},
		{name: "an agent with no cap is a closed seat, reported and not carried over", spec: `{"hooked_agent":"claude-haiku"}`, wantNoSeat: true},
		{name: "an agent and a cap", spec: `{"hooked_agent":"claude-haiku","max_hooked":3}`, wantAgent: "claude-haiku", wantCap: 3},
		{name: "a zero cap and no agent names no seat", spec: `{"max_hooked":0}`, wantNoSeat: true},
		{name: "prefer_hooked alone names no seat", spec: `{"prefer_hooked":true}`, wantNoSeat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := doc(tc.spec)
			got := takePatrolSeatKeys(m)
			if !got.changed {
				t.Error("a document holding retired keys reported no change")
			}
			if tc.wantNoSeat {
				if got.hooked != nil {
					t.Errorf("hooked = %+v, want none", got.hooked)
				}
			} else if got.hooked == nil || got.hooked.agent != tc.wantAgent || got.hooked.cap != tc.wantCap {
				t.Errorf("hooked = %+v, want %s at cap %d", got.hooked, tc.wantAgent, tc.wantCap)
			}
			spec := m["patrols"].(map[string]any)["spec_dispatch"].(map[string]any)
			if len(spec) != 0 {
				t.Errorf("spec_dispatch keeps %v", spec)
			}
			if again := takePatrolSeatKeys(m); again.changed || again.hooked != nil {
				t.Errorf("a second take found %+v", again)
			}
		})
	}
}

// gt pool migrate is a verb of gt pool with a --dry-run flag.
func TestPoolMigrateCommandIsRegistered(t *testing.T) {
	t.Parallel()
	cmd, _, err := poolCmd.Find([]string{"migrate"})
	if err != nil || cmd != poolMigrateCmd {
		t.Fatalf("gt pool migrate is not registered: %v", err)
	}
	if poolMigrateCmd.Flags().Lookup("dry-run") == nil {
		t.Error("gt pool migrate has no --dry-run flag")
	}
}

// A town whose only word on the polecats' model is role_agents.polecat keeps it:
// it becomes the one seat, so migrating does not move the town onto default_agent.
func TestPoolMigrateRolePolecatWithNoPoolBecomesTheSeat(t *testing.T) {
	t.Parallel()
	root, settings := writeMigrateTown(t, []byte(`{"type":"town-settings","version":1,`+
		`"role_agents":{"polecat":"claude-sonnet","crew":"claude-opus"}}`))

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	var ts config.TownSettings
	if err := config.DecodeJSONFile(settings, readFile(t, settings), &ts); err != nil {
		t.Fatalf("the settings file does not load: %v", err)
	}
	if got := ts.PolecatPool.Seats; len(got) != 1 || got["claude-sonnet"].Cap != poolMigrateDefaultSeatCap {
		t.Errorf("seats = %+v, want claude-sonnet alone at the default cap\n%s", got, out.String())
	}
	if _, kept := ts.RoleAgents["polecat"]; kept || ts.RoleAgents["crew"] != "claude-opus" {
		t.Errorf("role_agents = %v, want polecat gone and crew kept", ts.RoleAgents)
	}
}

// With a pool agent the seat map already names the polecats' model, so
// role_agents.polecat is only removed, as before.
func TestPoolMigrateRolePolecatIsDroppedWhenThePoolHasASeat(t *testing.T) {
	t.Parallel()
	root, settings := writeMigrateTown(t, []byte(`{"type":"town-settings","version":1,`+
		`"role_agents":{"polecat":"claude-opus"},"polecat_pool":{"agent":"claude-sonnet","max_seats":2}}`))

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	var ts config.TownSettings
	if err := config.DecodeJSONFile(settings, readFile(t, settings), &ts); err != nil {
		t.Fatalf("the settings file does not load: %v", err)
	}
	if got := ts.PolecatPool.Seats; len(got) != 1 || got["claude-sonnet"].Cap != 2 {
		t.Errorf("seats = %+v, want claude-sonnet 2 alone", got)
	}
}

// A second seat that was closed (a hooked_agent with no max_hooked) is reported
// and not carried over, so a stale agent name there cannot fail the seat loader
// and block the whole migration.
func TestPoolMigrateClosedStaleHookedAgentDoesNotBlock(t *testing.T) {
	t.Parallel()
	root, settings := writeMigrateTown(t, []byte(`{"type":"town-settings","version":1,`+
		`"polecat_pool":{"agent":"claude-sonnet","max_seats":2},`+
		`"daemon":{"patrols":{"spec_dispatch":{"enabled":true,"hooked_agent":"model-that-no-longer-exists"}}}}`))

	var out bytes.Buffer
	if err := runPoolMigrate(&out, root, false, migrateNow); err != nil {
		t.Fatalf("runPoolMigrate: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "model-that-no-longer-exists") || !strings.Contains(out.String(), "closed") {
		t.Errorf("the dropped closed seat is not reported:\n%s", out.String())
	}
	var ts config.TownSettings
	if err := config.DecodeJSONFile(settings, readFile(t, settings), &ts); err != nil {
		t.Fatalf("the settings file does not load: %v", err)
	}
	if got := ts.PolecatPool.Seats; len(got) != 1 || got["claude-sonnet"].Cap != 2 {
		t.Errorf("seats = %+v, want claude-sonnet 2 alone", got)
	}
}

// A cap that is not a number is refused, not read as absent: reading it as absent
// would write the default cap over what the operator meant, silently.
func TestPoolMigrateRefusesAWrongTypedCap(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"max_seats": `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-sonnet","max_seats":"three"}}`,
		"max_hooked": `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-sonnet","max_seats":2},` +
			`"daemon":{"patrols":{"spec_dispatch":{"hooked_agent":"deepseek-flash","max_hooked":"two"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, settings := writeMigrateTown(t, []byte(body))
			before := readFile(t, settings)
			var out bytes.Buffer
			err := runPoolMigrate(&out, root, false, migrateNow)
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "not a number") {
				t.Fatalf("err = %v, want a refusal naming %s", err, name)
			}
			if !bytes.Equal(before, readFile(t, settings)) {
				t.Error("a refused migration changed the file")
			}
		})
	}
}

// An explicit max_seats of zero is no cap: the migration falls back to
// max_overflow as it does for an absent max_seats, so the operator's cap does
// not shrink to the default of 2.
func TestMigratePoolKeysExplicitZeroMaxSeatsFallsBackToMaxOverflow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		want int
	}{
		{"zero max_seats takes max_overflow", `{"polecat_pool":{"agent":"a","max_seats":0,"max_overflow":5}}`, 5},
		{"negative max_seats takes max_overflow", `{"polecat_pool":{"agent":"a","max_seats":-1,"max_overflow":5}}`, 5},
		{"zero max_seats and no max_overflow keeps the default", `{"polecat_pool":{"agent":"a","max_seats":0}}`, poolMigrateDefaultSeatCap},
		{"zero max_seats and zero max_overflow keeps the default", `{"polecat_pool":{"agent":"a","max_seats":0,"max_overflow":0}}`, poolMigrateDefaultSeatCap},
		{"a positive max_seats wins over max_overflow", `{"polecat_pool":{"agent":"a","max_seats":3,"max_overflow":5}}`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, ok := jsonTreeOf(t, []byte(tc.doc)).(map[string]any)
			if !ok {
				t.Fatalf("%s is not an object", tc.doc)
			}
			if _, err := migratePoolKeys(m, nil, false); err != nil {
				t.Fatal(err)
			}
			pool := m["polecat_pool"].(map[string]any)
			seat := pool["seats"].(map[string]any)["a"].(map[string]any)
			if got, _ := intOf(seat["cap"]); got != tc.want {
				t.Errorf("seats.a.cap = %d, want %d", got, tc.want)
			}
		})
	}
}

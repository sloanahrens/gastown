package townstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoFileMachineJSON is a machine file carrying the rig registry as its
// "registry" section, the two-file layout's marker. There is no mayor/rigs.json
// beside it.
const twoFileMachineJSON = `{"type":"town","version":2,"name":"t","registry":{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/gastown.git"}}}}`

// twoFileSettingsJSON is the settings file carrying the landing worker's rig
// list as its "daemon" section. It names "nowhere", which the registry does not
// hold.
const twoFileSettingsJSON = `{"daemon":{"patrols":{"landing_worker":{"enabled":true,"rigs":["gastown","nowhere"]}}}}`

// writeTwoFileTown lays out a town on the two-file layout: the machine file and
// the settings file, and no retired registry or patrol file. It returns the root.
func writeTwoFileTown(t *testing.T, machineJSON, settingsJSON string) string {
	t.Helper()
	town := t.TempDir()
	for rel, body := range map[string]string{
		"mayor/town.json":      machineJSON,
		"settings/config.json": settingsJSON,
	} {
		path := filepath.Join(town, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// TestDayCheckNamesTheHostSectionOfARetiredRegistry: with the registry hosted in
// the machine file, a landing-list name the registry does not hold is reported
// against the machine file and its registry section. The retired registry file
// is not named, since it does not exist on this town.
func TestDayCheckNamesTheHostSectionOfARetiredRegistry(t *testing.T) {
	t.Parallel()
	town := writeTwoFileTown(t, twoFileMachineJSON, twoFileSettingsJSON)
	host := newFakeHost(gitAnswer(nil))
	rows := CheckGitSpeed(context.Background(), host.env(town))
	unreg := findRow(t, rows, "landing worker rig list: nowhere")
	if unreg.OK {
		t.Fatalf("row = %+v, want a failed row for the unregistered rig", unreg)
	}
	const registry = `mayor/town.json (section "registry")`
	if !strings.Contains(unreg.Result, "(read from "+registry+")") {
		t.Errorf("row = %+v, want the registry reported as %s", unreg, registry)
	}
	if strings.Contains(unreg.Result, "mayor/rigs.json") {
		t.Errorf("row = %+v, want no reference to the retired registry file", unreg)
	}

	_, err := RecordGitBaseline(context.Background(), host.env(town), false)
	if err == nil || !strings.Contains(err.Error(), "(read from "+registry+")") || strings.Contains(err.Error(), "mayor/rigs.json") {
		t.Errorf("RecordGitBaseline error = %v, want the refusal to name the registry as %s", err, registry)
	}
}

// TestDayCheckNamesTheHostSectionOfAnUnreadableRegistry: a registry section the
// machine file holds but that does not parse is reported against the machine
// file and its registry section.
func TestDayCheckNamesTheHostSectionOfAnUnreadableRegistry(t *testing.T) {
	t.Parallel()
	town := writeTwoFileTown(t, `{"type":"town","version":2,"name":"t","registry":"not an object"}`, twoFileSettingsJSON)
	rows := CheckGitSpeed(context.Background(), newFakeHost(gitAnswer(nil)).env(town))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want the one not-run row: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.OK || !strings.HasPrefix(r.Result, "not run: ") {
		t.Fatalf("row = %+v, want a not-run row", r)
	}
	if !strings.Contains(r.Result, `mayor/town.json (section "registry")`) || strings.Contains(r.Result, "mayor/rigs.json") {
		t.Errorf("row = %+v, want the unreadable registry reported against the machine file's registry section", r)
	}
}

// TestDayCheckNamesTheHostSectionOfARetiredPatrolList: with the landing worker's
// rig list hosted in the settings file, an unreadable list is reported against
// that file and its daemon section, never against the retired patrol file.
func TestDayCheckNamesTheHostSectionOfARetiredPatrolList(t *testing.T) {
	t.Parallel()
	town := writeTwoFileTown(t, twoFileMachineJSON, `{"daemon":"not an object"}`)
	host := newFakeHost(gitAnswer(nil))
	rows := CheckGitSpeed(context.Background(), host.env(town))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want the one not-run row: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.OK || !strings.HasPrefix(r.Result, "not run: ") {
		t.Fatalf("row = %+v, want a not-run row", r)
	}
	const settings = `settings/config.json (section "daemon")`
	if !strings.Contains(r.Result, settings) {
		t.Errorf("row = %+v, want the list reported against %s", r, settings)
	}
	if strings.Contains(r.Result, "mayor/daemon.json") {
		t.Errorf("row = %+v, want no reference to the retired patrol file", r)
	}
}

// TestDayCheckKeepsThePlainNameOnAOneFileTown: a town with a real registry file
// and a real patrol file names both by their plain paths, not by a section.
func TestDayCheckKeepsThePlainNameOnAOneFileTown(t *testing.T) {
	t.Parallel()

	t.Run("unregistered rig", func(t *testing.T) {
		t.Parallel()
		town := writeTown(t, townFiles{
			daemonJSON: `{"patrols":{"landing_worker":{"enabled":true,"rigs":["gastown","nowhere"]}}}`,
		})
		rows := CheckGitSpeed(context.Background(), newFakeHost(gitAnswer(nil)).env(town))
		unreg := findRow(t, rows, "landing worker rig list: nowhere")
		if !strings.Contains(unreg.Result, "(read from mayor/rigs.json)") {
			t.Errorf("row = %+v, want the registry reported as the plain mayor/rigs.json", unreg)
		}
	})

	t.Run("unreadable patrol list", func(t *testing.T) {
		t.Parallel()
		town := writeTown(t, townFiles{daemonJSON: "{not json"})
		rows := CheckGitSpeed(context.Background(), newFakeHost(gitAnswer(nil)).env(town))
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want the one not-run row: %+v", len(rows), rows)
		}
		if !strings.Contains(rows[0].Result, "(mayor/daemon.json patrols.landing_worker.rigs)") {
			t.Errorf("row = %+v, want the patrol list reported as the plain mayor/daemon.json", rows[0])
		}
	})
}

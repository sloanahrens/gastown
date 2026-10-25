package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// applySettingsFixture is a settings file with keys the apply must keep.
const applySettingsFixture = `{"type":"town-settings","version":1,"default_agent":"old",` +
	`"role_agents":{"mayor":"keep-me"},"polecat_pool":{"max_total":4}}`

func applyTable(t *testing.T, body string) *Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	writeModelFile(t, path, body)
	table, err := Load(path, testBuiltins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return table
}

func TestApplyRolesWritesTheTablesRolesAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings", "config.json")
	writeModelFile(t, settings, applySettingsFixture)
	table := applyTable(t, `{"default":"sonnet","roles":{"crew":"haiku"}}`)

	written, err := ApplyRoles(settings, "stamp", table)
	if err != nil {
		t.Fatalf("ApplyRoles: %v", err)
	}
	if len(written) != 1 || written[0].Path != settings {
		t.Fatalf("written = %+v, want the settings file", written)
	}
	var doc map[string]any
	data, _ := os.ReadFile(settings)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	roles, _ := doc["role_agents"].(map[string]any)
	if roles["crew"] != "claude-haiku" {
		t.Errorf("role_agents.crew = %v, want claude-haiku", roles["crew"])
	}
	if roles["mayor"] != "keep-me" {
		t.Errorf("role_agents.mayor = %v, want the untouched value", roles["mayor"])
	}
	if doc["default_agent"] != "claude-sonnet" {
		t.Errorf("default_agent = %v, want claude-sonnet", doc["default_agent"])
	}
	if _, ok := doc["polecat_pool"]; !ok {
		t.Error("polecat_pool was dropped by the apply")
	}
}

func TestApplyRolesRefusesAnUnvalidatedTableWritingNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings", "config.json")
	writeModelFile(t, settings, applySettingsFixture)
	before, _ := os.ReadFile(settings)
	table := applyTable(t, `{"roles":{"default":"haiku"}}`)

	if _, err := ApplyRoles(settings, "stamp", table); err == nil || !strings.Contains(err.Error(), "default is not a role") {
		t.Fatalf("ApplyRoles of a table naming a role default = %v, want the default-role refusal", err)
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Errorf("a refused apply changed the settings:\n%s", after)
	}
	if backups, _ := filepath.Glob(settings + ".*.bak"); len(backups) != 0 {
		t.Errorf("a refused apply left a backup: %v", backups)
	}
}

func TestWrittenFailNamesAFileItCannotPutBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	written := Written{{Path: path, Backup: filepath.Join(dir, "missing.bak")}}

	err := written.Fail(errors.New("the write failed"))
	if err == nil || !strings.Contains(err.Error(), "not put back") || !strings.Contains(err.Error(), path) {
		t.Errorf("Fail = %v, want an error naming %s as not put back", err, path)
	}
}

// A second write in the same switch really fails: its file sits in a read-only
// directory, so config.ReplaceFile errors, and the rollback puts the first file
// back as it was (gt-jaagc).
func TestApplyRollsBackWhenASecondWriteReallyFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings", "config.json")
	writeModelFile(t, settings, applySettingsFixture)
	before, _ := os.ReadFile(settings)
	table := applyTable(t, `{"roles":{"crew":"haiku"}}`)

	written, err := ApplyRoles(settings, "stamp", table)
	if err != nil {
		t.Fatalf("ApplyRoles: %v", err)
	}
	if after, _ := os.ReadFile(settings); bytes.Equal(before, after) {
		t.Fatal("the first write did not change the settings, so there is nothing to roll back")
	}

	readOnly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit cleared to refuse the write
	if err := os.Chmod(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: cleanup must restore its search bit
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	second := filepath.Join(readOnly, "reviewer.json")
	secondErr := config.ReplaceFile(second, []byte("{}\n"), 0o600)
	if secondErr == nil {
		t.Fatal("the second write succeeded in a read-only directory, so it did not fail")
	}

	failure := written.Fail(secondErr)
	if !strings.Contains(failure.Error(), "nothing was changed") {
		t.Errorf("Fail = %v, want the rollback reported", failure)
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Errorf("the rollback did not put the first file back:\n%s", after)
	}
}

// A rollback that cannot put one file back still puts back the rest, and names
// every file it could not restore (gt-mgixx).
func TestWrittenFailRestoresTheRestAndNamesEveryStuckFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	okBackup := filepath.Join(dir, "ok.bak")
	if err := os.WriteFile(okBackup, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	stuckA := filepath.Join(dir, "a.json")
	stuckB := filepath.Join(dir, "b.json")
	written := Written{
		{Path: stuckA, Backup: filepath.Join(dir, "a-missing.bak")},
		{Path: ok, Backup: okBackup},
		{Path: stuckB, Backup: filepath.Join(dir, "b-missing.bak")},
	}

	err := written.Fail(errors.New("the write failed"))
	for _, want := range []string{stuckA, stuckB} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Fail = %v, want it to name %s", err, want)
		}
	}
	if data, _ := os.ReadFile(ok); string(data) != "original" {
		t.Errorf("the restorable file = %q, want it put back to original", data)
	}
}

// The stamp becomes part of the backup's name, so one that is not a file name
// element is refused before anything is written (gt-mgixx).
func TestApplyRolesRefusesAStampThatIsNotOneFileNameElement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings", "config.json")
	writeModelFile(t, settings, applySettingsFixture)
	before, _ := os.ReadFile(settings)
	table := applyTable(t, `{"roles":{"crew":"haiku"}}`)

	for _, stamp := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := ApplyRoles(settings, stamp, table); err == nil {
			t.Errorf("ApplyRoles with stamp %q = nil error, want a refusal", stamp)
		}
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Error("a refused stamp changed the settings")
	}
}

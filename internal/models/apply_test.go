package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestWrittenFailPutsBackTheFirstFile(t *testing.T) {
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
	// A later write in the same switch fails: the settings written so far go back.
	failure := written.Fail(errors.New("the reviewer write failed"))
	if !strings.Contains(failure.Error(), "the reviewer write failed") {
		t.Errorf("Fail = %v, want the cause in it", failure)
	}
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Errorf("Fail did not put the settings back:\n%s", after)
	}
}

func TestWrittenFailNamesAFileItCannotPutBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	written := Written{{Path: path, Backup: filepath.Join(dir, "missing.bak")}}

	err := written.Fail(errors.New("the write failed"))
	if err == nil || !strings.Contains(err.Error(), "could not be put back") || !strings.Contains(err.Error(), path) {
		t.Errorf("Fail = %v, want an error naming %s as not put back", err, path)
	}
}

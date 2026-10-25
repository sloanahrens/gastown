package models

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testBuiltins stands in for the profiles the gt command ships: the table's own
// format owns none of them (gt-jaagc).
var testBuiltins = map[string]Profile{
	"haiku":  {Preset: "claude-haiku", Model: "haiku"},
	"sonnet": {Preset: "claude-sonnet", Model: "sonnet"},
	"opus":   {Preset: "claude", Model: "opus"},
}

func writeModelFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A town that never wrote a table declares nothing, and still has the profiles
// gt ships, so a switch needs no hand edit of the file (gt-jaagc).
func TestLoadMissingTableIsEmptyWithTheBuiltins(t *testing.T) {
	t.Parallel()

	table, err := Load(filepath.Join(t.TempDir(), "settings", "models.json"), testBuiltins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if table.Loaded() {
		t.Error("a table that is not there reads as loaded")
	}
	if len(table.Roles) != 0 || table.Default != "" {
		t.Errorf("a table that is not there declares %+v", table)
	}
	for name, preset := range map[string]string{"haiku": "claude-haiku", "sonnet": "claude-sonnet", "opus": "claude"} {
		p, ok := table.ProfileOf(name)
		if !ok || p.Preset != preset {
			t.Errorf("builtin profile %s = %+v, %v; want preset %s", name, p, ok, preset)
		}
	}
}

// The table's format ships no profile of its own: which profiles exist is the
// caller's, so this package names no agent (gt-jaagc).
func TestLoadWithNoBuiltinsHasNoProfiles(t *testing.T) {
	t.Parallel()

	table, err := Load(filepath.Join(t.TempDir(), "models.json"), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if names := table.ProfileNames(); names != "none" {
		t.Errorf("a table loaded with no builtins has profiles %s, want none", names)
	}
}

// The file's entry for a name wins over the builtin of that name, and the
// builtins a file does not name are still there.
func TestLoadMergesTheFileOverTheBuiltins(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "models.json")
	writeModelFile(t, path, `{"profiles":{"haiku":{"preset":"deepseek-flash"},"flash":{"preset":"deepseek-flash"}},
		"default":"flash","roles":{"crew":"haiku"}}`)
	table, err := Load(path, testBuiltins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := table.RolePreset("crew"); got != "deepseek-flash" {
		t.Errorf("crew's preset = %q, want the file's override of haiku", got)
	}
	if _, ok := table.ProfileOf("flash"); !ok {
		t.Error("the file's own profile is not there")
	}
	if p, ok := table.ProfileOf("sonnet"); !ok || p.Preset != "claude-sonnet" {
		t.Errorf("the builtin sonnet = %+v, %v; want it kept under the file", p, ok)
	}
	if table.Default != "flash" {
		t.Errorf("default = %q, want flash", table.Default)
	}
}

// A table nobody can read must not pass for an empty one: an empty table
// declares nothing, and a broken one is the operator's to fix.
func TestLoadRefusesAFileThatDoesNotParse(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`{"roles":`, `{"roles":{"crew":1}}`, `{"default":7}`, `{"profiles":{"haiku":"claude"}}`} {
		path := filepath.Join(t.TempDir(), "models.json")
		writeModelFile(t, path, body)
		if _, err := Load(path, nil); err == nil {
			t.Errorf("Load(%s) = no error, want one", body)
		}
	}
}

// A write owns roles and default and nothing else: a key or a profile field
// this gt does not know survives it (gt-jaagc).
func TestSaveKeepsKeysAndProfileFieldsItDidNotChange(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "settings", "models.json")
	writeModelFile(t, path, `{"version":2,"future":{"kept":true},
		"profiles":{"flash":{"preset":"deepseek-flash","effort":"low"}},"roles":{"crew":"flash"}}`)
	table, err := Load(path, testBuiltins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	table.Roles["witness"] = "sonnet"
	table.Default = "sonnet"
	if err := table.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var doc map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the written file does not parse: %v\n%s", err, b)
	}
	if _, ok := doc["future"]; !ok {
		t.Errorf("a key this gt does not know was dropped:\n%s", b)
	}
	profiles, _ := doc["profiles"].(map[string]any)
	flash, _ := profiles["flash"].(map[string]any)
	if flash["effort"] != "low" {
		t.Errorf("a profile field this gt does not know was dropped:\n%s", b)
	}
	if doc["version"] != float64(2) {
		t.Errorf("version = %v, want the file's 2 kept", doc["version"])
	}
	roles, _ := doc["roles"].(map[string]any)
	if roles["crew"] != "flash" || roles["witness"] != "sonnet" {
		t.Errorf("roles = %v, want the old entry kept and the new one added", roles)
	}
	if doc["default"] != "sonnet" {
		t.Errorf("default = %v, want sonnet", doc["default"])
	}
}

// A new table carries the version its reader expects.
func TestSaveWritesAVersionForANewFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "settings", "models.json")
	table, err := Load(path, testBuiltins)
	if err != nil {
		t.Fatal(err)
	}
	table.Roles["crew"] = "haiku"
	if err := table.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["version"] != float64(1) {
		t.Errorf("a new table has version %v, want 1:\n%s", doc["version"], b)
	}
}

func tableWithRoles(t *testing.T, roles, profiles, def string) *Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	writeModelFile(t, path, `{"default":`+quote(def)+`,"profiles":{`+profiles+`},"roles":{`+roles+`}}`)
	table, err := Load(path, testBuiltins)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return table
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + s + `"`
}

// Every refusal a write depends on: a polecat entry, a default read as a role,
// an unknown role, a profile that is not there, and a profile without what its
// role needs (gt-jaagc, gt-mukgk).
func TestValidateRefusesRolesAWriteMayNotCarry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		roles string
		def   string
		want  string
		err   error
	}{
		{name: "polecat", roles: `"polecat":"haiku"`, want: "polecat is not a table role", err: ErrPolecatRole},
		{name: "default as a role", roles: `"default":"haiku"`, err: ErrDefaultRole},
		{name: "an unknown role", roles: `"harbormaster":"haiku"`, want: `role "harbormaster"`},
		{name: "a missing profile", roles: `"crew":"nosuch"`, want: `no such profile`},
		{name: "a profile without a preset", roles: `"crew":"bodiless"`, want: "names no preset"},
		{name: "a reviewer profile without a model", roles: `"om-review":"bodiless"`, want: "cannot set the reviewer's backend"},
		{name: "a default naming no profile", def: "nosuch", want: "default = \"nosuch\""},
		{name: "a default naming a profile without a preset", def: "bodiless", want: "names no preset"},
	} {
		table := tableWithRoles(t, tc.roles, `"bodiless":{}`, tc.def)
		err := table.Validate()
		if err == nil {
			t.Errorf("%s: Validate = nil, want a refusal", tc.name)
			continue
		}
		if tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.err)
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want it to name %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateAcceptsEveryRoleItMayCarry(t *testing.T) {
	t.Parallel()

	table := tableWithRoles(t,
		`"crew":"haiku","witness":"sonnet","om-review":"sonnet","refinery":"opus"`,
		``, "opus")
	if err := table.Validate(); err != nil {
		t.Errorf("Validate = %v, want a valid table", err)
	}
}

func TestRoleProfileAndPresetFollowTheTablesNames(t *testing.T) {
	t.Parallel()

	table := tableWithRoles(t, `"crew":"haiku"`, ``, "")
	if p, ok := table.RoleProfile("crew"); !ok || p.Preset != "claude-haiku" {
		t.Errorf("crew's profile = %+v, %v; want the builtin haiku", p, ok)
	}
	if _, ok := table.RoleProfile("witness"); ok {
		t.Error("a role the table does not name has a profile")
	}
	if got := table.RolePreset("witness"); got != "" {
		t.Errorf("RolePreset(witness) = %q, want none", got)
	}
	var nilTable *Table
	if got := nilTable.RolePreset("crew"); got != "" {
		t.Errorf("a nil table gives crew %q, want nothing", got)
	}
	if _, ok := nilTable.ProfileOf("haiku"); ok {
		t.Error("a nil table has profiles")
	}
}

// A "roles": null table loads with an empty roles map, so set-role can assign
// into it instead of panicking on a nil map (gt-jaagc).
func TestLoadNullRolesIsAnEmptyTable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"roles":null}`), 0o644); err != nil {
		t.Fatal(err)
	}
	table, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if table.Roles == nil {
		t.Fatal("Roles is nil after a null roles key, want an empty map")
	}
	table.Roles["crew"] = "haiku"
	if table.Roles["crew"] != "haiku" {
		t.Error("the roles map did not keep an assignment")
	}
}

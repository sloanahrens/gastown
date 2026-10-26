package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// modelEnvFor is a town's files with the agent resolver answered yes to every
// preset: these tests are about the files, not about which agents the machine
// running them resolves (gt-jaagc).
func modelEnvFor(town, om string) modelEnv {
	return modelEnv{
		paths:  modelPathsFor(town, om),
		agents: func(string, string) bool { return true },
	}
}

// modelWrite puts a fixture at path.
func modelWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// modelTown is a town whose settings name every key a switch must leave alone:
// another role, the default agent, the seat map and the theme.
func modelTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	modelWrite(t, filepath.Join(town, "settings", "config.json"), `{
  "type": "town-settings",
  "version": 1,
  "cli_theme": "dark",
  "default_agent": "claude-sonnet",
  "role_agents": {"crew": "claude-sonnet", "witness": "claude-opus"},
  "polecat_pool": {"seats": {"deepseek-flash": {"cap": 2}}}
}`)
	return town
}

func modelTablePath(town string) string { return filepath.Join(town, "settings", "models.json") }
func modelSettingsPath(town string) string {
	return filepath.Join(town, "settings", "config.json")
}

func modelPutTable(t *testing.T, town, body string) {
	t.Helper()
	modelWrite(t, modelTablePath(town), body)
}

// modelOMConfig is a reviewer config beside the town, with the keys a switch
// must keep around its backend.
func modelOMConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "om", "config.json")
	modelWrite(t, path, body)
	return path
}

func modelDecode(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s does not parse: %v\n%s", path, err, b)
	}
	return doc
}

// modelRowFields is one show row's cells, requiring the five-column layout.
func modelRowFields(t *testing.T, out, role string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 5 && f[0] == role {
			return f
		}
	}
	t.Fatalf("no %s row in:\n%s", role, out)
	return nil
}

// modelFiles lists the files under a town, so a read can prove it wrote nothing.
func modelFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const modelTableFixture = `{
  "version": 1,
  "default": "sonnet",
  "profiles": {"flash": {"preset": "deepseek-flash", "om_backend": ["~/bin/claude-deepseek-flash", "-p"]}},
  "roles": {"crew": "haiku", "om-review": "sonnet"},
  "future": {"kept": true}
}`

// show prints each role's profile, preset and live setting, and exits 4 while a
// live setting differs from the table — the check a scheduler runs before it
// trusts the table (gt-jaagc).
func TestModelShowPrintsTheTableAndExitsFourOnDrift(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)

	var out bytes.Buffer
	err := runModelShow(&out, modelEnvFor(town, om))
	if code := exitCodeForError(err); code != 4 {
		t.Errorf("show of a drifted town exits %d, want 4:\n%s", code, out.String())
	}
	crew := modelRowFields(t, out.String(), "crew")
	if !reflect.DeepEqual(crew, []string{"crew", "haiku", "claude-haiku", "claude-sonnet", "claude-haiku"}) {
		t.Errorf("crew row = %v, want the table's haiku against the live claude-sonnet", crew)
	}
	if row := modelRowFields(t, out.String(), "om-review"); row[3] != "sonnet" || row[4] != "-" {
		t.Errorf("om-review row = %v, want a live sonnet and no drift", row)
	}
	if !strings.Contains(out.String(), "om backend: /x/claude -p --model sonnet") {
		t.Errorf("show does not print the live om backend it read:\n%s", out.String())
	}

	// The same town once the live settings match is clean.
	modelWrite(t, modelSettingsPath(town), `{"type":"town-settings","version":1,
		"default_agent":"claude-sonnet","role_agents":{"crew":"claude-haiku"}}`)
	out.Reset()
	if err := runModelShow(&out, modelEnvFor(town, om)); err != nil {
		t.Errorf("show of a town that runs its table = %v, want no error:\n%s", err, out.String())
	}
}

// show reads and writes nothing: a check that created the file it checks would
// pass itself (gt-jaagc).
func TestModelShowWritesNothing(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	before := modelFiles(t, town)
	var out bytes.Buffer
	if err := runModelShow(&out, modelEnvFor(town, "")); err != nil {
		t.Errorf("show of a town with no table = %v, want no error:\n%s", err, out.String())
	}
	if after := modelFiles(t, town); !reflect.DeepEqual(before, after) {
		t.Errorf("show wrote %v", after)
	}
	if !strings.Contains(out.String(), "no table yet") {
		t.Errorf("show of a town with no table does not say so:\n%s", out.String())
	}
}

// A live value the town did not write is no value: show reports the missing
// setting rather than the agent a spawn would fall back to.
func TestModelShowReportsNoLiveValueItDidNotFind(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	modelPutTable(t, town, `{"roles":{"crew":"haiku"},"default":"sonnet"}`)
	var out bytes.Buffer
	err := runModelShow(&out, modelEnvFor(town, ""))
	if exitCodeForError(err) != 4 {
		t.Errorf("show of an unapplied table = %d, want 4:\n%s", exitCodeForError(err), out.String())
	}
	if row := modelRowFields(t, out.String(), "crew"); row[3] != "-" || row[4] != "claude-haiku" {
		t.Errorf("crew row = %v, want no live value and the table's preset as drift", row)
	}
	if row := modelRowFields(t, out.String(), "default"); row[3] != "-" {
		t.Errorf("default row = %v, want no live default_agent", row)
	}
}

// A reviewer config nobody can read is drift, not a clean review: the table's
// claim about the reviewer is unverified (gt-jaagc).
func TestModelShowReportsAnUnreadableReviewerConfig(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	modelPutTable(t, town, `{"roles":{"om-review":"sonnet"}}`)
	om := modelOMConfig(t, `{"backend":`)

	var out bytes.Buffer
	err := runModelShow(&out, modelEnvFor(town, om))
	if exitCodeForError(err) != 4 {
		t.Errorf("show with an unreadable reviewer config = %d, want 4:\n%s", exitCodeForError(err), out.String())
	}
	if row := modelRowFields(t, out.String(), "om-review"); row[3] != "(unreadable)" || row[4] != "(unreadable)" {
		t.Errorf("om-review row = %v, want (unreadable) in both the live and the drift columns", row)
	}
	if !strings.Contains(out.String(), "could not be read") {
		t.Errorf("show does not say the reviewer config could not be read:\n%s", out.String())
	}
}

// A reviewer profile that names no model cannot be checked against the live
// backend, and saying so is the drift (gt-jaagc).
func TestModelShowReportsAReviewerProfileWithNoModel(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	modelPutTable(t, town, `{"profiles":{"bodyless":{"preset":"claude-haiku"}},"roles":{"om-review":"bodyless"}}`)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)

	var out bytes.Buffer
	err := runModelShow(&out, modelEnvFor(town, om))
	if exitCodeForError(err) != 4 {
		t.Errorf("show with a reviewer profile that names no model = %d, want 4:\n%s", exitCodeForError(err), out.String())
	}
	if !strings.Contains(out.String(), "(no model in profile)") {
		t.Errorf("show does not name the profile's missing model:\n%s", out.String())
	}
}

// One command switches the reviewer: the table, then the om backend with its
// model swapped, then the read-back (gt-jaagc).
func TestModelSetRoleSwitchesTheReviewerInOneStep(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"],"threshold":0.7,"depth":"standard","future":{"kept":true}}`)

	var out bytes.Buffer
	if err := runModelSetRole(&out, modelEnvFor(town, om), "om-review", "haiku"); err != nil {
		t.Fatalf("set-role om-review haiku: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "om-review: sonnet -> haiku") {
		t.Errorf("set-role does not state the change:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "read back") {
		t.Errorf("set-role does not report the read-back:\n%s", out.String())
	}

	got := modelDecode(t, om)
	backend, _ := got["backend"].([]any)
	want := []any{"/x/claude", "-p", "--model", "haiku"}
	if !reflect.DeepEqual(backend, want) {
		t.Errorf("om backend = %v, want the live command with its model swapped %v", backend, want)
	}
	if got["threshold"] != 0.7 || got["depth"] != "standard" {
		t.Errorf("the settings beside the backend were changed: %v", got)
	}
	if _, ok := got["future"]; !ok {
		t.Errorf("a key this gt does not know was dropped: %v", got)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(om), "config.json.bak-*"))
	if err != nil || len(backups) != 1 {
		t.Errorf("reviewer backups = %v, %v; want the previous copy kept", backups, err)
	}

	table := modelDecode(t, modelTablePath(town))
	roles, _ := table["roles"].(map[string]any)
	if roles["om-review"] != "haiku" || roles["crew"] != "haiku" {
		t.Errorf("table roles = %v, want om-review switched and crew kept", roles)
	}
	if _, ok := table["future"]; !ok {
		t.Errorf("a table key this gt does not know was dropped: %v", table)
	}
}

// A set-role for one role changes only that role: not another role, not the
// default agent, not the seat map, and not the reviewer's config (gt-mukgk).
func TestModelSetRoleChangesOnlyThatRole(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)
	omBefore, err := os.ReadFile(om)
	if err != nil {
		t.Fatal(err)
	}
	settingsBefore := modelDecode(t, modelSettingsPath(town))
	tablePath := modelPathsFor(town, om).table
	tableBefore := modelDecode(t, tablePath)

	var out bytes.Buffer
	if err := runModelSetRole(&out, modelEnvFor(town, om), "crew", "sonnet"); err != nil {
		t.Fatalf("set-role crew sonnet: %v\n%s", err, out.String())
	}

	// The table is the record of the switch: it may change only the crew role.
	tableAfter := modelDecode(t, tablePath)
	for _, doc := range []map[string]any{tableBefore, tableAfter} {
		roles, _ := doc["roles"].(map[string]any)
		delete(roles, "crew")
	}
	if !reflect.DeepEqual(tableBefore, tableAfter) {
		t.Errorf("set-role crew changed more than roles.crew in the table:\nbefore %v\nafter  %v", tableBefore, tableAfter)
	}

	settingsAfter := modelDecode(t, modelSettingsPath(town))
	for _, doc := range []map[string]any{settingsBefore, settingsAfter} {
		roles, _ := doc["role_agents"].(map[string]any)
		delete(roles, "crew")
	}
	if !reflect.DeepEqual(settingsBefore, settingsAfter) {
		t.Errorf("set-role crew changed more than role_agents.crew:\nbefore %v\nafter  %v", settingsBefore, settingsAfter)
	}
	roles, _ := modelDecode(t, modelSettingsPath(town))["role_agents"].(map[string]any)
	if roles["crew"] != "claude-sonnet" {
		t.Errorf("role_agents.crew = %v, want claude-sonnet", roles["crew"])
	}
	if _, ok := roles["polecat"]; ok {
		t.Error("set-role wrote role_agents.polecat, a retired key (gt-mukgk)")
	}
	if pool, _ := modelDecode(t, modelSettingsPath(town))["polecat_pool"].(map[string]any); pool == nil || pool["seats"] == nil {
		t.Errorf("set-role changed the polecat pool: %v", pool)
	}
	if omAfter, err := os.ReadFile(om); err != nil || !bytes.Equal(omBefore, omAfter) {
		t.Errorf("set-role crew changed the reviewer's config: %v\n%s", err, omAfter)
	}
}

// The roles a table may not carry are refused before either file is written.
func TestModelSetRoleRefusesRolesItMayNotCarry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		role    string
		profile string
		want    string
	}{
		{"polecat", "polecat", "haiku", "polecat is not a table role"},
		{"default", "default", "haiku", "default is not a role"},
		{"an unknown role", "harbormaster", "haiku", `role "harbormaster"`},
		{"an unknown profile", "crew", "nosuch", `profile "nosuch"`},
	} {
		town := modelTown(t)
		om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)
		settingsBefore, err := os.ReadFile(modelSettingsPath(town))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = runModelSetRole(&out, modelEnvFor(town, om), tc.role, tc.profile)
		if err == nil {
			t.Errorf("%s: set-role = nil, want a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: set-role = %v, want it to name %q", tc.name, err, tc.want)
		}
		if _, err := os.Stat(modelTablePath(town)); !os.IsNotExist(err) {
			t.Errorf("%s: a refused set-role wrote the table: %v", tc.name, err)
		}
		if settingsAfter, err := os.ReadFile(modelSettingsPath(town)); err != nil || !bytes.Equal(settingsBefore, settingsAfter) {
			t.Errorf("%s: a refused set-role changed the settings: %v", tc.name, err)
		}
	}
}

// A preset no agent resolves is refused before it is written, where every spawn
// would warn and fall back to the default.
func TestModelSetRoleRefusesAPresetTheTownCannotResolve(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, `{"profiles":{"typo":{"preset":"claude-opos"}},"roles":{"crew":"haiku"}}`)
	tableBefore, err := os.ReadFile(modelTablePath(town))
	if err != nil {
		t.Fatal(err)
	}
	settingsBefore, err := os.ReadFile(modelSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}

	// The town resolves no preset: a test's resolver, not the machine's agents
	// (gt-jaagc).
	env := modelEnvFor(town, "")
	env.agents = func(string, string) bool { return false }

	var out bytes.Buffer
	err = runModelSetRole(&out, env, "crew", "typo")
	if err == nil || !strings.Contains(err.Error(), "claude-opos") {
		t.Fatalf("set-role to a preset no agent resolves = %v, want a refusal naming it", err)
	}
	if after, _ := os.ReadFile(modelTablePath(town)); !bytes.Equal(tableBefore, after) {
		t.Error("a refused set-role wrote the table")
	}
	if after, _ := os.ReadFile(modelSettingsPath(town)); !bytes.Equal(settingsBefore, after) {
		t.Error("a refused set-role wrote the settings")
	}
}

// A reviewer config that does not parse is refused whole: writing it would drop
// the settings it holds.
func TestModelSetRoleWritesNothingWhenTheReviewerConfigDoesNotParse(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":`)

	var out bytes.Buffer
	if err := runModelSetRole(&out, modelEnvFor(town, om), "om-review", "haiku"); err == nil {
		t.Fatal("set-role with an unreadable reviewer config = nil, want a refusal")
	}
	if b, err := os.ReadFile(modelTablePath(town)); err != nil || !strings.Contains(string(b), `"om-review": "sonnet"`) {
		t.Errorf("a refused set-role wrote the table:\n%s", b)
	}
}

// A reviewer profile whose own backend runs a different model than it names is
// refused, or the output would say haiku while the reviewer ran sonnet.
func TestModelSetRoleRefusesAProfileWhoseBackendRunsAnotherModel(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, `{"profiles":{"mixed":{"model":"haiku","om_backend":["/x/claude","-p","--model","sonnet"]}},"roles":{"crew":"haiku"}}`)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)

	var out bytes.Buffer
	err := runModelSetRole(&out, modelEnvFor(town, om), "om-review", "mixed")
	if err == nil || !strings.Contains(err.Error(), "runs sonnet") {
		t.Fatalf("set-role to a profile whose backend runs another model = %v, want a refusal naming it", err)
	}
	if !strings.Contains(string(mustRead(t, om)), "sonnet") || strings.Contains(string(mustRead(t, om)), "haiku") {
		t.Errorf("a refused set-role wrote the reviewer's backend: %s", mustRead(t, om))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The default profile is one command too: the table's default key and
// default_agent, then the read-back.
func TestModelSetDefaultWritesTheDefaultProfile(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	settingsBefore := modelDecode(t, modelSettingsPath(town))

	var out bytes.Buffer
	if err := runModelSetDefault(&out, modelEnvFor(town, ""), "haiku"); err != nil {
		t.Fatalf("set-default haiku: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "default: claude-sonnet -> claude-haiku") || !strings.Contains(out.String(), "read back") {
		t.Errorf("set-default does not state the change and its read-back:\n%s", out.String())
	}
	if row := modelDecode(t, modelTablePath(town))["default"]; row != "haiku" {
		t.Errorf("table default = %v, want haiku", row)
	}

	settingsAfter := modelDecode(t, modelSettingsPath(town))
	delete(settingsBefore, "default_agent")
	delete(settingsAfter, "default_agent")
	if !reflect.DeepEqual(settingsBefore, settingsAfter) {
		t.Errorf("set-default changed more than default_agent:\nbefore %v\nafter  %v", settingsBefore, settingsAfter)
	}
	if got := modelDecode(t, modelSettingsPath(town))["default_agent"]; got != "claude-haiku" {
		t.Errorf("default_agent = %v, want claude-haiku", got)
	}
}

// apply writes the whole table in one pass and prints it back with the live
// values it found, leaving a role the table does not name alone (gt-jaagc).
func TestModelApplyWritesEveryRoleAndTheReviewer(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"],"threshold":0.7}`)

	var out bytes.Buffer
	if err := runModelApply(&out, modelEnvFor(town, om)); err != nil {
		t.Fatalf("apply: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "applied: 2 roles") {
		t.Errorf("apply does not say what it applied:\n%s", out.String())
	}
	roles, _ := modelDecode(t, modelSettingsPath(town))["role_agents"].(map[string]any)
	if roles["crew"] != "claude-haiku" {
		t.Errorf("role_agents.crew = %v, want claude-haiku", roles["crew"])
	}
	if roles["witness"] != "claude-opus" {
		t.Errorf("apply changed role_agents.witness to %v; the table does not name it", roles["witness"])
	}
	if got := modelDecode(t, modelSettingsPath(town))["default_agent"]; got != "claude-sonnet" {
		t.Errorf("default_agent = %v, want the table's sonnet", got)
	}
	backend, _ := modelDecode(t, om)["backend"].([]any)
	if !reflect.DeepEqual(backend, []any{"/x/claude", "-p", "--model", "sonnet"}) {
		t.Errorf("om backend = %v, want the table's sonnet", backend)
	}
	if modelDecode(t, om)["threshold"] != 0.7 {
		t.Errorf("apply dropped a setting beside the backend: %v", modelDecode(t, om))
	}
	if row := modelRowFields(t, out.String(), "crew"); row[3] != "claude-haiku" || row[4] != "-" {
		t.Errorf("crew row after apply = %v, want the live value and no drift", row)
	}
}

// apply writes nothing when any part of the table is invalid, so a single bad
// entry cannot leave the town half-applied (gt-jaagc).
func TestModelApplyValidatesTheWholeTableBeforeWriting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		table string
		want  string
	}{
		{"an unknown role", `{"roles":{"crew":"haiku","harbormaster":"sonnet"}}`, `role "harbormaster"`},
		{"a missing profile", `{"roles":{"crew":"nosuch"}}`, "no such profile"},
		{"a profile without a preset", `{"profiles":{"bodyless":{"om_backend":["claude","-p"]}},"roles":{"crew":"bodyless"}}`, "names no preset"},
		{"a default naming no profile", `{"default":"nosuch","roles":{"crew":"haiku"}}`, "default = \"nosuch\""},
		{"a role named default", `{"roles":{"default":"haiku"}}`, "default is not a role"},
	} {
		town := modelTown(t)
		modelPutTable(t, town, tc.table)
		om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)
		settingsBefore := mustRead(t, modelSettingsPath(town))
		omBefore := mustRead(t, om)

		var out bytes.Buffer
		err := runModelApply(&out, modelEnvFor(town, om))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: apply = %v, want a refusal naming %q", tc.name, err, tc.want)
			continue
		}
		if after := mustRead(t, modelSettingsPath(town)); !bytes.Equal(settingsBefore, after) {
			t.Errorf("%s: a refused apply wrote the settings:\n%s", tc.name, after)
		}
		if after := mustRead(t, om); !bytes.Equal(omBefore, after) {
			t.Errorf("%s: a refused apply wrote the reviewer's config:\n%s", tc.name, after)
		}
	}
}

func TestModelApplyRefusesWithoutATable(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	before := mustRead(t, modelSettingsPath(town))
	var out bytes.Buffer
	err := runModelApply(&out, modelEnvFor(town, ""))
	if err == nil || !strings.Contains(err.Error(), "nothing to apply") {
		t.Fatalf("apply with no table = %v, want a refusal saying there is nothing to apply", err)
	}
	if after := mustRead(t, modelSettingsPath(town)); !bytes.Equal(before, after) {
		t.Error("apply with no table wrote the settings")
	}
}

// The read-back is what makes the "it is live" claim checkable: a value that did
// not land is an error naming what was written and what was found (gt-jaagc).
func TestModelReadBackRefusesAValueThatDidNotLand(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	p := modelPathsFor(town, modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`))

	if _, err := readBackRoleAgent(p, "crew", "claude-haiku"); err == nil {
		t.Error("readBackRoleAgent with the live claude-sonnet = nil, want a refusal")
	}
	if got, err := readBackRoleAgent(p, "crew", "claude-sonnet"); err != nil || got != "claude-sonnet" {
		t.Errorf("readBackRoleAgent of the value that is there = %q, %v; want it", got, err)
	}
	if _, err := readBackRoleAgent(modelPathsFor(t.TempDir(), ""), "crew", "claude-haiku"); err == nil {
		t.Error("readBackRoleAgent with no settings file = nil, want a refusal")
	}

	if _, err := readBackOMBackend(p, []string{"/x/claude", "-p", "--model", "haiku"}); err == nil {
		t.Error("readBackOMBackend of a backend that is not there = nil, want a refusal")
	}
	if got, err := readBackOMBackend(p, []string{"/x/claude", "-p", "--model", "sonnet"}); err != nil || len(got) != 4 {
		t.Errorf("readBackOMBackend of the backend that is there = %q, %v; want it", got, err)
	}
	bad := modelPathsFor(town, modelOMConfig(t, `{"backend":`))
	if _, err := readBackOMBackend(bad, []string{"claude"}); err == nil {
		t.Error("readBackOMBackend of an unreadable config = nil, want a refusal")
	}
}

// modelReadOnly makes dir refuse new files, so the next write under it fails;
// the permission is put back before the test's cleanup walks the tree (gt-jaagc).
func modelReadOnly(t *testing.T, dir string) {
	t.Helper()
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit to be read
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: cleanup must restore its search bit
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// A failure after the first of the two writes puts that file back: a switch
// either changes both files or neither, never only the table (gt-jaagc).
func TestModelSetRolePutsTheTableBackWhenTheReviewerWriteFails(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)
	omBefore := mustRead(t, om)
	modelReadOnly(t, filepath.Dir(om))

	var out bytes.Buffer
	err := runModelSetRole(&out, modelEnvFor(town, om), "om-review", "haiku")
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("set-role whose reviewer write fails = %v, want a failure saying nothing changed", err)
	}
	if got := modelDecode(t, modelTablePath(town)); got["roles"].(map[string]any)["om-review"] != "sonnet" {
		t.Errorf("the table was left saying om-review = %v after the live write failed", got["roles"])
	}
	if got := mustRead(t, om); !bytes.Equal(got, omBefore) {
		t.Errorf("the reviewer's config changed: %s", got)
	}
	if out.Len() != 0 {
		t.Errorf("a failed switch claimed a change:\n%s", out.String())
	}
}

// apply is one command over two files too: a failure on the reviewer's config
// puts the town settings back as it found them (gt-jaagc).
func TestModelApplyPutsTheSettingsBackWhenTheReviewerWriteFails(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	modelPutTable(t, town, modelTableFixture)
	om := modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`)
	settingsBefore := mustRead(t, modelSettingsPath(town))
	modelReadOnly(t, filepath.Dir(om))

	var out bytes.Buffer
	err := runModelApply(&out, modelEnvFor(town, om))
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("apply whose reviewer write fails = %v, want a failure saying nothing changed", err)
	}
	if got := mustRead(t, modelSettingsPath(town)); !bytes.Equal(got, settingsBefore) {
		t.Errorf("apply left the settings changed after the reviewer write failed:\n%s", got)
	}
}

// The undo itself: a file that was there comes back with its content, one the
// write created is removed, and the error keeps the cause (gt-jaagc).
func TestModelWritesPutBackEveryFileWritten(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existing := filepath.Join(dir, "written.json")
	modelWrite(t, existing, "old")
	backup, err := config.BackupFile(existing, "stamp")
	if err != nil {
		t.Fatal(err)
	}
	modelWrite(t, existing, "new")
	created := filepath.Join(dir, "created.json")
	modelWrite(t, created, "new")

	cause := errors.New("the second write failed")
	err = writtenModelFiles{
		{path: existing, backup: backup},
		{path: created},
	}.fail(cause)
	if !errors.Is(err, cause) {
		t.Errorf("fail = %v, want it to carry the cause", err)
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("fail = %v, want it to say the files were put back", err)
	}
	if got := string(mustRead(t, existing)); got != "old" {
		t.Errorf("%s reads %q after the undo, want the content before the write", existing, got)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Errorf("the file a failed command created is still there: %v", err)
	}
}

// omPathLabel reads a directory name that starts with two dots as a name, not as
// the parent directory (gt-jaagc).
func TestModelOMPathLabelKeepsADotDotNameInsideHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	inside := filepath.Join(home, "..config", "om", "config.json")
	if got, want := omPathLabelIn(home, inside), "~/..config/om/config.json"; got != want {
		t.Errorf("omPathLabelIn(%s, %s) = %q, want %q", home, inside, got, want)
	}
	outside := filepath.Join(filepath.Dir(home), "elsewhere", "config.json")
	if got := omPathLabelIn(home, outside); got != outside {
		t.Errorf("omPathLabelIn(%s, %s) = %q, want the path itself", home, outside, got)
	}
	if got := omPathLabelIn(home, ""); got != "(no home directory)" {
		t.Errorf("omPathLabelIn of no path = %q, want it to say there is no home", got)
	}
	if got := omPathLabelIn("", inside); got != inside {
		t.Errorf("omPathLabelIn with no home = %q, want the path itself", got)
	}
}

// The default_agent read-back refuses a value that did not land, as the role and
// reviewer read-backs do (gt-jaagc).
func TestModelReadBackDefaultRefusesAValueThatDidNotLand(t *testing.T) {
	t.Parallel()

	town := modelTown(t)
	p := modelPathsFor(town, modelOMConfig(t, `{"backend":["/x/claude","-p","--model","sonnet"]}`))
	if _, err := writeModelLive(p, "readback-default", func(ts *config.TownSettings) {
		ts.DefaultAgent = "claude-sonnet"
	}); err != nil {
		t.Fatalf("seed default_agent: %v", err)
	}

	if _, err := readBackDefaultAgent(p, "claude-haiku"); err == nil {
		t.Error("readBackDefaultAgent with the live claude-sonnet = nil, want a refusal")
	}
	if got, err := readBackDefaultAgent(p, "claude-sonnet"); err != nil || got != "claude-sonnet" {
		t.Errorf("readBackDefaultAgent of the value that is there = %q, %v; want it", got, err)
	}
	if _, err := readBackDefaultAgent(modelPathsFor(t.TempDir(), ""), "claude-haiku"); err == nil {
		t.Error("readBackDefaultAgent with no settings file = nil, want a refusal")
	}
}

// A failed switch that cannot put a file back says so: the error names the file
// it could not restore, rather than reporting a clean rollback (gt-jaagc).
func TestModelWritesFailNamesAFileItCannotPutBack(t *testing.T) {
	t.Parallel()

	ws := writtenModelFiles{{
		path:   filepath.Join(t.TempDir(), "models.json"),
		backup: filepath.Join(t.TempDir(), "missing.bak"),
	}}
	err := ws.fail(errors.New("the live write failed"))
	if err == nil || !strings.Contains(err.Error(), "could not be put back") {
		t.Errorf("fail = %v, want an error saying the file could not be put back", err)
	}
}

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeadOperationalKeysAreDeclared: every key the loader warns about must
// still decode, or the warning could never fire. The follow-up deletion
// (gt-y3pgh.2.13's child) is what removes the declarations; until then a
// settings file carrying the key loads.
func TestDeadOperationalKeysAreDeclared(t *testing.T) {
	t.Parallel()

	for _, key := range deadOperationalKeys {
		data := nestedJSON(t, key, nil)
		if err := DecodeJSONFile("config.json", data, &TownSettings{}); err != nil {
			t.Errorf("%s must stay declared until the deletion lands: %v", key, err)
		}
	}
}

// TestDeadOperationalWarnings: a dead key earns one warning naming the key and
// the file; a live key beside it earns none.
func TestDeadOperationalWarnings(t *testing.T) {
	t.Parallel()

	const file = "settings/config.json"
	data := []byte(`{"type":"town-settings","version":1,"operational":{` +
		`"dolt":{"max_connections":1000,"commits_per_day_warn":800}}}`)

	got := deadOperationalWarnings(file, data)
	want := file + ": operational.dolt.max_connections is set but nothing reads it"
	if len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("warnings = %q, want one containing %q", got, want)
	}
}

// TestDeadOperationalWarningsCaseAndSection: a key is matched the way
// encoding/json assigns it (case-insensitively) and a section declared whole
// as json.RawMessage warns once for the section, not for each key inside it.
func TestDeadOperationalWarningsCaseAndSection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
		want string
	}{
		{"case", `{"operational":{"dolt":{"MAX_CONNECTIONS":1000}}}`, "operational.dolt.MAX_CONNECTIONS"},
		{"raw section", `{"operational":{"web":{"max_body_len":10}}}`, "operational.web"},
	}
	for _, tc := range cases {
		got := deadOperationalWarnings("config.json", []byte(tc.data))
		if len(got) != 1 || !strings.Contains(got[0], "config.json: "+tc.want) {
			t.Errorf("%s: warnings = %q, want one naming %s", tc.name, got, tc.want)
		}
	}
}

// TestDeadOperationalWarningsQuiet: live keys only, an empty file and a file
// that does not parse as JSON all yield nothing here — the last is the strict
// decode's error to report, not a warning to repeat.
func TestDeadOperationalWarningsQuiet(t *testing.T) {
	t.Parallel()

	for _, data := range []string{
		`{"operational":{"dolt":{"commits_per_day_warn":800},"container_gate":{"slots":4}}}`,
		``,
		`{"operational":`,
	} {
		if got := deadOperationalWarnings("config.json", []byte(data)); len(got) != 0 {
			t.Errorf("warnings for %q = %q, want none", data, got)
		}
	}
}

// TestLoadOrCreateTownSettingsLoadsDeadOperationalKey: the warning is not an
// error — the file's live keys still decode and the load returns.
func TestLoadOrCreateTownSettingsLoadsDeadOperationalKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"type":"town-settings","version":1,"operational":{` +
		`"dolt":{"max_connections":1000,"commits_per_day_warn":800}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := loaded.Operational.GetDoltConfig().CommitsPerDayWarnV(); got != 800 {
		t.Errorf("commits_per_day_warn = %d, want the live 800 beside the dead key", got)
	}
}

// nestedJSON builds {"a":{"b":{"c":value}}} from the dotted key "a.b.c".
func nestedJSON(t *testing.T, key string, value any) []byte {
	t.Helper()
	parts := strings.Split(key, ".")
	var doc any = value
	for i := len(parts) - 1; i >= 0; i-- {
		doc = map[string]any{parts[i]: doc}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

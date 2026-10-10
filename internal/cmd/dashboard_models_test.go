package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// The header strip names the model a reviewer runs, not the command that runs
// it: a backend is an argv, and its raw command line says nothing about which
// model an operator is looking at (gt-bj47s).
func TestOMModelName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		backend []string
		want    string
	}{
		{"the reviewer's own config", []string{"/Users/x/.local/bin/claude", "-p", "--model", "sonnet"}, "sonnet"},
		{"a model given with =", []string{"claude", "--model=sonnet", "-p"}, "sonnet"},
		{"a model given short", []string{"claude", "-m", "sonnet"}, "sonnet"},
		{"a vendor-prefixed model", []string{"claude", "--model", "deepseek-flash"}, "flash"},
		{"a wrapper that names no model", []string{"/x/claude-deepseek-flash", "-p"}, "flash"},
		{"a model with a release in it", []string{"claude", "--model", "claude-3-5-haiku-2026"}, "haiku"},
		// An unknown backend is not a family, and its own name is all the town
		// knows about it.
		{"an unknown backend", []string{"/usr/local/bin/aider", "--yes"}, "aider"},
		{"an unknown model", []string{"claude", "--model", "gpt-5"}, "gpt-5"},
		{"a flag with no value", []string{"/x/aider", "--model"}, "aider"},
		{"no backend at all", nil, ""},
	} {
		if got := omModelName(tc.backend); got != tc.want {
			t.Errorf("%s: omModelName(%q) = %q, want %q", tc.name, tc.backend, got, tc.want)
		}
	}
}

// modelWord is a word match, not a substring one: a backend called "flashback"
// is not a flash model.
func TestOMModelNameMatchesWholeWords(t *testing.T) {
	t.Parallel()

	if got := omModelName([]string{"/x/flashback"}); got != "flashback" {
		t.Errorf("a name that merely contains a family was read as one: %q", got)
	}
}

func writeModelsSettings(t *testing.T, townRoot, body string) {
	t.Helper()

	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeOMConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The strip reads both configs whole on every poll, so what it shows is what
// the files say now (gt-bj47s).
func TestModelsReaderReadsTheTownAndTheReviewer(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"deepseek-flash"},"polecat_pool":{"agent":"deepseek-flash","max_seats":4}}`)
	r := &modelsReader{townRoot: town, omPath: writeOMConfig(t, `{"backend":["/Users/x/.local/bin/claude","-p","--model","sonnet"],"depth":"standard"}`)}

	m := r.read()
	if m.Polecat != "deepseek-flash" {
		t.Errorf("polecat model = %q, want deepseek-flash", m.Polecat)
	}
	if m.SeatCap != 4 {
		t.Errorf("seat cap = %d, want the pool's max_seats", m.SeatCap)
	}
	if m.OM != "sonnet" {
		t.Errorf("om model = %q, want sonnet", m.OM)
	}

	// A mode switch is a file the reader has not read yet: the next read is
	// the new model, with no cache in between.
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"claude-sonnet"},"polecat_pool":{"agent":"deepseek-flash","max_seats":2}}`)
	r.omPath = writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)
	m = r.read()
	if m.Polecat != "claude-sonnet" || m.SeatCap != 2 || m.OM != "flash" {
		t.Errorf("a switched mode did not show on the next read: %+v", m)
	}
}

// A config the town cannot read leaves its field empty — the page calls that
// unknown — and never stops the other config from reporting (gt-bj47s).
func TestModelsReaderLeavesAnUnreadableConfigUnknown(t *testing.T) {
	t.Parallel()

	om := writeOMConfig(t, `{"backend":["/x/claude-deepseek-flash","-p"]}`)

	// No settings file at all: a town that never set a model.
	m := (&modelsReader{townRoot: t.TempDir(), omPath: om}).read()
	if m.Polecat != "" || m.SeatCap != 0 || m.OM != "flash" {
		t.Errorf("a missing settings file took the reviewer with it: %+v", m)
	}

	// A settings file that does not parse: the town's model is not a guess.
	town := t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":`)
	m = (&modelsReader{townRoot: town, omPath: om}).read()
	if m.Polecat != "" || m.SeatCap != 0 {
		t.Errorf("an unparseable settings file was read as a model: %+v", m)
	}
	if m.OM != "flash" {
		t.Errorf("an unparseable settings file took the reviewer with it: %+v", m)
	}

	// No reviewer's config: the om model is unknown, the town's still reports.
	town = t.TempDir()
	writeModelsSettings(t, town, `{"role_agents":{"polecat":"deepseek-flash"}}`)
	m = (&modelsReader{townRoot: town, omPath: filepath.Join(t.TempDir(), "gone.json")}).read()
	if m.OM != "" {
		t.Errorf("a missing reviewer's config was read as a model: %+v", m)
	}
	if m.Polecat != "deepseek-flash" {
		t.Errorf("a missing reviewer's config took the polecat model with it: %+v", m)
	}
	// A town with no pool declares no cap, which is not a cap of zero.
	if m.SeatCap != 0 {
		t.Errorf("a town with no pool declared a cap: %+v", m)
	}
}

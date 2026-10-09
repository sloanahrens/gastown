package townconfig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// repoRoot is the module root, taken from this file's path so the shipped
// example files resolve whatever directory the test runs from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// TestLoadRejectsRigSettingsThatDoNotParse: a registered rig whose
// settings/config.json does not decode fails the load, so no session spawns
// for it with the town's default agent instead of the rig's (gt-ptysu).
func TestLoadRejectsRigSettingsThatDoNotParse(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	path := filepath.Join(root, "gastown", "settings", "config.json")
	write(t, root, "gastown/settings/config.json", `{"type":"rig-settings","version":1,"zz_unknown":1}`)

	_, err := Load(root)
	var pe *config.ParseError
	if !errors.As(err, &pe) || pe.Path != path {
		t.Fatalf("Load = %v, want a ParseError naming %s", err, path)
	}
	if len(pe.Keys) != 1 || pe.Keys[0] != "zz_unknown" {
		t.Errorf("ParseError.Keys = %v, want [zz_unknown]", pe.Keys)
	}
}

// TestLoadAcceptsARigSettingsFile: the new check rejects only a broken file.
func TestLoadAcceptsARigSettingsFile(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	write(t, root, "gastown/settings/config.json", `{"type":"rig-settings","version":1,"agent":"claude"}`)

	if _, err := Load(root); err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
}

// TestLoadAcceptsAnAbsentRigSettingsFile: a registered rig with no settings
// file is the default, not an error.
func TestLoadAcceptsAnAbsentRigSettingsFile(t *testing.T) {
	t.Parallel()
	if _, err := Load(copyLiveTown(t)); err != nil {
		t.Fatalf("Load = %v, want nil", err)
	}
}

// TestLoadAcceptsTheShippedExamples: an operator copies docs/examples into a
// town, so both files go through the loaders that read them there (gt-ptysu).
// A comment key or a retired key would make the copy fail strict decode.
func TestLoadAcceptsTheShippedExamples(t *testing.T) {
	t.Parallel()
	examples := filepath.Join(repoRoot(t), "docs", "examples")
	root := copyLiveTown(t)
	for _, f := range []struct{ src, dst string }{
		{"town-settings.example.json", FileSettings},
		{"rig-settings.example.json", "gastown/settings/config.json"},
	} {
		data, err := os.ReadFile(filepath.Join(examples, f.src))
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, f.dst, string(data))
	}

	if _, err := Load(root); err != nil {
		t.Fatalf("Load with the shipped examples = %v", err)
	}
}

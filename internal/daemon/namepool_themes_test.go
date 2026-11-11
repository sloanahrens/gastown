package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// themeTown builds a fake town: one registry entry per rig, and each rig's
// settings file with the namepool style given in styles (a rig absent from the
// map has no settings file). A theme file is written for each name in themes.
func themeTown(t *testing.T, rigs []string, styles map[string]string, themes []string) string {
	t.Helper()
	root := t.TempDir()
	entries := make([]string, 0, len(rigs))
	for _, rig := range rigs {
		entries = append(entries, `"`+rig+`":{"git_url":"file:///src/`+rig+`","added_at":"2026-01-01T00:00:00Z"}`)
		if style, ok := styles[rig]; ok {
			writeThemeFile(t, filepath.Join(root, rig, "settings", "config.json"),
				`{"namepool":{"style":"`+style+`"}}`)
		} else {
			if err := os.MkdirAll(filepath.Join(root, rig), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeThemeFile(t, filepath.Join(root, "mayor", "town.json"), `{"name":"fake"}`)
	writeThemeFile(t, filepath.Join(root, "mayor", "rigs.json"),
		`{"version":1,"rigs":{`+strings.Join(entries, ",")+`}}`)
	for _, name := range themes {
		writeThemeFile(t, filepath.Join(root, "settings", "themes", name+".txt"), "alpha-one\n")
	}
	return root
}

func writeThemeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Each unparked rig whose namepool style has no theme file gets one line, and
// only those rigs: a rig with its theme file, a built-in style, no settings at
// all, and a parked rig with a missing theme are not listed.
func TestUnresolvedThemeLinesListsEachUnparkedRigWithAMissingThemeFile(t *testing.T) {
	t.Parallel()
	rigs := []string{"alpha", "beta", "delta", "gamma", "zeta"}
	root := themeTown(t, rigs, map[string]string{
		"alpha": "gone",    // missing theme file
		"beta":  "here",    // theme file present
		"delta": "mad-max", // built-in style
		"gamma": "gone",    // missing, but parked below
		// zeta has no settings file: its style is the rig-name hash, built-in
	}, []string{"here"})
	if _, err := townconfig.Park(root, "gamma", config.RigParked{Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), By: "operator", Reason: "test"}); err != nil {
		t.Fatalf("Park(gamma): %v", err)
	}

	lines := unresolvedThemeLines(root, rigs)
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want exactly one line, for alpha", lines)
	}
	line := lines[0]
	for _, want := range []string{"alpha", `"gone"`, filepath.Join(root, "settings", "themes", "gone.txt"), "refused"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q does not name %q", line, want)
		}
	}
}

// A park state that cannot be read is reported for each rig, not skipped: the
// theme of such a rig is unchecked, and the line says so.
func TestUnresolvedThemeLinesReportsAnUnreadableParkState(t *testing.T) {
	t.Parallel()
	root := themeTown(t, []string{"alpha"}, map[string]string{"alpha": "gone"}, nil)
	writeThemeFile(t, filepath.Join(root, "mayor", "rigs.json"), `{not json`)

	lines := unresolvedThemeLines(root, []string{"alpha"})
	if len(lines) != 1 || !strings.Contains(lines[0], "park state unreadable") || !strings.Contains(lines[0], "alpha") {
		t.Fatalf("lines = %q, want one line saying the park state of alpha is unreadable", lines)
	}
}

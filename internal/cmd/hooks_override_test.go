package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/hooks"
)

func writeOverride(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := hooks.MarshalConfig(&hooks.HooksConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// editedPath runs gt hooks override in home and returns the path the editor
// was handed; the recording opener stands in for the editor process.
func editedPath(t *testing.T, home hooks.Home, target string) string {
	t.Helper()
	var opened string
	open := func(path string) error {
		opened = path
		return nil
	}
	if err := hooksOverrideWith(home, open, []string{target}); err != nil {
		t.Fatalf("hooksOverrideWith(%q): %v", target, err)
	}
	return opened
}

// TestHooksOverrideEditsTheFileItLoaded pins gt-arz8h: the editor must open the
// override the read cascade loaded. With GT_HOME set and the override only
// under ~/.gt, opening $GT_HOME/.gt would save a file that shadows the one in
// effect on the next sync.
func TestHooksOverrideEditsTheFileItLoaded(t *testing.T) {
	t.Parallel()
	gtHome, userHome := t.TempDir(), t.TempDir()
	home := hooks.HomeAtWithGT(gtHome, userHome)

	fallback := filepath.Join(userHome, ".gt", "hooks-overrides", "crew.json")
	writeOverride(t, fallback)

	if got := editedPath(t, home, "crew"); got != fallback {
		t.Errorf("editor opened %q, want the loaded override %q", got, fallback)
	}
}

// TestHooksOverrideCreatesInPrimaryDir pins the other half: when no override
// exists anywhere, gt hooks override creates it at the primary write path.
func TestHooksOverrideCreatesInPrimaryDir(t *testing.T) {
	t.Parallel()
	gtHome, userHome := t.TempDir(), t.TempDir()
	home := hooks.HomeAtWithGT(gtHome, userHome)

	want := filepath.Join(gtHome, ".gt", "hooks-overrides", "crew.json")
	if got := editedPath(t, home, "crew"); got != want {
		t.Errorf("editor opened %q, want the primary path %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("override was not created at the primary path: %v", err)
	}
}

// TestHooksOverrideCommandOpensLoadedOverride runs the command's own RunE, so
// the hop from an invocation to the editor is covered: editedPath stubs both
// Home and opener, and a RunE handed the wrong Home, or a no-op opener, would
// still pass it (gt-arsyc).
func TestHooksOverrideCommandOpensLoadedOverride(t *testing.T) {
	t.Parallel()
	gtHome, userHome := t.TempDir(), t.TempDir()
	home := hooks.HomeAtWithGT(gtHome, userHome)

	loaded := filepath.Join(userHome, ".gt", "hooks-overrides", "crew.json")
	writeOverride(t, loaded)

	var opened string
	cmd := &cobra.Command{}
	cmd.SetContext(withHooksOverrideEnv(context.Background(), home, func(path string) error {
		opened = path
		return nil
	}))

	if err := hooksOverrideCmd.RunE(cmd, []string{"crew"}); err != nil {
		t.Fatalf("gt hooks override crew: %v", err)
	}
	if opened != loaded {
		t.Errorf("editor opened %q, want the loaded override %q", opened, loaded)
	}
}

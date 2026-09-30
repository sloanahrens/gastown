package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// RequireTownEnv returns an initialized town's root and skips the test
// anywhere else: outside a workspace, or in one without mayor/rigs.json.
func TestRequireTownEnv_ReturnsRoot(t *testing.T) {
	t.Parallel()
	town := makeFakeTown(t)
	if got := requireTownEnv(t, func() (string, error) { return town, nil }); got != town {
		t.Errorf("requireTownEnv = %q, want %q", got, town)
	}

	half := t.TempDir()
	if err := os.MkdirAll(filepath.Join(half, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, find := range map[string]func() (string, error){
		"no workspace":       func() (string, error) { return "", errors.New("not in a workspace") },
		"empty root":         func() (string, error) { return "", nil },
		"no mayor/rigs.json": func() (string, error) { return half, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var skipped bool
			t.Run("inner", func(t *testing.T) {
				t.Cleanup(func() { skipped = t.Skipped() })
				requireTownEnv(t, find)
				t.Error("requireTownEnv returned instead of skipping")
			})
			if !skipped {
				t.Errorf("%s: the test was not skipped", name)
			}
		})
	}
}

package templates_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDaemonComposeWithoutTheFlagEqualsMainsOutput renders the live spec with a
// config directory and no secrets manifest. The golden file is the output of the
// unmodified writer on main, generated and committed before the secrets manifest
// existed, so a future change to the no-manifest render shows up here rather than
// being compared against output the change itself produced (gt-gg9hv.66).
func TestDaemonComposeWithoutTheFlagEqualsMainsOutput(t *testing.T) {
	t.Parallel()
	s := liveSpec()
	s.ConfigDir = "/host/home/.config/gt"
	out, _ := render(t, s)

	golden, err := os.ReadFile(filepath.Join("testdata", "daemon-compose-live-configdir.golden.yml"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if out != string(golden) {
		t.Errorf("the render without a secrets manifest differs from the golden\ngot:\n%s\nwant:\n%s", out, golden)
	}
}

package polecat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// writePolecatRigConfig writes body to <townRoot>/<name>/config.json and
// returns the rig path, so a Manager built on it reads a real config file.
func writePolecatRigConfig(t *testing.T, townRoot, name, body string) string {
	t.Helper()
	rigPath := filepath.Join(townRoot, name)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rigPath, err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return rigPath
}

// TestNewManagerReadsPolecatNamesFromRigConfig is the positive half of
// gt-w8dw5: a rig config.json that decodes still supplies polecat_names.
func TestNewManagerReadsPolecatNamesFromRigConfig(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigPath := writePolecatRigConfig(t, townRoot, "testrig", `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "polecat_names": ["alpha", "beta"]
}`)

	m := newTestManager(&rig.Rig{Name: "testrig", Path: rigPath}, nil, nil, newNoDatabaseDB())
	if got := len(m.namePool.CustomNames); got != 2 {
		t.Errorf("namePool.CustomNames = %v; want the two configured names", m.namePool.CustomNames)
	}
	if rig.RigConfigWarned(rigPath) {
		t.Errorf("RigConfigWarned(%s) = true for a valid config", rigPath)
	}
}

// TestNewManagerReportsUnparseableRigConfig pins the other half: the
// polecat_names read reports a config.json that does not decode instead of
// silently falling back to the theme pool.
func TestNewManagerReportsUnparseableRigConfig(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigPath := writePolecatRigConfig(t, townRoot, "testrig", `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "polecat_namess": ["alpha", "beta"]
}`)

	m := newTestManager(&rig.Rig{Name: "testrig", Path: rigPath}, nil, nil, newNoDatabaseDB())
	if len(m.namePool.CustomNames) != 0 {
		t.Errorf("namePool.CustomNames = %v; want the theme fallback", m.namePool.CustomNames)
	}
	if !rig.RigConfigWarned(rigPath) {
		t.Errorf("newManager fell back to the theme pool without reporting the parse error")
	}
}

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A prime bd call whose workspace names no database is refused before bd
// starts, so bd never reads its built-in default "beads" in its place.
func TestExecPrimeBdCallRefusesNamelessWorkspace(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := execPrimeExternalCommand(context.Background(), workDir, "bd", "kv", "list", "--json")
	if !errors.Is(err, beads.ErrNoConfiguredDatabase) {
		t.Fatalf("err = %v, want ErrNoConfiguredDatabase", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(workDir, ".beads")) {
		t.Errorf("err = %v, want it to name the workspace", err)
	}
}

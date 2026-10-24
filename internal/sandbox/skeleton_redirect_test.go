package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// extractSkeleton unpacks the archive under dir, as `tar xzf` does in /sandbox.
func extractSkeleton(t *testing.T, data []byte, dir string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Inside the sandbox, gt prime re-runs the beads redirect setup for the
// polecat's worktree. It must land on the rig's shared beads (mayor/rig/.beads),
// where the skeleton put them: the rig root's .beads is only a pointer there, as
// on the host. Without the pointer the setup picked the rig root itself, a
// directory with no workspace, and bd failed in every sandbox polecat.
func TestSkeletonRedirectSetupLandsOnTheSharedBeads(t *testing.T) {
	t.Parallel()
	spec := fixtureSpec(t)
	// The real town.json carries the rig registry (the rig's dolt_database), which
	// is what makes the redirect setup treat the rig as having its own database.
	files := minimalTown()
	files["mayor/town.json"] = `{"type":"town","registry":{"rigs":{"gastown":{"dolt_database":"gt"}}}}`
	spec.TownRoot = writeTown(t, files)
	data, err := RenderSkeleton(spec)
	if err != nil {
		t.Fatal(err)
	}
	sandboxRoot := t.TempDir()
	extractSkeleton(t, data, sandboxRoot)
	town := filepath.Join(sandboxRoot, skeletonRoot)
	worktree := filepath.Join(town, "gastown", "polecats", "ace", "gastown")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := beads.ComputeRedirectTarget(town, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if want := "../../../mayor/rig/.beads"; got != want {
		t.Errorf("redirect target = %q, want %q", got, want)
	}
}

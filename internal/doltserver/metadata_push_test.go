package doltserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureMetadataPushesTheWorkspace: every write of the files bd reads tells
// the bd gateway its workspace is stale, including the pass that changes
// nothing — the gateway serves its own copy, so a skipped push is a stale tree.
func TestEnsureMetadataPushesTheWorkspace(t *testing.T) {
	t.Parallel()
	var pushed []string
	h := &host{
		pushWorkspace: func(_ context.Context, townRoot string) (string, error) {
			pushed = append(pushed, townRoot)
			return "manifest", nil
		},
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"the first write", "a pass that changes nothing"} {
		pushed = nil
		if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
			t.Fatalf("EnsureMetadataForBeadsDir on %s: %v", pass, err)
		}
		if len(pushed) != 1 || pushed[0] != townRoot {
			t.Errorf("%s pushed %v, want the town root %s once", pass, pushed, townRoot)
		}
	}
}

// TestEnsureMetadataFastPathReportsAPortFileFailure: the pass that changes no
// metadata still writes the port file, and a failure there is returned before
// the push, so a workspace is never announced as current on a failed write.
func TestEnsureMetadataFastPathReportsAPortFileFailure(t *testing.T) {
	t.Parallel()
	pushed := 0
	h := newFakeHost().townNamesPort(DefaultPort).host()
	h.pushWorkspace = func(context.Context, string) (string, error) {
		pushed++
		return "manifest", nil
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The first pass writes metadata.json and the port file, so the second
	// pass has nothing to change and takes the fast path.
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the first pass: %v", err)
	}
	portPath := filepath.Join(beadsDir, "dolt-server.port")
	if err := os.Remove(portPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(portPath, 0o755); err != nil {
		t.Fatal(err)
	}
	pushed = 0
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err == nil {
		t.Fatal("the fast path succeeded with dolt-server.port blocked by a directory")
	}
	if pushed != 0 {
		t.Errorf("pushed %d times after a failed port-file write, want none", pushed)
	}
}

// TestEnsureMetadataReportsAFailedPush: a push that fails is returned, so a
// caller learns the container still serves the tree the write replaced.
func TestEnsureMetadataReportsAFailedPush(t *testing.T) {
	t.Parallel()
	h := &host{
		pushWorkspace: func(context.Context, string) (string, error) {
			return "", errors.New("the gateway at gw:7440 did not answer")
		},
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq")
	if err == nil {
		t.Fatal("EnsureMetadataForBeadsDir succeeded with a failed push")
	}
	if !strings.Contains(err.Error(), "bd gateway's workspace was not updated") {
		t.Errorf("error = %v, want it to name the stale workspace", err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error = %v, want the push's own failure", err)
	}
}

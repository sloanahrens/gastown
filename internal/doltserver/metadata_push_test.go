package doltserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureMetadataPushesTheWorkspace: a write of the files bd reads tells the
// bd gateway its workspace is stale, naming the town root the push is built
// from.
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
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("EnsureMetadataForBeadsDir: %v", err)
	}
	if len(pushed) != 1 || pushed[0] != townRoot {
		t.Errorf("pushed %v, want the town root %s once", pushed, townRoot)
	}
}

// TestEnsureMetadataDoesNotPushWhenNothingChanged: a pass over files that are
// already current pushes nothing, so the five-minute reconcile does not resend
// the whole workspace on every tick.
func TestEnsureMetadataDoesNotPushWhenNothingChanged(t *testing.T) {
	t.Parallel()
	pushed := 0
	h := &host{
		pushWorkspace: func(context.Context, string) (string, error) {
			pushed++
			return "manifest", nil
		},
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the first pass: %v", err)
	}
	pushed = 0
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the second pass: %v", err)
	}
	if pushed != 0 {
		t.Errorf("pushed %d times on an unchanged pass, want none", pushed)
	}
}

// TestEnsureMetadataPushesAPortFileChange: a pass that leaves metadata.json
// alone but rewrites dolt-server.port changes a file bd reads, so it pushes.
func TestEnsureMetadataPushesAPortFileChange(t *testing.T) {
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
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the first pass: %v", err)
	}
	// A wrong port with current metadata takes the fast path, writes the file
	// and pushes the change.
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushed = 0
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the port-file pass: %v", err)
	}
	if pushed != 1 {
		t.Errorf("pushed %d times after a port-file change, want one", pushed)
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

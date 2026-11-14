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
		workspace: &workspaceState{},
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

// TestEnsureMetadataRetriesAFailedPush: a push the gateway missed is sent again
// by the next pass, even when that pass changes nothing; once a push lands, a
// pass over the same files pushes nothing.
func TestEnsureMetadataRetriesAFailedPush(t *testing.T) {
	t.Parallel()
	pushed := 0
	failures := 1
	h := &host{
		workspace: &workspaceState{},
		pushWorkspace: func(context.Context, string) (string, error) {
			pushed++
			if failures > 0 {
				failures--
				return "", errors.New("the gateway at gw:7440 did not answer")
			}
			return "manifest", nil
		},
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The first pass writes the files and its push fails.
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err == nil {
		t.Fatal("the first pass succeeded with a failed push")
	}
	// The second pass changes nothing, so it must still push the tree the
	// gateway did not get.
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the retry pass: %v", err)
	}
	if pushed != 2 {
		t.Fatalf("pushed %d times after a failed push and a retry, want two", pushed)
	}
	// The retry landed, so a third pass over the same files does not push.
	if err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the pass after the retry: %v", err)
	}
	if pushed != 2 {
		t.Errorf("pushed %d times after a landed push on an unchanged pass, want still two", pushed)
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

// TestFreshWorkspaceRecordPushesOnTheFirstPass: a process that has pushed nothing
// knows nothing about the gateway's tree, so its first pass over files that are
// already current still pushes: a push the previous process missed before it
// restarted is not lost.
func TestFreshWorkspaceRecordPushesOnTheFirstPass(t *testing.T) {
	t.Parallel()
	if !newWorkspaceState().stale {
		t.Fatal("a fresh workspace record reads as current")
	}
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := newFakeHost().townNamesPort(DefaultPort).host()
	first.pushWorkspace = func(context.Context, string) (string, error) { return "m", nil }
	if err := first.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the first process: %v", err)
	}
	pushed := 0
	second := newFakeHost().townNamesPort(DefaultPort).host()
	second.workspace = newWorkspaceState()
	second.pushWorkspace = func(context.Context, string) (string, error) {
		pushed++
		return "m", nil
	}
	if err := second.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the restarted process: %v", err)
	}
	if pushed != 1 {
		t.Errorf("the restarted process pushed %d times over current files, want one", pushed)
	}
	if err := second.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq"); err != nil {
		t.Fatalf("the second pass: %v", err)
	}
	if pushed != 1 {
		t.Errorf("a landed push was resent: %d pushes, want one", pushed)
	}
}

// TestEnsureMetadataMarksStaleWhenThePortFileChangedAndTheWriteFails: bd reads the
// port file, so once it changed the gateway's tree is out of date even if the
// metadata.json write after it fails.
func TestEnsureMetadataMarksStaleWhenThePortFileChangedAndTheWriteFails(t *testing.T) {
	t.Parallel()
	h := newFakeHost().townNamesPort(DefaultPort).host()
	h.workspace = &workspaceState{} // known current before the pass
	h.pushWorkspace = func(context.Context, string) (string, error) { return "m", nil }
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "metadata.json"), 0o755); err != nil {
		t.Fatal(err) // a directory where the file belongs: the write after the port file fails
	}
	err := h.EnsureMetadataForBeadsDir(townRoot, beadsDir, "hq")
	if err == nil {
		t.Skip("the metadata write did not fail on this layout, so the ordering cannot be driven")
	}
	if _, statErr := os.Stat(filepath.Join(beadsDir, "dolt-server.port")); statErr != nil {
		t.Skipf("the pass failed before the port file was written (%v): %v", err, statErr)
	}
	if !h.isWorkspaceStale() {
		t.Errorf("the port file changed and the metadata write failed (%v), but the workspace is not marked stale", err)
	}
}

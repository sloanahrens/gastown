package beads

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestShowMultipleKeepsGroupWrapperOptions: a routed group's wrapper comes
// from pinnedToBeadsDir, so it carries the parent's isolation and server port
// and resolves its workDir through the shared missing-parent fallback.
// Hand-rolling the wrapper (workDir: filepath.Dir(targetDir) with no
// isolated/serverPort/townRoot) made a group whose rig directory routes.jsonl
// names but that was never checked out locally chdir into a directory that
// does not exist — "fork/exec: no such file or directory" — which failed the
// whole merge-request hydration instead of that one group (gt-m0fvn).
func TestShowMultipleKeepsGroupWrapperOptions(t *testing.T) {
	t.Parallel()
	townRoot, rigDir := newTestTown(t)

	// One rig checked out locally and one named in routes.jsonl but never
	// created, so its group's beadsDir has no existing parent.
	existingRig := filepath.Join(townRoot, "checked-out-rig")
	if err := os.MkdirAll(existingRig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRoutes(filepath.Join(townRoot, ".beads"), []Route{
		{Prefix: "yy-", Path: "checked-out-rig"},
		{Prefix: "zz-", Path: "aliased-rig-never-checked-out"},
	}); err != nil {
		t.Fatal(err)
	}

	const port = 3399
	r := newRecorder(func(args []string) reply {
		id := args[len(args)-1]
		return reply{stdout: `[{"id":"` + id + `","title":"` + id + `"}]`}
	})
	b := newBeads(beadsFields{workDir: rigDir, isolated: true, serverPort: port, exec: r.exec})

	got, err := b.ShowMultiple([]string{"yy-a", "zz-b"})
	if err != nil {
		t.Fatalf("ShowMultiple: %v", err)
	}
	if got["yy-a"] == nil || got["zz-b"] == nil {
		t.Fatalf("ShowMultiple = %v, want both yy-a and zz-b", got)
	}

	// Each routed group ran in its own wrapper: cwd is the checked-out rig for
	// yy-, the town root for the aliased rig whose parent is missing.
	wantDir := map[string]string{
		filepath.Join(existingRig, ".beads"):                               existingRig,
		filepath.Join(townRoot, "aliased-rig-never-checked-out", ".beads"): townRoot,
	}
	seen := 0
	for _, c := range r.calls() {
		beadsDir, ok := lastEnvValue(c.env, "BEADS_DIR")
		if !ok {
			continue
		}
		want, routed := wantDir[beadsDir]
		if !routed {
			t.Fatalf("bd call for beadsDir %q, want one of the two routed groups", beadsDir)
		}
		seen++
		if c.dir != want {
			t.Errorf("group %s ran with cwd %q, want %q", beadsDir, c.dir, want)
		}
		if v, _ := lastEnvValue(c.env, "GT_DOLT_PORT"); v != strconv.Itoa(port) {
			t.Errorf("group %s: GT_DOLT_PORT = %q, want %d (group wrapper lost isolated/serverPort)", beadsDir, v, port)
		}
		if v, _ := lastEnvValue(c.env, "BEADS_DOLT_SERVER_PORT"); v != strconv.Itoa(port) {
			t.Errorf("group %s: BEADS_DOLT_SERVER_PORT = %q, want %d", beadsDir, v, port)
		}
	}
	if seen != 2 {
		t.Errorf("saw %d calls carrying a routed BEADS_DIR, want 2", seen)
	}
}

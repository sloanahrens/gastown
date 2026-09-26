package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRawTrackedDeps_RealBdSchema pins the raw-edge record schema to the bd
// binary itself (gt-r12y). The rest of the suite drives rawTrackedDeps through
// a stub that hand-writes depends_on_id and type, so it agrees with the parser
// by construction: a bd release that renames either field leaves those tests
// green while cross-rig convoys render 0/0 again (gt-44z1).
//
// The workspace is a real embedded bd database, and the edges are the
// production shape — a convoy's `tracks` edge to another rig, whose target
// cannot resolve locally, plus a local `blocks` edge that must not become a
// tracked issue.
func TestRawTrackedDeps_RealBdSchema(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not installed, skipping real-schema test")
	}

	ws := t.TempDir()
	runRealBd(t, bdPath, ws, "init", "--prefix", "zz", "--quiet", "--non-interactive")
	convoy := createRealBdIssue(t, bdPath, ws, "convoy")
	blocked := createRealBdIssue(t, bdPath, ws, "not tracked")
	runRealBd(t, bdPath, ws, "dep", "add", convoy, "external:om:om-target", "-t", "tracks", "--quiet")
	runRealBd(t, bdPath, ws, "dep", "add", convoy, blocked, "--quiet")

	// Assert the record fields the parser reads directly, so a rename names the
	// missing field instead of surfacing as an empty result.
	raw := runRealBd(t, bdPath, ws, "dep", "list", convoy, convoy, "--json")
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("parsing bd's raw-edge output: %v\n%s", err, raw)
	}
	if len(records) != 2 {
		t.Fatalf("raw-edge records = %d, want 2 (one tracks, one blocks)\n%s", len(records), raw)
	}
	for i, rec := range records {
		for _, field := range []string{"depends_on_id", "type"} {
			if _, ok := rec[field]; !ok {
				t.Fatalf("raw-edge record %d carries no %q; bd changed its record schema\n%s", i, field, raw)
			}
		}
	}

	// Then through the production path, which is where a rename degrades
	// silently to the join-based fallbacks instead of failing.
	f := &LiveConvoyFetcher{townRoot: ws, cmdTimeout: 30 * time.Second, bdBin: bdPath}
	deps, err := f.rawTrackedDeps(convoy)
	if err != nil {
		t.Fatalf("rawTrackedDeps against real bd: %v", err)
	}
	got := make([]string, 0, len(deps))
	for _, dep := range deps {
		got = append(got, dep.ID)
	}
	if want := []string{"om-target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked ids = %q, want %q (the blocks edge must not become a tracked issue)", got, want)
	}
}

// TestRawTrackedDeps_RefusesSchemaDrift covers the other half of the pin: when
// bd's raw-edge records stop carrying the fields the parser reads, the fetch
// reports it rather than returning no edges. An empty slice reads to the caller
// as "this convoy tracks nothing" and hands the row to the join-based
// fallbacks, which drop cross-database edges — the 0/0 render gt-44z1 fixed,
// with nothing left in the log to explain the regression.
//
// The payload is bd show's dependency shape (id, dependency_type): the same
// edges described with different field names (see bdShowTrackedDeps), and so
// the plausible rename.
func TestRawTrackedDeps_RefusesSchemaDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
echo '[{"issue_id":"hq-cv-7rzqg","id":"external:om:om-59p","dependency_type":"tracks"}]'
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath}
	_, err := f.rawTrackedDeps("hq-cv-7rzqg")
	if err == nil {
		t.Fatal("rawTrackedDeps accepted records carrying neither depends_on_id nor type, so a renamed bd field renders cross-rig convoys 0/0 unnoticed")
	}
	if !strings.Contains(err.Error(), "depends_on_id") {
		t.Fatalf("error = %v, want it to name the field bd stopped sending", err)
	}
}

// runRealBd runs the bd binary in dir and returns its stdout.
func runRealBd(t *testing.T, bdPath, dir string, args ...string) []byte {
	t.Helper()

	cmd := exec.Command(bdPath, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bd %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// createRealBdIssue creates one task in the workspace and returns its ID.
func createRealBdIssue(t *testing.T, bdPath, dir, title string) string {
	t.Helper()

	out := runRealBd(t, bdPath, dir, "create", "--title", title, "--type", "task", "--json")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatalf("parsing bd create output %q: %v", out, err)
	}
	if created.ID == "" {
		t.Fatalf("bd create returned no id: %s", out)
	}
	return created.ID
}

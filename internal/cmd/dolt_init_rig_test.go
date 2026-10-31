package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// `gt dolt init-rig` on a tracker rig returns without an error, leaves no
// database behind and says why: InitRig made none, so there is no location to
// report and "already exists" would name a database this rig never had
// (gt-p85pw.102).
func TestDoltInitRigTrackerRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"ag-","path":"agrig/mayor/rig","backend":"tracker"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := doltInitRig(&out, townRoot, "agrig", doltserver.InitRig); err != nil {
		t.Fatalf("doltInitRig: %v", err)
	}
	if _, err := os.Stat(doltserver.RigDatabaseDir(townRoot, "agrig")); !os.IsNotExist(err) {
		t.Errorf("database directory for a tracker rig: %v, want it absent", err)
	}
	if got := out.String(); !strings.Contains(got, "tracker backend") || !strings.Contains(got, "agrig") {
		t.Errorf("output = %q, want the reason the rig has no database here", got)
	}
}

// A town whose Dolt is on another host keeps its rig databases there: the
// location `gt dolt init-rig` reports is that server's host:port, and the
// local .dolt-data path (a directory this town never has) is not printed
// (gt-pbp1k).
func TestDoltInitRigRemoteTownLocation(t *testing.T) {
	t.Parallel()
	for _, created := range []bool{true, false} {
		name := "already exists"
		if created {
			name = "created"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			townRoot := townRootWithDoltEndpoint(t, "10.0.0.5", 3307)
			fakeInitRig := func(_, _ string) (doltserver.InitRigResult, error) {
				return doltserver.InitRigResult{Created: created, ServerWasRunning: true}, nil
			}

			var out bytes.Buffer
			if err := doltInitRig(&out, townRoot, "agrig", fakeInitRig); err != nil {
				t.Fatalf("doltInitRig: %v", err)
			}

			got := out.String()
			if !strings.Contains(got, "Location: 10.0.0.5:3307") {
				t.Errorf("output = %q, want the remote host and port in the location line", got)
			}
			if local := filepath.Join(townRoot, ".dolt-data"); strings.Contains(got, local) {
				t.Errorf("output = %q, want no local data directory path for a remote town", got)
			}
		})
	}
}

// A town whose Dolt is on this machine still reports the rig's directory in
// the local data directory.
func TestDoltInitRigLocalTownLocation(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	fakeInitRig := func(_, _ string) (doltserver.InitRigResult, error) {
		return doltserver.InitRigResult{Created: true}, nil
	}

	var out bytes.Buffer
	if err := doltInitRig(&out, townRoot, "agrig", fakeInitRig); err != nil {
		t.Fatalf("doltInitRig: %v", err)
	}

	got := out.String()
	rigDir := doltserver.RigDatabaseDir(townRoot, "agrig")
	if !strings.Contains(got, "Location: "+rigDir) {
		t.Errorf("output = %q, want the rig's local directory %q", got, rigDir)
	}
	if !strings.Contains(got, "Data dir: "+filepath.Join(townRoot, ".dolt-data")) {
		t.Errorf("output = %q, want the local data directory", got)
	}
}

// townRootWithDoltEndpoint builds the town config a remote town has: the Dolt
// endpoint in mayor/town.json is what doltserver.DefaultConfig resolves.
func townRootWithDoltEndpoint(t *testing.T, host string, port int) string {
	t.Helper()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"type":"town","version":2,"name":"t","dolt":{"host":%q,"port":%d}}`, host, port)
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

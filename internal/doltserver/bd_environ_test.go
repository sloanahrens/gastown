package doltserver

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A town that names its Dolt endpoint makes that endpoint the one its bd calls
// dial, whatever the inherited environment holds: the rig's beads dir need not
// sit under the town, so bd cannot apply the endpoint itself (gt-xnv94x).
func TestBdEnvironForNamedEndpointBeatsInheritedPort(t *testing.T) {
	t.Parallel()
	inherited := []string{"PATH=/usr/bin", "BEADS_DOLT_PORT=1", "BEADS_DOLT_SERVER_PORT=1"}
	named := t.TempDir()
	writeDoltTown(t, named, `"dolt":{"host":"127.0.0.1","port":4407}`)
	unnamed := t.TempDir()
	writeDoltTown(t, unnamed, "")

	for _, tc := range []struct {
		name, townRoot string
		want           []string
	}{
		{
			name:     "named endpoint replaces the inherited port",
			townRoot: named,
			want: []string{"PATH=/usr/bin", "BEADS_DOLT_SERVER_HOST=127.0.0.1",
				"BEADS_DOLT_SERVER_PORT=4407", "BEADS_DOLT_PORT=4407"},
		},
		{
			name:     "town with no endpoint leaves the environment untouched",
			townRoot: unnamed,
			want:     inherited,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &host{environList: func() []string { return inherited }}
			got := h.bdEnvironFor(tc.townRoot)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("bdEnvironFor(%s) = %v, want %v", filepath.Base(tc.townRoot), got, tc.want)
			}
		})
	}
}

// writeDoltTown writes mayor/town.json under root with extra appended to the
// town object, and returns root.
func writeDoltTown(t *testing.T, root, extra string) {
	t.Helper()
	town := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"`
	if extra != "" {
		town += "," + extra
	}
	town += "}"
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(town), 0o600); err != nil {
		t.Fatal(err)
	}
}

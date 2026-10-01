package rig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/wisp"
)

// opStateTown writes a town whose registry holds testrig, parked when
// parked is non-empty (the raw "parked" JSON value).
func opStateTown(t *testing.T, parked string) string {
	t.Helper()
	townRoot := t.TempDir()
	entry := `{"git_url":"x","added_at":"2026-09-07T00:00:00Z","beads":{"repo":"","prefix":"tr"}`
	if parked != "" {
		entry += `,"parked":` + parked
	}
	entry += `}`
	files := map[string]string{
		"mayor/town.json": `{"type":"town","version":2,"name":"gt","created_at":"2026-09-07T00:00:00Z"}`,
		"mayor/rigs.json": `{"version":1,"rigs":{"testrig":` + entry + `}}`,
	}
	for rel, body := range files {
		path := filepath.Join(townRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "testrig"), 0o755); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// TestGetOpState_ReadsRegistry covers what `gt rig park` writes: a parked
// record on the rig's mayor/rigs.json entry. No bead is read for a parked rig.
func TestGetOpState_ReadsRegistry(t *testing.T) {
	t.Parallel()
	townRoot := opStateTown(t, `{"since":"2026-09-30T00:00:00Z","by":"sloan","reason":"r"}`)
	state, source := getOpState(townRoot, "testrig", noRigBead())
	if state != OpStateParked || source != OpStateSourceRegistry {
		t.Errorf("GetOpState() = %q, %q; want %q, %q", state, source, OpStateParked, OpStateSourceRegistry)
	}
}

// TestGetOpState_UnreadableParkStateIsParked: the registry read fails
// closed, so a town whose rigs.json does not load reads every rig as parked.
func TestGetOpState_UnreadableParkStateIsParked(t *testing.T) {
	t.Parallel()
	townRoot := opStateTown(t, "")
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state, _ := getOpState(townRoot, "testrig", noRigBead()); state != OpStateParked {
		t.Errorf("GetOpState() state = %q, want %q", state, OpStateParked)
	}
}

// TestGetOpState_UnparkedRigIsOperational pins the default. A registered,
// unparked rig with no readable identity bead is operational: reading a
// failed or absent bead as docked would put a dead marker on a working rig,
// and the dashboard's whole use for this value is deciding what to mark.
func TestGetOpState_UnparkedRigIsOperational(t *testing.T) {
	t.Parallel()
	townRoot := opStateTown(t, "")
	state, source := getOpState(townRoot, "testrig", noRigBead())
	if state != OpStateOperational {
		t.Errorf("GetOpState() state = %q, want %q", state, OpStateOperational)
	}
	if source != OpStateSourceDefault {
		t.Errorf("GetOpState() source = %q, want %q", source, OpStateSourceDefault)
	}
}

// TestGetOpState_WispFileWithoutStatusIsNotParked: a wisp config file that
// records only unrelated keys is not a legacy park record.
func TestGetOpState_WispFileWithoutStatusIsNotParked(t *testing.T) {
	t.Parallel()
	townRoot := opStateTown(t, "")
	if err := wisp.NewConfig(townRoot, "testrig").Set("auto_restart", true); err != nil {
		t.Fatal(err)
	}
	if state, _ := getOpState(townRoot, "testrig", noRigBead()); state != OpStateOperational {
		t.Errorf("GetOpState() state = %q, want %q", state, OpStateOperational)
	}
}

// TestGetOpState_LegacyWispParkReadsParked: a rig parked by an older gt (a
// wisp "status" value) reads as parked until gt rig park --migrate.
func TestGetOpState_LegacyWispParkReadsParked(t *testing.T) {
	t.Parallel()
	townRoot := opStateTown(t, "")
	if err := wisp.NewConfig(townRoot, "testrig").Set("status", "parked"); err != nil {
		t.Fatal(err)
	}
	if state, _ := getOpState(townRoot, "testrig", noRigBead()); state != OpStateParked {
		t.Errorf("GetOpState() state = %q, want %q", state, OpStateParked)
	}
}

// TestOpState_Label is what the panels print: a lowercase marker word for a
// rig that takes no work, and nothing at all otherwise — a badge reading
// "operational" on every healthy rig would train the eye to skip it.
func TestOpState_Label(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state OpState
		want  string
	}{
		{OpStateOperational, ""},
		{OpStateParked, "parked"},
		{OpStateDocked, "docked"},
	}
	for _, tt := range tests {
		if got := tt.state.Label(); got != tt.want {
			t.Errorf("%q.Label() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// TestGetOpState_ReadsIdentityBeadLabel covers docked: the rig identity
// bead's status:docked label, read through bd. A status:parked label no
// longer parks a rig (gt-y3pgh.4): parked lives in the registry.
func TestGetOpState_ReadsIdentityBeadLabel(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		label     string
		wantState OpState
	}{
		{"status:parked", OpStateOperational},
		{"status:docked", OpStateDocked},
		{"priority:high", OpStateOperational},
	} {
		t.Run(tt.label, func(t *testing.T) {
			t.Parallel()
			townRoot := opStateTown(t, "")
			rigPath := filepath.Join(townRoot, "testrig")
			if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"type":"rig","name":"testrig","beads":{"prefix":"tr"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			db := beadsfake.New()
			db.Seed(beads.Issue{ID: "tr-rig-testrig", Title: "testrig", Labels: []string{tt.label}})

			state, source := getOpState(townRoot, "testrig", db)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if tt.wantState != OpStateOperational && source != OpStateSourceGlobal {
				t.Errorf("source = %q, want %q", source, OpStateSourceGlobal)
			}
		})
	}
}

package rig

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/wisp"
)

// TestGetOpState_ReadsWispLayer covers what `gt rig park` actually writes: a
// wisp config keyed by rig name. The bead-label fallback underneath it needs a
// populated beads database and is exercised where it can be stubbed; here the
// point is that the local layer answers first and alone.
func TestGetOpState_ReadsWispLayer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    string
		wantState OpState
		wantSrc   string
	}{
		{name: "parked", status: RigStatusParked, wantState: OpStateParked, wantSrc: OpStateSourceLocal},
		{name: "docked", status: RigStatusDocked, wantState: OpStateDocked, wantSrc: OpStateSourceLocal},
		// `gt rig park` and `gt rig dock` write lowercase, but the CLI's
		// reader lowercases before comparing, so a hand-edited wisp file
		// must not read as operational.
		{name: "mixed case", status: "Parked", wantState: OpStateParked, wantSrc: OpStateSourceLocal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			if err := os.MkdirAll(filepath.Join(townRoot, "testrig"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := wisp.NewConfig(townRoot, "testrig").Set(RigStatusKey, tt.status); err != nil {
				t.Fatal(err)
			}

			state, source := getOpState(townRoot, "testrig", noRigBead)
			if state != tt.wantState {
				t.Errorf("GetOpState() state = %q, want %q", state, tt.wantState)
			}
			if source != tt.wantSrc {
				t.Errorf("GetOpState() source = %q, want %q", source, tt.wantSrc)
			}
		})
	}
}

// TestGetOpState_UnparkedRigIsOperational pins the default. A rig with no wisp
// entry and no readable identity bead is operational: reading a failed or
// absent bead as "parked" would put a dead marker on a working rig, and the
// dashboard's whole use for this value is deciding what to mark.
func TestGetOpState_UnparkedRigIsOperational(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "testrig"), 0o755); err != nil {
		t.Fatal(err)
	}

	state, source := getOpState(townRoot, "testrig", noRigBead)
	if state != OpStateOperational {
		t.Errorf("GetOpState() state = %q, want %q", state, OpStateOperational)
	}
	if source != OpStateSourceDefault {
		t.Errorf("GetOpState() source = %q, want %q", source, OpStateSourceDefault)
	}
}

// TestGetOpState_UnsetWispKeyIsNotParked guards the empty-string case: the
// wisp layer reports "" for a key it has never been given, which must fall
// through rather than match an empty status.
func TestGetOpState_UnsetWispKeyIsNotParked(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "testrig"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A wisp file that exists but records an unrelated key.
	if err := wisp.NewConfig(townRoot, "testrig").Set("auto_restart", true); err != nil {
		t.Fatal(err)
	}

	if state, _ := getOpState(townRoot, "testrig", noRigBead); state != OpStateOperational {
		t.Errorf("GetOpState() state = %q, want %q", state, OpStateOperational)
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

// TestGetOpState_ReadsIdentityBeadLabel covers the fallback under the wisp
// layer: with no wisp entry, a status label on the rig identity bead parks or
// docks the rig, read through bd.
func TestGetOpState_ReadsIdentityBeadLabel(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		label     string
		wantState OpState
	}{
		{"status:parked", OpStateParked},
		{"status:docked", OpStateDocked},
		{"priority:high", OpStateOperational},
	} {
		t.Run(tt.label, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			rigPath := filepath.Join(townRoot, "testrig")
			if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(`{"type":"rig","name":"testrig","beads":{"prefix":"tr"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			var shown []string
			run := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
				if bdVerb(c.Args) != "show" {
					return nil, nil, nil
				}
				shown = append(shown, c.Args[1])
				return []byte(`[{"id":"tr-rig-testrig","title":"testrig","status":"open","labels":["` + tt.label + `"]}]`), nil, nil
			}

			state, source := getOpState(townRoot, "testrig", run)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if tt.wantState != OpStateOperational && source != OpStateSourceGlobal {
				t.Errorf("source = %q, want %q", source, OpStateSourceGlobal)
			}
			if len(shown) != 1 || shown[0] != "tr-rig-testrig" {
				t.Errorf("bd show calls = %v, want one read of tr-rig-testrig", shown)
			}
		})
	}
}

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
)

// setupSessionSeatTown writes a minimal town whose "gastown" rig owns the named
// polecats and returns its root. Same shape as setupPolecatCapacityRig, plus
// the polecats a seat address needs to match.
func setupSessionSeatTown(t *testing.T, polecats ...string) string {
	t.Helper()

	// EvalSymlinks: macOS temp dirs resolve through /var → /private/var, and
	// workspace.Find compares the town root it returns against the forbidden
	// root as text.
	townRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := config.SaveTownConfig(filepath.Join(mayorDir, "town.json"), &config.TownConfig{
		Type:      "town",
		Version:   config.CurrentTownVersion,
		Name:      "session-seat-test",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write town config: %v", err)
	}
	if err := config.SaveRigsConfig(filepath.Join(mayorDir, "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "https://example.invalid/gastown.git"},
		},
	}); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}

	polecatsDir := filepath.Join(townRoot, "gastown", "polecats")
	if err := os.MkdirAll(polecatsDir, 0o755); err != nil {
		t.Fatalf("mkdir polecats: %v", err)
	}
	for _, p := range polecats {
		if err := os.MkdirAll(filepath.Join(polecatsDir, p), 0o755); err != nil {
			t.Fatalf("mkdir polecat %s: %v", p, err)
		}
	}

	return townRoot
}

// rigsIn looks rigs up in the town at townRoot.
func rigsIn(townRoot string) func(string) (string, *rig.Rig, error) {
	return func(rigName string) (string, *rig.Rig, error) { return getRigIn(townRoot, rigName) }
}

func TestResolveSessionSeatAcceptsPolecats(t *testing.T) {
	t.Parallel()
	townRoot := setupSessionSeatTown(t, "amber", "onyx")

	tests := []struct {
		address  string
		wantName string
	}{
		{"gastown/amber", "amber"},
		{"gastown/onyx", "onyx"},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			seat, err := resolveSessionSeatWith([]string{tt.address}, rigsIn(townRoot))
			if err != nil {
				t.Fatalf("resolveSessionSeat(%q) = %v, want a seat", tt.address, err)
			}
			if seat.Rig != "gastown" || seat.Name != tt.wantName {
				t.Errorf("resolveSessionSeat(%q) = {%s %s}, want {gastown %s}",
					tt.address, seat.Rig, seat.Name, tt.wantName)
			}
			if seat.Mgr == nil {
				t.Errorf("resolveSessionSeat(%q) returned a seat with no session manager", tt.address)
			}
		})
	}
}

// TestResolveSessionSeatRejectsUnknownName is the other half of the resolver's
// contract: a name that is neither a polecat of the rig nor one of its roles
// does not resolve, whatever verb asked.
func TestResolveSessionSeatRejectsUnknownName(t *testing.T) {
	t.Parallel()
	townRoot := setupSessionSeatTown(t, "amber")

	_, err := resolveSessionSeatWith([]string{"gastown/ghost"}, rigsIn(townRoot))
	if err == nil {
		t.Fatal("resolveSessionSeat(gastown/ghost) = no error, want not-found")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to name the missing polecat and say not found", err)
	}
}

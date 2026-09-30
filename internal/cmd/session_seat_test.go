package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

// setupSessionSeatTown writes a minimal town whose "gastown" rig owns the named
// polecats, then makes that town the process's working directory so
// workspace.FindFromCwd resolves it. Same shape as setupPolecatCapacityRig,
// plus the polecats a seat address needs to match.
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

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	return townRoot
}

func TestResolveSessionSeatAcceptsPolecats(t *testing.T) {
	setupSessionSeatTown(t, "amber", "onyx")

	tests := []struct {
		address  string
		wantName string
	}{
		{"gastown/amber", "amber"},
		{"gastown/onyx", "onyx"},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			seat, err := resolveSessionSeat([]string{tt.address})
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
	setupSessionSeatTown(t, "amber")

	_, err := resolveSessionSeat([]string{"gastown/ghost"})
	if err == nil {
		t.Fatal("resolveSessionSeat(gastown/ghost) = no error, want not-found")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to name the missing polecat and say not found", err)
	}
}

// sessionAddressVerbs are the subcommands that take a <rig>/<name> address, in
// the order a caller meets them.
func sessionAddressVerbs() []struct {
	name string
	run  func(*cobra.Command, []string) error
} {
	return []struct {
		name string
		run  func(*cobra.Command, []string) error
	}{
		{"start", runSessionStart},
		{"restart", runSessionRestart},
		{"status", runSessionStatus},
		{"attach", runSessionAttach},
		{"capture", runSessionCapture},
		{"inject", runSessionInject},
	}
}

// TestSessionVerbsAgreeOnAnUnknownPolecat pins the contract this fix exists
// for (gt-pud2g): one address gets one answer, so a caller cannot read success
// or a live-but-stopped polecat out of a seat that does not exist.
//
// Before the shared resolver, `gt session restart` printed
// "✓ Session restarted. Attach with: …" and `gt session status` printed
// "State: ○ stopped" (exit 0) for a name `gt session start` refused as not
// found. A witness recovering a stalled polecat reads the restart line and
// moves on, leaving no session and nobody watching it.
func TestSessionVerbsAgreeOnAnUnknownPolecat(t *testing.T) {
	setupSessionSeatTown(t, "amber")

	// inject refuses without a message before it resolves anything; give it
	// one so the not-found answer is what the verb is measured on.
	oldMessage := sessionMessage
	sessionMessage = "hello"
	t.Cleanup(func() { sessionMessage = oldMessage })

	var first string
	for _, verb := range sessionAddressVerbs() {
		t.Run(verb.name, func(t *testing.T) {
			var err error
			stdout, _ := captureStdio(t, func() {
				err = verb.run(nil, []string{"gastown/ghost"})
			})

			if err == nil {
				t.Fatalf("gt session %s gastown/ghost = success, want not-found", verb.name)
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("gt session %s error = %q, want not-found", verb.name, err)
			}
			if stdout != "" {
				t.Errorf("gt session %s printed %q on a refused address, want nothing", verb.name, stdout)
			}

			if first == "" {
				first = err.Error()
				return
			}
			if err.Error() != first {
				t.Errorf("gt session %s answered %q, but an earlier verb answered %q; one address, one answer",
					verb.name, err, first)
			}
		})
	}
}

//go:build integration

package mail

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/testutil"
)

// Against a real bd on the test Dolt container.

func TestIntegrationValidateRecipient(t *testing.T) {
	// Skip if bd CLI is not available or not functional (e.g., missing DLLs on Windows CI)
	if out, err := exec.Command("bd", "version").CombinedOutput(); err != nil {
		t.Skipf("bd CLI not functional, skipping test: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	// Start an ephemeral Dolt container to prevent bd init from creating
	// databases on the production server (port 3307).
	testutil.RequireDoltContainer(t)
	doltPort, _ := strconv.Atoi(testutil.DoltContainerPort())

	// Create isolated beads environment for testing
	tmpDir := t.TempDir()
	townRoot := tmpDir

	// Create .beads directory and initialize
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("creating beads dir: %v", err)
	}

	// Use beads.NewIsolatedWithPort with a unique random prefix to avoid Dolt
	// primary key collisions with production beads (e.g., gt-mayor).
	// NewIsolatedWithPort directs bd init to the ephemeral server via
	// --server-port and GT_DOLT_PORT, and uses --db flag for subsequent
	// commands (bypassing Dolt). We set BEADS_DB so that the Router's
	// external bd calls also use the same isolated SQLite database.
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	prefix := "vr" + hex.EncodeToString(buf[:])
	b := beads.NewIsolatedWithPort(townRoot, doltPort)
	if err := b.Init(prefix); err != nil {
		t.Fatalf("bd init: %v", err)
	}

	// Point BEADS_DB at the isolated SQLite file so the Router's
	// runBdCommand (which inherits process env) uses it too.
	beadsDB := filepath.Join(beadsDir, "beads.db")
	t.Setenv("BEADS_DB", beadsDB)

	// Register type config required for agent beads.
	if _, err := b.Run("config", "set", "types.custom", constants.BeadsCustomTypes); err != nil {
		t.Fatalf("config set types.custom: %v", err)
	}
	if _, err := b.Run("config", "set", "types.infra", constants.BeadsInfraTypes); err != nil {
		t.Fatalf("config set types.infra: %v", err)
	}

	// Create test agent beads with gt:agent label.
	// Safe to use "gt-" prefixed IDs since both NewIsolated (--db) and the
	// Router (BEADS_DB env) point to the same local SQLite database.
	createAgent := func(id, title string) {
		if _, err := b.Run("create", title, "--labels=gt:agent", "--id="+id, "--force"); err != nil {
			t.Fatalf("creating agent %s: %v", id, err)
		}
	}

	createAgent("gt-mayor", "Mayor agent")
	createAgent("gt-deacon", "Deacon agent")
	createAgent("gt-testrig-witness", "Test witness")
	createAgent("gt-testrig-crew-alice", "Test crew alice")
	createAgent("gt-testrig-polecat-bob", "Test polecat bob")

	// Create dog directory for workspace fallback validation (deacon/dogs/fido).
	// The workspace fallback handles cases where agent beads are missing or
	// the bead DB is unavailable (e.g., after Dolt reset).
	dogDir := filepath.Join(townRoot, "deacon", "dogs", "fido")
	if err := os.MkdirAll(dogDir, 0755); err != nil {
		t.Fatalf("creating dog dir: %v", err)
	}

	r := NewRouterWithTownRoot(townRoot, townRoot)

	tests := []struct {
		name     string
		identity string
		wantErr  bool
		errMsg   string
	}{
		// Overseer is always valid (human operator, no agent bead)
		{"overseer", "overseer", false, ""},

		// Town-level agents (validated against beads)
		{"mayor", "mayor/", false, ""},
		{"deacon", "deacon/", false, ""},

		// Rig-level agents (validated against beads)
		{"witness", "testrig/witness", false, ""},
		{"crew member", "testrig/alice", false, ""},
		{"polecat", "testrig/bob", false, ""},

		// Dog agents (validated via workspace fallback: deacon/dogs/<name> directory)
		{"dog agent", "deacon/dogs/fido", false, ""},

		// Invalid addresses - should fail
		{"bare name", "ruby", true, "no agent found"},
		{"nonexistent rig agent", "testrig/nonexistent", true, "no agent found"},
		{"wrong rig", "wrongrig/alice", true, "no agent found"},
		{"misrouted town agent", "testrig/mayor", true, "no agent found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.validateRecipient(tt.identity)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateRecipient(%q) expected error, got nil", tt.identity)
				} else if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("validateRecipient(%q) error = %v, want containing %q", tt.identity, err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateRecipient(%q) unexpected error: %v", tt.identity, err)
				}
			}
		})
	}
}

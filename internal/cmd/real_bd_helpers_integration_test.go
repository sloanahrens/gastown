//go:build integration

package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

func requireBd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Fatalf("bd CLI not on PATH: %v", err)
	}
}

// setupPatrolTestDB runs bd init in a fresh directory against a database
// leased from the shared Dolt test container, and returns the directory and
// an isolated client for it.
func setupPatrolTestDB(t *testing.T) (string, *beads.Beads) {
	t.Helper()
	testutil.RequireDoltContainer(t)
	port, _ := strconv.Atoi(testutil.DoltContainerPort())
	tmpDir := t.TempDir()
	b := beads.NewIsolatedWithPort(tmpDir, port)
	// Use a unique prefix per test run to avoid cross-run contamination
	// in the shared Dolt database.
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	prefix := "pt" + hex.EncodeToString(buf[:])
	if err := b.Init(prefix); err != nil {
		testutil.FailContainerInit(t, b, err)
	}

	// No cleanup: Init took a database from the container's pre-created
	// pool, and a DROP while other tests run would break their migrations
	// (testutil/doltpool.go). The database goes with the container.

	return tmpDir, b
}

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

// requireUpdateFlag skips the test when the bd serving b does not offer flag on
// bd update. The sweep image's bd is built at the committed pin and moves only
// with an image pair; a pair cannot publish while a sweep is red, so a test that
// needs a flag newer than that bd would hold the sweep red until the pair it
// waits for exists (gt-8mvpvz). The skip names the bd found and the beads
// commit the test needs, so the reason is in the log, not a dropped test.
func requireUpdateFlag(t *testing.T, b *beads.Beads, flag, neededBeadsCommit string) {
	t.Helper()
	offers, err := b.UpdateOffersFlag(flag)
	if err != nil {
		t.Fatalf("asking bd update --help for %s: %v", flag, err)
	}
	if offers {
		return
	}
	version, _ := b.Version()
	t.Skipf("bd %s (%s) offers no %s on bd update; the test needs beads %s, which the next image pair carries",
		version.Version, version.Commit, flag, neededBeadsCommit)
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
	if err := beads.InitForTest(b, prefix); err != nil {
		testutil.FailContainerInit(t, b, err)
	}

	// No cleanup: Init took a database from the container's pre-created
	// pool, and a DROP while other tests run would break their migrations
	// (testutil/doltpool.go). The database goes with the container.

	return tmpDir, b
}

package testutil

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// containerInitOutcome is what a failed container-backed bd init means for the
// test that made it. The zero value is containerInitFailed, so an outcome that
// is never classified fails the test rather than skipping it.
type containerInitOutcome int

const (
	containerInitFailed containerInitOutcome = iota
	containerInitGone
)

// String names the outcome, so an assertion failure reads as the decision it
// is rather than as 0 or 1.
func (o containerInitOutcome) String() string {
	switch o {
	case containerInitFailed:
		return "initFailed"
	case containerInitGone:
		return "containerGone"
	}
	return fmt.Sprintf("containerInitOutcome(%d)", int(o))
}

// classifyContainerInit separates the one transient condition from every
// regression: the container this test pinned is gone, or bd said something
// else.
func classifyContainerInit(b *beads.Beads, err error) containerInitOutcome {
	if b != nil && b.ContainerUnavailable(err) {
		return containerInitGone
	}
	return containerInitFailed
}

// SkipOrFailContainerInit skips t when b's init failed because the shared test
// Dolt container is gone, and fails t for every other error — a blanket skip on
// any error leaves the suite green everywhere while hiding exactly the
// regressions it exists to catch (gt-cbtl).
func SkipOrFailContainerInit(t *testing.T, b *beads.Beads, err error) {
	t.Helper()
	if classifyContainerInit(b, err) == containerInitGone {
		t.Skipf("test Dolt container gone, skipping: %v", err)
	}
	t.Fatalf("bd init failed: %v", err)
}

// DropTestDatabaseOnCleanup drops the database b's isolated Init created on the
// shared test Dolt container when t ends. The drop is a catalog change, so it
// runs under the exclusive side of the catalog gate
// (beads.ReleaseTestDatabase, batched) rather than racing the bd calls of the tests still
// running.
func DropTestDatabaseOnCleanup(t testing.TB, b *beads.Beads) {
	t.Helper()
	name, port := b.TestDatabaseName(), b.TestServerPort()
	if name == "" || port == 0 {
		t.Fatalf("DropTestDatabaseOnCleanup: no isolated test database to drop (name=%q port=%d); call it after a successful Init", name, port)
	}
	t.Cleanup(func() {
		if err := beads.ReleaseTestDatabase(port, name); err != nil {
			t.Logf("cleanup: drop test database %s: %v", name, err)
		}
	})
}

// ChangeDoltCatalog runs statements (CREATE/DROP DATABASE and the like) on the
// shared test Dolt container under the exclusive side of the catalog gate, so
// they cannot fail a concurrent test's bd store open or migration.
func ChangeDoltCatalog(t testing.TB, statements ...string) error {
	t.Helper()
	port, err := strconv.Atoi(DoltContainerPort())
	if err != nil {
		return fmt.Errorf("test Dolt container port %q: %w", DoltContainerPort(), err)
	}
	return beads.ExecTestCatalogDDL(port, statements...)
}

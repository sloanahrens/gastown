package testutil

import (
	"fmt"
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

// FailContainerInit fails t on a container-backed bd init error, saying
// whether the shared test Dolt container was gone or bd answered. Both fail:
// the caller opted in to the container tests (GT_TEST_DOCKER=1), so a container
// that went away is lost coverage, not an absence of evidence to skip on — and
// a blanket skip leaves the suite green while hiding exactly the regressions it
// exists to catch (gt-cbtl).
func FailContainerInit(t *testing.T, b *beads.Beads, err error) {
	t.Helper()
	if classifyContainerInit(b, err) == containerInitGone {
		t.Fatalf("bd init failed: the test Dolt container is gone: %v", err)
	}
	t.Fatalf("bd init failed: %v", err)
}

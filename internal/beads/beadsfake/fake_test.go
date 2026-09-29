package beadsfake

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestFakeClientContract(t *testing.T) {
	t.Parallel()
	RunClientContract(t, func(t *testing.T) beads.Client { return New() })
}

func TestFakeAdminContract(t *testing.T) {
	t.Parallel()
	RunAdminContract(t, func(t *testing.T) AdminClient { return New() })
}

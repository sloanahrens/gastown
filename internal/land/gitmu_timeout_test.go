package land

import (
	"testing"
	"time"
)

// The handle Lander.open builds bounds every call that sets no deadline of its
// own, the worktree add, remove and prune among them, at the two minute
// default. Without it a hung git would hold gitMu, the lock every landing and
// ahead review share, for ever (gt-c36zsm).
func TestGitMuTimeoutDefault(t *testing.T) {
	t.Parallel()
	l := &Lander{}
	g := l.open(t.TempDir())
	timed, ok := g.(interface{ Timeout() time.Duration })
	if !ok {
		t.Fatalf("the handle Lander.open builds (%T) reports no default timeout", g)
	}
	if got := timed.Timeout(); got != 2*time.Minute {
		t.Errorf("handle default timeout = %v, want %v", got, 2*time.Minute)
	}
}

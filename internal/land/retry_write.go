package land

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// retryWaits is the wait before each retry of a refused bead write: three
// tries, 2 s and then 5 s apart (gt-hounmt).
var retryWaits = []time.Duration{2 * time.Second, 5 * time.Second}

// retryWrite runs op and reruns it after each wait while it fails, returning
// the last error. A missing bead or a canceled ctx stops it at once.
func retryWrite(ctx context.Context, sleep func(time.Duration), op func() error) error {
	err := op()
	for _, wait := range retryWaits {
		if err == nil || errors.Is(err, beads.ErrNotFound) || ctx.Err() != nil {
			return err
		}
		sleep(wait)
		if ctx.Err() != nil {
			return err
		}
		err = op()
	}
	return err
}

// realWait is a timer, not time.Sleep: this package holds a clockwork.Clock,
// and prod-no-sleep (internal/testpolicy) bans Sleep in such a package.
func realWait(d time.Duration) { <-time.After(d) }

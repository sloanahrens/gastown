// Package beadstest holds beads.Client wrappers that tests in more than one
// package share.
package beadstest

import (
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// Counting wraps a Client and counts its List calls, keeping each read's
// created-after cutoff, so a test can price the reads a caller makes.
type Counting struct {
	beads.Client
	Lists   int
	Cutoffs []time.Time
}

// List counts the read and passes it to the wrapped client.
func (c *Counting) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.Lists++
	c.Cutoffs = append(c.Cutoffs, opts.CreatedAfter)
	return c.Client.List(opts)
}

// Failing is a Client whose every read fails.
type Failing struct{ beads.Client }

// List fails the read.
func (Failing) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("bd: read failed")
}

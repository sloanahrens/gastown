package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/seatship"
)

// The bounds a seat's record is read with: the most one outbox file may hold,
// and the transcript tail kept when the seat did not submit.
const (
	seatShipOutboxCap = 64 << 10
	seatShipTailBytes = 64 << 10
)

// seatShipDeps is what runSeatShip reads and writes, so a test supplies each:
// the bead store the record is posted to, the seat's Claude projects and
// outbox directories, the time the whole run may take, and where a failure is
// written.
type seatShipDeps struct {
	Beads    beads.Client
	Projects string
	Outbox   string
	Deadline time.Duration
	Stderr   io.Writer
}

// runSeatShip is gt seat ship: a finished seat posts its record as one SEAT-END
// comment on its bead, and the sandbox can exit instead of waiting for the
// daemon to pull the record out (gt-7h8s5r). It returns the exit code: 0 once
// the comment is posted, 1 with one `seat ship: <reason>` line on d.Stderr for
// any failure. Nothing is retried.
func runSeatShip(ctx context.Context, d seatShipDeps, rig, polecat, bead string) int {
	fail := func(err error) int {
		reason := strings.Join(strings.Fields(err.Error()), " ")
		_, _ = fmt.Fprintf(d.Stderr, "seat ship: %s\n", reason)
		return 1
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	submitted, err := seatSubmitted(d.Beads, bead)
	if err != nil {
		return fail(fmt.Errorf("reading %s: %w", bead, err))
	}
	payload, err := seatship.Collect(seatship.CollectInput{
		Rig:         rig,
		Polecat:     polecat,
		Bead:        bead,
		Submitted:   submitted,
		ProjectsDir: d.Projects,
		OutboxDir:   d.Outbox,
		OutboxCap:   seatShipOutboxCap,
		TailBytes:   seatShipTailBytes,
	})
	if err != nil {
		return fail(err)
	}
	comment, err := seatship.Encode(payload)
	if err != nil {
		return fail(err)
	}
	if err := d.Beads.AddComment(bead, comment); err != nil {
		return fail(fmt.Errorf("posting the record on %s: %w", bead, err))
	}
	return 0
}

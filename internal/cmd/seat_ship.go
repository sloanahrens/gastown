package cmd

import (
	"context"
	"errors"
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
// any failure. Nothing is retried. The whole run, reads and post, must finish
// within d.Deadline.
func runSeatShip(ctx context.Context, d seatShipDeps, rig, polecat, bead string) int {
	fail := func(err error) int {
		reason := strings.Join(strings.Fields(err.Error()), " ")
		_, _ = fmt.Fprintf(d.Stderr, "seat ship: %s\n", reason)
		return 1
	}
	if d.Deadline <= 0 {
		return fail(fmt.Errorf("deadline %s is not positive", d.Deadline))
	}
	if err := ctx.Err(); err != nil {
		return fail(seatShipStopped(ctx, d.Deadline))
	}
	// The bead store's calls take no context, so a call that hangs cannot be
	// canceled. The run goes in its own goroutine and this function waits on
	// the deadline instead. The channel holds one outcome, so a run that
	// finishes after the deadline sends and exits with no reader. Only the
	// calling goroutine writes to d.Stderr.
	runCtx, cancel := context.WithTimeout(ctx, d.Deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- seatShipRecord(d, rig, polecat, bead) }()
	select {
	case err := <-done:
		if err != nil {
			return fail(err)
		}
		return 0
	case <-runCtx.Done():
		return fail(seatShipStopped(ctx, d.Deadline))
	}
}

// seatShipStopped is why a run ended before its record was posted. A canceled
// parent context is reported as canceled; only an expired deadline names the
// deadline.
func seatShipStopped(ctx context.Context, deadline time.Duration) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return errors.New("canceled")
	}
	return fmt.Errorf("deadline %s passed before the record was posted", deadline)
}

// seatShipRecord reads the seat's record and posts it as the bead's SEAT-END
// comment. It returns the first failure, wrapped with the step it came from.
func seatShipRecord(d seatShipDeps, rig, polecat, bead string) error {
	submitted, err := seatSubmitted(d.Beads, bead)
	if err != nil {
		return fmt.Errorf("reading %s: %w", bead, err)
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
		return fmt.Errorf("collecting %s: %w", bead, err)
	}
	comment, err := seatship.Encode(payload)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", bead, err)
	}
	if err := d.Beads.AddComment(bead, comment); err != nil {
		return fmt.Errorf("posting the record on %s: %w", bead, err)
	}
	return nil
}

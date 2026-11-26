package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/session"
)

// seatShipDeadline is how long gt seat ship may take: the seat's sandbox waits
// for it before it exits, so a hung bead store must not hold the seat up. It is
// the default of the --deadline flag.
const seatShipDeadline = 60 * time.Second

func init() {
	seatCmd.AddCommand(newSeatShipCmd(openSeatShipClient, runSeatShip))
}

// openSeatShipClient is the beads client for the directory gt acts for: the one
// reader of it is internal/callercwd, the source gt done reads too.
func openSeatShipClient() (beads.Client, error) {
	return openSeatShipClientFrom(callercwd.Getwd)
}

// openSeatShipClientFrom opens the client for the directory getwd names; a
// getwd that fails is the error, never a client for the wrong directory.
func openSeatShipClientFrom(getwd func() (string, error)) (beads.Client, error) {
	cwd, err := getwd()
	if err != nil {
		return nil, fmt.Errorf("seat ship: the working directory: %w", err)
	}
	return beads.New(cwd), nil
}

// newSeatShipCmd builds gt seat ship over the two seams a test replaces: how the
// beads client is opened and the function that does the work. A command built
// per test keeps flag state out of the shared one.
func newSeatShipCmd(
	open func() (beads.Client, error),
	run func(ctx context.Context, d seatShipDeps, rig, polecat, bead string) int,
) *cobra.Command {
	var rig, polecat, bead, projects, outbox string
	var deadline time.Duration
	c := &cobra.Command{
		Use:   "ship",
		Short: "Post this seat's record as one comment on its bead",
		Long: `Post this seat's record as one comment on its bead.

A finished seat runs this so its sandbox can exit at once: the record (its usage,
its outbox files, and its transcript tail when it did not submit) travels in one
SEAT-END comment on the bead instead of waiting for the daemon to pull it out.
It prints one "seat ship: <reason>" line and exits 1 when it cannot post.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deadline <= 0 {
				return fmt.Errorf("--deadline must be positive, got %s", deadline)
			}
			client, err := open()
			if err != nil {
				return err
			}
			deps := seatShipDeps{
				Beads:    client,
				Projects: projects,
				Outbox:   outbox,
				Deadline: deadline,
				Stderr:   cmd.ErrOrStderr(),
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if code := run(ctx, deps, rig, polecat, bead); code != 0 {
				return NewSilentExit(code)
			}
			return nil
		},
	}
	c.Flags().StringVar(&rig, "rig", "", "The rig the seat works in")
	c.Flags().StringVar(&polecat, "polecat", "", "The seat's polecat name")
	c.Flags().StringVar(&bead, "bead", "", "The bead the seat worked on")
	c.Flags().StringVar(&projects, "projects", session.SandboxProjectsDir, "The seat's Claude projects directory")
	c.Flags().StringVar(&outbox, "outbox", session.SandboxOutboxDir, "The seat's outbox directory")
	c.Flags().DurationVar(&deadline, "deadline", seatShipDeadline, "How long the command may take, e.g. 30s (must be positive)")
	for _, name := range []string{"rig", "polecat", "bead"} {
		_ = c.MarkFlagRequired(name)
	}
	return c
}

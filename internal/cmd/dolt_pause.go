package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	doltPauseReason string
	doltPauseUntil  time.Duration
)

var doltPauseCmd = &cobra.Command{
	Use:   "pause --reason <why> --until <duration>",
	Short: "Mark the Dolt server deliberately paused",
	Long: `Write the Dolt pause marker, daemon/dolt.pause {actor, reason, until}.

The owner of a deliberate outage (full GC, the nightly backup, an operator)
writes it before stopping or busying the server. While it holds:
  - the daemon neither probes, starts nor restarts the server
  - gt dolt start and gt up refuse to start it
  - a gt client whose Dolt call fails reports
    "Dolt paused by <actor> until <t>: <reason>"

The marker lapses on its own at --until (at most 24h ahead), so a crashed
owner cannot pause the town forever. This command only writes the marker; it
does not stop the server. Lift it with 'gt dolt unpause'.

The actor is BD_ACTOR, else git user.name.`,
	Example: `  gt dolt pause --reason "full gc" --until 30m`,
	Args:    cobra.NoArgs,
	RunE:    runDoltPause,
}

var doltUnpauseCmd = &cobra.Command{
	Use:   "unpause",
	Short: "Remove the Dolt pause marker",
	Long: `Remove daemon/dolt.pause. The daemon resumes managing the server on its
next health tick; start it now with 'gt dolt start' if it is down.`,
	Args: cobra.NoArgs,
	RunE: runDoltUnpause,
}

func init() {
	doltPauseCmd.Flags().StringVar(&doltPauseReason, "reason", "", "Why Dolt is paused (required)")
	doltPauseCmd.Flags().DurationVar(&doltPauseUntil, "until", 0, "How long the pause holds, e.g. 30m (required, at most 24h)")
	_ = doltPauseCmd.MarkFlagRequired("reason")
	_ = doltPauseCmd.MarkFlagRequired("until")
	doltCmd.AddCommand(doltPauseCmd)
	doltCmd.AddCommand(doltUnpauseCmd)
}

func runDoltPause(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	actor := pauseActor(os.Getenv, func() string {
		name, _ := git.NewGit(townRoot).ConfigGet("user.name")
		return name
	})
	m, err := newPauseMarker(actor, doltPauseReason, doltPauseUntil, time.Now())
	if err != nil {
		return err
	}
	if err := doltpause.Write(townRoot, m); err != nil {
		return fmt.Errorf("writing pause marker: %w", err)
	}
	fmt.Printf("%s %s\n", style.Bold.Render("⏸"), m.Message())
	return nil
}

func runDoltUnpause(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	had, err := doltpause.Remove(townRoot)
	if err != nil {
		return fmt.Errorf("removing pause marker: %w", err)
	}
	if !had {
		fmt.Println("Dolt was not paused")
		return nil
	}
	fmt.Printf("%s Dolt pause lifted\n", style.Bold.Render("✓"))
	return nil
}

// pauseActor names who is pausing Dolt: BD_ACTOR, else the git identity.
func pauseActor(getenv func(string) string, gitName func() string) string {
	if actor := strings.TrimSpace(getenv("BD_ACTOR")); actor != "" {
		return actor
	}
	return strings.TrimSpace(gitName())
}

// newPauseMarker builds the marker gt dolt pause writes, refusing a pause
// with no actor or reason, or one that does not run forward within
// doltpause.MaxDuration.
func newPauseMarker(actor, reason string, d time.Duration, now time.Time) (doltpause.Marker, error) {
	if actor == "" {
		return doltpause.Marker{}, errors.New("gt dolt pause needs BD_ACTOR or git user.name to name the actor")
	}
	if strings.TrimSpace(reason) == "" {
		return doltpause.Marker{}, errors.New("gt dolt pause needs --reason")
	}
	if d <= 0 || d > doltpause.MaxDuration {
		return doltpause.Marker{}, fmt.Errorf("--until must be above 0 and at most %s, got %s", doltpause.MaxDuration, d)
	}
	return doltpause.Marker{Actor: actor, Reason: strings.TrimSpace(reason), Until: now.Add(d), Since: now}, nil
}

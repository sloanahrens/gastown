package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var deaconHeartbeatPollerIntervalFlag string

func init() {
	deaconCmd.AddCommand(deaconHeartbeatPollerCmd)
	deaconHeartbeatPollerCmd.Flags().StringVar(&deaconHeartbeatPollerIntervalFlag, "interval",
		deacon.DefaultHeartbeatPollInterval, "Poll interval (e.g., 3m)")
}

var deaconHeartbeatPollerCmd = &cobra.Command{
	Use:    "heartbeat-poller <session>",
	Short:  "Background heartbeat poller for the Deacon session",
	Hidden: true, // Internal command — launched by 'gt deacon start', not by users.
	Long: `Refreshes the Deacon's liveness heartbeat on a fixed interval, independent
of any gt/bd command invocation.

touchDeaconHeartbeat (persistentPreRun) only refreshes the heartbeat when a
gt command runs (gt-13z). A patrol step that spends a long stretch on
bd/git/grep investigation without invoking gt otherwise leaves the heartbeat
stale for the whole stretch, risking the daemon's very-stale kill even
though the Deacon is actively working (gt-x8y).

This command runs as a long-lived background process, started automatically
by 'gt deacon start' and stopped by 'gt deacon stop'/'gt deacon restart'. It
exits when:
  - The target tmux session dies
  - It receives SIGTERM (from StopHeartbeatPoller or session teardown)

While the Deacon is paused, ticks are skipped rather than the poller exiting,
so it resumes touching automatically when the Deacon unpauses.

Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runDeaconHeartbeatPoller,
}

func runDeaconHeartbeatPoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	pollInterval, err := time.ParseDuration(deaconHeartbeatPollerIntervalFlag)
	if err != nil {
		return fmt.Errorf("invalid --interval: %w", err)
	}

	t := tmux.NewTmux()

	// Verify session exists before starting the loop.
	if exists, _ := t.HasSession(sessionName); !exists {
		return fmt.Errorf("session %q not found", sessionName)
	}

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	sessionAlive := func() bool {
		exists, _ := t.HasSession(sessionName)
		return exists
	}

	for {
		select {
		case <-sigCh:
			return nil // graceful shutdown

		case <-ticker.C:
			if deaconHeartbeatPollOnce(townRoot, sessionAlive) {
				return nil // session gone, exit
			}
		}
	}
}

// deaconHeartbeatPollOnce runs a single poll cycle: if the session is gone,
// it reports that the poller should exit; otherwise it touches the Deacon's
// heartbeat (skipping the touch, but not exiting, while paused) and reports
// that the poller should keep running.
//
// Factored out of the ticker loop so the "does this tick actually keep the
// heartbeat fresh with zero gt invocations" behavior is testable without a
// real tmux session or timers.
func deaconHeartbeatPollOnce(townRoot string, sessionAlive func() bool) (shouldExit bool) {
	if !sessionAlive() {
		return true
	}
	_ = deacon.TouchIfActive(townRoot)
	return false
}

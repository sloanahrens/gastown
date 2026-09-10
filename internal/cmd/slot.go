package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	slotRunRole    string
	slotRunTimeout time.Duration
	slotStatusJSON bool
)

var slotCmd = &cobra.Command{
	Use:     "slot",
	GroupID: GroupServices,
	Short:   "Coordinate the town-level container-suite gate slot",
	Long: `The container-suite gate slot ensures only one Docker-backed test suite
(beads' Dolt containers, gastown's testcontainers patrol tests, etc.) runs at
a time against the host's Docker VM. The VM has a fixed CPU/memory bound
(see 'gt doctor'); two container-backed suites running concurrently inside it
starve each other even when the host itself shows plenty of idle CPU.

The slot is a kernel-managed advisory lock (flock), not a pid file: the
kernel releases it automatically when the holding process dies by any means,
including SIGKILL, so there is nothing to reclaim and no stale-lock cleanup
required.`,
}

var slotRunCmd = &cobra.Command{
	Use:   "run -- <command> [args...]",
	Short: "Acquire the container-gate slot, run a command, then release it",
	Long: `Acquires the container-gate slot (waiting if another rig currently holds it),
runs the given command with the slot held, and releases the slot when the
command exits — regardless of whether it succeeded, failed, or this gt
process itself is killed (the kernel releases the underlying flock on
process death).

Run this wrapped around any suite that spins Docker/testcontainers, e.g.:

  gt slot run --role gastown/refinery -- make test`,
	Args:               cobra.MinimumNArgs(1),
	DisableFlagParsing: false,
	RunE:               runSlotRun,
}

var slotStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the container-gate slot is currently held",
	RunE:  runSlotStatus,
}

func init() {
	slotRunCmd.Flags().StringVar(&slotRunRole, "role", "", "Identifier for the holder, shown in 'gt status' (e.g. rig/role or MR id)")
	slotRunCmd.Flags().DurationVar(&slotRunTimeout, "timeout", 60*time.Minute, "Max time to wait for the slot to free up (0 = wait forever)")

	slotStatusCmd.Flags().BoolVar(&slotStatusJSON, "json", false, "Output as JSON")

	slotCmd.AddCommand(slotRunCmd)
	slotCmd.AddCommand(slotStatusCmd)
	rootCmd.AddCommand(slotCmd)
}

func runSlotRun(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	role := slotRunRole
	if role == "" {
		role = fmt.Sprintf("pid-%d", os.Getpid())
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Waiting for container-gate slot (role=%s)...\n", role)
	h, err := slot.Acquire(townRoot, role, slotRunTimeout)
	if err != nil {
		return fmt.Errorf("acquiring container-gate slot: %w", err)
	}
	defer h.Release()
	fmt.Fprintf(cmd.OutOrStdout(), "Container-gate slot acquired (role=%s).\n", role)

	sub := exec.Command(args[0], args[1:]...) //nolint:gosec // G204: args come from the operator's own CLI invocation
	sub.Stdin = os.Stdin
	sub.Stdout = os.Stdout
	sub.Stderr = os.Stderr

	// Forward interrupts to the child so it can shut down its containers
	// cleanly; the slot itself is released either by our deferred Release()
	// on a graceful return, or by the kernel if we are killed outright.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			if sub.Process != nil {
				_ = sub.Process.Signal(os.Interrupt)
			}
		}
	}()

	runErr := sub.Run()
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("running %s: %w", args[0], runErr)
	}
	return nil
}

func runSlotStatus(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	held, owner, err := slot.Status(townRoot)
	if err != nil {
		return fmt.Errorf("checking container-gate slot: %w", err)
	}

	if slotStatusJSON {
		return printSlotStatusJSON(cmd, held, owner)
	}

	if !held {
		fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: free")
		return nil
	}

	if owner != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Container-gate slot: held by %s (pid %d, since %s, age %s)\n",
			owner.Role, owner.PID, owner.AcquiredAt.Format(time.RFC3339), time.Since(owner.AcquiredAt).Round(time.Second))
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: held (owner metadata unavailable)")
	}
	return nil
}

func printSlotStatusJSON(cmd *cobra.Command, held bool, owner *slot.Owner) error {
	type jsonOut struct {
		Held  bool        `json:"held"`
		Owner *slot.Owner `json:"owner,omitempty"`
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(jsonOut{Held: held, Owner: owner})
}

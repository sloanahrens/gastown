package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
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
	pool := containerGatePool(townRoot)
	h, err := slot.AcquirePool(townRoot, role, slotRunTimeout, pool)
	if err != nil {
		return fmt.Errorf("acquiring container-gate slot: %w", err)
	}
	defer func() { _ = h.Release() }()
	fmt.Fprintf(cmd.OutOrStdout(), "Container-gate slot %d/%d acquired (role=%s).\n", h.Index, pool.Slots, role)

	// env(1) semantics: leading VAR=value tokens set the child's environment.
	// The polecat formula wraps the rig's test_command verbatim
	// ("gt slot run -- GOFLAGS=-p=8 make test"), and exec'ing "GOFLAGS=-p=8"
	// as a program fails with "executable file not found" (gt-18nx).
	envAssigns, cmdArgs := splitEnvPrefix(args)
	if len(cmdArgs) == 0 {
		return fmt.Errorf("gt slot run: no command after environment assignment(s) %v", envAssigns)
	}
	sub := exec.Command(cmdArgs[0], cmdArgs[1:]...) //nolint:gosec // G204: args come from the operator's own CLI invocation
	if len(envAssigns) > 0 {
		sub.Env = append(os.Environ(), envAssigns...)
	}
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
		return fmt.Errorf("running %s: %w", cmdArgs[0], runErr)
	}
	return nil
}

func runSlotStatus(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rep, err := slot.StatusPool(townRoot, containerGatePool(townRoot))
	if err != nil {
		return fmt.Errorf("checking container-gate slot: %w", err)
	}

	if slotStatusJSON {
		return printSlotStatusJSON(cmd, rep)
	}

	if rep.Total > 1 {
		fmt.Fprintf(cmd.OutOrStdout(), "Container-gate pool: %d/%d held (%d reserved for the refinery)\n", rep.HeldCount, rep.Total, rep.Reserved)
		for _, st := range rep.Slots {
			label := "free"
			if st.Held {
				if st.Owner != nil {
					label = fmt.Sprintf("held by %s (pid %d, since %s, age %s)", st.Owner.Role, st.Owner.PID,
						st.Owner.AcquiredAt.Format(time.RFC3339), time.Since(st.Owner.AcquiredAt).Round(time.Second))
				} else {
					label = "held (owner metadata unavailable)"
				}
			}
			tag := ""
			if st.Index < rep.Reserved {
				tag = " [gate-reserved]"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  slot %d%s: %s\n", st.Index, tag, label)
		}
		if rep.HeldCount > 0 {
			return nil
		}
	} else if rep.Held {
		if rep.Owner != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Container-gate slot: held by %s (pid %d, since %s, age %s)\n",
				rep.Owner.Role, rep.Owner.PID, rep.Owner.AcquiredAt.Format(time.RFC3339), time.Since(rep.Owner.AcquiredAt).Round(time.Second))
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: held (owner metadata unavailable)")
		}
		return nil
	}

	if rep.DockerUnknown {
		fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: unknown — Docker daemon unreachable, cannot verify no suite is running")
		return nil
	}

	if len(rep.UnwrappedContainers) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: BUSY — unwrapped container suite detected (not holding the slot token):")
		for _, c := range rep.UnwrappedContainers {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", c)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "This suite should have been run via 'gt slot run -- <command>'.")
		return nil
	}

	if rep.Total > 1 {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: free")
	return nil
}

func printSlotStatusJSON(cmd *cobra.Command, rep slot.Report) error {
	type jsonOut struct {
		Held                bool             `json:"held"`
		Owner               *slot.Owner      `json:"owner,omitempty"`
		Busy                bool             `json:"busy"`
		UnwrappedContainers []string         `json:"unwrapped_containers,omitempty"`
		DockerUnknown       bool             `json:"docker_unknown"`
		Saturated           bool             `json:"saturated"` // every slot held; busy also covers unwrapped/unknown
		Slots               []slot.SlotState `json:"slots,omitempty"`
		HeldCount           int              `json:"held_count"`
		Total               int              `json:"total"`
		Reserved            int              `json:"reserved_for_gate"`
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(jsonOut{
		Held:                rep.Held,
		Owner:               rep.Owner,
		Busy:                rep.Busy(),
		UnwrappedContainers: rep.UnwrappedContainers,
		DockerUnknown:       rep.DockerUnknown,
		Saturated:           rep.AllHeld(),
		Slots:               rep.Slots,
		HeldCount:           rep.HeldCount,
		Total:               rep.Total,
		Reserved:            rep.Reserved,
	})
}

// envAssignmentRe matches a leading environment assignment token as env(1)
// accepts it: an identifier, "=", and any value (possibly empty).
var envAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// splitEnvPrefix peels leading VAR=value tokens off args, returning them and
// the remaining command. A token that merely contains "=" later in the word
// (e.g. "--flag=value") is part of the command, not an assignment; splitting
// stops at the first non-assignment token.
func splitEnvPrefix(args []string) (envAssigns, cmdArgs []string) {
	i := 0
	for i < len(args) && envAssignmentRe.MatchString(args[i]) {
		i++
	}
	return args[:i], args[i:]
}

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	slotRunRole       string
	slotRunTimeout    time.Duration
	slotRunNice       int
	slotStatusJSON    bool
	slotReapOlderThan time.Duration
	slotReapDryRun    bool
	slotReapJSON      bool
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
including SIGKILL, so the lock itself needs no reclaim path.

The containers a dead suite leaves behind are a separate matter — run
'gt slot reap' when a container the gate matches looks stale, and read
'gt slot status' to see which containers it is judging.`,
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

var slotReapCmd = &cobra.Command{
	Use:   "reap",
	Short: "Delete stale gate containers and the owner files dead suites left behind",
	Long: `Removes the container-gate's debris: containers the gate matches that are older
than the staleness window with no live testcontainers ryuk reaper for their
session, and owner metadata files whose slot nobody holds anymore.

A container-backed suite that dies uncleanly leaves its containers running. The
gate used to read any matching container as a live suite, so a single hours-old
orphan blocked every wrapped suite town-wide until someone removed it by hand.
'gt slot run' now walks past debris on its own; this command is the explicit,
evidence-printing way to actually delete it.

Reaping is the mayor's and the doctor's call, not a polecat's: a polecat's slot
token is its promise that its own suite cleans up after itself, and reaping
another holder's containers mid-run would break that suite. Use --dry-run to
read the verdicts first.`,
	Args: cobra.NoArgs,
	RunE: runSlotReap,
}

func init() {
	slotRunCmd.Flags().StringVar(&slotRunRole, "role", "", "Identifier for the holder, shown in 'gt status' (e.g. rig/role or MR id). It also scopes nesting: a wrapper nested inside another holder stays reentrant only if it passes that holder's role")
	slotRunCmd.Flags().DurationVar(&slotRunTimeout, "timeout", 60*time.Minute, "Max time to wait for the slot to free up (0 = wait forever)")
	slotRunCmd.Flags().IntVar(&slotRunNice, "nice", -1, "CPU niceness for the command (default: 10 for non-gate roles, 0 for refinery/batch/main-branch-test; 0 disables)")

	slotStatusCmd.Flags().BoolVar(&slotStatusJSON, "json", false, "Output as JSON")

	slotReapCmd.Flags().DurationVar(&slotReapOlderThan, "older-than", slot.StaleContainerWindow,
		"Age past which a container with no live reaper counts as debris")
	slotReapCmd.Flags().BoolVar(&slotReapDryRun, "dry-run", false, "Report what would be removed without removing anything")
	slotReapCmd.Flags().BoolVar(&slotReapJSON, "json", false, "Output as JSON")

	slotCmd.AddCommand(slotRunCmd)
	slotCmd.AddCommand(slotStatusCmd)
	slotCmd.AddCommand(slotReapCmd)
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
	// Gate-class holders (refinery, batch gate, main-branch test) are the
	// merge path's critical section; a polecat's own suite is optional
	// verification. When both run at once on a CPU-bound host, three
	// concurrent suites tripled the refinery's gate (gt-93m1), so non-gate
	// holders run under nice(1) unless --nice says otherwise.
	niceness := slotRunNiceness(role, slotRunNice)
	cmdArgs = withNice(cmdArgs, niceness)
	if niceness > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Running at nice %d (non-gate holder; --nice 0 to disable).\n", niceness)
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
		printSlotDebris(cmd, rep.DebrisContainers)
		return nil
	}

	printSlotDebris(cmd, rep.DebrisContainers)

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
		DebrisContainers    []string         `json:"debris_containers,omitempty"`
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
		DebrisContainers:    rep.DebrisContainers,
		DockerUnknown:       rep.DockerUnknown,
		Saturated:           rep.AllHeld(),
		Slots:               rep.Slots,
		HeldCount:           rep.HeldCount,
		Total:               rep.Total,
		Reserved:            rep.Reserved,
	})
}

// printSlotDebris names the gate containers Acquire walks past. They are shown
// because a debris reading is the evidence behind a grant: an operator who
// sees the gate report free while a container is up needs to know which
// container, how old it is, and what would remove it (gt-ul1k).
func printSlotDebris(cmd *cobra.Command, debris []string) {
	if len(debris) == 0 {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Gate debris (%d), ignored by Acquire — 'gt slot reap' removes it:\n", len(debris))
	for _, c := range debris {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", c)
	}
}

func runSlotReap(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	report, reapErr := slot.Reap(townRoot, slot.ReapOptions{
		OlderThan: slotReapOlderThan,
		DryRun:    slotReapDryRun,
	})
	if slotReapJSON {
		if err := printSlotReapJSON(cmd, report); err != nil {
			return err
		}
		return reapErr
	}
	printSlotReapReport(cmd, report)

	// The owner-file half runs before the container half, so a docker failure
	// still leaves real reaping behind it to report — which is why the report
	// prints first and the error second.
	if reapErr != nil {
		return reapErr
	}
	return nil
}

func printSlotReapReport(cmd *cobra.Command, report slot.ReapReport) {
	out := cmd.OutOrStdout()
	verb := "Removed"
	if report.DryRun {
		verb = "Would remove (dry run)"
	}

	if len(report.Debris) == 0 {
		fmt.Fprintf(out, "No gate debris older than %s.\n", report.OlderThan)
	} else {
		fmt.Fprintf(out, "%s %d gate container(s) older than %s with no live reaper:\n", verb, len(report.Debris), report.OlderThan)
		for _, v := range report.Debris {
			fmt.Fprintf(out, "  %s — %s; labels: %s\n", v.Container.Display(), v.Reason, v.Container.LabelSummary())
		}
	}

	if len(report.OwnerFiles) > 0 {
		fmt.Fprintf(out, "%s %d stale owner file(s) (their slot is not held):\n", verb, len(report.OwnerFiles))
		for _, f := range report.OwnerFiles {
			fmt.Fprintf(out, "  slot %d: pid %d, acquired %s\n", f.Slot, f.PID, f.AcquiredAt.Format(time.RFC3339))
		}
	}

	for _, failure := range report.Failed {
		fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s\n", failure)
	}
	if len(report.Kept) > 0 {
		fmt.Fprintf(out, "Kept %d container(s) that could still be a running suite.\n", len(report.Kept))
	}
}

func printSlotReapJSON(cmd *cobra.Command, report slot.ReapReport) error {
	type removedEntry struct {
		Container string `json:"container"`
		Reason    string `json:"reason"`
		Labels    string `json:"labels"`
	}
	type ownerEntry struct {
		Slot       int    `json:"slot"`
		PID        int    `json:"pid"`
		AcquiredAt string `json:"acquired_at,omitempty"`
		Path       string `json:"path"`
	}
	type jsonOut struct {
		OlderThan  string         `json:"older_than"`
		DryRun     bool           `json:"dry_run"`
		Debris     []removedEntry `json:"debris,omitempty"`
		Removed    []string       `json:"removed,omitempty"`
		Failed     []string       `json:"failed,omitempty"`
		Kept       int            `json:"kept"`
		OwnerFiles []ownerEntry   `json:"owner_files,omitempty"`
	}

	out := jsonOut{OlderThan: report.OlderThan.String(), DryRun: report.DryRun, Kept: len(report.Kept)}
	for _, v := range report.Debris {
		out.Debris = append(out.Debris, removedEntry{v.Container.Display(), v.Reason, v.Container.LabelSummary()})
	}
	out.Removed = report.Removed
	out.Failed = report.Failed
	for _, f := range report.OwnerFiles {
		entry := ownerEntry{Slot: f.Slot, PID: f.PID, Path: f.Path}
		if !f.AcquiredAt.IsZero() {
			entry.AcquiredAt = f.AcquiredAt.Format(time.RFC3339)
		}
		out.OwnerFiles = append(out.OwnerFiles, entry)
	}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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

// defaultNonGateNice is the nice(1) increment for non-gate slot holders.
const defaultNonGateNice = 10

// slotRunNiceness resolves the niceness for a holder: an explicit --nice
// wins; otherwise gate-class roles (slot.IsGateRole) run at normal priority
// and everyone else at defaultNonGateNice.
func slotRunNiceness(role string, flag int) int {
	if flag >= 0 {
		return flag
	}
	if slot.IsGateRole(role) {
		return 0
	}
	return defaultNonGateNice
}

// withNice prefixes cmdArgs with nice(1) at the given increment when it is
// positive and a nice binary exists; otherwise returns cmdArgs unchanged.
func withNice(cmdArgs []string, niceness int) []string {
	if niceness <= 0 || len(cmdArgs) == 0 {
		return cmdArgs
	}
	nicePath, err := exec.LookPath("nice")
	if err != nil {
		return cmdArgs
	}
	out := make([]string, 0, len(cmdArgs)+3)
	out = append(out, nicePath, "-n", strconv.Itoa(niceness))
	return append(out, cmdArgs...)
}

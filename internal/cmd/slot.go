package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
(gt-bcsq is where that bound was measured and the gate agreed on.)

The slot is a kernel-managed advisory lock (flock), not a pid file: the
kernel releases it automatically when the holding process dies by any means,
including SIGKILL, so the lock itself needs no reclaim path.

The wait is measured, not silent (gt-dc81): 'gt slot run' prints how long it
waited, every grant and release writes a slot_wait / slot_hold event to the
town's events log ('gt feed --plain' renders them), and 'gt slot status'
reports the recent acquisitions with their wait durations and the reason each
one waited — so the question "is the gate constricting the town?" can be
answered from the status output instead of from panes.

The containers a dead suite leaves behind are a separate matter — run
'gt slot reap' when a container the gate matches looks stale, and read
'gt slot status' to see which containers it is judging.`,
}

// slotAcquiredFormat is the line every holder gets the moment it holds the
// slot. Script plugins outside Go read it to tell a command that never ran
// from one that ran and failed — the rebuild plugin greps for it to decide
// between deferring (nothing was built) and escalating a failure — so it is a
// string to keep stable; TestSlotAcquiredFormat pins it (gt-kox0).
const slotAcquiredFormat = "Container-gate slot acquired (role=%s, waited %s, slot %d/%d).\n"

var slotRunCmd = &cobra.Command{
	Use:   "run -- <command> [args...]",
	Short: "Acquire the container-gate slot, run a command, then release it",
	Long: `Acquires the container-gate slot (waiting if another rig currently holds it),
runs the given command with the slot held, and releases the slot when the
command exits — regardless of whether it succeeded, failed, or this gt
process itself is killed (the kernel releases the underlying flock on
process death).

The acquire line reports how long this invocation waited and the release is
recorded with the command's exit status (gt-dc81), so a suite that queues
behind another is visible in 'gt feed' rather than looking like a hang.

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
session, containers whose owner labels name a test process on this host that
is provably gone (at any age), and owner metadata files whose slot nobody
holds anymore.

Test containers started by internal/testutil carry gastown.test.owner-pid,
gastown.test.owner-host and gastown.test.owner-start labels. An owner is
provably gone when its pid no longer exists, or when the pid now names a
process with a different start time (the pid was reused). A labeled container
whose owner is confirmed alive (same pid and start time) is never debris,
whatever its age. When the start time cannot be compared, the owner counts as
alive only while the container is younger than the staleness window; past it
the age/ryuk rules below decide (gt-ehlga).

A container-backed suite that dies uncleanly leaves its containers running. The
gate used to read any matching container as a live suite, so a single hours-old
orphan blocked every wrapped suite town-wide until someone removed it by hand.
'gt slot run' now walks past debris on its own; this command is the explicit,
evidence-printing way to actually delete it.

Reaping is the mayor's and the doctor's call, not a polecat's: a polecat's slot
token is its promise that its own suite cleans up after itself, and reaping
another holder's containers mid-run would break that suite. The one exception
is a container whose owner is provably gone: nothing can be using it, so a slot
acquisition ('gt slot run', gt done's gates, the refinery) that checks docker,
which it does whenever no slot is held, removes those automatically and logs
each removal on stderr. Use --dry-run to
read the verdicts first.

An unlabeled container (started by an older build, or outside internal/testutil)
is judged by age alone. To clear a young unlabeled orphan without waiting out the
window, confirm with 'gt slot reap --dry-run --older-than 1m' that it is the one
you mean and that no live test process owns it, then run
'gt slot reap --older-than 1m'.`,
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

	// Resolve and validate the command before taking the slot: the gate is the
	// merge path's critical section, and a command that cannot run must not hold
	// it (gt-f4xe). Leading VAR=value tokens are env(1) assignments, so the
	// program is looked up under the PATH the child will see (gt-18nx).
	envAssigns, cmdArgs := splitEnvPrefix(args)
	program, err := resolveSlotCommand(envAssigns, cmdArgs)
	if err != nil {
		return fmt.Errorf("gt slot run: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Waiting for container-gate slot (role=%s)...\n", role)
	pool := containerGatePool(townRoot)
	h, err := slot.AcquirePool(townRoot, role, slotRunTimeout, pool)
	if err != nil {
		return fmt.Errorf("acquiring container-gate slot: %w", err)
	}
	defer func() { _ = h.Release() }()
	// The wait is reported even when it was negligible: a queued invocation and
	// the one it queued behind only read as a pair (gt-dc81).
	fmt.Fprintf(cmd.OutOrStdout(), slotAcquiredFormat,
		role, h.WaitedFor.Round(time.Second), h.Index, pool.Slots)

	// A polecat's own suite is optional verification where a gate-class holder
	// is the merge path's critical section, and three concurrent suites
	// tripled the refinery's gate (gt-93m1) — so non-gate holders run under
	// nice(1) unless --nice says otherwise.
	niceness := slotRunNiceness(role, slotRunNice)
	if niceness > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Running at nice %d (non-gate holder; --nice 0 to disable).\n", niceness)
	}
	sub := slotChildCommand(program, cmdArgs, niceWrapper(niceness))
	if len(envAssigns) > 0 {
		sub.Env = slotRunEnv(envAssigns)
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
			// os.Exit skips deferred calls, so the hold has to be closed here
			// rather than by the deferred Release above — otherwise the release
			// (and its held_s) would never be recorded for a failing suite,
			// which is exactly the suite an operator is most likely to be
			// looking up (gt-dc81).
			_ = h.ReleaseWithExit(exitErr.ExitCode())
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

	// The ring file, not the live flocks: what the gate has cost lately, which
	// is the half of the picture the held/free state cannot show (gt-dc81).
	history, err := slot.History(townRoot)
	if err != nil {
		return fmt.Errorf("reading container-gate history: %w", err)
	}

	if slotStatusJSON {
		return printSlotStatusJSON(cmd, rep, history)
	}

	printSlotStatusText(cmd, rep)
	printSlotHistory(cmd, rep, history)
	return nil
}

func printSlotStatusText(cmd *cobra.Command, rep slot.Report) {
	printPoolStatusText(cmd, rep)
	printSlotMarkers(cmd, rep)
}

// printSlotMarkers lists the in-flight markers the pool does not count, with
// the hold's age: a review in flight is what makes the rebuild plugin defer,
// and how long it has been running is the first thing its reader asks
// (gt-97cm).
func printSlotMarkers(cmd *cobra.Command, rep slot.Report) {
	for _, st := range rep.Slots {
		if !st.Marker {
			continue
		}
		if st.Owner != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "In-flight marker %s: held by %s (pid %d, since %s, age %s)\n",
				st.Name, st.Owner.Role, st.Owner.PID,
				st.Owner.AcquiredAt.Format(time.RFC3339), time.Since(st.Owner.AcquiredAt).Round(time.Second))
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "In-flight marker %s: held (owner metadata unavailable)\n", st.Name)
	}
}

// printPoolStatusText renders the pool's own picture: the slots and what each
// one is doing, never the marker rows that follow them (see printSlotMarkers).
func printPoolStatusText(cmd *cobra.Command, rep slot.Report) {
	if rep.Total > 1 {
		fmt.Fprintf(cmd.OutOrStdout(), "Container-gate pool: %d/%d held (%d reserved for the refinery)\n", rep.HeldCount, rep.Total, rep.Reserved)
		for _, st := range rep.Slots {
			if st.Marker {
				continue
			}
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
			return
		}
	} else if rep.Held {
		if rep.Owner != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Container-gate slot: held by %s (pid %d, since %s, age %s)\n",
				rep.Owner.Role, rep.Owner.PID, rep.Owner.AcquiredAt.Format(time.RFC3339), time.Since(rep.Owner.AcquiredAt).Round(time.Second))
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: held (owner metadata unavailable)")
		}
		return
	}

	if rep.DockerUnknown {
		fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: unknown — Docker daemon unreachable, cannot verify no suite is running")
		return
	}

	if len(rep.UnwrappedContainers) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: BUSY — unwrapped container suite detected (not holding the slot token):")
		for _, c := range rep.UnwrappedContainers {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", c)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "This suite should have been run via 'gt slot run -- <command>'.")
		printSlotDebris(cmd, rep.DebrisContainers)
		return
	}

	printSlotDebris(cmd, rep.DebrisContainers)

	if rep.Total > 1 {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Container-gate slot: free")
}

// slotHistoryShown is how many recent acquisitions the plain status lists.
// Enough to see a queue forming, few enough that the live picture stays at the
// top of the output; --json carries the whole ring.
const slotHistoryShown = 5

// printSlotHistory renders the ring file: the wait distribution the gate has
// been imposing, and for each recent acquisition why it waited and how its hold
// ended. rep is the same report the held/free picture above was printed from,
// so an entry with no release on record is read against the live pool instead
// of claiming a holder the pool does not count (gt-2tqe).
func printSlotHistory(cmd *cobra.Command, rep slot.Report, history []slot.HistoryEntry) {
	out := cmd.OutOrStdout()
	if len(history) == 0 {
		return
	}

	summary := slot.SummarizeWaits(history)
	fmt.Fprintf(out, "Recent acquisitions (n=%d): p50 %s, p95 %s, max %s\n",
		summary.N, summary.P50.Round(time.Second), summary.P95.Round(time.Second), summary.Max.Round(time.Second))

	shown := slot.ResolveHolds(history, rep)
	if len(shown) > slotHistoryShown {
		shown = shown[len(shown)-slotHistoryShown:]
	}
	for _, e := range shown {
		line := fmt.Sprintf("  %s %s slot %d: waited %s, %s",
			e.TS, e.Role, e.Slot, time.Duration(e.WaitedS*float64(time.Second)).Round(time.Second), slotHoldOutcome(e))
		if detail := slotHistoryReason(e.HistoryEntry); detail != "" {
			line += " — " + detail
		}
		fmt.Fprintln(out, line)
	}
}

// slotHoldOutcome renders how one entry's hold ended. An entry with no release
// on record is named against the pool — "held open" only while the pool still
// counts that holder, and "abandoned" naming the slot it no longer holds
// otherwise — so the line agrees with the held/free picture above it (gt-2tqe).
func slotHoldOutcome(h slot.ResolvedHold) string {
	switch {
	case h.TimedOut:
		return "gave up"
	case h.HeldS != nil:
		return "held " + time.Duration(*h.HeldS*float64(time.Second)).Round(time.Second).String()
	case h.Resolution == slot.HoldLive:
		return fmt.Sprintf("held open (pid %d holds slot %d now)", h.PID, h.Slot)
	case h.Resolution == slot.HoldUnmatched:
		return fmt.Sprintf("held open (slot %d held, owner metadata unavailable)", h.Slot)
	}
	// Everything else open is abandoned: the pool no longer counts its holder,
	// so the hold ended without a release. Whoever took the slot since is the
	// evidence that it was re-granted rather than merely dropped.
	if h.ReclaimedBy != nil {
		return fmt.Sprintf("abandoned (pid %d never released; slot %d since taken by %s pid %d)",
			h.PID, h.Slot, h.ReclaimedBy.Role, h.ReclaimedBy.PID)
	}
	return fmt.Sprintf("abandoned (pid %d never released; slot %d free)", h.PID, h.Slot)
}

// slotHistoryReason renders one entry's wait reason with the evidence the
// overseer's gt-dc81 amendment requires: the holder for a token wait, the
// container names for an unwrapped suite, the probe error otherwise.
func slotHistoryReason(e slot.HistoryEntry) string {
	switch e.Reason {
	case slot.WaitReasonTokenHeld:
		if e.HolderRole != "" {
			return fmt.Sprintf("token_held by %s (pid %d)", e.HolderRole, e.HolderPID)
		}
		return string(slot.WaitReasonTokenHeld)
	case slot.WaitReasonUnwrappedContainers:
		if len(e.Containers) > 0 {
			return "unwrapped_containers: " + strings.Join(e.Containers, ", ")
		}
		return string(slot.WaitReasonUnwrappedContainers)
	case slot.WaitReasonDaemonUnreachable:
		if e.DockerError != "" {
			return "daemon_unreachable: " + e.DockerError
		}
		return string(slot.WaitReasonDaemonUnreachable)
	default:
		return ""
	}
}

func printSlotStatusJSON(cmd *cobra.Command, rep slot.Report, history []slot.HistoryEntry) error {
	type jsonOut struct {
		Held                bool                `json:"held"`
		Owner               *slot.Owner         `json:"owner,omitempty"`
		Busy                bool                `json:"busy"`
		UnwrappedContainers []string            `json:"unwrapped_containers,omitempty"`
		DebrisContainers    []string            `json:"debris_containers,omitempty"`
		DockerUnknown       bool                `json:"docker_unknown"`
		Saturated           bool                `json:"saturated"` // every slot held; busy also covers unwrapped/unknown
		Slots               []slot.SlotState    `json:"slots,omitempty"`
		HeldCount           int                 `json:"held_count"`
		Total               int                 `json:"total"`
		Reserved            int                 `json:"reserved_for_gate"`
		History             []slot.ResolvedHold `json:"history,omitempty"`
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
		History:             slot.ResolveHolds(history, rep),
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
		fmt.Fprintf(out, "No gate debris older than %s, and no container whose owning test process is gone.\n", report.OlderThan)
	} else {
		fmt.Fprintf(out, "%s %d gate container(s) that are debris (older than %s with no live reaper, or their owning test process is gone):\n", verb, len(report.Debris), report.OlderThan)
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

// resolveSlotCommand resolves the program gt slot run will exec, rejecting a
// command it must not take a gate slot for: one naming no program at all (only
// VAR=value assignments), or one whose program cannot be exec'd. It is called
// before slot.AcquirePool so the refusal costs nothing (gt-f4xe).
func resolveSlotCommand(envAssigns, cmdArgs []string) (string, error) {
	if len(cmdArgs) == 0 {
		return "", fmt.Errorf("no command after environment assignment(s) %v", envAssigns)
	}
	return lookPathForSlot(cmdArgs[0], slotChildPath(envAssigns))
}

// slotChildPath is the PATH the child is run with, and so the one its program
// is resolved against: the assigned PATH= when the leading assignments set one,
// the ambient PATH otherwise. slotRunEnv builds the child's environment out of
// those same assignments, so this is the PATH the child — and the nice(1)
// wrapper resolving the program inside it — searches.
func slotChildPath(envAssigns []string) string {
	for i := len(envAssigns) - 1; i >= 0; i-- {
		if path, ok := strings.CutPrefix(envAssigns[i], "PATH="); ok {
			return path
		}
	}
	return os.Getenv("PATH")
}

// lookPathForSlot resolves name against an explicit PATH the way exec.LookPath
// resolves against the ambient one, including an empty entry meaning the
// current directory. exec.LookPath offers no way to substitute a PATH but does
// resolve a name containing a separator directly, so joining each entry and
// delegating keeps exec.LookPath's own answer to "is this an executable file".
func lookPathForSlot(name, pathEnv string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		resolved, err := exec.LookPath(name)
		if err != nil {
			// Deliberately not exec.LookPath's own error: it reads well for a
			// bare name and poorly for a path, where it blames a $PATH that was
			// never consulted.
			return "", fmt.Errorf("not an executable file: %s", name)
		}
		return resolved, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		candidate := filepath.Join(dir, name)
		if dir == "" {
			// An empty entry means the current directory. filepath.Join would
			// clean "./name" to a bare name, which exec.LookPath reads as a
			// name to search the ambient PATH for (gt-f4xe).
			candidate = "." + string(os.PathSeparator) + name
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("executable file not found in $PATH: %s", name)
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

// niceWrapper is the nice(1) prefix for a holder at the given niceness: nil
// when niceness is 0 or no nice binary exists, in which case the caller runs
// the command itself.
func niceWrapper(niceness int) []string {
	if niceness <= 0 {
		return nil
	}
	nicePath, err := exec.LookPath("nice")
	if err != nil {
		return nil
	}
	return []string{nicePath, "-n", strconv.Itoa(niceness)}
}

// slotChildCommand builds the child process for a command resolveSlotCommand
// has already validated, wrapping it in nice(1) when wrapper is non-empty.
// Without a wrapper it execs the resolved program by path: exec.Command would
// resolve a bare argv[0] against gt's own PATH, where the child's assigned
// PATH does not apply, so validation and the exec would disagree (gt-f4xe).
func slotChildCommand(program string, cmdArgs, wrapper []string) *exec.Cmd {
	argv := append(append([]string(nil), wrapper...), cmdArgs...)
	if len(wrapper) == 0 {
		// Args[0] stays the operator's own token; Path is what runs.
		return &exec.Cmd{Path: program, Args: argv}
	}
	// With a wrapper, nice(1) is argv[0] and resolves the program in the
	// child, under the PATH that resolveSlotCommand validated against.
	return exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: args come from the operator's own CLI invocation
}

// slotRunEnv is the environment for the slot's child: this process's own, with
// the operator's leading VAR=value assignments applied over it.
//
// An assigned key's inherited entry is removed rather than left beside the
// assignment, so the child reads the operator's value by construction instead
// of because os/exec dedups the slice it is handed and keeps the last entry.
// That dedup is a rule of the exec path, not of the environment, and this repo
// does not otherwise lean on it: filterEnvKey and verifyGateEnv strip the
// inherited key before appending their own value for theirs (gt-g7ym).
//
// A repeated assignment settles on the last one, as env(1) leaves it.
func slotRunEnv(envAssigns []string) []string {
	env := os.Environ()
	for _, kv := range envAssigns {
		key, _, _ := strings.Cut(kv, "=")
		env = filterEnvKey(env, key)
		env = append(env, kv)
	}
	return env
}

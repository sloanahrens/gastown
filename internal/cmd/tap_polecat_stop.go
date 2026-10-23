package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// pendingWorkGracePeriod bounds how recently the branch's last commit must
// have landed before polecat-stop-check treats the polecat as still
// mid-workflow rather than abandoned. Claude Code's Stop event fires at the
// end of every assistant turn, not only when a session actually ends
// (gt-couv) — a turn that ends because the agent kicked off a background
// verification step (build/lint/test) looks identical, from this command's
// point of view, to an idle polecat that forgot to call gt done. A polecat
// that just committed is overwhelmingly likely to still be running its
// formula's post-commit steps, not gone.
const pendingWorkGracePeriod = 2 * time.Minute

var tapPolecatStopCmd = &cobra.Command{
	Use:   "polecat-stop-check",
	Short: "Auto-run gt done on session Stop if polecat has pending work",
	Long: `Safety net for the "idle polecat" problem: polecats that finish work
but forget to call gt done before the session ends.

This command is designed to run from a Claude Code Stop hook. Stop fires at
the end of every assistant turn, not only when a session truly ends, so it
also checks:
1. Whether this is a polecat session (GT_POLECAT env var)
2. Whether gt done has already run (heartbeat state is "exiting" or "idle")
3. Whether the polecat has commits, stashes, or non-runtime dirty work
4. Whether the polecat's own verification suite is still live — a suite child
   in its pane, or the container-gate slot it holds — or its last commit
   landed within the pending-work grace period; any of those means this Stop
   event is a turn boundary, not abandonment

If the polecat has pending work that wasn't submitted, and none of the
turn-boundary signals above applies, this command runs gt done to submit it.
If gt done already ran or there's nothing to submit, it exits silently.

Exit codes:
  0 - No action needed (not a polecat, already done, or gt done succeeded)
  1 - gt done was attempted but failed`,
	RunE:         runTapPolecatStop,
	SilenceUsage: true,
}

func init() {
	tapCmd.AddCommand(tapPolecatStopCmd)
}

func runTapPolecatStop(cmd *cobra.Command, args []string) error {
	// Only applies to polecats
	polecatName := os.Getenv("GT_POLECAT")
	if polecatName == "" {
		return nil // Not a polecat session — nothing to do
	}

	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" {
		return nil // No session tracking — can't check state
	}

	// Find town root for heartbeat check
	townRoot, _, _ := workspace.FindFromCwdWithFallback()
	if townRoot == "" {
		townRoot = workspace.TownRootFromEnv(os.Getenv)
	}
	if townRoot == "" {
		return nil // Can't find workspace — exit quietly
	}

	// Check heartbeat state: if already "exiting" or "idle", gt done already ran
	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)
	if hb != nil {
		state := hb.EffectiveState()
		if state == polecat.HeartbeatExiting || state == polecat.HeartbeatIdle {
			// gt done already ran or the polecat is idle. In a sandbox a clean,
			// pushed completion also ends this session, so the sandbox's main
			// process exits and the gateway deletes the sandbox.
			endSandboxSessionIfDone(sandboxSessionEnd{}, os.Getenv, townRoot, os.Getenv("GT_RIG"), polecatName, sessionName)
			return nil
		}
	}

	// Check if the polecat is on a feature branch with work to submit.
	rigName := os.Getenv("GT_RIG")
	if rigName == "" {
		return nil
	}

	// Reconstruct polecat worktree path
	polecatDir := filepath.Join(townRoot, rigName, "polecats", polecatName)
	// Try the nested clone layout first (polecats/<name>/<rig>/)
	cloneDir := filepath.Join(polecatDir, rigName)
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		// Fall back to flat layout
		cloneDir = polecatDir
		if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
			return nil // No git repo found — exit quietly
		}
	}

	// Check current branch — skip if on main/master
	branchCmd := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil // Can't determine branch — exit quietly
	}
	branch := strings.TrimSpace(string(branchOut))
	if branch == "main" || branch == "master" || branch == "HEAD" {
		return nil // On default branch — nothing to submit
	}

	pending, reason, err := polecatStopPendingWork(cloneDir, branch)
	if err != nil || !pending {
		return nil // Can't check, or no work to submit — don't block session stop
	}

	// This Stop event may just mark a turn boundary, not a real session end
	// (gt-couv): the polecat can still be legitimately waiting on a
	// background verification step. Two independent turn-end signals veto
	// the auto-run in that case; either one is enough to hold off.
	busy, busyReason, readErr := polecatStopVerificationRunning(townRoot, rigName, polecatName, sessionName)
	if readErr != nil {
		// An unreadable read does not hold the auto gt done back (see
		// polecatStopSuite), so say so: silently it is indistinguishable from a
		// seat that was read and found idle.
		fmt.Fprintf(os.Stderr, "polecat-stop-check: could not read whether a verification suite is live (%v) — an unreadable read is not busy here\n", readErr)
	}
	if busy {
		fmt.Fprintf(os.Stderr, "polecat-stop-check: %s — deferring gt done\n", busyReason)
		return nil
	}
	if recent, commitErr := polecatStopCommittedWithinGrace(cloneDir); commitErr == nil && recent {
		fmt.Fprintf(os.Stderr, "polecat-stop-check: last commit on %s is under %s old — deferring gt done\n", branch, pendingWorkGracePeriod)
		return nil
	}

	// Polecat has pending work! Run gt done as a safety net.
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "⚠️  Polecat %s has pending work on branch %s (%s)\n", polecatName, branch, reason)
	fmt.Fprintf(os.Stderr, "   Auto-running gt done as safety net...\n")
	fmt.Fprintf(os.Stderr, "\n")

	// Find gt binary path
	gtBin, err := os.Executable()
	if err != nil {
		gtBin = "gt"
	}

	// Run gt done in the polecat's worktree context
	doneCmd := exec.Command(gtBin, "done")
	doneCmd.Dir = cloneDir
	doneCmd.Stdout = os.Stdout
	doneCmd.Stderr = os.Stderr
	// Inherit environment (GT_POLECAT, GT_RIG, etc. are already set)
	doneCmd.Env = os.Environ()

	if err := doneCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Auto gt done failed: %v\n", err)
		// Don't return error — don't block session stop
		return nil
	}

	return nil
}

func polecatStopPendingWork(cloneDir, branch string) (bool, string, error) {
	return polecatStopPendingWorkIn(git.NewGit(cloneDir), branch)
}

// polecatStopGit is the part of *git.Git the pending-work check reads.
type polecatStopGit interface {
	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	BranchTargetStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error)
}

// polecatStopPendingWorkIn is polecatStopPendingWork over g.
func polecatStopPendingWorkIn(g polecatStopGit, branch string) (bool, string, error) {
	workStatus, err := g.CheckUncommittedWork()
	if err != nil {
		return false, "", err
	}

	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return true, fmt.Sprintf("%d non-runtime dirty file(s)", len(workStatus.NonRuntimePaths())), nil
	}
	if workStatus.StashCount > 0 {
		return true, fmt.Sprintf("%d branch stash(es)", workStatus.StashCount), nil
	}

	targetStatus, err := g.BranchTargetStatus(branch, "origin", nil)
	if err != nil {
		return false, "", err
	}
	if !targetStatus.Preserved && targetStatus.UnpreservedPatchCount > 0 {
		return true, fmt.Sprintf("%d unsubmitted commit(s)", targetStatus.UnpreservedPatchCount), nil
	}

	return false, "", nil
}

// polecatStopVerificationRunning reports whether this polecat's own
// verification suite is still live — its own suite holding a container-gate
// slot, or a suite invocation live under its own panes — and names it for the
// deferral message. The error is a read that failed; the caller reports it and
// carries on, since the reading stands on its own.
func polecatStopVerificationRunning(townRoot, rigName, polecatName, sessionName string) (bool, string, error) {
	return polecatStopSuite(stopCheckReads{townRoot: townRoot}, townRoot, rigName, polecatName, sessionName)
}

// polecatStopSuite is that decision over its reads.
//
// It is the Stop guard's side of the one check the daemon's overrun guard also
// asks (polecat.LiveSuite, gt-a1xhd). An unreadable read is "not busy" here,
// the turn-boundary reading: the grace period and the next turn's reads both
// still stand between this Stop event and an auto gt done, and a later Stop
// re-runs them. The overrun guard maps the same error the other way, and the
// error is returned rather than swallowed so the caller can say a read failed.
//
// A failed read only loses the answer that read would have given: the check
// still reports a suite the other read found, so an unreadable slot state
// cannot hide the suite running in this polecat's pane.
func polecatStopSuite(reads polecat.SuiteReads, townRoot, rigName, polecatName, sessionName string) (bool, string, error) {
	found, err := polecat.LiveSuite(reads, polecat.SuiteQuery{
		Session: sessionName,
		// `<rig>/<polecat>` is the role the formula's own gate wrap passes to
		// `gt slot run --role`. A slot held under any other role is some other
		// polecat's suite and says nothing about this one.
		Roles:  []string{rigName + "/" + polecatName},
		Suites: stopCheckSuites(townRoot, rigName),
		Split:  stopCheckTokens,
	})
	switch {
	case found.Slot.Held:
		return true, fmt.Sprintf("container-gate slot %d held by %s (verification suite running)", found.Slot.Index, found.Slot.Owner.Role), err
	case found.Invocation != "":
		return true, fmt.Sprintf("%s is still running in this polecat's pane", found.Invocation), err
	}
	return false, "", err
}

// stopCheckReads reads one live-suite question's evidence straight from the
// host. The overrun guard wraps the same reads in a per-cycle cache; this guard
// runs once at a turn boundary, so there is nothing to carry between seats.
//
// Only the flock half of the pool is read (StatusPoolLocksOnly): this runs at
// every turn boundary and the full StatusPool's `docker ps` was a subprocess per
// turn whose output was never read (gt-a8kx).
type stopCheckReads struct {
	townRoot string
}

// PanePIDs lists the pane pids of every pane in the polecat's session.
// Session-wide rather than just its first window (GetPanePID): a split session
// can hold the suite in another pane, and a suite this misses reads as "not
// working" — the direction that lets the auto gt done fire (gt-78n0).
func (r stopCheckReads) PanePIDs(sessionName string) ([]int, error) {
	out, err := tmux.BuildCommand("list-panes", "-a", "-F", "#{session_name}\t#{pane_pid}").Output()
	if err != nil {
		return nil, err
	}
	return polecat.ParsePanes(string(out), sessionName), nil
}

// ProcessTable reads the whole process table in one call. One call serves every
// pane's patterns, where a pgrep per pane would spawn one process per pane.
//
// -ww because the invocation being looked for sits at the end of a long
// wrapper argv (see stopCheckTokens); a width-truncated line drops exactly the
// part that carries it.
func (r stopCheckReads) ProcessTable() ([]polecat.Proc, error) {
	out, err := exec.Command("ps", "-ww", "-eo", "pid,ppid,args").Output() //nolint:gosec // G204: fixed command
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return polecat.ParseProcessTable(string(out)), nil
}

// Slots is the town's container-gate pool, flock picture only (gt-a8kx).
func (r stopCheckReads) Slots() (slot.Report, error) {
	return slot.StatusPoolLocksOnly(r.townRoot, containerGatePool(r.townRoot))
}

// polecatStopCommittedWithinGrace reports whether the branch's most recent
// commit landed less than pendingWorkGracePeriod ago.
func polecatStopCommittedWithinGrace(cloneDir string) (bool, error) {
	out, err := exec.Command("git", "-C", cloneDir, "log", "-1", "--format=%ct").Output()
	if err != nil {
		return false, err
	}
	return commitWithinGrace(string(out), time.Now())
}

// commitWithinGrace reads git log's %ct (a commit's unix time) and reports
// whether that commit is less than pendingWorkGracePeriod older than now. An
// empty log (no commit) is not within grace.
func commitWithinGrace(ct string, now time.Time) (bool, error) {
	tsStr := strings.TrimSpace(ct)
	if tsStr == "" {
		return false, nil
	}
	unixSeconds, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false, err
	}
	return now.Sub(time.Unix(unixSeconds, 0)) < pendingWorkGracePeriod, nil
}

// stopCheckMaxNesting bounds recursive expansion of a command that arrives as
// an argument to eval or a shell's -c. The wrappers in the wild nest one
// level; deeper than this is not a shape they produce, and the cap stops a
// pathological argv from expanding without end.
const stopCheckMaxNesting = 3

// stopCheckShellNames are the shells whose -c argument is a command to run
// rather than data. grep -c is why this is a name list and not "a -c flag
// means a command": grep -c "make test" counts matches.
var stopCheckShellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true,
}

// stopCheckTokens flattens an argv string into every command token it carries,
// including the ones inside quotes. Claude Code runs a background Bash command
// through a wrapper whose argv ends in `eval 'gt slot run --role … -- make
// test' < /dev/null`, so the invocation arrives as one quoted argument and a
// plain tokenization of the wrapper sees it as a single opaque token
// (gt-78n0). Expansion is limited to arguments a shell would execute — eval's
// body, a shell's -c — so a phrase that merely mentions a gate stays data.
//
// This is the Stop guard's own read of an argv; the overrun guard reads
// through polecat.GateTokens instead (see its file comment).
func stopCheckTokens(argv string) []string {
	return stopCheckExpand(shellTokenize(argv), 0)
}

// stopCheckExpand appends tokens and, after any argument a shell would
// execute, the tokens of that argument's own command line.
func stopCheckExpand(tokens []string, depth int) []string {
	out := make([]string, 0, len(tokens))
	for i, tok := range tokens {
		out = append(out, tok)
		if depth >= stopCheckMaxNesting || !strings.ContainsAny(tok, " \t") {
			continue
		}
		if !stopCheckExecutesArgument(tokens, i) {
			continue
		}
		out = append(out, stopCheckExpand(shellTokenize(tok), depth+1)...)
	}
	return out
}

// stopCheckExecutesArgument reports whether tokens[i] is an argument a shell
// would run as a command rather than a value: eval's body, or a shell's -c.
func stopCheckExecutesArgument(tokens []string, i int) bool {
	if i == 0 {
		return false // nothing introduces it, so nothing executes it
	}
	if strings.EqualFold(filepath.Base(tokens[i-1]), "eval") {
		return true
	}
	// A shell's -c may arrive in a short-flag cluster ("-lc", "-ec"); a long
	// flag never carries it.
	if i < 2 {
		return false
	}
	flag := strings.ToLower(tokens[i-1])
	if !strings.HasPrefix(flag, "-") || strings.HasPrefix(flag, "--") || !strings.Contains(flag, "c") {
		return false
	}
	name := strings.ToLower(filepath.Base(tokens[i-2]))
	name = strings.TrimPrefix(name, "-") // a login shell arrives as "-sh"
	return stopCheckShellNames[name]
}

// stopCheckSuites are the invocations that mean this polecat is still
// mid-workflow, in match order: a backgrounded gt done still running in the
// pane (gt-h3zjo), then the rig's configured gates, then the bare verification
// commands.
func stopCheckSuites(townRoot, rigName string) []polecat.Invocation {
	return stopCheckMatchingSuites(stopCheckGateInvocations(townRoot, rigName))
}

// stopCheckMatchingSuites puts the polecat's own always-on signals in front of
// the rig's configured gates.
//
// A backgrounded gt done clears the slot, pane-child, and grace-period checks
// the moment its own gate finishes (GT_TEST_DOCKER=0 holds no slot) while the
// process itself is still running — the next Stop event then races it with a
// second gt done (gt-h3zjo). Its own submission is the pending work this check
// would otherwise resubmit.
func stopCheckMatchingSuites(gates []polecat.Invocation) []polecat.Invocation {
	suites := []polecat.Invocation{{Program: "gt", Target: "done"}}
	suites = append(suites, gates...)
	return append(suites, commonBareVerificationInvocations...)
}

// stopCheckGateInvocations reduces the rig's configured test, lint and build
// commands to the pairs the pane scan matches. Test, lint and build are the
// gates the polecat formula runs itself, so a live one means the formula is
// mid-step — setup and typecheck are the refinery's gates and run there.
func stopCheckGateInvocations(townRoot, rigName string) []polecat.Invocation {
	var gates []polecat.Invocation
	for _, command := range polecat.ConfiguredCommands(townRoot, rigName) {
		if gate, ok := stopCheckGateFromCommand(command); ok {
			gates = append(gates, gate)
		}
	}
	return gates
}

// stopCheckGateFromCommand reduces a configured gate command to its
// program/target pair. Leading VAR=value assignments are stripped, since the
// formula runs the test command in that shape ("GOFLAGS=-p=8 make test"), and
// the target is the first argument that is not a flag, since only the target
// decides what runs ("make -j4 test" is still make test).
//
// A command that names a program and no target keeps its empty target here,
// which matches that program anywhere in an argv. The overrun guard reads the
// same command the other way and drops it (overrunGateFromCommand).
func stopCheckGateFromCommand(command string) (polecat.Invocation, bool) {
	return polecat.InvocationFromCommand(stopCheckTokens, command)
}

// commonBareVerificationInvocations are verification commands recognized in
// a polecat's pane even when the rig's own gate config doesn't name them —
// commands a polecat runs bare as part of its own quality-gate habits
// regardless of what the rig configured for test/lint/build.
var commonBareVerificationInvocations = []polecat.Invocation{
	{Program: "go", Target: "test"},
	{Program: "go", Target: "vet"},
}

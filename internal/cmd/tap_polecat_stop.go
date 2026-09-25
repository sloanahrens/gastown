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
	"github.com/steveyegge/gastown/internal/rig"
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
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	if townRoot == "" {
		return nil // Can't find workspace — exit quietly
	}

	// Check heartbeat state: if already "exiting" or "idle", gt done already ran
	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)
	if hb != nil {
		state := hb.EffectiveState()
		if state == polecat.HeartbeatExiting || state == polecat.HeartbeatIdle {
			return nil // gt done already ran or polecat is idle — nothing to do
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
	// background verification step. Three independent turn-end signals veto
	// the auto-run in that case; any one of them is enough to hold off.
	if busy, busyReason := polecatStopVerificationRunning(townRoot, rigName, polecatName); busy {
		fmt.Fprintf(os.Stderr, "polecat-stop-check: %s — deferring gt done\n", busyReason)
		return nil
	}
	if live, liveReason := polecatStopPaneChildRunning(townRoot, rigName, sessionName); live {
		fmt.Fprintf(os.Stderr, "polecat-stop-check: %s — deferring gt done\n", liveReason)
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
		fmt.Fprintf(os.Stderr, "   Witness will handle cleanup.\n")
		// Don't return error — don't block session stop
		return nil
	}

	return nil
}

func polecatStopPendingWork(cloneDir, branch string) (bool, string, error) {
	g := git.NewGit(cloneDir)
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

// polecatStopVerificationRunning reports whether this polecat currently
// holds the town-level container-gate slot (see internal/slot) — i.e. its
// own slot-wrapped build/test suite is still running in the background.
// A held slot whose owner role doesn't match this polecat is some other
// rig's suite and says nothing about this polecat's state, so it is not
// treated as busy here.
//
// Reads only the flock picture (StatusPoolLocksOnly): this runs at every
// turn boundary and nothing below looks at the container half, so the full
// StatusPool's `docker ps` was a subprocess per turn whose output was never
// read (gt-a8kx).
func polecatStopVerificationRunning(townRoot, rigName, polecatName string) (bool, string) {
	rep, err := slot.StatusPoolLocksOnly(townRoot, containerGatePool(townRoot))
	if err != nil || !rep.Held {
		return false, ""
	}
	mine := rep.HeldBy(rigName + "/" + polecatName)
	if len(mine) == 0 {
		return false, ""
	}
	return true, fmt.Sprintf("container-gate slot %d held by %s (verification suite running)", mine[0].Index, mine[0].Owner.Role)
}

// polecatStopCommittedWithinGrace reports whether the branch's most recent
// commit landed less than pendingWorkGracePeriod ago.
func polecatStopCommittedWithinGrace(cloneDir string) (bool, error) {
	out, err := exec.Command("git", "-C", cloneDir, "log", "-1", "--format=%ct").Output()
	if err != nil {
		return false, err
	}
	tsStr := strings.TrimSpace(string(out))
	if tsStr == "" {
		return false, nil
	}
	unixSeconds, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false, err
	}
	return time.Since(time.Unix(unixSeconds, 0)) < pendingWorkGracePeriod, nil
}

// polecatStopPaneChildRunning reports whether the polecat's own tmux pane
// still has a live verification child: a queued or holding `gt slot run`, a
// `go test`, or one of the rig's configured test/lint/build gates.
//
// The slot check above sees only a suite that already holds the town gate.
// This covers the wait before that gate: the polecat starts its own suite in
// a background shell, the shell queues behind another rig's, the turn ends,
// and Stop fires with no slot held and a commit too fresh to fall outside the
// grace period — so nothing holds back an auto gt done that would re-run the
// very suite already queued (gt-78n0). Claude Code keeps a background Bash
// under the pane's agent process, so the pane's process tree is where that
// suite shows up.
//
// A pane that cannot be located yields no verdict rather than a false "idle":
// the grace period and the slot check still stand between this Stop event and
// an auto gt done, and a later Stop event re-runs all three.
func polecatStopPaneChildRunning(townRoot, rigName, sessionName string) (bool, string) {
	panePIDs := polecatStopPanePIDs(sessionName)
	if len(panePIDs) == 0 {
		return false, ""
	}
	procs, err := polecatStopProcessTable()
	if err != nil {
		return false, ""
	}
	gates := polecatStopGateInvocations(townRoot, rigName)
	for _, argv := range stopCheckDescendantArgvs(panePIDs, procs) {
		if what, ok := polecatStopVerificationArgv(argv, gates); ok {
			return true, fmt.Sprintf("%s is still running in this polecat's pane", what)
		}
	}
	return false, ""
}

// polecatStopPanePIDs returns the pids of every pane in the polecat's session.
// Session-wide rather than just its first window (GetPanePID): a split session
// can hold the suite in another pane, and a suite this misses reads as "not
// working" — the direction that lets the auto gt done fire (gt-78n0).
func polecatStopPanePIDs(sessionName string) []int {
	out, err := tmux.BuildCommand("list-panes", "-a", "-F", "#{session_name}\t#{pane_pid}").Output()
	if err != nil {
		return nil
	}
	return parsePolecatStopPanes(string(out), sessionName)
}

// parsePolecatStopPanes picks sessionName's pane pids out of
// `list-panes -a -F "#{session_name}\t#{pane_pid}"` output.
func parsePolecatStopPanes(out, sessionName string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		name, pidStr, found := strings.Cut(line, "\t")
		if !found || name != sessionName {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// stopCheckProc is one process-table row: the pid, its parent, and the full
// argv the suite patterns are matched against.
type stopCheckProc struct {
	PID  int
	PPID int
	Args string
}

// polecatStopProcessTable reads the whole process table in one call. One call
// serves every pane's patterns, where a pgrep per pane would spawn one process
// per pane.
//
// -ww because the invocation being looked for sits at the end of a long
// wrapper argv (see stopCheckTokens); a width-truncated line drops exactly the
// part that carries it.
func polecatStopProcessTable() ([]stopCheckProc, error) {
	out, err := exec.Command("ps", "-ww", "-eo", "pid,ppid,args").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return parseStopCheckProcessTable(string(out)), nil
}

// parseStopCheckProcessTable parses `ps -ww -eo pid,ppid,args` output. args
// carries the rest of the line and contains whitespace, so only the first two
// fields are fixed.
func parseStopCheckProcessTable(out string) []stopCheckProc {
	var procs []stopCheckProc
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue // Header line
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		procs = append(procs, stopCheckProc{PID: pid, PPID: ppid, Args: strings.Join(fields[2:], " ")})
	}
	return procs
}

// stopCheckDescendantArgvs returns the argv of every process descended from
// one of roots, from a single process-table snapshot. The roots are excluded:
// a pane's root is the polecat's own agent process, and the question is what
// that agent has running, not whether the agent exists.
func stopCheckDescendantArgvs(roots []int, procs []stopCheckProc) []string {
	children := make(map[int][]stopCheckProc, len(procs))
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p)
	}
	seen := make(map[int]bool, len(procs))
	var argvs []string
	var walk func(pid int)
	walk = func(pid int) {
		for _, child := range children[pid] {
			if seen[child.PID] {
				continue // a pid reused within the snapshot would otherwise loop
			}
			seen[child.PID] = true
			argvs = append(argvs, child.Args)
			walk(child.PID)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return argvs
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

// stopCheckGate is one configured rig gate command reduced to the pair of
// tokens that identify it in another process's argv.
type stopCheckGate struct {
	Program string // lowercased program basename, e.g. "make"
	Target  string // lowercased basename of its first non-flag argument, e.g. "test"
}

// polecatStopGateInvocations reduces the rig's configured test, lint and build
// commands to the pairs the pane scan matches. Test, lint and build are the
// gates the polecat formula runs itself, so a live one means the formula is
// mid-step — setup and typecheck are the refinery's gates and run there.
func polecatStopGateInvocations(townRoot, rigName string) []stopCheckGate {
	mq := rig.ResolveMergeQueueConfig(townRoot, rigName)
	if mq == nil {
		return nil
	}
	var gates []stopCheckGate
	for _, command := range []string{mq.TestCommand, mq.LintCommand, mq.BuildCommand} {
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
func stopCheckGateFromCommand(command string) (stopCheckGate, bool) {
	_, args := splitEnvPrefix(stopCheckTokens(strings.TrimSpace(command)))
	if len(args) == 0 {
		return stopCheckGate{}, false
	}
	gate := stopCheckGate{Program: strings.ToLower(filepath.Base(args[0]))}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		gate.Target = strings.ToLower(filepath.Base(arg))
		break
	}
	return gate, true
}

// polecatStopVerificationArgv reports whether a process's argv is a live
// verification or build suite, and names it for the deferral message.
func polecatStopVerificationArgv(argv string, gates []stopCheckGate) (string, bool) {
	tokens := stopCheckInvocationTokens(argv)
	if len(tokens) == 0 {
		return "", false
	}
	// gt slot run is the formula's wrapper around the rig's container-backed
	// suite, and the one invocation that can be live while holding no slot —
	// which is the queue wait this check exists for.
	if i := findInvocation(tokens, "gt", "slot"); i >= 0 && i+2 < len(tokens) && tokens[i+2] == "run" {
		return "gt slot run", true
	}
	// A backgrounded gt done clears the slot, pane-child, and grace-period
	// checks the moment its own gate finishes (GT_TEST_DOCKER=0 holds no
	// slot) while the process itself is still running — the next Stop event
	// then races it with a second gt done (gt-h3zjo). Its own submission is
	// the pending work this check would otherwise resubmit.
	if findInvocation(tokens, "gt", "done") >= 0 {
		return "gt done", true
	}
	for _, gate := range gates {
		if stopCheckGateMatches(tokens, gate) {
			return strings.TrimSpace(gate.Program + " " + gate.Target), true
		}
	}
	// A rig whose config names no gate, or names its suite some other way,
	// must not silence the check: these invocations are a live suite however
	// the rig spells its own gate commands. "go vet" is the concrete gap
	// this list closes — the polecat completion protocol runs "go test ./...
	// && go vet ./..." as the Go quality gate, but only "go test" was
	// recognized here, so a bare go vet in the pane read as an idle polecat
	// mid-vet (gt-jn89).
	for _, gate := range commonBareVerificationInvocations {
		if stopCheckGateMatches(tokens, gate) {
			return strings.TrimSpace(gate.Program + " " + gate.Target), true
		}
	}
	return "", false
}

// commonBareVerificationInvocations are verification commands recognized in
// a polecat's pane even when the rig's own gate config doesn't name them —
// commands a polecat runs bare as part of its own quality-gate habits
// regardless of what the rig configured for test/lint/build.
var commonBareVerificationInvocations = []stopCheckGate{
	{Program: "go", Target: "test"},
	{Program: "go", Target: "vet"},
}

// stopCheckInvocationTokens reduces argv to the tokens the invocation
// matchers compare: lowercased, each reduced to its basename.
//
// The basename is load-bearing. A program reaches the process table as the
// path it was exec'd with — an observed suite child is
// "/Library/Developer/CommandLineTools/usr/bin/make test" — so "make test"
// from rig config matches only once both sides are basenames.
func stopCheckInvocationTokens(argv string) []string {
	tokens := stopCheckTokens(argv)
	for i, tok := range tokens {
		tokens[i] = strings.ToLower(filepath.Base(tok))
	}
	return tokens
}

// stopCheckGateMatches reports whether tokens contain the gate's invocation.
func stopCheckGateMatches(tokens []string, gate stopCheckGate) bool {
	if gate.Target == "" {
		for _, tok := range tokens {
			if tok == gate.Program {
				return true
			}
		}
		return false
	}
	return findInvocation(tokens, gate.Program, gate.Target) >= 0
}

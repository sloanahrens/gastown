package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/workspace"
)

// The five budgets of a cutover, in one place (gt-qpeei.30). Each wait runs on
// the injected clock and has a stated ending when its budget runs out.
const (
	// cutoverQuietBudget is how long step 1 waits for a quiet town.
	cutoverQuietBudget = 30 * time.Minute
	// cutoverRecheckBudget is step 2: the quiet check made again immediately
	// before the old daemon is touched. It is one poll, so it reads the probes
	// once more and gives up at once if the town stopped being quiet since step 1.
	cutoverRecheckBudget = defaultQuietPoll
	// cutoverStopBudget is how long step 3 waits for proof the old daemon
	// stopped: its own shutdown budget, and a margin for the service manager.
	cutoverStopBudget = daemon.ShutdownBudget + 30*time.Second
	// cutoverTakeBudget is how long step 4 waits for the new owner to take the
	// town.
	cutoverTakeBudget = 2 * time.Minute
	// cutoverHeartbeatBudget is how long step 5 waits for the new daemon's
	// second heartbeat: the heartbeat interval and a minute. The first beat comes
	// about a minute after the start and the second one interval after it.
	cutoverHeartbeatBudget = 5*time.Minute + time.Minute

	// cutoverPoll is how often a wait reads state.json and the job state.
	cutoverPoll = 5 * time.Second

	// cutoverHeartbeatsAfterStart is how many heartbeats step 5 needs the new
	// daemon to have written since the baseline: the second one proves it beats
	// on its own interval, not just that it wrote its start state.
	cutoverHeartbeatsAfterStart = 2
)

// cutoverSide is one end of a cutover: a daemon supervisor and what the
// cutover needs to say about it. The launchd side and the container side are
// both built from the town's own settings, whichever the town runs now.
type cutoverSide struct {
	name  string       // "launchd" or "container", for messages
	owner daemon.Owner // what the daemon under this side writes to state.json
	// stop, start and bootstrap are the supervisor's argv builders; a nil
	// bootstrap means the side has none to run before its start.
	stop, start, bootstrap func() ([]string, error)
	// job reads the side's live job or container state.
	job func() templates.SupervisorState
	// stopped says whether a job state proves the side's daemon is not running.
	stopped func(templates.SupervisorState) bool
	// restore is the command that brings this side back after the cutover has
	// stopped it. diagnose is the command that shows why it did not come up.
	restore, diagnose string
}

// cutoverPlan is one cutover run: which side goes down, which comes up, and the
// seams every external call and every read goes through. Nothing in it touches
// the host except through these, so a test runs it on a fake clock with
// recording fakes.
type cutoverPlan struct {
	townRoot string
	from, to cutoverSide
	clk      clockwork.Clock
	out      io.Writer
	// quiet waits for a quiet town for at most budget (WaitQuiet).
	quiet func(ctx context.Context, budget time.Duration) error
	// state reads state.json: nil when the file is missing.
	state func() (*daemon.State, error)
	// run runs one supervisor argv.
	run func(argv []string) error
	// guard asks the startup guard whether the side's daemon may take the town.
	guard func(side cutoverSide) error
	// rollback is the command that undoes the cutover; it is printed, never run.
	rollback string
	// baseline is state.json as step 3 last read it, the old daemon's clean
	// stop. Step 5 measures the new daemon's heartbeats against it.
	baseline *daemon.State
}

// execute runs the five steps. Every ending that is not success is an error
// whose message the operator can act on; the text printed before it says what
// state the town is left in and the exact command to restore it. Nothing is
// rolled back by itself.
func (p *cutoverPlan) execute(ctx context.Context) error {
	// Steps 1 and 2: quiet, twice. Neither has changed anything yet.
	if err := p.quiet(ctx, cutoverQuietBudget); err != nil {
		return fmt.Errorf("not cutting over to %s, nothing was changed: %w", p.to.name, err)
	}
	if err := p.quiet(ctx, cutoverRecheckBudget); err != nil {
		return fmt.Errorf("not cutting over to %s, nothing was changed: the town stopped being quiet: %w", p.to.name, err)
	}

	// Step 3: stop the old daemon and prove it stopped.
	if err := p.runBuilder(p.from.stop); err != nil {
		return fmt.Errorf("stopping the %s daemon failed, so %s was not started: %w; check its state before trying again", p.from.name, p.to.name, err)
	}
	if proven, why := p.waitFor(ctx, cutoverStopBudget, p.oldDaemonStopped); !proven {
		p.say("The %s stop command ran without error, but the old daemon is not proven stopped: last read, %s. %s was not started.", p.from.name, why, p.to.name)
		p.say("To restore the %s job: %s", p.from.name, p.from.restore)
		return fmt.Errorf("the old %s daemon is not proven stopped after %s: %s", p.from.name, cutoverStopBudget, why)
	}

	// Step 4: start the new daemon, through the guard, and wait for it to own
	// the town. The old daemon's clean stop is what the guard allows it on.
	if err := p.guard(p.to); err != nil {
		p.say("The old %s daemon is stopped and %s was not started. To restore the %s job: %s", p.from.name, p.to.name, p.from.name, p.from.restore)
		return fmt.Errorf("the startup guard refused to start %s: %w", p.to.name, err)
	}
	for _, build := range []func() ([]string, error){p.to.bootstrap, p.to.start} {
		if build == nil {
			continue
		}
		if err := p.runBuilder(build); err != nil {
			p.say("The old %s daemon is stopped and %s did not start. To restore the %s job: %s", p.from.name, p.to.name, p.from.name, p.from.restore)
			return fmt.Errorf("starting %s failed: %w", p.to.name, err)
		}
	}
	if took, why := p.waitFor(ctx, cutoverTakeBudget, func() (bool, string) { _, ok, why := p.newDaemonOwns(); return ok, why }); !took {
		p.say("%s started but never took the town (%s). Its logs: %s", p.to.name, why, p.to.diagnose)
		p.say("To go back: %s", p.rollback)
		return fmt.Errorf("%s never took the town within %s: %s", p.to.name, cutoverTakeBudget, why)
	}

	// Step 5: a second heartbeat, so the new daemon is known to keep beating.
	if beat, why := p.waitFor(ctx, cutoverHeartbeatBudget, p.newDaemonBeats); !beat {
		p.say("%s owns the town but is unhealthy: no second heartbeat (%s). Its logs: %s", p.to.name, why, p.to.diagnose)
		p.say("To go back: %s", p.rollback)
		return fmt.Errorf("%s owns the town but wrote no second heartbeat within %s: %s", p.to.name, cutoverHeartbeatBudget, why)
	}
	p.say("%s owns the town and has beaten twice.", p.to.name)
	return nil
}

// oldDaemonStopped is the proof step 3 waits for: state.json records a clean
// stop (running false) and the old side's job reader reports it not running,
// both known. A heartbeat that merely stopped moving is not proof, since a hung
// daemon stops writing too, and a job state that could not be read is unknown,
// which is not stopped either.
func (p *cutoverPlan) oldDaemonStopped() (bool, string) {
	st, err := p.state()
	switch {
	case err != nil:
		return false, fmt.Sprintf("state.json cannot be read: %v", err)
	case st == nil:
		return false, "state.json is missing, so no clean stop is recorded"
	case st.Running:
		return false, "state.json still records the daemon running (running=true)"
	}
	job := p.from.job()
	switch {
	case job.Err != nil:
		return false, fmt.Sprintf("the %s job state cannot be read: %v", p.from.name, job.Err)
	case !p.from.stopped(job):
		return false, fmt.Sprintf("the %s job still reads as running", p.from.name)
	}
	p.baseline = st
	return true, ""
}

// newDaemonOwns is what step 4 waits for: state.json names the new side as the
// owner and records it running. A heartbeat is step 5's wait, not this one's.
// It returns the state it read so step 5 can measure it.
func (p *cutoverPlan) newDaemonOwns() (*daemon.State, bool, string) {
	st, err := p.state()
	switch {
	case err != nil:
		return nil, false, fmt.Sprintf("state.json cannot be read: %v", err)
	case st == nil:
		return nil, false, "state.json is missing"
	case st.Owner == nil || *st.Owner != p.to.owner:
		return st, false, fmt.Sprintf("state.json names owner %s, not %s %q", ownerText(st.Owner), p.to.owner.Kind, p.to.owner.ID)
	case !st.Running:
		return st, false, "state.json records the daemon not running"
	}
	return st, true, ""
}

// newDaemonBeats is what step 5 waits for: the new side owns the town and has
// written cutoverHeartbeatsAfterStart heartbeats of its own, measured against
// the baseline step 3 read before the start, with a heartbeat time later than
// the baseline's.
//
// A daemon's heartbeat count starts at 0 in every process (startState builds a
// fresh record; it never loads the old file), so a record from a new process
// counts from 0 and needs the count itself to reach the threshold. A record
// that is the baseline's own process (same StartedAt) carries the baseline's
// count, so it needs the baseline's count plus the threshold. The first case is
// the real one; the second keeps a state file that was not rewritten, or a
// future daemon that carries its count over, from passing on beats the old
// owner wrote.
func (p *cutoverPlan) newDaemonBeats() (bool, string) {
	st, ok, why := p.newDaemonOwns()
	if !ok {
		return false, why
	}
	need := int64(cutoverHeartbeatsAfterStart)
	if b := p.baseline; b != nil {
		if st.StartedAt.Equal(b.StartedAt) {
			need += b.HeartbeatCount
		}
		if !st.LastHeartbeat.After(b.LastHeartbeat) {
			return false, fmt.Sprintf("the last heartbeat (%s) is not later than the old daemon's (%s)", st.LastHeartbeat.Format(time.RFC3339), b.LastHeartbeat.Format(time.RFC3339))
		}
	}
	if st.HeartbeatCount < need {
		return false, fmt.Sprintf("%d heartbeats counted, %d needed", st.HeartbeatCount, need)
	}
	return true, ""
}

// ownerText names a recorded owner for a message.
func ownerText(o *daemon.Owner) string {
	if o == nil {
		return "(none recorded)"
	}
	return fmt.Sprintf("%s %q", o.Kind, o.ID)
}

// waitFor polls cond once at once and then every cutoverPoll until it reports
// done or budget runs out on the plan's clock, and returns the last reason cond
// gave when it never did. A canceled context ends the wait as not done.
func (p *cutoverPlan) waitFor(ctx context.Context, budget time.Duration, cond func() (bool, string)) (bool, string) {
	deadline := p.clk.Now().Add(budget)
	for {
		done, why := cond()
		if done {
			return true, ""
		}
		if !p.clk.Now().Before(deadline) {
			return false, why
		}
		select {
		case <-ctx.Done():
			return false, fmt.Sprintf("%s (%v)", why, ctx.Err())
		case <-p.clk.After(cutoverPoll):
		}
	}
}

// runBuilder builds one supervisor argv and runs it. A builder that refuses
// (the container's settings are unset) is the error, and nothing ran.
func (p *cutoverPlan) runBuilder(build func() ([]string, error)) error {
	argv, err := build()
	if err != nil {
		return err
	}
	return p.run(argv)
}

func (p *cutoverPlan) say(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// containerStopped says whether a container's job state proves it is not
// running: docker does not know it, or reports a state that is not running. A
// state the reader could not take is never stopped.
func containerStopped(st templates.SupervisorState) bool {
	if st.Err != nil {
		return false
	}
	if !st.Loaded {
		return true
	}
	switch st.Status {
	case "created", "exited", "dead":
		return true
	}
	return false
}

// launchdStopped says whether the launchd job's state proves it is not running:
// the job is not loaded.
func launchdStopped(st templates.SupervisorState) bool {
	return st.Err == nil && !st.Loaded
}

// cutoverSides builds the launchd side and the container side of townRoot from
// its settings. The container side refuses, through its builders, a town that
// does not set both container fields.
func (c *daemonControl) cutoverSides(townRoot string) (launchd, container cutoverSide, err error) {
	plist, err := c.plistPath()
	if err != nil {
		return launchd, container, fmt.Errorf("finding the launchd job's file: %w", err)
	}
	l := c.fileSupervisor(plist, config.SupervisorLaunchd)
	cfg, err := c.townDaemonConfig(townRoot)
	if err != nil {
		return launchd, container, fmt.Errorf("reading the town's daemon settings: %w", err)
	}
	k, err := c.containerSupervisor(composeFilePath(townRoot), cfg)
	if err != nil {
		return launchd, container, err
	}
	launchd = cutoverSide{
		name:      config.SupervisorLaunchd,
		owner:     daemon.Owner{Kind: config.SupervisorLaunchd, ID: templates.LaunchdLabel},
		stop:      l.stop,
		start:     l.start,
		bootstrap: l.bootstrap,
		job:       l.jobState,
		stopped:   launchdStopped,
		restore:   l.load,
		diagnose:  fmt.Sprintf("launchctl print gui/%d/%s, and gt daemon logs", c.uid, templates.LaunchdLabel),
	}
	container = cutoverSide{
		name:     config.SupervisorContainer,
		stop:     k.stop,
		start:    k.start,
		job:      k.jobState,
		stopped:  containerStopped,
		restore:  k.load,
		diagnose: fmt.Sprintf("docker --context %s logs %s", c.dockerCLIContext(), cfg.ContainerName()),
	}
	if name, err := k.owner(); err == nil {
		container.owner = name
	} else {
		return launchd, container, err
	}
	return launchd, container, nil
}

// cutoverTo builds the plan that moves townRoot's daemon to the side named to
// ("container" or "launchd"), on the control's real seams except the ones the
// caller overrides afterwards.
func (c *daemonControl) cutoverTo(townRoot, to string, quiet QuietProbes, clk clockwork.Clock, out io.Writer) (*cutoverPlan, error) {
	launchd, container, err := c.cutoverSides(townRoot)
	if err != nil {
		return nil, err
	}
	p := &cutoverPlan{
		townRoot: townRoot,
		clk:      clk,
		out:      out,
		quiet: func(ctx context.Context, budget time.Duration) error {
			return WaitQuiet(ctx, quiet, clk, budget, defaultQuietPoll)
		},
		state: func() (*daemon.State, error) { return readCutoverState(townRoot) },
		run:   c.run,
	}
	switch to {
	case config.SupervisorContainer:
		p.from, p.to = launchd, container
		p.rollback = "gt daemon cutover --to launchd"
	case config.SupervisorLaunchd:
		p.from, p.to = container, launchd
		p.rollback = "gt daemon cutover --to container"
	default:
		return nil, fmt.Errorf("--to must be %s or %s, not %q", config.SupervisorContainer, config.SupervisorLaunchd, to)
	}
	p.guard = func(side cutoverSide) error {
		return c.guardCutoverStart(townRoot, side)
	}
	return p, nil
}

// guardCutoverStart asks the startup guard whether side's daemon may take the
// town, as the daemon itself will when it starts.
func (c *daemonControl) guardCutoverStart(townRoot string, side cutoverSide) error {
	guard := c.guard
	if guard == nil {
		guard = realStartGuard
	}
	return guard(townRoot, side.owner, false)
}

// readCutoverState reads state.json for the cutover's waits: nil when the file
// is missing, and an error when it cannot be read or does not parse.
func readCutoverState(townRoot string) (*daemon.State, error) {
	data, err := os.ReadFile(daemon.StateFile(townRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st daemon.State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", daemon.StateFile(townRoot), err)
	}
	return &st, nil
}

var daemonCutoverCmd = &cobra.Command{
	Use:   "cutover --to container|launchd",
	Short: "Move the town's daemon between launchd and its container",
	Long: `Move the town's daemon to the other supervisor in five steps, each with a
budget and a stated ending when it runs out:

  1. wait for a quiet town (dispatch hold on, no gt sling running, container-gate
     slot free), 30 minutes; nothing is changed if it never comes
  2. check quiet again immediately before the next step
  3. stop the old daemon and wait for proof it stopped: state.json records a clean
     stop and the old job reads as not running; on timeout the new side is not
     started and the command that restores the old one is printed
  4. start the new daemon through the startup guard and wait 2 minutes for
     state.json to name it as the owner; on timeout the rollback and its logs
     command are printed
  5. wait for the new daemon's second heartbeat; on timeout it is reported
     unhealthy with the rollback command

Nothing is rolled back by itself. The operator creates the dispatch hold; this
command only reads it. After a cutover set daemon.supervisor to match the new
side, so gt daemon stop, start and status act on it.`,
	Args: cobra.NoArgs,
	RunE: runDaemonCutover,
}

func init() {
	daemonCutoverCmd.Flags().String("to", "", "Supervisor to move the daemon to: container or launchd")
	_ = daemonCutoverCmd.MarkFlagRequired("to")
	daemonCmd.AddCommand(daemonCutoverCmd)
}

func runDaemonCutover(cmd *cobra.Command, args []string) error {
	return cutoverRunE(workspace.FindFromCwdOrError, cutoverCommand)(cmd, args)
}

// cutoverRunE is the command's RunE over its two seams, the town finder and the
// cutover itself, so a test drives the command's own error and success paths
// without a town on disk or a real daemon.
func cutoverRunE(find func() (string, error), cutover func(ctx context.Context, out io.Writer, townRoot, to string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		to, err := cmd.Flags().GetString("to")
		if err != nil {
			return err
		}
		townRoot, err := find()
		if err != nil {
			return fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
		if err := cutover(cmd.Context(), cmd.OutOrStdout(), townRoot, to); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s Cutover to %s done.\n", style.Bold.Render("✓"), to)
		return nil
	}
}

// cutoverCommand runs one cutover of townRoot's daemon to the side named to on
// the real control, with the host's quiet probes read through deps and the
// waits on clk.
func cutoverCommand(ctx context.Context, out io.Writer, townRoot, to string) error {
	return runCutover(ctx, out, townRoot, to, realDaemonControl(), clockwork.NewRealClock(), QuietProbeDeps{})
}

// runCutover is cutoverCommand over its seams: the control, the clock and the
// quiet probes' host reads.
func runCutover(ctx context.Context, out io.Writer, townRoot, to string, c *daemonControl, clk clockwork.Clock, deps QuietProbeDeps) error {
	quiet, err := NewQuietProbes(townRoot, deps)
	if err != nil {
		return err
	}
	p, err := c.cutoverTo(townRoot, to, quiet, clk, out)
	if err != nil {
		return err
	}
	return p.execute(ctx)
}

// containerDeploySupervisor is the production daemonimage.Supervisor: the
// container town's start builder and the container its settings name. It is
// built from the one containerComposeProject-validated settings read, so the
// container name Deploy checks a render against is the one the start argv
// recreates (gt-qpeei.25, gt-qpeei.30).
type containerDeploySupervisor struct {
	start     func() ([]string, error)
	container string
}

var _ daemonimage.Supervisor = containerDeploySupervisor{}

// StartArgv builds the container start argv, or the supervisor's refusal.
func (s containerDeploySupervisor) StartArgv() ([]string, error) { return s.start() }

// ContainerName is the container the town's settings name.
func (s containerDeploySupervisor) ContainerName() string { return s.container }

// deploySupervisor is townRoot's container supervisor as the Deploy interface
// wants it. A town that does not set both container fields is refused here, so
// no default container name is ever offered for a render.
func (c *daemonControl) deploySupervisor(townRoot string) (daemonimage.Supervisor, error) {
	cfg, err := c.townDaemonConfig(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading the town's daemon settings: %w", err)
	}
	if _, err := containerComposeProject(cfg); err != nil {
		return nil, err
	}
	sup, err := c.containerSupervisor(composeFilePath(townRoot), cfg)
	if err != nil {
		return nil, err
	}
	return containerDeploySupervisor{start: sup.start, container: cfg.ContainerName()}, nil
}

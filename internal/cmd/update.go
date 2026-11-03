package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// updateHeartbeatInterval is how often a running job refreshes its
	// heartbeat, so a hung job looks different from a slow one.
	updateHeartbeatInterval = 15 * time.Second

	// updateDrainTimeout, updateDrainIdleAfter and updateDrainPoll bound the
	// wait for working polecats; a working seat silent for the idle window
	// counts as drained.
	updateDrainTimeout   = 30 * time.Minute
	updateDrainIdleAfter = 5 * time.Minute
	updateDrainPoll      = 15 * time.Second

	// updateRestartWait and updateRestartPoll bound the wait for the daemon to
	// come back on the new build.
	updateRestartWait = 15 * time.Minute
	updateRestartPoll = 15 * time.Second

	// updateMaintWait and updateMaintPoll bound the wait for the daemon's
	// maintenance cycle.
	updateMaintWait = 30 * time.Minute
	updateMaintPoll = 15 * time.Second

	// updateRunIDSuffixBytes is how much entropy the run id's suffix carries.
	updateRunIDSuffixBytes = 4

	// updateRunIDLayout is the run id's timestamp: UTC, to the second.
	updateRunIDLayout = "20060102T150405Z"
)

// updateRequest is the command line, parsed into the shape runUpdate takes.
type updateRequest struct {
	DryRun     bool
	Skip       []string
	Only       []string
	Force      bool
	JSON       bool
	FromDaemon bool
}

// updateSelectableSteps names the steps --skip and --only accept, in the order
// a run works through them. The plan is not selectable: it always runs.
var updateSelectableSteps = []update.StepName{
	update.StepDrain,
	update.StepMaintenance,
	update.StepInstall,
	update.StepRestart,
	update.StepVerify,
	update.StepLift,
}

// updateSeams are the command's dependencies: the town lookup, the effects
// builder and the orchestrator. Production passes the real ones; a test passes
// fakes, so no test swaps a package variable.
type updateSeams struct {
	FindTownRoot func() (string, error)
	DepsFactory  func(townRoot string) (update.Deps, error)
	Run          func(context.Context, update.Deps, update.Options) (update.State, error)
}

// productionUpdateSeams wires the real town, effects and orchestrator.
func productionUpdateSeams() updateSeams {
	return updateSeams{
		FindTownRoot: workspace.FindFromCwdOrError,
		DepsFactory:  realUpdateDeps,
		Run:          update.Run,
	}
}

// updateCmd is the `gt update` job. `gt update status` lands separately.
var updateCmd = newUpdateCmd()

func init() {
	rootCmd.AddCommand(updateCmd)
}

// newUpdateCmd builds the production command; a test calls newUpdateCmdWith
// for fresh flag state over its own seams.
func newUpdateCmd() *cobra.Command {
	return newUpdateCmdWith(productionUpdateSeams())
}

// newUpdateCmdWith builds the command over seams.
func newUpdateCmdWith(seams updateSeams) *cobra.Command {
	var req updateRequest
	c := &cobra.Command{
		Use:     "update",
		GroupID: GroupConfig,
		Short:   "Install the latest build and run the town's maintenance as one job",
		Long: `Install the latest build and run the town's maintenance as one job.

A run takes the update lock, plans which steps have work, then drains the
polecats, asks the daemon for its maintenance cycle, builds and installs the
new binary, waits for the daemon's restart, verifies the town and releases its
hold. The plan is printed first, and --dry-run stops there.

Exit status: 0 for an ok run or a dry run, 1 failed, 3 partial, 4 aborted,
5 when another run holds the lock, 130 when interrupted.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd, req, seams)
		},
	}
	c.Flags().BoolVar(&req.DryRun, "dry-run", false, "Print the plan and change nothing")
	c.Flags().StringArrayVar(&req.Skip, "skip", nil, "Skip a step (repeatable): "+updateStepNames())
	c.Flags().StringArrayVar(&req.Only, "only", nil, "Run only a step (repeatable): "+updateStepNames())
	c.Flags().BoolVar(&req.Force, "force", false, "Plan an install even when the installed build is main")
	c.Flags().BoolVar(&req.JSON, "json", false, "Print the final state as JSON instead of the text report")
	c.Flags().BoolVar(&req.FromDaemon, "from-daemon", false, "Record the run's source as daemon instead of manual")
	return c
}

// runUpdate runs one update job over seams and turns its outcome into an exit
// status.
func runUpdate(cmd *cobra.Command, req updateRequest, seams updateSeams) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	skip, err := parseUpdateSelection(req.Skip)
	if err != nil {
		return err
	}
	only, err := parseUpdateSelection(req.Only)
	if err != nil {
		return err
	}

	townRoot, err := seams.FindTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	deps, err := seams.DepsFactory(townRoot)
	if err != nil {
		return fmt.Errorf("preparing the update: %w", err)
	}

	release, err := update.AcquireLock(update.Dir(townRoot))
	if errors.Is(err, update.ErrLocked) {
		return reportUpdateLocked(errOut, deps.Store)
	}
	if err != nil {
		return fmt.Errorf("taking the update lock: %w", err)
	}
	// Every way out of the run drops the lock; the explicit release below
	// frees it before the report is printed and this defer covers an early
	// return. AcquireLock's release is safe to call more than once.
	defer release()

	ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	ticker := time.NewTicker(updateHeartbeatInterval)
	defer ticker.Stop()
	stopHeartbeat := startUpdateHeartbeat(ctx, deps.Store, ticker.C, deps.Clock, updateHeartbeatError(errOut))

	st, runErr := seams.Run(ctx, deps, updateOptions(req, skip, only))
	stopHeartbeat()
	release()

	// A run that stopped without recording how it ended was either interrupted
	// or failed before it could publish an outcome. Cancellation is the
	// command's to name: Run may return only the context's error, and the
	// process must still report "interrupted" and exit 130.
	if !updateOutcomeTerminal(st.Outcome) {
		switch {
		case updateCancelled(ctx, runErr):
			st.Outcome = update.OutcomeInterrupted
		case runErr != nil:
			fmt.Fprintf(errOut, "gt update: %v\n", runErr)
			return NewSilentExit(1)
		default:
			fmt.Fprintln(errOut, "gt update: the run ended without a terminal outcome")
			return NewSilentExit(1)
		}
	}
	if err := writeUpdateResult(out, st, req.JSON); err != nil {
		fmt.Fprintf(errOut, "gt update: %v\n", err)
		return NewSilentExit(1)
	}
	// A run can end with a terminal outcome and still report a failure to
	// record it, such as a state write after the last step. The outcome decides
	// the exit status; the error is printed after the report rather than
	// replacing it.
	if runErr != nil {
		fmt.Fprintf(errOut, "gt update: %v\n", runErr)
	}
	code := updateOutcomeExitCode(st.Outcome)
	if code == 0 {
		return nil
	}
	return NewSilentExit(code)
}

// updateOutcomeTerminal reports whether the outcome says how the run ended,
// rather than leaving it running or unset.
func updateOutcomeTerminal(outcome update.Outcome) bool {
	switch outcome {
	case update.OutcomeOK, update.OutcomeFailed, update.OutcomePartial, update.OutcomeAborted, update.OutcomeInterrupted:
		return true
	default:
		return false
	}
}

// updateCancelled reports whether the run was interrupted: the command's
// context ended, or the orchestrator returned that error itself.
func updateCancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// parseUpdateSelection turns repeated --skip or --only names into the plan
// selection. An unknown name is an error that lists the valid ones.
func parseUpdateSelection(names []string) (map[update.StepName]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	selection := make(map[update.StepName]bool, len(names))
	for _, name := range names {
		step, ok := updateStepNamed(name)
		if !ok {
			return nil, fmt.Errorf("unknown update step %q; the valid steps are %s", name, updateStepNames())
		}
		selection[step] = true
	}
	return selection, nil
}

// updateStepNamed reports the step name that matches, so a typo is refused
// rather than silently selecting nothing.
func updateStepNamed(name string) (update.StepName, bool) {
	for _, step := range updateSelectableSteps {
		if string(step) == name {
			return step, true
		}
	}
	return "", false
}

// updateStepNames lists the selectable steps for a refusal or a flag's help.
func updateStepNames() string {
	names := make([]string, len(updateSelectableSteps))
	for i, step := range updateSelectableSteps {
		names[i] = string(step)
	}
	return strings.Join(names, ", ")
}

// updateOptions is the run's Options with the command line applied and the
// production wait bounds in place.
func updateOptions(req updateRequest, skip, only map[update.StepName]bool) update.Options {
	source := update.SourceManual
	if req.FromDaemon {
		source = update.SourceDaemon
	}
	return update.Options{
		DryRun: req.DryRun,
		Skip:   skip,
		Only:   only,
		Force:  req.Force,
		Source: source,
		Drain: update.DrainOptions{
			Timeout:   updateDrainTimeout,
			IdleAfter: updateDrainIdleAfter,
			Poll:      updateDrainPoll,
		},
		RestartWait: updateRestartWait,
		RestartPoll: updateRestartPoll,
		MaintWait:   updateMaintWait,
		MaintPoll:   updateMaintPoll,
	}
}

// updateOutcomeExitCode is the process status a finished run asks for. An
// outcome the command does not know is a failure, never a success.
func updateOutcomeExitCode(outcome update.Outcome) int {
	switch outcome {
	case update.OutcomeOK:
		return 0
	case update.OutcomeFailed:
		return 1
	case update.OutcomePartial:
		return 3
	case update.OutcomeAborted:
		return 4
	case update.OutcomeInterrupted:
		return 130
	default:
		return 1
	}
}

// writeUpdateResult prints the run's record: the plan, every step's result and
// the outcome, or the whole state as JSON when asked.
func writeUpdateResult(w io.Writer, st update.State, asJSON bool) error {
	if asJSON {
		return writeUpdateJSON(w, st)
	}
	writeUpdateReport(w, st)
	return nil
}

// writeUpdateJSON writes v as indented JSON. A value the JSON package cannot
// marshal fails it; a run's State never does, but the error is returned rather
// than dropped.
func writeUpdateJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the update state: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// writeUpdateReport is the text form: the plan the run started with, one line
// per step with its result and detail, then the outcome.
func writeUpdateReport(w io.Writer, st update.State) {
	if st.RunID != "" {
		fmt.Fprintf(w, "run: %s\n", st.RunID)
	}
	if st.Target.SHA != "" || st.Target.InstalledSHA != "" {
		fmt.Fprintf(w, "target: %s (installed: %s)\n", st.Target.SHA, st.Target.InstalledSHA)
	}
	for _, step := range st.Steps {
		action := "skip"
		if step.WillRun {
			action = "run"
		}
		fmt.Fprintf(w, "plan %s: %s: %s\n", step.Name, action, step.Reason)
	}
	for _, step := range st.Steps {
		fmt.Fprintf(w, "%s: %s", step.Name, step.Result)
		if step.Detail != "" {
			fmt.Fprintf(w, ": %s", step.Detail)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "outcome: %s\n", st.Outcome)
}

// reportUpdateLocked prints which run holds the lock. A holder that has not
// written its state yet is distinguished from one whose record cannot be read,
// so the operator never sees an unreadable record reported as no run.
func reportUpdateLocked(w io.Writer, store *update.Store) error {
	fmt.Fprintln(w, "another update run holds the lock")
	st, err := store.Read()
	if errors.Is(err, update.ErrNoState) {
		fmt.Fprintln(w, "it has not written its state yet")
		return NewSilentExit(5)
	}
	if err != nil {
		fmt.Fprintf(w, "the running run's state could not be read: %v\n", err)
		return NewSilentExit(5)
	}
	fmt.Fprintf(w, "run %s: step %s, started %s\n", st.RunID, st.Current, st.StartedAt.Format(time.RFC3339))
	return NewSilentExit(5)
}

// startUpdateHeartbeat runs the run's heartbeat until the returned stop
// function is called. Stop waits for the heartbeat to return, so no heartbeat
// write can follow the run's last state write or outlive the lock.
func startUpdateHeartbeat(ctx context.Context, store *update.Store, ticks <-chan time.Time, clock update.Clock, onError func(error)) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		update.RunHeartbeat(ctx, store, ticks, clock, onError)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// updateHeartbeatError reports a heartbeat write failure. A missing current
// state is the normal state before the run's first write and is ignored;
// every other failure is printed, because state.json is how the daemon and the
// dashboard see the job.
func updateHeartbeatError(w io.Writer) func(error) {
	return func(err error) {
		if errors.Is(err, update.ErrNoCurrent) {
			return
		}
		fmt.Fprintf(w, "gt update: heartbeat: %v\n", err)
	}
}

// realUpdateDeps wires the run's real effects: the state store, the plan and
// verify probes, the seat probe, the hold, the installer and the daemon
// request, all under one clock.
func realUpdateDeps(townRoot string) (update.Deps, error) {
	clock := updateClock{}
	repoRoot, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		return update.Deps{}, err
	}
	installer, err := realUpdateInstaller(townRoot)
	if err != nil {
		return update.Deps{}, err
	}
	return update.Deps{
		Clock: clock,
		Sleep: updateSleep,
		Store: &update.Store{Dir: update.Dir(townRoot)},
		Probes: newUpdateProbes(updateProbeEnv{
			TownRoot: townRoot,
			RepoRoot: repoRoot,
			Archive:  newArchiveGapFunc(townRoot),
		}),
		Seats:     newSeatProbe(realSeatProbeEnv(townRoot)),
		Hold:      update.NewHold(townRoot, updateFiles{}),
		Installer: installer,
		Maint: &update.MaintenanceRequest{
			Dir:   update.Dir(townRoot),
			Files: updateMaintFiles{},
			Clock: clock,
		},
		Verify:   newVerifyProbes(verifyProbeEnv{}),
		NewRunID: newUpdateRunID,
		Log:      os.Stderr,
	}, nil
}

// realUpdateInstaller builds the installer over the town's daemon settings. A
// settings file that does not parse is returned rather than ignored, so the
// trim environment never silently covers the wrong engine.
func realUpdateInstaller(townRoot string) (update.Installer, error) {
	patrol, err := loadUpdatePatrolConfig(townRoot)
	if err != nil {
		return nil, err
	}
	return newExecInstaller(townRoot, realRunFunc(), patrol)
}

// loadUpdatePatrolConfig reads the daemon settings the installer's image-trim
// environment comes from. A town with no settings file runs with none; every
// other failure is returned rather than guessed at.
func loadUpdatePatrolConfig(townRoot string) (*config.DaemonPatrolConfig, error) {
	patrol, err := config.LoadDaemonPatrolConfig(config.DaemonPatrolConfigPath(townRoot))
	if errors.Is(err, config.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return patrol, nil
}

// realSeatProbeEnv supplies the seat probe's production readings: the rig
// registry, each rig's polecat worktrees, the town's session prefixes and the
// heartbeats the daemon reads.
func realSeatProbeEnv(townRoot string) seatProbeEnv {
	return seatProbeEnv{
		Rigs: func() ([]string, error) {
			return knownRigNames(townRoot)
		},
		Polecats: func(rig string) ([]string, error) {
			return polecatDirs(filepath.Join(townRoot, rig, "polecats"))
		},
		Prefix: func(rig string) string {
			return townRegistry().PrefixForRig(rig)
		},
		Heartbeat: heartbeatReader(townRoot),
	}
}

// updateClock is the real clock: every timestamp comes from time.Now.
type updateClock struct{}

func (updateClock) Now() time.Time { return time.Now() }

// updateSleep waits d, or less when ctx ends, and reports the context's error
// so a wait stops with an interrupted run.
func updateSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// newUpdateRunID names the run with its start second and a short random
// suffix. A failed entropy read falls back to the process id, which still
// names the run uniquely enough for the lock message.
func newUpdateRunID() string {
	stamp := time.Now().UTC().Format(updateRunIDLayout)
	var suffix [updateRunIDSuffixBytes]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s-%d", stamp, os.Getpid())
	}
	return stamp + "-" + hex.EncodeToString(suffix[:])
}

// updateFiles is update.FileOps over the real file system: the dispatch hold
// the run may create, read and remove.
type updateFiles struct{}

func (updateFiles) Stat(path string) error {
	_, err := os.Stat(path)
	return err
}

func (updateFiles) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (updateFiles) CreateExclusive(path string, data []byte, mode os.FileMode) error {
	return createExclusiveFile(path, data, mode)
}

func (updateFiles) WriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func (updateFiles) Remove(path string) error { return os.Remove(path) }

// updateMaintFiles is update.MaintFiles over the real file system: the
// maintenance request and result files the run writes and reads.
type updateMaintFiles struct{}

func (updateMaintFiles) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (updateMaintFiles) CreateExclusive(path string, data []byte, mode os.FileMode) error {
	return createExclusiveFile(path, data, mode)
}

func (updateMaintFiles) Remove(path string) error { return os.Remove(path) }

// exclusiveFile is the handle an exclusive create writes and closes. A test
// supplies a fake so each write and close failure is driven without a broken
// file system.
type exclusiveFile interface {
	Write([]byte) (int, error)
	Close() error
}

// exclusiveOpener opens path exclusively for writing.
type exclusiveOpener func(path string, flag int, mode os.FileMode) (exclusiveFile, error)

// createExclusiveFile creates path with data, refusing an existing file so two
// racing runs cannot both own it.
func createExclusiveFile(path string, data []byte, mode os.FileMode) error {
	return writeExclusive(osOpenExclusive, path, data, mode)
}

// osOpenExclusive is the real opener; O_EXCL makes the file system decide
// which of two racing runs owns the path.
func osOpenExclusive(path string, flag int, mode os.FileMode) (exclusiveFile, error) {
	return os.OpenFile(path, flag, mode)
}

// writeExclusive creates path through open and writes data to it. A write or
// close failure is returned with the close's error kept, since a failed close
// can hide a failed write.
func writeExclusive(open exclusiveOpener, path string, data []byte, mode os.FileMode) error {
	f, err := open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		closeErr := f.Close()
		if closeErr != nil {
			return fmt.Errorf("write %s: %w (close file: %v)", path, err, closeErr)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/runnerroll"
	"github.com/steveyegge/gastown/internal/workspace"
)

// rollOwner is the owner the roll writes into the landing hold.
const rollOwner = "roll"

// rollHoldFile is the landing hold's file name at the town root. It matches
// runnerroll's landingHoldName, which the roll only prints in a removal command.
const rollHoldFile = "landing.hold"

// releaseBound bounds the release of the hold. The release runs after a signal
// has canceled the roll's context, so it gets a context of its own.
const releaseBound = time.Minute

// errApplyUnwired is what the default applier returns. It is refused before the
// roll takes the landing hold.
var errApplyUnwired = errors.New("the apply step is not wired yet")

// rollApplier is step 4 of the roll: the real one applies the manifests and waits
// for the rollout. Until it is wired, the roll refuses before it takes the hold.
type rollApplier interface {
	// Wired returns an error when Apply would not do real work.
	Wired() error
	Apply(ctx context.Context) error
}

// unwiredApplier is the default applier. It refuses, and does nothing else.
type unwiredApplier struct{}

func (unwiredApplier) Wired() error                { return errApplyUnwired }
func (unwiredApplier) Apply(context.Context) error { return errApplyUnwired }

// runnerRollDeps is what a roll runs with: the hold store, the two waits' reads,
// the clock they poll on, the applier, and the flags. The production command
// builds it with runnerRollDepsFor.
type runnerRollDeps struct {
	store       runnerroll.HoldStore
	holdPath    string
	landing     func(ctx context.Context) (pause string, passes int, known bool, err error)
	runs        func(ctx context.Context) ([]runnerroll.Run, error)
	clock       runnerroll.Clock
	now         func() time.Time
	apply       rollApplier
	landingWait time.Duration
	runsWait    time.Duration
	dryRun      bool
	out         io.Writer
}

var townRunnerCmd = &cobra.Command{
	Use:     "runner",
	GroupID: GroupDiag,
	Short:   "Change the gate runner's configuration",
	RunE:    requireSubcommand,
}

var townRunnerRollCmd = &cobra.Command{
	Use:   "roll",
	Short: "Roll the gate runner, holding landings and waiting for runs to finish",
	Long: `Roll the gate runner without killing a gate that is running. The steps run
in order, and each one is printed:

  1. take the landing hold, owner "roll", reason "runner roll"; a hold someone
     else holds refuses with that holder's line and nothing else is touched
  2. wait for the landing pass in flight to end (--landing-wait)
  3. wait for the gate and post-land runs to finish (--runs-wait)
  4. apply
  5. release the landing hold

The hold is released on every exit path, including SIGINT and SIGTERM. A failed
release prints the command that removes the file. A wait that times out exits
non-zero, applies nothing, and releases the hold.

The apply step is not wired yet, so a real roll refuses before it takes the
hold. --dry-run prints the steps and their bounds, and takes no hold and writes
nothing.

Examples:
  gt town runner roll --dry-run
  gt town runner roll --landing-wait 5m --runs-wait 30m`,
	Args: cobra.NoArgs,
	RunE: runTownRunnerRollCmd,
}

func init() {
	townCmd.AddCommand(townRunnerCmd)
	townRunnerCmd.AddCommand(townRunnerRollCmd)

	townRunnerRollCmd.Flags().Duration("landing-wait", 10*time.Minute, "longest wait for the landing pass in flight to end")
	townRunnerRollCmd.Flags().Duration("runs-wait", 20*time.Minute, "longest wait for the gate and post-land runs to finish")
	townRunnerRollCmd.Flags().Bool("dry-run", false, "print the steps and their bounds; take no hold and write nothing")
}

// runTownRunnerRollCmd is what `gt town runner roll` runs. A SIGINT or SIGTERM
// cancels the roll's context, so each wait returns and the hold is released.
func runTownRunnerRollCmd(cmd *cobra.Command, _ []string) error {
	landingWait, err := cmd.Flags().GetDuration("landing-wait")
	if err != nil {
		return fmt.Errorf("reading --landing-wait: %w", err)
	}
	runsWait, err := cmd.Flags().GetDuration("runs-wait")
	if err != nil {
		return fmt.Errorf("reading --runs-wait: %w", err)
	}
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("reading --dry-run: %w", err)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runTownRunnerRoll(ctx, runnerRollDepsFor(townRoot, cmd.OutOrStdout(), landingWait, runsWait, dryRun))
}

// runnerRollDepsFor is the production roll's deps. The apply step is the default
// refusal, so a real run stops before the hold. The store, the landing read and
// the runs lister are not wired here: the applier bead wires them, and until
// then the refusal comes first.
func runnerRollDepsFor(townRoot string, out io.Writer, landingWait, runsWait time.Duration, dryRun bool) runnerRollDeps {
	return runnerRollDeps{
		holdPath:    filepath.Join(townRoot, rollHoldFile),
		clock:       wallRollClock{},
		now:         time.Now,
		apply:       unwiredApplier{},
		landingWait: landingWait,
		runsWait:    runsWait,
		dryRun:      dryRun,
		out:         out,
	}
}

// wallRollClock is the production clock for the waits: the wall clock, and a
// Sleep that ends early when the context is done.
type wallRollClock struct{}

func (wallRollClock) Now() time.Time { return time.Now() }

func (wallRollClock) Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// step names step n of the roll, with its bound where it has one. The dry run and
// a real run print the same text.
func (d runnerRollDeps) step(n int) string {
	switch n {
	case 1:
		return fmt.Sprintf("take the landing hold (owner %s, reason %q)", rollOwner, runnerroll.RollPause)
	case 2:
		return fmt.Sprintf("wait for the landing pass in flight to end (bound %s)", d.landingWait)
	case 3:
		return fmt.Sprintf("wait for the gate and post-land runs to finish (bound %s)", d.runsWait)
	case 4:
		return "apply"
	default:
		return "release the landing hold"
	}
}

// runTownRunnerRoll runs the roll's five steps. The landing hold is released by a
// defer, so every exit after the take releases it. A release failure is returned
// unless the roll already failed.
func runTownRunnerRoll(ctx context.Context, d runnerRollDeps) (err error) {
	if d.landingWait <= 0 || d.runsWait <= 0 {
		return fmt.Errorf("the waits must be longer than zero: --landing-wait %s, --runs-wait %s", d.landingWait, d.runsWait)
	}
	if d.dryRun {
		fmt.Fprintln(d.out, "dry run: no hold is taken, nothing is read or written, and nothing is applied")
		for n := 1; n <= 5; n++ {
			fmt.Fprintf(d.out, "step %d/5: %s\n", n, d.step(n))
		}
		if werr := d.apply.Wired(); werr != nil {
			fmt.Fprintf(d.out, "a real roll refuses at step 4: %v\n", werr)
		}
		return nil
	}
	// Refuse before the hold: an unwired apply means the roll must not start.
	if werr := d.apply.Wired(); werr != nil {
		return werr
	}
	if d.store == nil || d.landing == nil || d.runs == nil || d.clock == nil || d.now == nil {
		return errors.New("the roll has no landing hold store or wait reads wired, so it cannot take the hold")
	}

	fmt.Fprintf(d.out, "step 1/5: %s\n", d.step(1))
	hold, err := runnerroll.Take(ctx, d.store, rollOwner, runnerroll.RollPause, d.now())
	if err != nil {
		return fmt.Errorf("taking the landing hold: %w", err)
	}
	defer func() {
		fmt.Fprintf(d.out, "step 5/5: %s\n", d.step(5))
		if rerr := d.releaseHold(ctx, hold); rerr != nil && err == nil {
			err = rerr
		}
	}()

	fmt.Fprintf(d.out, "step 2/5: %s\n", d.step(2))
	if err := runnerroll.WaitLandingIdle(ctx, d.landing, d.clock, d.landingWait, d.out); err != nil {
		return fmt.Errorf("waiting for the landing pass: %w", err)
	}

	fmt.Fprintf(d.out, "step 3/5: %s\n", d.step(3))
	if err := runnerroll.WaitRunsIdle(ctx, d.runs, d.clock, d.runsWait, d.out); err != nil {
		return fmt.Errorf("waiting for the gate and post-land runs: %w", err)
	}

	fmt.Fprintf(d.out, "step 4/5: %s\n", d.step(4))
	if err := d.apply.Apply(ctx); err != nil {
		return fmt.Errorf("applying the runner: %w", err)
	}
	return nil
}

// releaseHold gives the hold back on a context the roll's cancellation does not
// reach, with its own bound. A failed release prints the command that removes the
// file, and returns the error.
func (d runnerRollDeps) releaseHold(ctx context.Context, hold *runnerroll.Hold) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseBound)
	defer cancel()
	err := hold.Release(rctx)
	if err == nil {
		return nil
	}
	fmt.Fprintf(d.out, "the landing hold is still in place: %v\nremove it with: rm -f -- %s\n", err, d.holdPath)
	return fmt.Errorf("releasing the landing hold: %w", err)
}

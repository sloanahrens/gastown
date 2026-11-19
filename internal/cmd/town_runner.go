package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/runnerroll"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/workspace"
)

// rollOwner is the owner the roll writes into the landing hold.
const rollOwner = "roll"

// rollApplier is what the runner roll's target does: write the runner's
// manifests, wait for the rollout, and compare the render with the live cluster
// for --dry-run.
type rollApplier interface {
	// Wired returns an error when the applier has no seam it needs, so the roll
	// refuses before it takes the hold.
	Wired() error
	// Apply writes the runner's manifests.
	Apply(ctx context.Context) error
	// WaitReady waits for the Deployment's rollout and its Available condition.
	WaitReady(ctx context.Context) error
	// Diff returns the runner drift compare's lines, each prefixed with the
	// resource it describes. It reads the cluster and writes nothing.
	Diff(ctx context.Context) ([]string, error)
}

// runnerTarget is the runner as runnerroll.Run's target.
type runnerTarget struct {
	applier rollApplier
}

// Name names the runner in the roll's messages.
func (t runnerTarget) Name() string { return "runner" }

func (t runnerTarget) Diff(ctx context.Context) ([]string, error) { return t.applier.Diff(ctx) }

func (t runnerTarget) Apply(ctx context.Context) error { return t.applier.Apply(ctx) }

func (t runnerTarget) WaitReady(ctx context.Context) error { return t.applier.WaitReady(ctx) }

// readyConditionJSONPath reads the Deployment's Available condition: the one
// the API server sets when the Deployment has its minimum availability.
const readyConditionJSONPath = `jsonpath={.status.conditions[?(@.type=="Available")].status}`

// runnerApplier is the roll's real apply: it builds the runner's manifests from
// the checked-in control values and applies them through the injected cluster
// client, then waits for the rollout and the Deployment's readiness.
type runnerApplier struct {
	// kubectl is the cluster client every call goes through, so a test records
	// them and nothing reaches a cluster.
	kubectl update.RunFunc
	// values reads the checked-in values, and build renders them, so a manifest
	// that fails to build is an error before anything is applied.
	values func() (forgejorunner.Values, error)
	build  func(forgejorunner.Values) (forgejorunner.Manifests, error)
	// encode writes one manifest as the YAML kubectl reads on stdin.
	encode func(io.Writer, any) error
	// rolloutWait bounds the rollout status wait.
	rolloutWait time.Duration
	out         io.Writer
}

// Wired reports the seam the applier is missing. A roll without one refuses
// before it takes the landing hold rather than failing halfway through.
func (a runnerApplier) Wired() error {
	switch {
	case a.kubectl == nil:
		return errors.New("no cluster client is wired, so the roll cannot apply the runner")
	case a.values == nil || a.build == nil:
		return errors.New("no runner values or renderer are wired, so the roll cannot build the manifests")
	case a.encode == nil:
		return errors.New("no manifest encoder is wired, so the roll cannot write the manifests for kubectl")
	case a.rolloutWait <= 0:
		return fmt.Errorf("the rollout wait must be longer than zero, not %s", a.rolloutWait)
	}
	return nil
}

// manifests is the render the apply and the diff both start from.
func (a runnerApplier) manifests() (forgejorunner.Manifests, error) {
	v, err := a.values()
	if err != nil {
		return forgejorunner.Manifests{}, fmt.Errorf("reading the control cluster's runner values: %w", err)
	}
	m, err := a.build(v)
	if err != nil {
		return forgejorunner.Manifests{}, fmt.Errorf("rendering the runner: %w", err)
	}
	return m, nil
}

// Apply writes the ConfigMap, then the two claims, then the Deployment. A build
// or an apply that fails returns an error, so the roll stops and leaves the
// caller to release the hold. The Recreate strategy rolls the pod; this never
// deletes it.
func (a runnerApplier) Apply(ctx context.Context) error {
	m, err := a.manifests()
	if err != nil {
		return err
	}
	objects := []struct {
		kind   string
		name   string
		object any
	}{
		{"configmap", m.ConfigMap.Metadata.Name, m.ConfigMap},
		{"persistentvolumeclaim", m.DindClaim.Metadata.Name, m.DindClaim},
		{"persistentvolumeclaim", m.RunnerClaim.Metadata.Name, m.RunnerClaim},
		{"deployment", m.Deployment.Metadata.Name, m.Deployment},
	}
	for _, object := range objects {
		if err := a.apply(ctx, object.kind, object.name, object.object); err != nil {
			return err
		}
	}
	return nil
}

// WaitReady waits for the Deployment's rollout within rolloutWait, then reads
// its Available condition. It renders the manifests again to find the
// Deployment, so it reads the values a second time and holds no state between
// the two steps.
func (a runnerApplier) WaitReady(ctx context.Context) error {
	m, err := a.manifests()
	if err != nil {
		return err
	}
	return a.waitReady(ctx, m.Deployment.Metadata.Namespace, m.Deployment.Metadata.Name)
}

// apply encodes one manifest and hands it to kubectl on stdin. The error names
// the object and kubectl's exit status, never its output, since a manifest's
// fields can hold a value that is a secret.
func (a runnerApplier) apply(ctx context.Context, kind, name string, object any) error {
	var body bytes.Buffer
	if err := a.encode(&body, object); err != nil {
		return fmt.Errorf("building the %s %s: %w", kind, name, err)
	}
	fmt.Fprintf(a.out, "applying %s/%s\n", kind, name)
	_, exit, err := a.kubectl(withRunStdin(ctx, body.Bytes()), "", nil, "kubectl", "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("applying the %s %s: %w", kind, name, err)
	}
	if exit != 0 {
		return fmt.Errorf("applying the %s %s: kubectl exited %d", kind, name, exit)
	}
	return nil
}

// waitReady waits for the Deployment's rollout within rolloutWait, then reads
// its Available condition. A rollout status that exits non-zero, or a condition
// that is not True, is an error: a Deployment kubectl could not confirm ready is
// not one, and the roll must not report success.
func (a runnerApplier) waitReady(ctx context.Context, namespace, name string) error {
	resource := "deployment/" + name
	fmt.Fprintf(a.out, "waiting for %s to roll out (bound %s)\n", resource, a.rolloutWait)
	_, exit, err := a.kubectl(ctx, "", nil, "kubectl", "rollout", "status", resource, "-n", namespace, "--timeout="+a.rolloutWait.String())
	if err != nil {
		return fmt.Errorf("waiting for the rollout of %s: %w", resource, err)
	}
	if exit != 0 {
		return fmt.Errorf("%s did not complete its rollout within %s: kubectl rollout status exited %d", resource, a.rolloutWait, exit)
	}
	out, exit, err := a.kubectl(ctx, "", nil, "kubectl", "get", "deployment", name, "-n", namespace, "-o", readyConditionJSONPath)
	if err != nil {
		return fmt.Errorf("reading the readiness of %s: %w", resource, err)
	}
	if exit != 0 {
		return fmt.Errorf("reading the readiness of %s: kubectl exited %d", resource, exit)
	}
	if status := strings.TrimSpace(out); status != "True" {
		return fmt.Errorf("%s is not ready: its Available condition is %q", resource, status)
	}
	fmt.Fprintf(a.out, "%s is ready\n", resource)
	return nil
}

// Diff is the --dry-run compare: the runner drift compare's lines over the same
// render Apply writes, or none when the live objects match it.
func (a runnerApplier) Diff(ctx context.Context) ([]string, error) {
	targets, err := runnerDriftTargetsFrom(a.values, a.build)
	if err != nil {
		return nil, err
	}
	d := clusterDriftDeps{
		render:  clusterRenderDeps{encode: a.encode},
		kubectl: a.kubectl,
	}
	var lines []string
	for _, target := range targets {
		differences, err := target.differences(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, line := range differences {
			lines = append(lines, target.resource+": "+line)
		}
	}
	return lines, nil
}

// rollSignals is the roll's signal source: a context a signal cancels, and the
// function that stops listening for another. It is a seam so a test delivers the
// signal itself.
type rollSignals func() (context.Context, context.CancelFunc)

// liveRollSignals is the production source: SIGINT and SIGTERM cancel the roll.
func liveRollSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// runnerRollDeps is what a roll runs with: the hold store, the two waits' reads,
// the clock they poll on, the signal source, the applier, and the flags. The
// production command builds it with runnerRollDepsFor.
type runnerRollDeps struct {
	store       runnerroll.HoldStore
	holdPath    string
	landing     func(ctx context.Context) (pause string, passes int, known bool, err error)
	runs        func(ctx context.Context) ([]runnerroll.WorkflowRun, error)
	clock       runnerroll.Clock
	now         func() time.Time
	signals     rollSignals
	apply       rollApplier
	landingWait time.Duration
	runsWait    time.Duration
	rolloutWait time.Duration
	dryRun      bool
	out         io.Writer
}

var townRunnerCmd = &cobra.Command{
	Use:   "runner",
	Short: "Change the gate runner's configuration",
	RunE:  requireSubcommand,
}

var townRunnerRollCmd = &cobra.Command{
	Use:   "roll",
	Short: "Roll the gate runner, holding landings and waiting for runs to finish",
	Long: `Roll the gate runner without killing a gate that is running. --dry-run
prints the steps, their bounds and their order, then the runner drift compare's
lines against the live cluster, and takes no hold and writes nothing.

The hold is released on every exit path, including SIGINT and SIGTERM. A failed
release reads the hold again and prints what to do by whose hold it is now: a
removal command only while the file is this roll's. A wait that times out, a
manifest that cannot be built, an apply that fails and a rollout that does not
become ready each exit non-zero and release the hold. The Deployment's Recreate
strategy rolls the pod; this never deletes it.

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
	townRunnerRollCmd.Flags().Duration("rollout-wait", 10*time.Minute, "longest wait for the Deployment's rollout to complete")
	townRunnerRollCmd.Flags().Bool("dry-run", false, "print the steps and their bounds, then the drift lines; take no hold and apply nothing")
}

// runTownRunnerRollCmd is what `gt town runner roll` runs, in the town the
// working directory is in.
func runTownRunnerRollCmd(cmd *cobra.Command, _ []string) error {
	return runTownRunnerRollIn(cmd, workspace.FindFromCwdOrError)
}

// runTownRunnerRollIn reads the roll's flags from cmd and runs the roll in the
// town findTown names.
func runTownRunnerRollIn(cmd *cobra.Command, findTown func() (string, error)) error {
	landingWait, err := cmd.Flags().GetDuration("landing-wait")
	if err != nil {
		return fmt.Errorf("reading --landing-wait: %w", err)
	}
	runsWait, err := cmd.Flags().GetDuration("runs-wait")
	if err != nil {
		return fmt.Errorf("reading --runs-wait: %w", err)
	}
	rolloutWait, err := cmd.Flags().GetDuration("rollout-wait")
	if err != nil {
		return fmt.Errorf("reading --rollout-wait: %w", err)
	}
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("reading --dry-run: %w", err)
	}
	townRoot, err := findTown()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return runTownRunnerRoll(runnerRollDepsFor(townRoot, cmd.OutOrStdout(), landingWait, runsWait, rolloutWait, dryRun))
}

// runnerRollDepsFor is the production roll's deps. The applier is wired to the
// control cluster's checked-in values, the renderer and kubectl. The hold store
// and the two wait reads are not wired here, so a real roll refuses before it
// takes the hold until another bead supplies them; --dry-run needs neither.
func runnerRollDepsFor(townRoot string, out io.Writer, landingWait, runsWait, rolloutWait time.Duration, dryRun bool) runnerRollDeps {
	return runnerRollDeps{
		holdPath: filepath.Join(townRoot, runnerroll.LandingHoldName),
		clock:    wallRollClock{},
		now:      time.Now,
		signals:  liveRollSignals,
		apply: runnerApplier{
			kubectl:     realRunFunc(),
			values:      forgejorunner.ControlValues,
			build:       forgejorunner.Build,
			encode:      forgejorunner.YAML,
			rolloutWait: rolloutWait,
			out:         out,
		},
		landingWait: landingWait,
		runsWait:    runsWait,
		rolloutWait: rolloutWait,
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
		return fmt.Sprintf("apply the manifests, then wait for the rollout (bound %s) and the Deployment's readiness condition", d.rolloutWait)
	default:
		return "release the landing hold"
	}
}

// rollContext is the context the roll runs under: the shared signal rule's,
// which a dry run may run without since it takes no hold and only reads.
func (d runnerRollDeps) rollContext() (context.Context, context.CancelFunc) {
	return rollContextFrom(d.signals)
}

// runTownRunnerRoll runs the roll over the runner: runnerroll.Run owns the
// steps, and this supplies the runner's seams, its step names and its target.
func runTownRunnerRoll(d runnerRollDeps) error {
	if d.apply == nil {
		return errors.New("the roll has no applier, so it cannot apply the runner")
	}
	// Refuse before the hold: an applier missing a seam it needs would otherwise
	// fail halfway through.
	if err := d.apply.Wired(); err != nil {
		return err
	}
	// A roll with no signal source cannot stop on Ctrl-C, and the hold it took
	// would outlive the process.
	if !d.dryRun && d.signals == nil {
		return errors.New("the roll has no signal source, so an interrupted roll could not stop listening")
	}
	ctx, stop := d.rollContext()
	defer stop()
	deps := runnerroll.RollDeps{
		Store:    d.store,
		HoldPath: d.holdPath,
		Owner:    rollOwner,
		Reason:   runnerroll.RollPause,
		Landing:  d.landing,
		Runs:     d.runs,
		Clock:    d.clock,
		Now:      d.now,
		Out:      d.out,
	}
	opts := runnerroll.RollOptions{
		LandingWait: d.landingWait,
		RunsWait:    d.runsWait,
		RolloutWait: d.rolloutWait,
		DryRun:      d.dryRun,
		Step:        d.step,
	}
	return runnerroll.Run(ctx, deps, runnerTarget{applier: d.apply}, opts)
}

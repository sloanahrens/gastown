package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
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
	m, err := a.manifests()
	if err != nil {
		return nil, err
	}
	return runnerDriftDifferences(ctx, a.kubectl, a.encode, m)
}

// runnerDriftDifferences is the gate runner's drift compare (gt-pkz70c) over one
// render: every rendered field of the Deployment and its ConfigMap the live
// objects carry differently, each prefixed with the resource it describes, or
// none when they match. It reads the cluster and writes nothing.
func runnerDriftDifferences(ctx context.Context, kubectl update.RunFunc, encode func(io.Writer, any) error, m forgejorunner.Manifests) ([]string, error) {
	d := clusterDriftDeps{
		render:  clusterRenderDeps{encode: encode},
		kubectl: kubectl,
	}
	var lines []string
	for _, target := range runnerDriftTargets(m) {
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
	townRunnerCmd.AddCommand(townRunnerResizeCmd)

	townRunnerRollCmd.Flags().Duration("landing-wait", 10*time.Minute, "longest wait for the landing pass in flight to end")
	townRunnerRollCmd.Flags().Duration("runs-wait", 20*time.Minute, "longest wait for the gate and post-land runs to finish")
	townRunnerRollCmd.Flags().Duration("rollout-wait", 10*time.Minute, "longest wait for the Deployment's rollout to complete")
	townRunnerRollCmd.Flags().Bool("dry-run", false, "print the steps and their bounds, then the drift lines; take no hold and apply nothing")
	townRunnerResizeCmd.Flags().String(dindCPUFlag, "", "the engine sidecar's CPU: a positive whole number of cores, such as 4, or of millicores, such as 4000m")
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

// dindCPUFlag is the resize's flag: the CPU the engine sidecar should carry.
const dindCPUFlag = "dind-cpu"

// runnerPodsSelector selects the runner's pods by the label its Deployment's
// selector carries.
const runnerPodsSelector = "app.kubernetes.io/name=" + forgejorunner.Name

// The resize conditions the kubelet sets on a pod whose in-place resize it has
// not finished. PodResizePending carries the Infeasible and Deferred reasons,
// which leave the pod at its old limit.
const (
	podResizePending    = "PodResizePending"
	podResizeInProgress = "PodResizeInProgress"
)

// dindCPURE is the whole grammar --dind-cpu accepts: a positive whole number
// of cores, or of millicores with the m suffix.
var dindCPURE = regexp.MustCompile(`^[1-9][0-9]*m?$`)

var townRunnerResizeCmd = &cobra.Command{
	Use:   "resize --dind-cpu N",
	Short: "Change the gate runner's engine CPU in place, without a restart",
	Long: `Patch the runner pod's engine sidecar to N CPU through the resize
subresource, then read the pod back and print the limit it now asks for. N is a
positive whole number of cores, such as 4, or of millicores, such as 4000m, and
at most the allocatable CPU of the node the pod runs on.

The patch is the only write; the pod is never deleted, restarted or replaced. A
pod that reports the resize as pending or infeasible exits non-zero with the
pod's resize status, and one whose resize is in progress prints the status and
the requested limit.

The command then runs the gate runner's drift compare and says whether the
rendered pod template asks for the same CPU. When the template asks for
another value, the command names it and its path: the next restart builds the
pod from the template and reverts the limit, since a resize changes only the
running pod.

Examples:
  gt town runner resize --dind-cpu 12
  gt town runner resize --dind-cpu 6000m`,
	Args: cobra.NoArgs,
	RunE: runTownRunnerResizeCmd,
}

// runTownRunnerResizeCmd is what `gt town runner resize` runs.
func runTownRunnerResizeCmd(cmd *cobra.Command, _ []string) error {
	return runTownRunnerResizeIn(cmd, runnerResizeDepsFor)
}

// runTownRunnerResizeIn reads --dind-cpu from cmd and runs the resize over the
// deps depsFor builds, so a test drives the command's own flag read.
func runTownRunnerResizeIn(cmd *cobra.Command, depsFor func(io.Writer, string) runnerResizeDeps) error {
	cpu, err := cmd.Flags().GetString(dindCPUFlag)
	if err != nil {
		return fmt.Errorf("reading --%s: %w", dindCPUFlag, err)
	}
	return runTownRunnerResize(cmd.Context(), depsFor(cmd.OutOrStdout(), cpu))
}

// runnerResizeDeps is what a resize runs with: the cluster client every call
// goes through, the render's seams for the template and the drift compare, the
// CPU the flag names, and where it prints.
type runnerResizeDeps struct {
	kubectl update.RunFunc
	values  func() (forgejorunner.Values, error)
	build   func(forgejorunner.Values) (forgejorunner.Manifests, error)
	encode  func(io.Writer, any) error
	dindCPU string
	out     io.Writer
}

// runnerResizeDepsFor is the production resize's deps: the control cluster's
// checked-in values and renderer, the encoder the drift compare reads, kubectl,
// and the flag's CPU.
func runnerResizeDepsFor(out io.Writer, cpu string) runnerResizeDeps {
	return runnerResizeDeps{
		kubectl: realRunFunc(),
		values:  forgejorunner.ControlValues,
		build:   forgejorunner.Build,
		encode:  forgejorunner.YAML,
		dindCPU: cpu,
		out:     out,
	}
}

// Wired reports the seam the resize is missing, so the command refuses before
// its first cluster call rather than partway through.
func (d runnerResizeDeps) Wired() error {
	switch {
	case d.kubectl == nil:
		return errors.New("no cluster client is wired, so the resize cannot reach the runner")
	case d.values == nil || d.build == nil:
		return errors.New("no runner values or renderer are wired, so the resize cannot read the template")
	case d.encode == nil:
		return errors.New("no manifest encoder is wired, so the resize cannot compare the template")
	case d.out == nil:
		return errors.New("no output is wired, so the resize cannot report what it did")
	}
	return nil
}

// manifests is the render the resize, the drift compare and the template read
// start from.
func (d runnerResizeDeps) manifests() (forgejorunner.Manifests, error) {
	v, err := d.values()
	if err != nil {
		return forgejorunner.Manifests{}, fmt.Errorf("reading the control cluster's runner values: %w", err)
	}
	m, err := d.build(v)
	if err != nil {
		return forgejorunner.Manifests{}, fmt.Errorf("rendering the runner: %w", err)
	}
	return m, nil
}

// runnerPodList is the part of `kubectl get pods -o json` the resize reads.
type runnerPodList struct {
	Items []runnerPod `json:"items"`
}

// runnerPod is one pod: its name and deletion mark, the node it runs on, its
// two container lists, and the conditions that carry a resize's status.
type runnerPod struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string            `json:"nodeName"`
		Containers     []runnerContainer `json:"containers"`
		InitContainers []runnerContainer `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Conditions []runnerPodCondition `json:"conditions"`
	} `json:"status"`
}

// runnerContainer is the part of a pod container the resize reads: its name and
// its CPU limit.
type runnerContainer struct {
	Name      string `json:"name"`
	Resources struct {
		Limits map[string]string `json:"limits"`
	} `json:"resources"`
}

// runnerPodCondition is one pod condition: a resize condition carries the
// kubelet's reason and message.
type runnerPodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// parseDindCPU reads the --dind-cpu value as milli-cores. A fraction, a sign or
// a unit other than m is refused, naming the whole grammar the flag accepts, and
// so is a value too large to hold.
func parseDindCPU(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("--%s is required: the CPU the engine sidecar should carry", dindCPUFlag)
	}
	if !dindCPURE.MatchString(value) {
		return 0, fmt.Errorf("--%s %q: want a positive whole number of cores, such as 4, or of millicores, such as 4000m", dindCPUFlag, value)
	}
	milli := strings.HasSuffix(value, "m")
	amount, err := strconv.ParseInt(strings.TrimSuffix(value, "m"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("--%s %s is too large", dindCPUFlag, value)
	}
	if !milli {
		if amount > math.MaxInt64/1000 {
			return 0, fmt.Errorf("--%s %s is too large", dindCPUFlag, value)
		}
		amount *= 1000
	}
	return amount, nil
}

// runTownRunnerResize resizes the runner pod's engine CPU in place and reports
// whether the rendered template asks for the same value.
func runTownRunnerResize(ctx context.Context, d runnerResizeDeps) error {
	if err := d.Wired(); err != nil {
		return err
	}
	// The flag is judged before anything is read, so a typo touches neither the
	// render nor the cluster.
	cpuMilli, err := parseDindCPU(d.dindCPU)
	if err != nil {
		return err
	}
	m, err := d.manifests()
	if err != nil {
		return err
	}
	namespace := m.Deployment.Metadata.Namespace
	pod, list, err := d.findRunnerPod(ctx, namespace)
	if err != nil {
		return err
	}
	if err := d.checkAllocatable(ctx, pod, cpuMilli); err != nil {
		return err
	}
	if err := d.patchDindCPU(ctx, namespace, pod.Metadata.Name, list, d.dindCPU); err != nil {
		return err
	}
	limit, err := d.readBackDindCPU(ctx, namespace, pod.Metadata.Name, list)
	if err != nil {
		return err
	}
	return d.reportTemplate(ctx, m, limit)
}

// getObject reads one object through kubectl as JSON. The error names what
// could not be read and kubectl's exit status, never its answer, since a pod's
// spec can carry a value that is a secret.
func (d runnerResizeDeps) getObject(ctx context.Context, what string, args ...string) (string, error) {
	args = append(append([]string{"get"}, args...), "-o", "json")
	out, exit, err := d.kubectl(ctx, "", nil, "kubectl", args...)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", what, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("reading %s: kubectl exited %d", what, exit)
	}
	return out, nil
}

// findRunnerPod reads the runner's pods and returns the one the resize changes
// with the spec list its engine sidecar is in. No pod, or more than one, is
// refused rather than guessed at: a resize aimed at the wrong pod changes the
// gate's engine.
func (d runnerResizeDeps) findRunnerPod(ctx context.Context, namespace string) (runnerPod, string, error) {
	out, err := d.getObject(ctx, "the runner pods", "pods", "-n", namespace, "-l", runnerPodsSelector)
	if err != nil {
		return runnerPod{}, "", err
	}
	var listing runnerPodList
	if err := json.Unmarshal([]byte(out), &listing); err != nil {
		return runnerPod{}, "", errors.New("the runner pods' read is not a pod list")
	}
	var live []runnerPod
	for _, pod := range listing.Items {
		if pod.Metadata.DeletionTimestamp == nil {
			live = append(live, pod)
		}
	}
	switch len(live) {
	case 0:
		return runnerPod{}, "", fmt.Errorf("namespace %q holds no runner pod that is not being deleted, so there is nothing to resize", namespace)
	case 1:
	default:
		names := make([]string, 0, len(live))
		for _, pod := range live {
			names = append(names, pod.Metadata.Name)
		}
		return runnerPod{}, "", fmt.Errorf("namespace %q holds %d runner pods (%s), so the resize does not know which to change", namespace, len(live), strings.Join(names, ", "))
	}
	pod := live[0]
	list, err := dindContainerList(pod)
	if err != nil {
		return runnerPod{}, "", fmt.Errorf("pod %s: %w", pod.Metadata.Name, err)
	}
	return pod, list, nil
}

// dindContainerList names the spec list the pod carries its engine sidecar in:
// initContainers while the native sidecar renders it, containers otherwise. A
// pod that carries it in both is refused rather than patched in one of them.
func dindContainerList(pod runnerPod) (string, error) {
	inContainers := containerIndex(pod.Spec.Containers, forgejorunner.DindContainerName) >= 0
	inInitContainers := containerIndex(pod.Spec.InitContainers, forgejorunner.DindContainerName) >= 0
	switch {
	case inContainers && inInitContainers:
		return "", fmt.Errorf("names a %s container in both containers and initContainers, so the resize does not know which to change", forgejorunner.DindContainerName)
	case inInitContainers:
		return "initContainers", nil
	case inContainers:
		return "containers", nil
	default:
		return "", fmt.Errorf("names no %s container in its containers or initContainers", forgejorunner.DindContainerName)
	}
}

// containerIndex is the position of the named container in list, or -1.
func containerIndex(list []runnerContainer, name string) int {
	for i, container := range list {
		if container.Name == name {
			return i
		}
	}
	return -1
}

// checkAllocatable refuses a CPU larger than the node the runner pod runs on
// allocates, since the cluster cannot serve a limit above it.
func (d runnerResizeDeps) checkAllocatable(ctx context.Context, pod runnerPod, wantMilli int64) error {
	name := pod.Spec.NodeName
	if name == "" {
		return fmt.Errorf("pod %s names no node, so the allocatable CPU to check against cannot be read", pod.Metadata.Name)
	}
	out, err := d.getObject(ctx, "node/"+name, "node", name)
	if err != nil {
		return err
	}
	var node struct {
		Status struct {
			Allocatable map[string]string `json:"allocatable"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &node); err != nil {
		return fmt.Errorf("the read of node/%s is not a node object", name)
	}
	allocatable := node.Status.Allocatable["cpu"]
	if allocatable == "" {
		return fmt.Errorf("node/%s names no allocatable CPU, so the resize cannot check its limit", name)
	}
	nodeCores, ok := quantityValue(allocatable)
	if !ok {
		return fmt.Errorf("node/%s allocates %q, which is not a CPU quantity, so the resize cannot check its limit", name, allocatable)
	}
	if big.NewRat(wantMilli, 1000).Cmp(nodeCores) > 0 {
		return fmt.Errorf("--%s %s is more than node/%s allocates (%s)", dindCPUFlag, d.dindCPU, name, allocatable)
	}
	return nil
}

// dindResizePatch is the patch's body: the engine sidecar's CPU limit in the
// list the pod carries it in. A strategic merge keys the container lists by
// name, so only the named container's limit changes.
func dindResizePatch(list, cpu string) string {
	body := map[string]any{
		"spec": map[string]any{
			list: []any{map[string]any{
				"name": forgejorunner.DindContainerName,
				"resources": map[string]any{
					"limits": map[string]any{"cpu": cpu},
				},
			}},
		},
	}
	// The body holds only strings, so the encoder cannot refuse it.
	patch, _ := json.Marshal(body)
	return string(patch)
}

// patchDindCPU is the resize's only write: a subresource patch through kubectl.
// The pod is never deleted, restarted or replaced.
func (d runnerResizeDeps) patchDindCPU(ctx context.Context, namespace, pod, list, cpu string) error {
	patch := dindResizePatch(list, cpu)
	fmt.Fprintf(d.out, "patching pod %s's %s container to cpu %s\n", pod, forgejorunner.DindContainerName, cpu)
	_, exit, err := d.kubectl(ctx, "", nil, "kubectl", "patch", "pod", pod, "-n", namespace, "--subresource", "resize", "--patch", patch)
	if err != nil {
		return fmt.Errorf("patching pod %s: %w", pod, err)
	}
	if exit != 0 {
		return fmt.Errorf("patching pod %s: kubectl exited %d", pod, exit)
	}
	return nil
}

// readBackDindCPU reads the pod after the patch. A resize the pod reports as
// pending or infeasible exits non-zero with that status; one in progress prints
// it and the limit the spec asks for.
func (d runnerResizeDeps) readBackDindCPU(ctx context.Context, namespace, name, list string) (string, error) {
	out, err := d.getObject(ctx, "pod/"+name, "pod", name, "-n", namespace)
	if err != nil {
		return "", err
	}
	var pod runnerPod
	if err := json.Unmarshal([]byte(out), &pod); err != nil {
		return "", fmt.Errorf("the read of pod %s is not a pod object", name)
	}
	status, failed := pod.resizeStatus()
	if status != "" {
		fmt.Fprintf(d.out, "pod %s reports the resize as %s\n", name, status)
	}
	if failed {
		return "", fmt.Errorf("pod %s reports the resize as %s", name, status)
	}
	limit, err := pod.dindCPU(list)
	if err != nil {
		return "", fmt.Errorf("pod %s: %w", name, err)
	}
	fmt.Fprintf(d.out, "pod %s now asks for cpu %s\n", name, limit)
	return limit, nil
}

// resizeStatus is the resize status the pod reports as one line, and whether the
// resize cannot proceed: PodResizePending covers both the Infeasible and the
// Deferred reason, which leave the pod at its old limit.
func (p runnerPod) resizeStatus() (string, bool) {
	for _, condition := range p.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case podResizePending:
			return resizeConditionText("pending", condition), true
		case podResizeInProgress:
			return resizeConditionText("in progress", condition), false
		}
	}
	return "", false
}

// resizeConditionText is a resize condition as its state, reason and message.
func resizeConditionText(state string, condition runnerPodCondition) string {
	text := state
	if condition.Reason != "" {
		text += " (" + condition.Reason + ")"
	}
	if condition.Message != "" {
		text += ": " + condition.Message
	}
	return text
}

// dindCPU is the CPU limit the pod's spec now asks for its engine sidecar.
func (p runnerPod) dindCPU(list string) (string, error) {
	containers := p.Spec.Containers
	if list == "initContainers" {
		containers = p.Spec.InitContainers
	}
	index := containerIndex(containers, forgejorunner.DindContainerName)
	if index < 0 {
		return "", fmt.Errorf("its %s list no longer names the %s container", list, forgejorunner.DindContainerName)
	}
	cpu := containers[index].Resources.Limits["cpu"]
	if cpu == "" {
		return "", fmt.Errorf("its %s %s container reports no cpu limit", list, forgejorunner.DindContainerName)
	}
	return cpu, nil
}

// reportTemplate runs the gate runner's drift compare, then says whether the
// rendered template asks for the CPU the pod now carries. A template that asks
// for another value is named with its path, since the next restart builds the
// pod from the template and reverts the limit.
func (d runnerResizeDeps) reportTemplate(ctx context.Context, m forgejorunner.Manifests, podCPU string) error {
	lines, err := runnerDriftDifferences(ctx, d.kubectl, d.encode, m)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(d.out, "no differences: the live runner matches the rendered manifests")
	} else {
		fmt.Fprintln(d.out, "the live runner differs from the rendered manifests:")
		for _, line := range lines {
			fmt.Fprintln(d.out, line)
		}
	}
	templateCPU, path, err := templateDindCPU(m)
	if err != nil {
		return err
	}
	templateCores, ok := quantityValue(templateCPU)
	if !ok {
		return fmt.Errorf("the rendered template's dind CPU %q is not a CPU quantity", templateCPU)
	}
	podCores, ok := quantityValue(podCPU)
	if !ok {
		return fmt.Errorf("the pod's dind CPU %q is not a CPU quantity", podCPU)
	}
	resource := "deployment/" + m.Deployment.Metadata.Name
	if templateCores.Cmp(podCores) == 0 {
		fmt.Fprintf(d.out, "the rendered template is in step with the pod on %s at %s: both ask for cpu %s\n", resource, path, podCPU)
		return nil
	}
	fmt.Fprintf(d.out, "the rendered template differs from the pod on %s at %s: it asks for cpu %s, not the pod's %s, so the next restart reverts the pod to %s\n", resource, path, templateCPU, podCPU, templateCPU)
	return nil
}

// templateDindCPU is the CPU the rendered pod template asks for the engine
// sidecar, and the JSON path that names it in the Deployment.
func templateDindCPU(m forgejorunner.Manifests) (string, string, error) {
	spec := m.Deployment.Spec.Template.Spec
	if index := renderedContainerIndex(spec.InitContainers, forgejorunner.DindContainerName); index >= 0 {
		return spec.InitContainers[index].Resources.Limits.CPU, fmt.Sprintf("/spec/template/spec/initContainers/%d/resources/limits/cpu", index), nil
	}
	if index := renderedContainerIndex(spec.Containers, forgejorunner.DindContainerName); index >= 0 {
		return spec.Containers[index].Resources.Limits.CPU, fmt.Sprintf("/spec/template/spec/containers/%d/resources/limits/cpu", index), nil
	}
	return "", "", fmt.Errorf("the rendered Deployment %s names no %s container in its pod template", m.Deployment.Metadata.Name, forgejorunner.DindContainerName)
}

// renderedContainerIndex is the position of the named container in list, or -1.
func renderedContainerIndex(list []forgejorunner.Container, name string) int {
	for index, container := range list {
		if container.Name == name {
			return index
		}
	}
	return -1
}

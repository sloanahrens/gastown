package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/runnerroll"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/workspace"
)

// clusterRollOwner is the owner the cluster roll writes into the landing hold,
// apart from the gate runner roll's so a refused release names which roll took
// it.
const clusterRollOwner = "cluster-roll"

var townClusterRollCmd = &cobra.Command{
	Use:   "roll",
	Short: "Apply the daemon namespace's render, holding landings and waiting for runs",
	Long: `Apply the daemon namespace's render to the control cluster, the way a render
change reaches the cluster between rolls. This is the overseer's attended path:
it runs with the operator's own kubeconfig, and it needs no approval record and
no deployer credential.

The render is built the way drift builds its: from the flags the last live
render recorded and the image values file, so run gt town cluster render
--record-flags first. In the render's order the roll applies the claims, the
ServiceAccount, the Services, the Deployments and the tracker-backup CronJob
through kubectl, then waits for each Deployment's rollout and its Available
condition. The Namespace and the Secret documents are not applied: the
namespace the recorded flags name must already exist, and Secrets stay
operator-created. A render that holds a static document, a Secret, an admission
policy or a Role among them, is refused by name before the hold is taken.

Before the hold is taken the roll compares the rendered Deployments with the
live ones and prints every difference. Only the Deployments are compared: a
change to a Service, a claim, the ServiceAccount or the tracker backup CronJob
alone is not a difference this command sees, and those kinds are applied only
beside a Deployment that differs. A cluster whose Deployments already match is
never restarted: the roll says so and exits 0 without taking the hold. An input
it cannot read stops it there too, since an unread object is not one in step.
--dry-run prints the steps, their bounds and the diff lines, and takes no hold
and writes nothing.

The hold is released on every exit path, SIGINT and SIGTERM included, so a
failed apply, rollout or Ready wait releases it and exits non-zero.

A real roll needs the landing hold store and the two wait reads; this build
wires neither, so it refuses before taking the hold and --dry-run is the ready
path. Run it in the town root.

Examples:
  gt town cluster roll --dry-run
  gt town cluster roll --landing-wait 5m --runs-wait 30m --rollout-wait 15m`,
	Args: cobra.NoArgs,
	RunE: runTownClusterRollCmd,
}

func init() {
	townClusterCmd.AddCommand(townClusterRollCmd)

	townClusterRollCmd.Flags().Duration("landing-wait", 10*time.Minute, "longest wait for the landing pass in flight to end")
	townClusterRollCmd.Flags().Duration("runs-wait", 20*time.Minute, "longest wait for the gate and post-land runs to finish")
	townClusterRollCmd.Flags().Duration("rollout-wait", 10*time.Minute, "longest wait for each Deployment's rollout to complete")
	townClusterRollCmd.Flags().Bool("dry-run", false, "print the steps, their bounds and the diff lines; take no hold and apply nothing")
}

// runTownClusterRollCmd is what `gt town cluster roll` runs, in the town the
// working directory is in.
func runTownClusterRollCmd(cmd *cobra.Command, _ []string) error {
	return runTownClusterRollIn(cmd, workspace.FindFromCwdOrError)
}

// runTownClusterRollIn reads the roll's flags from cmd and runs the roll in the
// town findTown names.
func runTownClusterRollIn(cmd *cobra.Command, findTown func() (string, error)) error {
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
	return runTownClusterRoll(clusterRollDepsFor(townRoot, cmd.OutOrStdout(), landingWait, runsWait, rolloutWait, dryRun))
}

// clusterRollDeps is what the cluster roll runs with: the applier, the landing
// hold and its path, the two waits' reads, the clock they poll on, the signal
// source, the bounds and the flags. The production command builds it with
// clusterRollDepsFor.
type clusterRollDeps struct {
	apply       *clusterRollApplier
	store       runnerroll.HoldStore
	holdPath    string
	landing     func(ctx context.Context) (pause string, passes int, known bool, err error)
	runs        func(ctx context.Context) ([]runnerroll.WorkflowRun, error)
	clock       runnerroll.Clock
	now         func() time.Time
	signals     rollSignals
	landingWait time.Duration
	runsWait    time.Duration
	rolloutWait time.Duration
	dryRun      bool
	out         io.Writer
}

// clusterRollDepsFor is the production roll's deps: the applier over the render
// seams, the operator's kubeconfig and kubectl. The hold store and the two wait
// reads are not wired here, as the gate runner roll's are not, so a real roll
// refuses before it takes the hold; --dry-run needs neither.
func clusterRollDepsFor(townRoot string, out io.Writer, landingWait, runsWait, rolloutWait time.Duration, dryRun bool) clusterRollDeps {
	// The command found the town already; the render takes that root rather than
	// searching for one again that the hold path and the render could disagree on.
	render := realClusterRenderDeps(out)
	render.findTown = func() (string, error) { return townRoot, nil }
	return clusterRollDeps{
		apply: &clusterRollApplier{
			render:      render,
			kubectl:     realRunFunc(),
			rolloutWait: rolloutWait,
			out:         out,
		},
		holdPath:    filepath.Join(townRoot, runnerroll.LandingHoldName),
		clock:       wallRollClock{},
		now:         time.Now,
		signals:     liveRollSignals,
		landingWait: landingWait,
		runsWait:    runsWait,
		rolloutWait: rolloutWait,
		dryRun:      dryRun,
		out:         out,
	}
}

// step names step n of the roll, with its bound where it has one. The dry run
// and a real run print the same text.
func (d clusterRollDeps) step(n int) string {
	switch n {
	case 1:
		return fmt.Sprintf("take the landing hold (owner %s, reason %q)", clusterRollOwner, runnerroll.RollPause)
	case 2:
		return fmt.Sprintf("wait for the landing pass in flight to end (bound %s)", d.landingWait)
	case 3:
		return fmt.Sprintf("wait for the gate and post-land runs to finish (bound %s)", d.runsWait)
	case 4:
		return fmt.Sprintf("apply the daemon namespace's manifests, then wait for each Deployment's rollout (bound %s) and its Available condition", d.rolloutWait)
	default:
		return "release the landing hold"
	}
}

// rollContext is the context the cluster roll runs under: the shared signal
// rule's, which a dry run may run without since it takes no hold and only reads.
func (d clusterRollDeps) rollContext() (context.Context, context.CancelFunc) {
	return rollContextFrom(d.signals)
}

// rollContextFrom is the context a roll runs under: its signal source's, with
// the listening stopped after the first signal, so a second Ctrl-C takes the
// default action. A dry run may have no source, since it takes no hold and only
// reads; a real roll refuses that before it starts.
func rollContextFrom(signals rollSignals) (context.Context, context.CancelFunc) {
	if signals == nil {
		return context.Background(), func() {}
	}
	ctx, stop := signals()
	// The first signal cancels the roll. Stop listening on that signal once it
	// does, so a second Ctrl-C takes the default action and ends the process.
	unregister := context.AfterFunc(ctx, stop)
	return ctx, func() {
		// True means the callback never ran, so this call is the one that stops.
		if unregister() {
			stop()
		}
	}
}

// checkSeams refuses a real roll whose hold or waits are not wired, before it
// reads the cluster: a roll that cannot take the hold must not diff anything.
func (d clusterRollDeps) checkSeams() error {
	var missing []string
	if d.store == nil {
		missing = append(missing, "the landing hold store")
	}
	if d.landing == nil {
		missing = append(missing, "the landing health read")
	}
	if d.runs == nil {
		missing = append(missing, "the run list")
	}
	if len(missing) > 0 {
		return fmt.Errorf("a real roll needs %s, and this command does not wire %s yet; --dry-run needs neither", strings.Join(missing, ", "), them(len(missing)))
	}
	return nil
}

// them is "it" for one thing and "them" for more, so the refusal reads as one
// sentence whatever is missing.
func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// runTownClusterRoll runs the roll over the daemon namespace: runnerroll.Run
// owns the steps, and this supplies the namespace's applier, its step names and
// its target. The render is built once, before the hold, and compared with the
// live cluster, so an empty diff never restarts a clean cluster, a refused
// render never takes the hold, and the applied set is the one that was compared.
func runTownClusterRoll(d clusterRollDeps) error {
	if d.out == nil {
		return errors.New("the roll has nowhere to print, so an operator could not see the steps it took")
	}
	if d.apply == nil {
		return errors.New("the roll has no applier, so it cannot apply the daemon namespace")
	}
	// Refuse before the hold: an applier missing a seam it needs would otherwise
	// fail halfway through.
	if err := d.apply.Wired(); err != nil {
		return err
	}
	if !d.dryRun {
		// A roll with no signal source cannot stop on Ctrl-C, and the hold it took
		// would outlive the process.
		if d.signals == nil {
			return errors.New("the roll has no signal source, so an interrupted roll could not stop listening")
		}
		if err := d.checkSeams(); err != nil {
			return err
		}
	}
	ctx, stop := d.rollContext()
	defer stop()

	// Render once, before the hold: the diff, the apply and the ready wait read
	// this render, so a file that changes under the roll cannot make the decision
	// and the applied set disagree. A document the roll does not apply refuses
	// here, before the hold is taken.
	m, err := d.apply.manifests()
	if err != nil {
		return err
	}
	target := clusterRollTarget{applier: d.apply, manifests: m}

	if !d.dryRun {
		lines, err := target.Diff(ctx)
		if err != nil {
			return fmt.Errorf("comparing the %s with the live cluster: %w", d.apply.Name(), err)
		}
		if len(lines) == 0 {
			fmt.Fprintf(d.out, "no differences: %s; no hold was taken and nothing was applied\n", target.DiffMatch())
			return nil
		}
		fmt.Fprintln(d.out, target.DiffHeading())
		for _, line := range lines {
			fmt.Fprintln(d.out, line)
		}
	}
	deps := runnerroll.RollDeps{
		Store:    d.store,
		HoldPath: d.holdPath,
		Owner:    clusterRollOwner,
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
	return runnerroll.Run(ctx, deps, target, opts)
}

// clusterRollTarget is the daemon namespace as runnerroll.Run's target: the one
// render the run built, so the diff, the apply and the ready wait cannot read
// different renders.
type clusterRollTarget struct {
	applier   *clusterRollApplier
	manifests k8sdaemon.Manifests
}

// Name names the target in the roll's messages.
func (t clusterRollTarget) Name() string { return t.applier.Name() }

// DiffMatch is the line the roll prints when the compare finds nothing. Only the
// rendered Deployments are compared, so the line names the Deployments and never
// the namespace or the manifests.
func (t clusterRollTarget) DiffMatch() string {
	return "the live Deployments match the rendered Deployments"
}

// DiffHeading introduces the lines where the Deployments differ.
func (t clusterRollTarget) DiffHeading() string {
	return "the live Deployments differ from the rendered Deployments:"
}

// Diff is the drift compare's lines for every rendered Deployment, each prefixed
// with the resource it describes, or none when the live objects match the render.
func (t clusterRollTarget) Diff(ctx context.Context) ([]string, error) {
	return t.applier.diff(ctx, t.manifests)
}

// Apply writes the apply set through the cluster client, one object per call. An
// apply that fails returns an error, so the roll stops and leaves the caller to
// release the hold.
func (t clusterRollTarget) Apply(ctx context.Context) error {
	return t.applier.apply(ctx, t.manifests)
}

// WaitReady waits for each rendered Deployment's rollout within rolloutWait, then
// reads its Available condition.
func (t clusterRollTarget) WaitReady(ctx context.Context) error {
	return t.applier.waitReady(ctx, t.manifests)
}

// clusterRollApplier is the daemon namespace as the roll applies it: it
// renders from the recorded flags the way drift does, applies the workload
// kinds, and waits for each rendered Deployment.
type clusterRollApplier struct {
	// render builds the manifests from the recorded flags and the values file,
	// and encodes each object for kubectl's stdin.
	render clusterRenderDeps
	// kubectl is the cluster client every read and write goes through, so a test
	// records them and nothing reaches a cluster.
	kubectl update.RunFunc
	// rolloutWait bounds each Deployment's rollout status wait.
	rolloutWait time.Duration
	out         io.Writer
}

// Name names the target in the roll's messages.
func (a clusterRollApplier) Name() string { return "daemon namespace" }

// Wired reports the seam the applier is missing. A roll without one refuses
// before it takes the landing hold rather than failing halfway through.
func (a clusterRollApplier) Wired() error {
	switch {
	case a.kubectl == nil:
		return errors.New("no cluster client is wired, so the roll cannot read or apply the daemon namespace")
	case a.render.findTown == nil || a.render.build == nil || a.render.encode == nil:
		return errors.New("no render seams are wired, so the roll cannot build the manifests to apply")
	case a.render.containerEnv == nil || a.render.readFile == nil || a.render.lstat == nil || a.render.stat == nil:
		return errors.New("no render input seams are wired, so the roll cannot read the values the manifests are built from")
	case a.rolloutWait <= 0:
		return fmt.Errorf("the rollout wait must be longer than zero, not %s", a.rolloutWait)
	}
	return nil
}

// manifests is the one render the run applies: the flags the last live render
// recorded, the image values file and the secrets manifest, built the way drift
// builds them. runTownClusterRoll calls it once and passes the result to the
// diff, the apply and the ready wait. A document the roll does not apply refuses
// the render here, before the hold is taken.
func (a clusterRollApplier) manifests() (k8sdaemon.Manifests, error) {
	root, err := a.render.findTown()
	if err != nil {
		return k8sdaemon.Manifests{}, fmt.Errorf("not in a Gas Town workspace, so the recorded render flags cannot be read: %w", err)
	}
	recordPath := renderFlagsPathIn(root)
	flags, ok, err := loadRenderFlags(recordPath)
	if err != nil {
		return k8sdaemon.Manifests{}, err
	}
	if !ok {
		return k8sdaemon.Manifests{}, fmt.Errorf("no live render has recorded its flags at %s, so there is no render to apply; run a live render with --record-flags first", recordPath)
	}
	// The record is under the root this function just found, so the values
	// assembly takes that same root rather than a second lookup.
	findTown := func() (string, error) { return root, nil }
	v, _, _, err := clusterRenderValues(a.render, flags, findTown)
	if err != nil {
		return k8sdaemon.Manifests{}, err
	}
	m, err := a.render.build(v)
	if err != nil {
		return k8sdaemon.Manifests{}, fmt.Errorf("cannot render the daemon namespace's manifests: %w", err)
	}
	if err := refuseUnappliedDocuments(m); err != nil {
		return k8sdaemon.Manifests{}, err
	}
	return m, nil
}

// refuseUnappliedDocuments refuses a render that holds a static document, naming
// the first by kind and name. The static set can hold a Secret, an admission
// policy, a Role or another control-cluster document; those are applied by hand
// or by the deployer plan, never by a roll.
func refuseUnappliedDocuments(m k8sdaemon.Manifests) error {
	if len(m.Static) == 0 {
		return nil
	}
	object := m.Static[0]
	if object.Kind == "" || object.Name == "" {
		return errors.New("the render holds a static document that names no kind and no name, so the roll cannot name what it refuses; nothing was applied")
	}
	return fmt.Errorf("the render holds the static document %s %q, which the roll does not apply: a Secret, an admission policy, a Role or any other static document is applied by hand or by the deployer plan, and nothing was applied", object.Kind, object.Name)
}

// clusterRollDeploymentTargets is the rendered Deployments in the render's
// order: the daemon's always, the others only when their flags rendered one.
func clusterRollDeploymentTargets(m k8sdaemon.Manifests) []driftTarget {
	targets := []driftTarget{deploymentTarget(m.Deployment.Metadata.Name, m.Deployment)}
	for _, deployment := range []k8sdaemon.Deployment{m.DashboardDeployment, m.TrackerDeployment, m.BdGatewayDeployment} {
		// Build leaves a Deployment zero unless its flags rendered one, so a zero
		// object is one that was not rendered and is not waited for.
		if deployment.Kind != "" {
			targets = append(targets, deploymentTarget(deployment.Metadata.Name, deployment))
		}
	}
	return targets
}

// clusterApplyObject is one document the roll applies: its kind, its name and
// the object the render produced for it.
type clusterApplyObject struct {
	kind   string
	name   string
	object any
}

// clusterRollObjects is the apply set in the render's own order, which is the
// order the render writes its numbered files: the claims, the ServiceAccount,
// the Services, the Deployments and the tracker-backup CronJob. The Namespace is
// left out (the roll applies into the namespace the recorded flags name), and so
// are the Secret documents and the role bindings, which stay operator-created or
// belong to the deployer plan.
func clusterRollObjects(m k8sdaemon.Manifests) []clusterApplyObject {
	var objects []clusterApplyObject
	add := func(kind, name string, object any) {
		objects = append(objects, clusterApplyObject{kind: kind, name: name, object: object})
	}
	for _, claim := range []*k8sdaemon.PersistentVolumeClaim{
		&m.TownStateClaim,
		&m.LandingWorkClaim,
		m.DoltBackupClaim,
		claimIfSet(m.TrackerBackupClaim),
		claimIfSet(m.BdGatewayClaim),
		claimIfSet(m.BdGatewayLogClaim),
	} {
		// A host path replaces the dolt-backup claim, so a nil claim is one that
		// was not rendered and is not applied.
		if claim != nil {
			add("persistentvolumeclaim", claim.Metadata.Name, *claim)
		}
	}
	add("serviceaccount", m.ServiceAccount.Metadata.Name, m.ServiceAccount)
	for _, service := range []k8sdaemon.Service{
		m.ForgejoService,
		m.DashboardService,
		m.TrackerService,
		m.BdGatewayService,
		m.BdGatewayPushService,
	} {
		if service.Kind != "" {
			add("service", service.Metadata.Name, service)
		}
	}
	for _, deployment := range []k8sdaemon.Deployment{
		m.Deployment,
		m.DashboardDeployment,
		m.TrackerDeployment,
		m.BdGatewayDeployment,
	} {
		if deployment.Kind != "" {
			add("deployment", deployment.Metadata.Name, deployment)
		}
	}
	if m.TrackerBackupCronJob.Kind != "" {
		add("cronjob", m.TrackerBackupCronJob.Metadata.Name, m.TrackerBackupCronJob)
	}
	return objects
}

// claimIfSet is c as a pointer, or nil when the render did not write it, so the
// apply set skips a claim that was not rendered.
func claimIfSet(c k8sdaemon.PersistentVolumeClaim) *k8sdaemon.PersistentVolumeClaim {
	if c.Kind == "" {
		return nil
	}
	return &c
}

// diff is the drift compare's lines for every Deployment in the render, each
// prefixed with the resource it describes, or none when the live objects match.
func (a clusterRollApplier) diff(ctx context.Context, m k8sdaemon.Manifests) ([]string, error) {
	d := clusterDriftDeps{
		render:  clusterRenderDeps{encode: a.render.encode},
		kubectl: a.kubectl,
	}
	var lines []string
	for _, target := range clusterRollDeploymentTargets(m) {
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

// apply writes the render's apply set through the cluster client, one object per
// call. A call that fails returns an error, so the roll stops and leaves the
// caller to release the hold.
func (a clusterRollApplier) apply(ctx context.Context, m k8sdaemon.Manifests) error {
	for _, object := range clusterRollObjects(m) {
		if err := a.applyObject(ctx, object); err != nil {
			return err
		}
	}
	return nil
}

// waitReady waits for each rendered Deployment's rollout within rolloutWait, then
// reads its Available condition.
func (a clusterRollApplier) waitReady(ctx context.Context, m k8sdaemon.Manifests) error {
	for _, target := range clusterRollDeploymentTargets(m) {
		if err := waitDeploymentReady(ctx, a.kubectl, a.out, target.namespace, target.resource, a.rolloutWait); err != nil {
			return err
		}
	}
	return nil
}

// applyObject encodes one manifest and hands it to kubectl on stdin. The error
// names the object and kubectl's exit status, never its output, since a
// manifest's fields can hold a value that is a secret.
func (a clusterRollApplier) applyObject(ctx context.Context, object clusterApplyObject) error {
	var body bytes.Buffer
	if err := a.render.encode(&body, object.object); err != nil {
		return fmt.Errorf("building the %s %s: %w", object.kind, object.name, err)
	}
	fmt.Fprintf(a.out, "applying %s/%s\n", object.kind, object.name)
	_, exit, err := a.kubectl(withRunStdin(ctx, body.Bytes()), "", nil, "kubectl", "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("applying the %s %s: %w", object.kind, object.name, err)
	}
	if exit != 0 {
		return fmt.Errorf("applying the %s %s: kubectl exited %d", object.kind, object.name, exit)
	}
	return nil
}

// waitDeploymentReady waits for resource's rollout within bound, then reads its
// Available condition. A rollout status that exits non-zero, or a condition that
// is not True, is an error: a Deployment kubectl could not confirm ready is not
// one, and the roll must not report success.
func waitDeploymentReady(ctx context.Context, kubectl update.RunFunc, out io.Writer, namespace, resource string, bound time.Duration) error {
	fmt.Fprintf(out, "waiting for %s to roll out (bound %s)\n", resource, bound)
	_, exit, err := kubectl(ctx, "", nil, "kubectl", "rollout", "status", resource, "-n", namespace, "--timeout="+bound.String())
	if err != nil {
		return fmt.Errorf("waiting for the rollout of %s: %w", resource, err)
	}
	if exit != 0 {
		return fmt.Errorf("%s did not complete its rollout within %s: kubectl rollout status exited %d", resource, bound, exit)
	}
	name := strings.TrimPrefix(resource, "deployment/")
	status, exit, err := kubectl(ctx, "", nil, "kubectl", "get", "deployment", name, "-n", namespace, "-o", readyConditionJSONPath)
	if err != nil {
		return fmt.Errorf("reading the readiness of %s: %w", resource, err)
	}
	if exit != 0 {
		return fmt.Errorf("reading the readiness of %s: kubectl exited %d", resource, exit)
	}
	if status = strings.TrimSpace(status); status != "True" {
		return fmt.Errorf("%s is not ready: its Available condition is %q", resource, status)
	}
	fmt.Fprintf(out, "%s is ready\n", resource)
	return nil
}

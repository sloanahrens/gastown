package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/deploy/k8s/sbx"
	"github.com/steveyegge/gastown/internal/update"
)

// townClusterDriftCmd compares the live control cluster's Deployments with the
// ones the recorded flags render, without applying anything.
var townClusterDriftCmd = &cobra.Command{
	Use:   "drift",
	Short: "Report the live objects' differences from the render, ignoring image rolls",
	Long: `Render the daemon's Deployment, and the dashboard's, trackerd's and the bd
gateway's when the recorded flags name each, from the flags the last live render
recorded and the image values file. Render the gate runner's Deployment and its
ConfigMap from the checked-in control values. The Services, the claims, the
ServiceAccount and the tracker backup CronJob are compared beside the
Deployments, each one the flags render. Read each live object through kubectl
and print every field the render names and the live object carries
differently.

An image roll is not a difference: the container images, the runner's included,
the polecat-image and restartedAt annotations, and the fields the API server
fills in are skipped, so a cluster in step prints nothing and exits 0. Each
difference prints one line naming its JSON path and a fingerprint of each side,
never either value, since a Deployment's spec or the runner's config.yml can
hold a secret, and the command exits 1.

When the recorded flags, the values file, a render or a live object cannot be
read, the command names what it could not read and exits 2, so a script can tell
that apart from a cluster that differs: an unread object is not one in step.

The admission policies and their bindings, and the update Job's Roles and
RoleBindings, are applied by hand, so no render names them. They are compared with
the documents embedded in this binary from deploy/k8s at the commit it was built
from, so a match means the live object equals the running build's file, not the
working tree's. Every other object the static manifests hold is compared the same
way, read by its kind's scope; a Secret is never read, and each one prints one
line saying it is not compared, since the cluster fills its data. A static object
the cluster does not hold is drift, and every field its document names prints as
absent with exit 1, since the compare exists to notice that the apply never made
it.

Nothing is applied and no file is written; the only cluster calls are the reads.
The flags come from the recorded file, so a live render must have run with
--record-flags first.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runClusterDrift(cmd.Context(), realClusterDriftDeps(cmd.OutOrStdout()))
	},
}

func init() {
	townClusterCmd.AddCommand(townClusterDriftCmd)
}

// clusterDriftDeps is what a drift run takes from the host: the render's own
// seams for the town, the values file and the manifests, the gate runner's
// render, the hand-applied objects' targets from the embedded documents, the
// embedded static set, the checked-in exceptions file and the run's clock, the
// kubectl runner that reads the live objects, and where it prints.
type clusterDriftDeps struct {
	render        clusterRenderDeps
	runner        func() ([]driftTarget, error)
	policyTargets func() ([]driftTarget, error)
	staticObjects func() ([]k8s.StaticObject, error)
	exceptions    func() []byte
	now           func() time.Time
	kubectl       update.RunFunc
	out           io.Writer
	// cluster is the cluster the run compares and execVia the form the sandbox
	// reads take to reach kubectl; the flags that set them are a later bead's.
	cluster string
	execVia string
}

// realClusterDriftDeps is the host's: the render's seams, the runner's render,
// the embedded documents' targets, the embedded static set, the embedded
// exceptions file, the run's clock, kubectl, and the command's output.
func realClusterDriftDeps(out io.Writer) clusterDriftDeps {
	return clusterDriftDeps{
		render:        realClusterRenderDeps(out),
		runner:        realRunnerDriftTargets,
		policyTargets: policyDriftTargets,
		staticObjects: k8s.Static,
		exceptions:    func() []byte { return k8s.DriftExceptionsYAML },
		now:           time.Now,
		kubectl:       realRunFunc(),
		out:           out,
	}
}

// staticPolicyTargets is the hand-applied objects' targets through the seam. A
// deps value built without one fails the run instead of skipping them: a run
// that cannot read the embedded documents must not report a cluster in step.
func (d clusterDriftDeps) staticPolicyTargets() ([]driftTarget, error) {
	if d.policyTargets == nil {
		return nil, errors.New("no embedded admission and RBAC targets are wired, so the hand-applied objects cannot be compared")
	}
	return d.policyTargets()
}

// staticSet is the embedded static objects through the seam. A deps value built
// without one fails the run instead of skipping them: a run that cannot read the
// static set must not report a cluster in step.
func (d clusterDriftDeps) staticSet() ([]k8s.StaticObject, error) {
	if d.staticObjects == nil {
		return nil, errors.New("no embedded static objects are wired, so the static set cannot be compared")
	}
	return d.staticObjects()
}

// runnerTargets is the gate runner's targets through the seam. A deps value built
// without one fails the run instead of panicking on the call: a run that cannot
// render the runner must not report a cluster in step.
func (d clusterDriftDeps) runnerTargets() ([]driftTarget, error) {
	if d.runner == nil {
		return nil, errors.New("no render of the gate runner is wired, so the runner cannot be compared")
	}
	return d.runner()
}

// realRunnerDriftTargets is the gate runner's render: the control cluster's
// checked-in values, built into the Deployment and the ConfigMap the compare
// reads (gt-pkz70c).
func realRunnerDriftTargets() ([]driftTarget, error) {
	return runnerDriftTargetsFrom(forgejorunner.ControlValues, forgejorunner.Build)
}

// runnerDriftTargetsFrom builds the runner's targets from values and build. A
// values file it cannot decode, or a build that refuses, is an error rather than
// a target list that silently skips the runner.
func runnerDriftTargetsFrom(
	values func() (forgejorunner.Values, error),
	build func(forgejorunner.Values) (forgejorunner.Manifests, error),
) ([]driftTarget, error) {
	v, err := values()
	if err != nil {
		return nil, fmt.Errorf("reading the control cluster's runner values: %w", err)
	}
	m, err := build(v)
	if err != nil {
		return nil, fmt.Errorf("rendering the runner: %w", err)
	}
	return runnerDriftTargets(m), nil
}

// runnerDriftTargets is the two objects a runner render puts in the cluster: the
// Deployment that runs it and the ConfigMap that holds config.yml.
func runnerDriftTargets(m forgejorunner.Manifests) []driftTarget {
	return []driftTarget{
		{kind: "Deployment", resource: "deployment/" + m.Deployment.Metadata.Name, namespace: m.Deployment.Metadata.Namespace, object: m.Deployment},
		{kind: "ConfigMap", resource: "configmap/" + m.ConfigMap.Metadata.Name, namespace: m.ConfigMap.Metadata.Namespace, object: m.ConfigMap},
	}
}

// driftExitUnreadable is the status of a run that never compared: the recorded
// flags, the values file, the render or a live Deployment could not be read. It
// is 2 rather than 1 so a caller can tell an unreadable input from a cluster
// that is out of step.
const driftExitUnreadable = 2

// runClusterDrift compares each rendered object, the flags' Deployments and the
// gate runner's Deployment and ConfigMap, with the live one, prints every
// difference, and returns a silent exit 1 when it found one. Every input it
// cannot read ends the run with driftExitUnreadable and an error naming what it
// could not read, never 0, since an unread object is not one in step.
func runClusterDrift(ctx context.Context, d clusterDriftDeps) error {
	err := compareClusterDrift(ctx, d)
	if err == nil {
		return nil
	}
	// A difference is the answer, not a failure to read: it keeps its own code
	// and prints no error line.
	if _, ok := IsSilentExit(err); ok {
		return err
	}
	return &ExitCodeError{Code: driftExitUnreadable, Err: err}
}

// compareClusterDrift is the run behind runClusterDrift: it returns raw errors,
// and runClusterDrift gives every one of them the unreadable exit status.
func compareClusterDrift(ctx context.Context, d clusterDriftDeps) error {
	// Read once, before any kubectl read: an expired entry refuses the run.
	if d.exceptions == nil || d.now == nil {
		return errors.New("the drift exceptions file and the run's clock are not both wired, so no difference can be judged")
	}
	exceptions, err := loadDriftExceptions(d.exceptions(), d.now())
	if err != nil {
		return fmt.Errorf("cannot read the drift exceptions to explain differences: %w", err)
	}
	root, err := d.render.findTown()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace, so the recorded render flags cannot be read: %w", err)
	}
	recordPath := renderFlagsPathIn(root)
	flags, ok, err := loadRenderFlags(recordPath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no live render has recorded its flags at %s, so there is no render to compare the live Deployments with; run a live render with --record-flags first", recordPath)
	}
	// The record is under the root the run already found, so the values assembly
	// takes the same root rather than a second lookup that could disagree with it.
	findTown := func() (string, error) { return root, nil }
	v, _, _, err := clusterRenderValues(d.render, flags, findTown)
	if err != nil {
		return err
	}
	m, err := d.render.build(v)
	if err != nil {
		return fmt.Errorf("cannot render the Deployments to compare: %w", err)
	}
	targets := []driftTarget{deploymentTarget(k8sdaemon.Name, m.Deployment)}
	// Build leaves the dashboard objects zero unless the flags render one, so a
	// cluster running no dashboard is never asked for one. The tracker and the bd
	// gateway follow the same rule.
	if m.DashboardDeployment.Kind != "" {
		targets = append(targets, deploymentTarget(k8sdaemon.DashboardName, m.DashboardDeployment))
	}
	if m.TrackerDeployment.Kind != "" {
		targets = append(targets, deploymentTarget(k8sdaemon.TrackerName, m.TrackerDeployment))
	}
	if m.BdGatewayDeployment.Kind != "" {
		targets = append(targets, deploymentTarget(k8sdaemon.BdGatewayName, m.BdGatewayDeployment))
	}
	// The runner is rendered from the checked-in control values, so its targets
	// exist whether or not the recorded flags name it.
	runnerTargets, err := d.runnerTargets()
	if err != nil {
		return fmt.Errorf("cannot render the gate runner to compare: %w", err)
	}
	targets = append(targets, runnerTargets...)
	// The Services, the claims, the ServiceAccount and the backup CronJob follow
	// the Deployments: a render that names them has more than its workloads to
	// keep in step, and an unread one is not a cluster in step (gt-03u02a).
	targets = append(targets, renderedNamespacedTargets(m)...)
	// The admission policies and the update Job's RBAC are applied by hand, so
	// no render names them; the embedded documents are compared beside the
	// rendered objects (gt-aar839).
	staticTargets, err := d.staticPolicyTargets()
	if err != nil {
		return fmt.Errorf("cannot read the embedded admission and RBAC documents to compare: %w", err)
	}
	targets = append(targets, staticTargets...)
	// Every other static object is compared beside the policy rows' documents,
	// read by its kind's scope; a Secret builds no target and is named as not
	// compared instead (gt-w6g6ok).
	objects, err := d.staticSet()
	if err != nil {
		return fmt.Errorf("cannot read the embedded static set to compare: %w", err)
	}
	staticObjects, notCompared, err := staticObjectTargets(objects, policyTargetsCovered)
	if err != nil {
		return fmt.Errorf("cannot build the static objects' targets to compare: %w", err)
	}
	targets = append(targets, staticObjects...)
	differed := false
	for _, target := range targets {
		lines, absent, err := target.differencesAndAbsence(ctx, d)
		if err != nil {
			return err
		}
		unexplained, explained := lines, []string(nil)
		if !absent {
			unexplained, explained = filterExplained(lines, target.exceptionObject(), exceptions)
		}
		for _, line := range unexplained {
			fmt.Fprintf(d.out, "%s: %s\n", target.resource, line)
		}
		for _, line := range explained {
			fmt.Fprintf(d.out, "%s: explained: %s\n", target.resource, line)
		}
		differed = differed || absent || len(unexplained) > 0
	}
	// The not-compared lines print after every difference, and never count as
	// one: a Secret whose data the cluster fills is named, not drift.
	for _, line := range notCompared {
		fmt.Fprintln(d.out, line)
	}
	if differed {
		return NewSilentExit(1)
	}
	return nil
}

// deploymentTarget is the drift target of a rendered Deployment: the resource
// kubectl reads it by, and the namespace it lives in.
func deploymentTarget(name string, deployment k8sdaemon.Deployment) driftTarget {
	return driftTarget{
		kind:      "Deployment",
		resource:  "deployment/" + name,
		namespace: deployment.Metadata.Namespace,
		object:    deployment,
	}
}

// driftTarget is one object a drift run compares: what kind it is, so a refusal
// names it, the resource kubectl reads it by, its namespace, whether it is
// cluster-scoped, whether the cluster not holding it is drift rather than an
// unreadable input, and the object the render or the embedded file produced for
// it.
type driftTarget struct {
	kind      string
	resource  string
	namespace string
	cluster   bool
	// absentIsDrift reports that a live object the cluster does not hold is a
	// difference to report, not an input that could not be read. A hand-applied
	// static object sets it; a rendered workload does not.
	absentIsDrift bool
	object        any
}

// differences is every rendered field of the target the live object carries
// differently, or the reason either side could not be read. A live object the
// cluster does not hold is every field absent when the target says absence is
// drift, and an unreadable input otherwise.
func (t driftTarget) differences(ctx context.Context, d clusterDriftDeps) ([]string, error) {
	lines, _, err := t.differencesAndAbsence(ctx, d)
	return lines, err
}

// differencesAndAbsence is differences with whether the cluster does not hold the object.
func (t driftTarget) differencesAndAbsence(ctx context.Context, d clusterDriftDeps) ([]string, bool, error) {
	rendered, err := decodeRenderedObject(d, t.object)
	if err != nil {
		return nil, false, fmt.Errorf("the rendered %s cannot be read back: %w", t.resource, err)
	}
	live, absent, err := readLiveObject(ctx, d, t.resource, t.namespace, t.cluster)
	if err != nil {
		return nil, false, err
	}
	if absent {
		if !t.absentIsDrift {
			return nil, false, fmt.Errorf("the cluster does not hold the %s, so it was not read", t.resource)
		}
		lines, err := objectAbsent(t.kind, rendered)
		return lines, true, err
	}
	lines, err := objectDrift(t.kind, rendered, live)
	return lines, false, err
}

func (t driftTarget) exceptionObject() string {
	_, name, _ := strings.Cut(t.resource, "/")
	return t.kind + "/" + t.namespace + "/" + name
}

// decodeRenderedObject decodes the rendered object to the field tree a live
// answer is compared in.
func decodeRenderedObject(d clusterDriftDeps, object any) (map[string]any, error) {
	var body bytes.Buffer
	if err := d.render.encode(&body, object); err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(body.Bytes(), &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// readLiveObject reads resource, such as deployment/daemon, through kubectl: from
// its namespace when the target is namespaced, with no namespace when it is
// cluster-scoped. A target that is namespaced with no namespace, or cluster-scoped
// with one, is refused rather than read against a namespace the target never
// named. The read passes --ignore-not-found, so an object the cluster does not
// hold is an empty answer with exit 0 and reports absent, while a read that
// failed still exits nonzero. An answer that is neither empty nor one JSON object
// is refused, since an unread object must not read as a cluster in step. The
// output stays out of the errors: a Deployment's spec can carry a value that is a
// secret. The render's builders validated its namespace as a DNS-1123 label, and
// the policy targets read the constant, so the namespace reaches kubectl as a
// name it cannot read as a flag.
func readLiveObject(ctx context.Context, d clusterDriftDeps, resource, namespace string, cluster bool) (map[string]any, bool, error) {
	var args []string
	switch {
	case cluster && namespace != "":
		return nil, false, fmt.Errorf("the cluster-scoped %s is named in namespace %q, so it was not read", resource, namespace)
	case !cluster && namespace == "":
		return nil, false, fmt.Errorf("the namespaced %s names no namespace, so it was not read", resource)
	case cluster:
		args = []string{"get", resource, "--ignore-not-found", "-o", "json"}
	default:
		args = []string{"get", resource, "-n", namespace, "--ignore-not-found", "-o", "json"}
	}
	out, exit, err := d.kubectl(ctx, "", nil, "kubectl", args...)
	if err != nil {
		return nil, false, fmt.Errorf("reading the live %s: %w", resource, err)
	}
	if exit != 0 {
		return nil, false, fmt.Errorf("the live %s could not be read: kubectl exited %d", resource, exit)
	}
	if strings.TrimSpace(out) == "" {
		return nil, true, nil
	}
	var live map[string]any
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		return nil, false, fmt.Errorf("the live %s answer is not a JSON object: %w", resource, err)
	}
	return live, false, nil
}

// compareSbxClusterDrift renders the gt-sbx set from the sandbox paths the
// recorded render flags name, then compares it with the sandbox cluster through
// the read runner d.execVia builds. Its status is the compare's: nil for a
// cluster in step, a drift error for a difference, an unreadable one for a read
// that failed.
func compareSbxClusterDrift(ctx context.Context, d clusterDriftDeps) error {
	root, err := d.render.findTown()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace, so the recorded render flags cannot be read: %w", err)
	}
	recordPath := renderFlagsPathIn(root)
	flags, ok, err := loadRenderFlags(recordPath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no live render has recorded its flags at %s, so there is no gt-sbx render to compare the sandbox cluster with; nothing was read", recordPath)
	}
	runner, err := clusterKubectl(d.execVia, "", d.kubectl)
	if err != nil {
		return err
	}
	set, err := d.render.renderSbx(ctx, sbx.Inputs{
		Read:             d.render.readFile,
		Run:              sbxHelmRunner(d.render.runHelm, ""),
		AgentSandboxPath: flags.sbxManifests,
		ChartDir:         flags.sbxChart,
		ValuesPath:       flags.sbxValues,
	})
	if err != nil {
		return fmt.Errorf("cannot render the gt-sbx desired set to compare: %w", err)
	}
	return compareSbxDrift(ctx, d, set, runner)
}

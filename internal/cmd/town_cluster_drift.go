package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// townClusterDriftCmd compares the live control cluster's Deployments with the
// ones the recorded flags render, without applying anything.
var townClusterDriftCmd = &cobra.Command{
	Use:   "drift",
	Short: "Report the live Deployments' differences from the render, ignoring image rolls",
	Long: `Render the daemon's Deployment, and the dashboard's when the recorded flags name
it, from the flags the last live render recorded and the image values file. Read
each of those live Deployments through kubectl and print every field the render
names and the live object carries differently.

An image roll is not a difference: the container images, the polecat-image and
restartedAt annotations, and the fields the API server fills in are skipped, so a
cluster in step prints nothing and exits 0. Each difference prints one line naming
its JSON path and a fingerprint of each side, never either value, since a
Deployment's spec can hold a secret, and the command exits 1.

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
// seams for the town, the values file and the manifests, the kubectl runner that
// reads the live Deployments, and where it prints.
type clusterDriftDeps struct {
	render  clusterRenderDeps
	kubectl update.RunFunc
	out     io.Writer
}

// realClusterDriftDeps is the host's: the render's seams, kubectl, and the
// command's output.
func realClusterDriftDeps(out io.Writer) clusterDriftDeps {
	return clusterDriftDeps{render: realClusterRenderDeps(out), kubectl: realRunFunc(), out: out}
}

// runClusterDrift compares each Deployment the recorded flags render with the live
// one, prints every difference, and returns a silent exit 1 when it found one.
// Every input it cannot read is an error naming what it could not read, never an
// empty difference list, since an unread Deployment is not one in step.
func runClusterDrift(ctx context.Context, d clusterDriftDeps) error {
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
	targets := []driftTarget{{name: k8sdaemon.Name, deployment: m.Deployment}}
	// Build leaves the dashboard objects zero unless the flags render one, so a
	// cluster running no dashboard is never asked for one.
	if m.DashboardDeployment.Kind != "" {
		targets = append(targets, driftTarget{name: k8sdaemon.DashboardName, deployment: m.DashboardDeployment})
	}
	differed := false
	for _, target := range targets {
		lines, err := target.differences(ctx, d)
		if err != nil {
			return err
		}
		for _, line := range lines {
			fmt.Fprintf(d.out, "deployment/%s: %s\n", target.name, line)
		}
		differed = differed || len(lines) > 0
	}
	if differed {
		return NewSilentExit(1)
	}
	return nil
}

// driftTarget is one Deployment a drift run compares: its name in the cluster and
// the object the render produced for it.
type driftTarget struct {
	name       string
	deployment k8sdaemon.Deployment
}

// differences is every rendered field of the target the live Deployment carries
// differently, or the reason either side could not be read.
func (t driftTarget) differences(ctx context.Context, d clusterDriftDeps) ([]string, error) {
	rendered, err := decodeRenderedDeployment(d, t.deployment)
	if err != nil {
		return nil, fmt.Errorf("the rendered %s Deployment cannot be read back: %w", t.name, err)
	}
	live, err := readLiveDeployment(ctx, d, t.name, t.deployment.Metadata.Namespace)
	if err != nil {
		return nil, err
	}
	return deploymentDrift(rendered, live)
}

// decodeRenderedDeployment decodes the rendered object to the field tree a live
// answer is compared in.
func decodeRenderedDeployment(d clusterDriftDeps, deployment k8sdaemon.Deployment) (map[string]any, error) {
	var body bytes.Buffer
	if err := d.render.encode(&body, deployment); err != nil {
		return nil, err
	}
	var object map[string]any
	if err := yaml.Unmarshal(body.Bytes(), &object); err != nil {
		return nil, err
	}
	return object, nil
}

// readLiveDeployment reads the named Deployment's live object through kubectl. An
// answer that is not one JSON object is refused, since an unread object must not
// read as a cluster in step. The output stays out of the errors: a Deployment's
// spec can carry a value that is a secret. Build validated the namespace as a
// DNS-1123 label, so it reaches kubectl as a name it cannot read as a flag.
func readLiveDeployment(ctx context.Context, d clusterDriftDeps, name, namespace string) (map[string]any, error) {
	out, exit, err := d.kubectl(ctx, "", nil, "kubectl", "get", "deployment/"+name, "-n", namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("reading the live deployment/%s: %w", name, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("the live deployment/%s could not be read: kubectl exited %d", name, exit)
	}
	var live map[string]any
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		return nil, fmt.Errorf("the live deployment/%s answer is not a JSON object: %w", name, err)
	}
	return live, nil
}
